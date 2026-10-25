# Successbox

A local gRPC service for creating sandboxes, executing commands, and connecting to an RDP desktop. The frontend receives a sandbox ID; it does not need a guest IP address. The backend never opens a viewer window.

| Host | Runtime | Desktop transport |
| --- | --- | --- |
| Linux | gVisor `runsc` | Guest `socat` bridges the RDP port through `runsc exec` IO |
| macOS, Apple Silicon | Apple Containerization / Virtualization.framework | Guest `socat` bridges the RDP port through Containerization's process IO |

Both paths carry RDP bytes through the same `Display` RPC. The macOS backend does not use private Apple APIs, virtual graphics, or USB devices. A headless RDP desktop must already be installed and started inside the supplied guest filesystem.

## Build

The Go module uses Go 1.27.1. The Swift package requires Swift 6.2 or newer and targets macOS 26 or newer; dependencies are pinned in `Package.swift` and `Package.resolved`.

```sh
make go     # Native sbx client; Linux arm64 and amd64 gVisor backends
make apple  # Native Apple backend, ad-hoc signed for virtualization
make check  # Go tests with the race detector
```

Go protobuf bindings are checked in. `make generate` requires `protoc`, `protoc-gen-go`, and `protoc-gen-go-grpc` in `.tools/bin` or `PATH`. SwiftPM generates the Swift bindings from the same schema during its build.

## Start the service

On Linux, install `runsc` and its accompanying release files, then run the matching backend directly:

```sh
./bin/successbox-gvisor-linux-amd64 \
  --socket /absolute/path/successbox/control.sock \
  --state /absolute/path/successbox/state \
  --runsc /absolute/path/to/runsc
```

On macOS, provide an ARM64 Linux kernel and a Containerization `initfs.ext4` containing `vminitd` and `vmexec`:

```sh
./bin/successbox-apple \
  --socket /absolute/path/successbox/control.sock \
  --state /absolute/path/successbox/state \
  --kernel /absolute/path/to/vmlinux-arm64 \
  --initfs /absolute/path/to/initfs.ext4
```

The initfs is separate from the application's rootfs. Both services restrict their control socket to its owner. The service has no external guest network: RDP is reached through the runtime transport, not an exposed host port. `--rdp-port` selects the guest port and defaults to 3389.

## Create, execute, display, close

Create a JSON file using the protobuf field names:

```json
{
  "rootfs": "/absolute/path/to/linux-rootfs",
  "env": ["TERM=xterm-256color"],
  "workingDir": "/",
  "desktop": false
}
```

```sh
./bin/sbx --socket /absolute/path/successbox/control.sock create box.json
./bin/sbx --socket /absolute/path/successbox/control.sock exec --sandbox SANDBOX_ID --tty -- /bin/sh
./bin/sbx --socket /absolute/path/successbox/control.sock close SANDBOX_ID
```

The default init is `/bin/sleep infinity`. To run a desktop, set `desktop` to `true` and supply `initArgv` for the image's startup command. That command must start an RDP server and its headless display/session components. Merely setting `desktop` does not install them. Both desktop backends need `/usr/bin/socat` in the guest image. Configure RDP authentication in the guest; the control protocol does not transmit a desktop password in its metadata.

A frontend implements an RDP client over `Display`. For testing with an existing RDP client, the CLI provides a loopback-only byte proxy:

```sh
./bin/sbx --socket /absolute/path/successbox/control.sock display --sandbox SANDBOX_ID --listen 127.0.0.1:13389
```

Connect the RDP client to that local port. The CLI does not launch it. This optional loopback listener belongs to the client; the daemon itself uses only its private control socket.

## Wire behavior

See [sandbox.proto](api/sandbox/v1/sandbox.proto).

| RPC | Request and response |
| --- | --- |
| `Create` | Filesystem, environment, init, resource and desktop options → sandbox ID |
| `Exec` | First `start { sandbox_id, argv, terminal? }`; then stdin, EOF, signal or resize → stdout/stderr followed by exit code |
| `Display` | First `connect { id }`; then RDP bytes → `ready { protocol: RDP }` followed by RDP bytes |
| `Close` | Sandbox ID → completion after shutdown and state cleanup |

Display data is the RDP wire protocol, not decoded pixel frames. Keyboard and pointer input travel in the same RDP stream. Each new `Display` call creates a connection to the selected sandbox's RDP service. Disconnecting a viewer does not stop the sandbox.

