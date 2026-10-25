import ArgumentParser
import Containerization
import ContainerizationEXT4
import ContainerizationOS
import Foundation
import GRPCCore
import GRPCNIOTransportHTTP2
import SandboxProtocol

@main
struct SuccessboxApple: AsyncParsableCommand {
    @Option(help: "Private Unix socket for the control API.") var socket: String
    @Option(help: "Directory for sandbox state.") var state: String
    @Option(help: "ARM64 Linux kernel image.") var kernel: String
    @Option(help: "Containerization initfs.ext4 containing vminitd and vmexec.") var initfs: String
    @Option(help: "RDP port inside desktop sandboxes.") var rdpPort: UInt16 = 3389
    @Option(help: "Linux rootfs directory for hosting the gVisor backend.") var linuxRootfs: String?
    @Option(help: "Directory containing Linux arm64 successbox-gvisor, runsc and gvisor-bin/gvisor_sentry; requires --linux-rootfs.") var runtimeDirectory: String?
    @Option(help: "Memory in MiB for the Linux VM hosting gVisor.") var runtimeMemoryMib: UInt64 = 1024
    @Option(help: "Complete host user directory for the existing tmpfs and worktree mode.") var hostHome: String?
    @Option(help: "Inner gVisor network: none or slirp4netns.") var network: String = "none"
    @Option(help: "slirp4netns executable path inside the Linux VM.") var slirp4netns: String = "/usr/bin/slirp4netns"

