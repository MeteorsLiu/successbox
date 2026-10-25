# Successbox

A local gRPC service for creating sandboxes, executing commands, and connecting to an RDP desktop. The frontend receives a sandbox ID; it does not need a guest IP address. The backend never opens a viewer window.

| Host | Runtime | Desktop transport |
| --- | --- | --- |
| Linux | gVisor `runsc` | Guest `socat` bridges the RDP port through `runsc exec` IO |
| macOS, Apple Silicon | Apple Containerization / Virtualization.framework | Guest `socat` bridges the RDP port through Containerization's process IO |
| macOS, Apple Silicon, gVisor mode | Apple Linux VM hosting the Linux gVisor backend | The existing gVisor `Display` stream crosses the VM's socket relay |

All modes carry RDP bytes through the same `Display` RPC. The macOS backend does not use private Apple APIs, virtual graphics, or USB devices. A headless RDP desktop must already be installed and started inside the supplied guest filesystem.

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

### Run gVisor on macOS

The Apple executable can start one Linux VM hosting the existing `successbox-gvisor` service. All sandbox RPCs pass through Containerization's Unix socket/vsock relay. Multiple gVisor sandboxes share that VM; the Swift launcher does not maintain a second sandbox ID table. This uses the Containerization SDK directly and does not require the `container` CLI, containerd, Docker or nested hardware virtualization.

Build the Linux arm64 runtime alongside the signed Apple executable:

```sh
make apple
mkdir -p bin/linux-runtime/gvisor-bin
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o bin/linux-runtime/successbox-gvisor ./cmd/successbox-gvisor
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go -C runtime build -o ../bin/linux-runtime/runsc ./cmd/runsc
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go -C runtime build -o ../bin/linux-runtime/gvisor-bin/gvisor_sentry ./cmd/sentry

./bin/successbox-apple \
  --socket /absolute/path/successbox/control.sock \
  --state /absolute/path/successbox/state \
  --kernel /absolute/path/to/vmlinux-arm64 \
  --initfs /absolute/path/to/initfs.ext4 \
  --linux-rootfs /absolute/path/to/linux-rootfs \
  --runtime-directory "$(pwd)/bin/linux-runtime"
```

The two new options must be supplied together. Without them, the executable continues to provide the original Apple backend. The Linux rootfs must contain `/bin/sh`, `mkdir` and the Linux programs the sandboxes will execute. Both that rootfs and the runtime directory are shared read-only; the VM has a private, sparse 4 GiB ext4 writable layer.

In this mode, `Create.rootfs`, `workingDir` and mount sources refer to paths **inside the Linux VM**. For example, `{"rootfs":"/","workingDir":"/"}` runs a gVisor sandbox using the VM's Linux environment. It does not expose the macOS root filesystem or run macOS applications. Host project sharing and worktree publication back to macOS are outside this startup step.

The Linux init moves itself into `/sys/fs/cgroup/successbox-host` before starting the backend. This leaves the parent free to enable controllers for gVisor's sibling sandbox cgroups; per-sandbox CPU and memory limits remain enabled. The outer runtime receives the existing OCI capabilities plus `CAP_SYS_ADMIN` and `CAP_SYS_RESOURCE` inside its VM. Inner sandbox capabilities remain those of the Linux backend.

The VM uses the pinned SDK's defaults: 4 vCPUs, a 1 GiB outer container memory limit, and 128 MiB of VM overhead. These resources are shared by the Linux backend and its sandboxes. Each `Close` removes only the selected gVisor sandbox. SIGINT or SIGTERM to the Apple launcher asks the Linux backend to close all remaining sandboxes, waits for it to exit, then stops the VM and removes its temporary disk and socket. Boot logs remain under `--state`. Abrupt host termination and restart recovery are not implemented.

The SDK creates the host socket before starting the guest daemon; socket existence alone is not a readiness signal. The forwarded `gVisor backend listening` log indicates that the Linux service has started. Its nonzero exit status is returned by the Apple launcher.

Run the native integration check with prepared kernel, initfs and rootfs assets:

```sh
go build -o bin/sbx ./cmd/sbx
python3 testdata/apple-gvisor.py \
  bin/sbx bin/successbox-apple \
  /absolute/path/to/vmlinux-arm64 /absolute/path/to/initfs.ext4 \
  /absolute/path/to/linux-rootfs bin/linux-runtime
```

The check exercises the socket relay, multiple sandboxes in one VM, filesystem isolation, actual Linux cgroup limits, binary IO, concurrent execution, TTY sizing/resizing/input/Ctrl-C, repeated Close and launcher shutdown with a sandbox still active. It requires macOS 26 or newer on Apple Silicon and runs directly through Virtualization.framework.

For the Display transport, supply a desktop rootfs with `/usr/local/bin/start-desktop` starting xrdp, `/usr/bin/socat`, and its test certificate:

```sh
python3 testdata/apple-gvisor-display.py \
  bin/sbx bin/successbox-apple \
  /absolute/path/to/vmlinux-arm64 /absolute/path/to/initfs.ext4 \
  /absolute/path/to/desktop-rootfs bin/linux-runtime \
  /absolute/path/to/test-rdp-certificate.pem
```

This check negotiates RDP and verifies the TLS certificate through `Display`, disconnects and reconnects, then checks that Exec and cleanup still work. It does not perform graphical login. Separate native validation of this startup mode completed graphical login, a 1024×768 framebuffer capture, a mouse click and keyboard input into xterm; Exec confirmed the terminal received `hello`.

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

See [sandbox.proto](api/sandbox/v1/sandbox.proto). The [frontend API contract](api/README.md) covers sandbox RPCs and GatewaySession, including authentication, message examples, response correlation and current integration gaps.

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

Read-only mounts remain read-only. For writable mounts, gVisor applies its overlay; the direct Apple backend currently copies the supplied source into sandbox state before sharing it. Neither path writes the sandbox's changes back to the host source. Apple mount copies can be expensive for large source directories.

The direct Apple backend defaults to 2 vCPUs and a 1 GiB container memory limit, with the SDK's 128 MiB guest overhead added to VM memory. Linux leaves limits unset unless requested; explicit limits require a privileged backend with working cgroups. The shared VM used by macOS gVisor mode has the separate defaults described above. These defaults are not benchmark settings.

## Continuous integration

Pull requests and pushes to `main` run Go race tests and builds on Linux amd64 (`ubuntu-latest`), Linux arm64 (`ubuntu-24.04-arm`), and macOS arm64 (`macos-26`). A separate `macos-26` job builds the Swift backend and generated protocol bindings, checks that `Package.resolved` stays unchanged, signs the binary with the virtualization entitlement, verifies its signature, and runs `--help`.

The Go transport tests use in-process gRPC fixtures and a controlled runsc stand-in. The main Go transport tests do not boot a guest or perform desktop login. The separate Platform workflow boots Linux guests for worktree checks; desktop validation below was performed separately with compatible kernel, initfs, rootfs, and RDP client assets.

## Verification scope

The Go tests exercise binary display forwarding, half-close, cancellation before and after guest PID publication, relay startup failure, invalid stream metadata, and sandbox closure during Display. Go cross-compilation and a Swift build do not establish that a desktop image boots or that a graphical login works. Runtime checks must also use a compatible kernel/initfs/rootfs and an RDP client on the target host.

Native VM checks on macOS 26.6.1 (arm64) have passed: Create, environment propagation, rootfs write isolation, a 512 KiB binary stdin/stdout roundtrip, separate stderr and exit status, concurrent Exec calls, repeated Close, and temporary state cleanup. An xrdp guest also passed RDP negotiation, TLS 1.3 handshake with verification against the test image's certificate, and disconnect/reconnect checks through `Display`.

