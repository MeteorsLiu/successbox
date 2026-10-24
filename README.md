# Successbox prototype

A local gRPC protocol and command-line client for creating sandboxes, executing commands, and connecting to an RDP desktop. The frontend receives a sandbox ID; it does not need a guest IP address. Runtime backends are added in the following stages.

## Build

The Go module uses Go 1.27.1.

```sh
make go     # Build the sbx client
make check  # Run Go tests with the race detector
```

Go protobuf bindings are checked in. `make generate` requires `protoc`, `protoc-gen-go`, and `protoc-gen-go-grpc` in `.tools/bin` or `PATH`.

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

## Continuous integration

GitHub Actions runs `go test -race ./...` and `go build ./...` on Linux amd64 (`ubuntu-latest`), Linux arm64 (`ubuntu-24.04-arm`), and macOS arm64 (`macos-26`) for pull requests and pushes to `main`. These jobs validate the protocol and client; they do not boot a sandbox or a graphical desktop.

The client tests check binary display forwarding and rejection of missing, unknown, or repeated protocol metadata.