    mutating func run() async throws {
        guard rdpPort != 0 else { throw ValidationError("rdp-port must be nonzero") }
        guard network == "none" || network == "slirp4netns" else {
            throw ValidationError("--network must be none or slirp4netns")
        }
        guard network == "none" || (linuxRootfs != nil && slirp4netns.hasPrefix("/")) else {
            throw ValidationError("networking requires gVisor mode and an absolute VM slirp4netns path")
        }
        guard (linuxRootfs == nil) == (runtimeDirectory == nil) else {
            throw ValidationError("--linux-rootfs and --runtime-directory must be provided together")
        }
        guard runtimeMemoryMib > 0,
              runtimeMemoryMib <= (UInt64(Int64.max) - VMResources.guestMemoryOverhead) / (1024 * 1024) else {
            throw ValidationError("--runtime-memory-mib must be nonzero and fit the VM memory range")
        }
        if let hostHome {
            var directory: ObjCBool = false
            guard linuxRootfs != nil, hostHome.hasPrefix("/"), hostHome != "/",
                  FileManager.default.fileExists(atPath: hostHome, isDirectory: &directory), directory.boolValue else {
                throw ValidationError("--host-home requires gVisor mode and an accessible absolute user directory")
            }
        }
        for path in [socket, state, kernel, initfs] {
            guard path.hasPrefix("/") else { throw ValidationError("paths must be absolute") }
        }
        for path in [kernel, initfs] {
            guard FileManager.default.isReadableFile(atPath: path) else {
                throw ValidationError("cannot read \(path)")
            }
        }
        // This standalone daemon owns its creation mask, including the RPC socket.
        umask(0o077)
        try FileManager.default.createDirectory(atPath: state, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        try FileManager.default.createDirectory(at: URL(fileURLWithPath: socket).deletingLastPathComponent(), withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        if let linuxRootfs, let runtimeDirectory {
            // SDK socket relays use write(2). A disconnected RPC client must
            // produce EPIPE instead of terminating the VM owner with SIGPIPE.
            signal(SIGPIPE, SIG_IGN)
            for path in [linuxRootfs, runtimeDirectory] {
                var directory: ObjCBool = false
                guard path.hasPrefix("/"), FileManager.default.fileExists(atPath: path, isDirectory: &directory), directory.boolValue else {
                    throw ValidationError("not an accessible absolute directory: \(path)")
                }
            }
            for name in ["successbox-gvisor", "runsc", "gvisor-bin/gvisor_sentry"] {
                let path = URL(fileURLWithPath: runtimeDirectory).appendingPathComponent(name).path
                guard FileManager.default.isExecutableFile(atPath: path) else {
                    throw ValidationError("runtime executable is missing or not executable: \(path)")
                }
            }
            if hostHome != nil {
                let helper = URL(fileURLWithPath: runtimeDirectory).appendingPathComponent("worktrees").path
                guard FileManager.default.isExecutableFile(atPath: helper) else {
                    throw ValidationError("--host-home requires the worktrees executable in --runtime-directory")
                }
            }
            let root = URL(fileURLWithPath: state)
            let id = UUID().uuidString.lowercased()
            let directory = root.appendingPathComponent(id)
            try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: false, attributes: [.posixPermissions: 0o700])
            let output = OutputBuffer()
            let logging = Task {
                while let event = await output.next() {
                    switch event {
                    case .stdout(let data): try? FileHandle.standardOutput.write(contentsOf: data)
                    case .stderr(let data): try? FileHandle.standardError.write(contentsOf: data)
                    default: break
                    }
                }
            }
            defer { output.discard(); logging.cancel() }
            let (stopRequests, stop) = AsyncStream<Void>.makeStream(bufferingPolicy: .bufferingNewest(1))
            let signals = [SIGINT, SIGTERM].map { number in
                signal(number, SIG_IGN)
                let source = DispatchSource.makeSignalSource(signal: number, queue: .global())
                source.setEventHandler { stop.yield(()) }
                source.resume()
                return source
            }
            defer {
                stop.finish()
                for source in signals { source.cancel() }
            }
            var container: LinuxContainer?
            var vmnet: VmnetNetwork?
            var created = false
            let exitCode: Int32
            do {
                let upper = directory.appendingPathComponent("overlay.ext4")
                let filesystem = try EXT4.Formatter(.init(upper.path), minDiskSize: 4 * 1024 * 1024 * 1024)
                try filesystem.close()
                var config = LinuxContainer.Configuration()
                config.memoryInBytes = runtimeMemoryMib * 1024 * 1024
                if network == "slirp4netns" {
                    vmnet = try VmnetNetwork()
                    if let interface = try vmnet!.createInterface(id) {
                        config.interfaces = [interface]
                        config.dns = .init(nameservers: [vmnet!.ipv4Gateway.description])
                    }
                }
                config.bootLog = .file(path: root.appendingPathComponent("\(id).boot.log"))
                // The VM hosts a runtime. Move its init into a leaf cgroup so
                // runsc can enable controllers for sibling sandbox cgroups.
                config.process.arguments = ["/bin/sh", "-ec", """
                    mkdir -p /run/successbox
                    mkdir /sys/fs/cgroup/successbox-host
                    printf '%s\\n' "$$" > /sys/fs/cgroup/successbox-host/cgroup.procs
                    exec "$@"
                    """, "successbox-linux", "/opt/successbox-runtime/successbox-gvisor",
                    "--socket", "/run/successbox/control.sock", "--state", "/var/lib/successbox",
                    "--runsc", "/opt/successbox-runtime/runsc", "--rdp-port", String(rdpPort)]
                config.process.arguments += ["--network", network, "--slirp4netns", slirp4netns]
                if let hostHome {
                    let backendState = root.appendingPathComponent("gvisor").path
                    let results = URL(fileURLWithPath: backendState).appendingPathComponent("results").path
                    try FileManager.default.createDirectory(atPath: results, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o711])
                    // Only results persist on the host. Runtime sockets and tmpfs
                    // mount points remain on the VM filesystem, outside this share.
                    config.mounts.append(.share(source: results, destination: results))
                    // The trusted helper needs the source Git metadata to register
                    // worktrees. Guest access uses the Linux backend's tmpfs binds.
                    config.mounts.append(.share(source: hostHome, destination: hostHome))
                    config.process.arguments += ["--state", backendState,
                        "--worktrees", "/opt/successbox-runtime/worktrees"]
                }
                config.process.stdout = ProcessOutput(buffer: output, stderr: false)
                config.process.stderr = ProcessOutput(buffer: output, stderr: true)
                let extra: [CapabilityName] = network == "slirp4netns" ? [.sysAdmin, .sysResource, .netAdmin] : [.sysAdmin, .sysResource]
                config.process.capabilities.bounding += extra
                config.process.capabilities.effective += extra
                config.process.capabilities.permitted += extra
                config.mounts.append(.share(source: runtimeDirectory, destination: "/opt/successbox-runtime", options: ["ro"]))
                config.sockets = [UnixSocketConfiguration(
                    source: URL(fileURLWithPath: "/run/successbox/control.sock"),
                    destination: URL(fileURLWithPath: socket),
                    permissions: .init(rawValue: 0o600), direction: .outOf)]
                let vmm = VZVirtualMachineManager(
                    kernel: Kernel(path: URL(fileURLWithPath: kernel), platform: .linuxArm),
                    initialFilesystem: .block(format: "ext4", source: initfs, destination: "/", options: ["ro"]))
                let instance = try LinuxContainer(id,
                    rootfs: .share(source: linuxRootfs, destination: "/", options: ["ro"]),
                    writableLayer: .block(format: "ext4", source: upper.path, destination: "/"),
                    vmm: DirectoryRootfsVMM(base: vmm),
                    vm: VMResources(cpus: config.cpus, memoryInBytes: config.memoryInBytes + VMResources.guestMemoryOverhead),
                    configuration: config)
                container = instance
                try await instance.create()
                created = true
                try FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: socket)
                try await instance.start()
                let shutdown = Task {
                    for await _ in stopRequests {
                        do { try await instance.kill(.term) }
                        catch {
                            try? FileHandle.standardError.write(contentsOf: Data("cannot stop Linux backend: \(error)\n".utf8))
                        }
                    }
                }
                defer { shutdown.cancel() }
                exitCode = try await instance.wait().exitCode
            } catch {
                // Keep the disk if stopping fails: the VM may still use it.
                if created, let container { try await container.stop() }
                if #available(macOS 26, *) { try? vmnet?.releaseInterface(id) }
                try FileManager.default.removeItem(at: directory)
                throw error
            }
            output.finish(.exit(exitCode))
            await logging.value
            try await container!.stop()
            if #available(macOS 26, *) { try? vmnet?.releaseInterface(id) }
            try FileManager.default.removeItem(at: directory)
            if exitCode != 0 { throw ExitCode(exitCode) }
            return
        }
        let store = Sandboxes(root: URL(fileURLWithPath: state), kernel: kernel, initfs: initfs)
        let transport = HTTP2ServerTransport.Posix(address: .unixDomainSocket(path: socket), transportSecurity: .plaintext)
        let server = GRPCServer(transport: transport, services: [SandboxService(store: store, rdpPort: rdpPort)])
        let signals = [SIGINT, SIGTERM].map { number in
            signal(number, SIG_IGN)
            let source = DispatchSource.makeSignalSource(signal: number, queue: .global())
            source.setEventHandler {
                Task {
                    try? FileHandle.standardError.write(contentsOf: Data("received signal \(number); stopping backend\n".utf8))
                    await store.shutdown()
                    server.beginGracefulShutdown()
                }
            }
            source.resume()
            return source
        }
        defer { for source in signals { source.cancel() } }
        do {
            try await server.serve()
            try? FileHandle.standardError.write(contentsOf: Data("RPC server stopped normally\n".utf8))
        } catch {
            try? FileHandle.standardError.write(contentsOf: Data("RPC server failed: \(error)\n".utf8))
            await store.shutdown()
            throw error
        }
    }
}

