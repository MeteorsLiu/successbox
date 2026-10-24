# Successbox prototype

A local gRPC service for creating sandboxes, executing commands, and connecting to an RDP desktop. The frontend receives a sandbox ID; it does not need a guest IP address. The backend never opens a viewer window.

The Linux backend uses gVisor `runsc` directly. Guest `socat` bridges the RDP port through `runsc exec` IO, and the `Display` RPC carries the resulting RDP bytes. A headless RDP desktop must already be installed and started inside the supplied guest filesystem.

## Build

The Go module uses Go 1.27.1.

```sh
make go     # Native sbx client; Linux arm64 and amd64 gVisor backends
make check  # Go tests with the race detector
```

Go protobuf bindings are checked in. `make generate` requires `protoc`, `protoc-gen-go`, and `protoc-gen-go-grpc` in `.tools/bin` or `PATH`.

## Start the service

On Linux, install `runsc` and its accompanying release files, then run the matching backend directly:

```sh
./bin/successbox-gvisor-linux-amd64 \
  --socket /absolute/path/successbox/control.sock \
  --state /absolute/path/successbox/state \
  --runsc /absolute/path/to/runsc
```

The service restricts its control socket to its owner. The prototype has no external guest network: RDP is reached through the runtime transport, not an exposed host port. `--rdp-port` selects the guest port and defaults to 3389.

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

The default init is `/bin/sleep infinity`. To run a desktop, set `desktop` to `true` and supply `initArgv` for the image's startup command. That command must start an RDP server and its headless display/session components. Merely setting `desktop` does not install them. The desktop backend needs `/usr/bin/socat` in the guest image. Configure RDP authentication in the guest; the control protocol does not transmit a desktop password in its metadata.

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

Rootfs changes are temporary. gVisor uses its overlay filestore; closing the sandbox removes its temporary state after the runtime shuts down. Read-only mounts remain read-only, and writable mounts use an overlay without writing changes back to the host source.

Resource limits remain unset unless requested. Explicit limits require a privileged backend with working cgroups.

## Continuous integration

GitHub Actions runs `go test -race ./...` and `go build ./...` on Linux amd64 (`ubuntu-latest`), Linux arm64 (`ubuntu-24.04-arm`), and macOS arm64 (`macos-26`) for pull requests and pushes to `main`. The macOS job checks the shared client and transport tests; the gVisor service itself runs on Linux.

The Go tests exercise binary display forwarding, half-close, cancellation before and after guest PID publication, relay startup failure, invalid stream metadata, and sandbox closure during Display. Backend tests use a controlled runsc stand-in; CI does not boot a sandbox or a graphical desktop.

## Runtime verification

The Linux backends have been cross-compiled for arm64 and amd64. With the test cgroup hierarchy prepared, arm64 runsc passed Create, environment propagation, rootfs write isolation, binary stdin/stdout, separate stderr and exit status, and Close/cleanup inside an Apple-managed Linux VM, without Docker wrapping runsc. The cgroup layout was restored afterward. amd64 currently has build coverage only.

The arm64 backend also passed TTY size, resize, input, merged stderr, and SIGINT/exit checks. An xrdp guest passed certificate-verified TLS negotiation, reconnect, graphical login, an xterm framebuffer capture at 1024×768, and RDP mouse/keyboard input. Reading a file through Exec independently confirmed the text entered over RDP.

The minimal test image reports missing Openbox menu/PyXDG autostart components. These checks launch the terminal explicitly through Exec; desktop menus and autostart are not covered.

This prototype covers sandbox control and RDP transport. It does not yet integrate the frontend, MCP gateway, or LLM gateway.