Closing an Exec request stream ends stdin. For an interactive PTY, use terminal control characters rather than `stdin_closed`. An RPC disconnect does not terminate the command: subsequent output is drained and discarded. Reattaching to that command's terminal is not implemented. `Close` ends the sandbox's processes; closing an absent ID succeeds. Lifecycle state is currently held in the daemon, with no restart recovery.

## Filesystems and resources

Rootfs changes are temporary. gVisor uses its overlay filestore. Apple uses a read-only rootfs share with a private, sparse 4 GiB ext4 writable layer. Closing the sandbox removes its temporary state after the runtime shuts down.

When preparing a rootfs directory from an archive on macOS, preserve its file modes, for example with `tar -xpf rootfs.tar --no-same-owner -C rootfs`. Otherwise paths such as `/tmp` can lose their required 1777 permissions. An ordinary macOS user cannot restore arbitrary Linux owner IDs; the desktop test image initializes its `demo` user's home ownership inside the guest overlay before starting the session services.

Read-only mounts remain read-only. For writable mounts, gVisor applies its overlay; Apple currently copies the supplied source into sandbox state before sharing it. Neither path writes the sandbox's changes back to the host source. Apple mount copies can be expensive for large source directories.

Apple defaults to 2 vCPUs and a 1 GiB container memory limit, with the SDK's 128 MiB guest overhead added to VM memory. Linux leaves limits unset unless requested; explicit limits require a privileged backend with working cgroups. These defaults are not benchmark settings.

## Continuous integration

Pull requests and pushes to `main` run Go race tests and builds on Linux amd64 (`ubuntu-latest`), Linux arm64 (`ubuntu-24.04-arm`), and macOS arm64 (`macos-26`). A separate `macos-26` job builds the Swift backend and generated protocol bindings, checks that `Package.resolved` stays unchanged, signs the binary with the virtualization entitlement, verifies its signature, and runs `--help`.

The Go transport tests use in-process gRPC fixtures and a controlled runsc stand-in. CI does not boot a guest or perform desktop login; the runtime checks below were performed separately with compatible kernel, initfs, rootfs, and RDP client assets.

## Verification scope

The Go tests exercise binary display forwarding, half-close, cancellation before and after guest PID publication, relay startup failure, invalid stream metadata, and sandbox closure during Display. Go cross-compilation and a Swift build do not establish that a desktop image boots or that a graphical login works. Runtime checks must also use a compatible kernel/initfs/rootfs and an RDP client on the target host.

Native VM checks on macOS 26.6.1 (arm64) have passed: Create, environment propagation, rootfs write isolation, a 512 KiB binary stdin/stdout roundtrip, separate stderr and exit status, concurrent Exec calls, repeated Close, and temporary state cleanup. An xrdp guest also passed RDP negotiation, TLS 1.3 handshake with verification against the test image's certificate, and disconnect/reconnect checks through `Display`.

The Linux backends have been cross-compiled for arm64 and amd64. With the nested test environment's cgroup hierarchy prepared and restored by the test harness, arm64 runsc passed Create, environment propagation, rootfs write isolation, binary stdin/stdout, separate stderr and exit status, and Close/cleanup inside an Apple-managed Linux VM. Its xrdp guest passed RDP negotiation, certificate-verified TLS 1.3 handshakes, and display reconnect checks with `--network=none`. The Linux runtime was tested in that VM, without Docker wrapping runsc; amd64 currently has build coverage only.

Both arm64 backends passed TTY checks for initial size, resize, interactive input, merged stderr, and SIGINT/exit status. Both also passed full graphical checks using a native FreeRDP 3.32.2 validation client: certificate pinning, password authentication, Xorg/Openbox session startup, a visible xterm window, 1024×768 framebuffer capture, and RDP mouse/keyboard input. The terminal received `hello` through RDP, and reading its output file through Exec independently confirmed the input. The Linux test's cgroup layout was restored and both test VMs closed successfully.

The minimal test image still reports missing Openbox menu/PyXDG autostart components. These checks launch the terminal explicitly through Exec; desktop menus and autostart are not covered.

Successbox currently covers sandbox control and RDP transport. It does not yet integrate the frontend, MCP gateway, or LLM gateway.