private struct Sandbox: Sendable {
    let container: LinuxContainer
    let directory: URL
    let options: Successbox_Sandbox_V1_CreateRequest
    var closing: Task<Void, any Error>?
}

private struct DirectoryRootfsVMM: VirtualMachineManager {
    let base: VZVirtualMachineManager

    func create(config: some VMCreationConfig) throws -> any VirtualMachineInstance {
        // VZVirtualMachineManager always constructs this concrete instance.
        let instance = try base.create(config: config) as! VZVirtualMachineInstance
        instance.withMountRegistry { registry in
            for (id, var mounts) in registry {
                guard mounts.first?.type == "virtiofs" else { continue }
                // Containerization 0.53 transforms additional directory mounts
                // this way, but misses rootfs. For example, tag abc is a
                // directory at /run/virtiofs/abc, not a separate virtiofs device.
                mounts[0].type = "none"
                mounts[0].source = "/run/virtiofs/\(mounts[0].source)"
                mounts[0].options = ["bind"] + mounts[0].options
                registry[id] = mounts
            }
        }
        return instance
    }
}

private actor Sandboxes {
    let root: URL
    let vmm: VZVirtualMachineManager
    private var boxes: [String: Sandbox] = [:]
    private var stopping = false

    init(root: URL, kernel: String, initfs: String) {
        self.root = root
        self.vmm = VZVirtualMachineManager(
            kernel: Kernel(path: URL(fileURLWithPath: kernel), platform: .linuxArm),
            initialFilesystem: .block(format: "ext4", source: initfs, destination: "/", options: ["ro"])
        )
    }

    func create(_ request: Successbox_Sandbox_V1_CreateRequest) async throws -> String {
        guard !stopping else { throw RPCError(code: .unavailable, message: "backend is stopping") }
        var isDirectory: ObjCBool = false
        guard request.rootfs.hasPrefix("/"), FileManager.default.fileExists(atPath: request.rootfs, isDirectory: &isDirectory), isDirectory.boolValue else {
            throw RPCError(code: .invalidArgument, message: "rootfs must be an accessible absolute directory")
        }
        guard request.workingDir.isEmpty || request.workingDir.hasPrefix("/") else {
            throw RPCError(code: .invalidArgument, message: "working_dir must be absolute")
        }
        for value in request.env {
            guard let separator = value.firstIndex(of: "="), separator != value.startIndex, !value.contains("\0") else {
                throw RPCError(code: .invalidArgument, message: "invalid environment entry")
            }
        }
        for mount in request.mounts {
            guard mount.source.hasPrefix("/"), mount.destination.hasPrefix("/"), URL(fileURLWithPath: mount.destination).standardized.path != "/", FileManager.default.fileExists(atPath: mount.source) else {
                throw RPCError(code: .invalidArgument, message: "mount paths must be absolute, source must exist, and destination must not replace rootfs")
            }
        }
        let cpus = request.cpus == 0 ? 2 : Int(request.cpus)
        let memory = request.memoryBytes == 0 ? UInt64(1024 * 1024 * 1024) : request.memoryBytes
        guard memory <= UInt64(Int64.max) - VMResources.guestMemoryOverhead else {
            throw RPCError(code: .invalidArgument, message: "memory_bytes is too large")
        }
        let id = UUID().uuidString.lowercased()
        let directory = root.appendingPathComponent(id)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: false, attributes: [.posixPermissions: 0o700])
        var container: LinuxContainer?
        var created = false
        do {
            let upper = directory.appendingPathComponent("overlay.ext4")
            let filesystem = try EXT4.Formatter(.init(upper.path), minDiskSize: 4 * 1024 * 1024 * 1024)
            try filesystem.close()
            var config = LinuxContainer.Configuration()
            // Keep the boot log outside the temporary overlay directory so a
            // failed Create still leaves the guest's diagnostics available.
            config.bootLog = .file(path: root.appendingPathComponent("\(id).boot.log"))
            config.cpus = cpus
            config.memoryInBytes = memory
            if !request.hostname.isEmpty { config.hostname = request.hostname }
            config.process.arguments = request.initArgv.isEmpty ? ["/bin/sleep", "infinity"] : request.initArgv
            config.process.environmentVariables += request.env
            config.process.workingDirectory = request.workingDir.isEmpty ? "/" : request.workingDir
            config.process.user = .init(uid: request.user.uid, gid: request.user.gid)
            if request.desktop {
                let extra: [CapabilityName] = [.sysAdmin, .sysResource, .sysBoot]
                config.process.capabilities.bounding += extra
                config.process.capabilities.effective += extra
                config.process.capabilities.permitted += extra
            }
            for (index, mount) in request.mounts.enumerated() {
                var source = mount.source
                if !mount.readOnly {
                    // Keep writes private, as runsc's overlay does. This backend
                    // snapshots writable shares; it does not write back into the host.
                    let copy = directory.appendingPathComponent("mount-\(index)")
                    try FileManager.default.copyItem(atPath: source, toPath: copy.path)
                    source = copy.path
                }
                config.mounts.append(.share(source: source, destination: mount.destination, options: mount.readOnly ? ["ro"] : []))
            }
            let instance = try LinuxContainer(id,
                rootfs: .share(source: request.rootfs, destination: "/", options: ["ro"]),
                writableLayer: .block(format: "ext4", source: upper.path, destination: "/"),
                vmm: DirectoryRootfsVMM(base: vmm),
                vm: VMResources(cpus: cpus, memoryInBytes: memory + VMResources.guestMemoryOverhead),
                configuration: config)
            container = instance
            try? FileHandle.standardError.write(contentsOf: Data("Create \(id): starting VM and configuring guest\n".utf8))
            try await instance.create()
            created = true
            try? FileHandle.standardError.write(contentsOf: Data("Create \(id): guest configured, starting init\n".utf8))
            try await instance.start()
            try? FileHandle.standardError.write(contentsOf: Data("Create \(id): init started\n".utf8))
            guard !stopping else { throw RPCError(code: .unavailable, message: "backend is stopping") }
            boxes[id] = Sandbox(container: instance, directory: directory, options: request)
            return id
        } catch {
            try? FileHandle.standardError.write(contentsOf: Data("Create \(id) failed: \(error)\n".utf8))
            if created, let container {
                // Preserve the disk if stop fails: a VM may still have it open.
                try await container.stop()
            }
            try FileManager.default.removeItem(at: directory)
            throw error
        }
    }

    func get(_ id: String) throws -> Sandbox {
        guard let box = boxes[id] else { throw RPCError(code: .notFound, message: "sandbox does not exist") }
        guard box.closing == nil else { throw RPCError(code: .failedPrecondition, message: "sandbox is closing") }
        return box
    }

    func close(_ id: String) async throws {
        guard !id.isEmpty else { throw RPCError(code: .invalidArgument, message: "sandbox ID is required") }
        guard var box = boxes[id] else { return }
        let cleanup: Task<Void, any Error>
        if let existing = box.closing {
            cleanup = existing
        } else {
            let container = box.container
            let directory = box.directory
            cleanup = Task {
                try await container.stop()
                try FileManager.default.removeItem(at: directory)
            }
            box.closing = cleanup
            boxes[id] = box
        }
        try await cleanup.value
        boxes.removeValue(forKey: id)
    }

    func shutdown() async {
        stopping = true
        for id in Array(boxes.keys) {
            do { try await close(id) }
            catch { try? FileHandle.standardError.write(contentsOf: Data("sandbox \(id) cleanup failed: \(error)\n".utf8)) }
        }
    }
}