The Linux backends have been cross-compiled for arm64 and amd64. With the nested test environment's cgroup hierarchy prepared and restored by the test harness, arm64 runsc passed Create, environment propagation, rootfs write isolation, binary stdin/stdout, separate stderr and exit status, and Close/cleanup inside an Apple-managed Linux VM. Its xrdp guest passed RDP negotiation, certificate-verified TLS 1.3 handshakes, and display reconnect checks with `--network=none`. The Linux runtime was tested in that VM, without Docker wrapping runsc; amd64 currently has build coverage only.

Both arm64 backends passed TTY checks for initial size, resize, interactive input, merged stderr, and SIGINT/exit status. Both also passed full graphical checks using a native FreeRDP 3.32.2 validation client: certificate pinning, password authentication, Xorg/Openbox session startup, a visible xterm window, 1024×768 framebuffer capture, and RDP mouse/keyboard input. The terminal received `hello` through RDP, and reading its output file through Exec independently confirmed the input. The Linux test's cgroup layout was restored and both test VMs closed successfully.

The minimal test image still reports missing Openbox menu/PyXDG autostart components. These checks launch the terminal explicitly through Exec; desktop menus and autostart are not covered.

Successbox covers sandbox control, RDP transport and host-side LLM/MCP gateway process management. See [gateway setup and validation](gateways/README.md). Agent configuration adaptation, the sandbox-to-gateway transport and the frontend are not yet integrated.

## Lazy worktrees on Linux

Build the custom runtime pair and helper from `runtime`, then enable it on the existing Linux service:

```sh
go build -o bin/successbox-gvisor ./cmd/successbox-gvisor
CGO_ENABLED=0 go -C runtime build -o ../bin/worktree-runtime/runsc ./cmd/runsc
CGO_ENABLED=0 go -C runtime build -o ../bin/worktree-runtime/gvisor-bin/gvisor_sentry ./cmd/sentry
CGO_ENABLED=0 go -C runtime build -o ../bin/worktree-runtime/worktrees ./cmd/worktrees
sudo ./bin/successbox-gvisor \
  --socket /var/lib/successbox/control.sock \
  --state /var/lib/successbox \
  --runsc "$(pwd)/bin/worktree-runtime/runsc" \
  --worktrees "$(pwd)/bin/worktree-runtime/worktrees"
```

Use the normal Create request with `rootfs` set to `/`, your working directory and the intended Linux UID/GID. This shares the Linux system as the sandbox's lower filesystem; runsc's overlay keeps ordinary writes private. There is no project-directory allowlist and no dedicated agent image. The `--worktrees` flag is optional; the existing backend behavior is unchanged when it is absent. The helper currently requires a rootful backend, but guest applications and repository workers can run as the user's UID/GID.

Accessing an existing Git repository creates one private copy and redirects pathname syscalls to it. The initial working directory is handled too. Create starts the helper, and Close first stops and deletes the guest, then publishes the private directories as detached worktrees in their original repositories. `git -C /original/project worktree list` shows the retained directories under `STATE/results/SANDBOX_ID/repo-*/worktree`. The original working files and branch are not overwritten. The per-sandbox `repositories.json` records the mapping; publication failures retain the files and make Close return an error.

The backend uses a private mount namespace, a disposable root overlay and private overlays for writable user binds. Only the current sandbox’s result directory is mounted writable at `/run/successbox-results`. Runtime state is hidden with guest tmpfs mounts, including paths exposed through bind aliases. It contains control sockets, manifests and other sandboxes' private results and must not be served through the host-root lower layer. The public Create/Exec/Display/Close protocol is unchanged.

Reflinks are used where supported; otherwise the first access copies that repository's files. This does not snapshot the entire filesystem, but it is not per-file lazy CoW. Separate retention of non-Git files is not implemented in this step: those writes remain in the disposable overlay. See [runtime boundaries](runtime/README.md#worktree-policy) for Git and namespace limitations.

The native Platform CI now boots this custom runtime on Linux amd64 and arm64. It tests the low-level worktree probe and the existing RPC lifecycle with root and UID 1000 guests. `testdata/gvisor-worktrees.sh` is the reproducible service test; it accepts paths to `sbx`, the service, runsc, the helper and a new fixture directory. These checks run directly on Linux, without Docker.