// One producer (the RPC) and one consumer (the guest stdin relay). A send
// waits until the SDK consumes the chunk instead of buffering an entire input.
private actor InputBuffer {
    private var value: Data?
    private var reader: CheckedContinuation<Data?, Never>?
    private var writer: CheckedContinuation<Void, Never>?
    private var finished = false

    func send(_ data: Data) async {
        guard !finished else { return }
        await withCheckedContinuation { continuation in
            if let reader {
                self.reader = nil
                reader.resume(returning: data)
                continuation.resume()
            } else {
                value = data
                writer = continuation
            }
        }
    }

    func next() async -> Data? {
        if let value {
            self.value = nil
            writer?.resume()
            writer = nil
            return value
        }
        if finished { return nil }
        return await withCheckedContinuation { reader = $0 }
    }

    func finish() {
        finished = true
        value = nil
        reader?.resume(returning: nil)
        reader = nil
        writer?.resume()
        writer = nil
    }
}

private struct ProcessInput: ReaderStream {
    let buffer: InputBuffer
    func stream() -> AsyncStream<Data> {
        AsyncStream(unfolding: { await buffer.next() }, onCancel: { Task { await buffer.finish() } })
    }
}

private enum ProcessEvent: Sendable {
    case stdout(Data)
    case stderr(Data)
    case exit(Int32)
    case failure(String)
}

// The SDK calls Writer synchronously on FileHandle readability callbacks.
// Backpressure blocks those callbacks, never a Swift cooperative task. All
// mutable fields below are protected by condition; at most one chunk is queued.
private final class OutputBuffer: @unchecked Sendable {
    private let condition = NSCondition()
    private var value: ProcessEvent?
    private var reader: CheckedContinuation<ProcessEvent?, Never>?
    private var terminal: ProcessEvent?
    private var finished = false

    func write(_ event: ProcessEvent) {
        condition.lock()
        defer { condition.unlock() }
        while value != nil && !finished { condition.wait() }
        if finished { return }
        if let reader {
            self.reader = nil
            reader.resume(returning: event)
        } else {
            value = event
        }
    }

    func next() async -> ProcessEvent? {
        await withTaskCancellationHandler {
            await withCheckedContinuation { continuation in
                condition.lock()
                defer { condition.unlock() }
                if let value {
                    self.value = nil
                    condition.broadcast()
                    continuation.resume(returning: value)
                } else if let terminal {
                    self.terminal = nil
                    continuation.resume(returning: terminal)
                } else if finished {
                    continuation.resume(returning: nil)
                } else {
                    reader = continuation
                }
            }
        } onCancel: { self.discard() }
    }

    func finish(_ event: ProcessEvent) {
        condition.lock()
        defer { condition.unlock() }
        guard !finished else { return }
        finished = true
        if let reader {
            self.reader = nil
            reader.resume(returning: event)
        } else {
            terminal = event
        }
        condition.broadcast()
    }

    func discard() {
        condition.lock()
        defer { condition.unlock() }
        finished = true
        value = nil
        terminal = nil
        reader?.resume(returning: nil)
        reader = nil
        condition.broadcast()
    }
}

private struct ProcessOutput: Writer {
    let buffer: OutputBuffer
    let stderr: Bool
    func write(_ data: Data) throws {
        for offset in stride(from: 0, to: data.count, by: 32768) {
            let chunk = data.subdata(in: offset..<min(offset + 32768, data.count))
            buffer.write(stderr ? .stderr(chunk) : .stdout(chunk))
        }
    }
    // wait() observes both EOFs; the process owner then sends the final result.
    func close() throws {}
}

private struct Execution: Sendable {
    let process: LinuxProcess
    let input: InputBuffer
    let output: OutputBuffer
}

private struct SandboxService: Successbox_Sandbox_V1_SandboxService.SimpleServiceProtocol {
    let store: Sandboxes
    let rdpPort: UInt16

    func create(request: Successbox_Sandbox_V1_CreateRequest, context: ServerContext) async throws -> Successbox_Sandbox_V1_SandboxRef {
        var ref = Successbox_Sandbox_V1_SandboxRef()
        ref.id = try await store.create(request)
        return ref
    }

    func close(request: Successbox_Sandbox_V1_SandboxRef, context: ServerContext) async throws -> Successbox_Sandbox_V1_Empty {
        try await store.close(request.id)
        return .init()
    }

    private func launch(_ box: Sandbox, argv: [String], terminal: Successbox_Sandbox_V1_TerminalSize?) async throws -> Execution {
        guard !argv.isEmpty else { throw RPCError(code: .invalidArgument, message: "argv must not be empty") }
        if let terminal {
            guard (1...65535).contains(terminal.columns), (1...65535).contains(terminal.rows) else {
                throw RPCError(code: .invalidArgument, message: "invalid terminal size")
            }
        }
        let input = InputBuffer()
        let output = OutputBuffer()
        var config = box.container.config.process
        config.arguments = argv
        config.terminal = terminal != nil
        config.stdin = ProcessInput(buffer: input)
        config.stdout = ProcessOutput(buffer: output, stderr: false)
        config.stderr = terminal == nil ? ProcessOutput(buffer: output, stderr: true) : nil
        let process = try await box.container.exec(UUID().uuidString, configuration: config)
        do {
            try await process.start()
            if let terminal { try await process.resize(to: .init(width: UInt16(terminal.columns), height: UInt16(terminal.rows))) }
        } catch {
            output.discard()
            await input.finish()
            try? await process.kill(.kill)
            try? await process.delete()
            throw error
        }
        // This task belongs to the sandbox, not to the lifetime of an Exec RPC.
        Task {
            do {
                let status = try await process.wait()
                try await process.delete()
                output.finish(.exit(status.exitCode))
            } catch {
                output.finish(.failure(String(describing: error)))
                try? await process.delete()
            }
            await input.finish()
        }
        return Execution(process: process, input: input, output: output)
    }

    func exec(request: RPCAsyncSequence<Successbox_Sandbox_V1_ExecInput, any Error>, response: RPCWriter<Successbox_Sandbox_V1_ExecOutput>, context: ServerContext) async throws {
        let (ready, continuation) = AsyncThrowingStream<Execution, any Error>.makeStream(bufferingPolicy: .bufferingOldest(1))
        let incoming = Task {
            var execution: Execution?
            do {
                var iterator = request.makeAsyncIterator()
                guard let first = try await iterator.next(), case .start(let start) = first.payload else {
                    throw RPCError(code: .invalidArgument, message: "first message must start an execution")
                }
                let box = try await store.get(start.sandboxID)
                let call = try await launch(box, argv: start.argv, terminal: start.hasTerminal ? start.terminal : nil)
                execution = call
                if case .terminated = continuation.yield(call) {
                    call.output.discard()
                    await call.input.finish()
                    return
                }
                continuation.finish()
                var closed = false
                while let message = try await iterator.next() {
                    switch message.payload {
                    case .stdin(let data):
                        guard !closed else { throw RPCError(code: .invalidArgument, message: "stdin is already closed") }
                        await call.input.send(data)
                    case .stdinClosed:
                        guard !start.hasTerminal else { throw RPCError(code: .failedPrecondition, message: "PTY input uses terminal control characters, not stdin_closed") }
                        closed = true
                        await call.input.finish()
                    case .signal(let number):
                        guard (1...64).contains(number) else { throw RPCError(code: .invalidArgument, message: "invalid Linux signal") }
                        try await call.process.kill(Signal(rawValue: number))
                    case .resize(let size):
                        guard start.hasTerminal, (1...65535).contains(size.columns), (1...65535).contains(size.rows) else {
                            throw RPCError(code: .invalidArgument, message: "resize requires a PTY and valid dimensions")
                        }
                        try await call.process.resize(to: .init(width: UInt16(size.columns), height: UInt16(size.rows)))
                    default:
                        throw RPCError(code: .invalidArgument, message: "start may only appear once")
                    }
                }
            } catch {
                continuation.finish(throwing: error)
                if !Task.isCancelled { execution?.output.finish(.failure(String(describing: error))) }
            }
            await execution?.input.finish()
        }
        defer { incoming.cancel() }
        var iterator = ready.makeAsyncIterator()
        guard let call = try await iterator.next() else { throw CancellationError() }
        defer {
            call.output.discard()
            Task { await call.input.finish() }
        }
        while let event = await call.output.next() {
            var message = Successbox_Sandbox_V1_ExecOutput()
            switch event {
            case .stdout(let data): message.stdout = data
            case .stderr(let data): message.stderr = data
            case .exit(let code):
                message.result.exitCode = code
                try await response.write(message)
                return
            case .failure(let detail): throw RPCError(code: .internalError, message: detail)
            }
            try await response.write(message)
        }
    }

    func display(request: RPCAsyncSequence<Successbox_Sandbox_V1_DisplayInput, any Error>, response: RPCWriter<Successbox_Sandbox_V1_DisplayOutput>, context: ServerContext) async throws {
        let (ready, continuation) = AsyncThrowingStream<Execution, any Error>.makeStream(bufferingPolicy: .bufferingOldest(1))
        let incoming = Task {
            var execution: Execution?
            do {
                var iterator = request.makeAsyncIterator()
                guard let first = try await iterator.next(), case .connect(let ref) = first.payload else {
                    throw RPCError(code: .invalidArgument, message: "first display message must select a sandbox")
                }
                let box = try await store.get(ref.id)
                guard box.options.desktop else { throw RPCError(code: .failedPrecondition, message: "sandbox has no desktop") }
                // Only the guest loopback RDP port is reachable. A per-viewer
                // socat process bridges stdio over the SDK's existing vsock IO.
                let call = try await launch(box, argv: ["/usr/bin/socat", "STDIO", "TCP:127.0.0.1:\(rdpPort)"], terminal: nil)
                execution = call
                if case .terminated = continuation.yield(call) {
                    call.output.discard()
                    await call.input.finish()
                    try? await call.process.kill(.kill)
                    return
                }
                continuation.finish()
                while let message = try await iterator.next() {
                    guard case .data(let data) = message.payload else {
                        throw RPCError(code: .invalidArgument, message: "connect may only appear once")
                    }
                    await call.input.send(data)
                }
            } catch {
                continuation.finish(throwing: error)
                if !Task.isCancelled { execution?.output.finish(.failure(String(describing: error))) }
            }
            await execution?.input.finish()
        }
        defer { incoming.cancel() }
        var iterator = ready.makeAsyncIterator()
        guard let call = try await iterator.next() else { throw CancellationError() }
        defer {
            call.output.discard()
            Task {
                await call.input.finish()
                try? await call.process.kill(.kill)
            }
        }
        var greeting = Successbox_Sandbox_V1_DisplayOutput()
        greeting.ready.protocol = .rdp
        try await response.write(greeting)
        while let event = await call.output.next() {
            switch event {
            case .stdout(let data):
                var message = Successbox_Sandbox_V1_DisplayOutput()
                message.data = data
                try await response.write(message)
            case .stderr:
                // Relay diagnostics are never inserted into the RDP byte stream.
                break
            case .exit(let code):
                guard code == 0 else { throw RPCError(code: .unavailable, message: "RDP relay exited with status \(code); check guest socat and RDP service") }
                return
            case .failure(let detail): throw RPCError(code: .unavailable, message: detail)
            }
        }
    }
}
