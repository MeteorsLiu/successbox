# Platform interception probe

This experiment runs the upstream sandboxexec Go API with a custom platform named `successbox`. It wraps the existing systrap platform and provides an interception point before Sentry dispatches syscalls. The default observer only counts calls. A separate build-tagged probe exercises temporary guest memory and argument rewriting; the Successbox RPC service remains unchanged.

The experiment is a separate Go module, pinned to the same gVisor Go-export revision used by `xgo-dev/sandbox`: `d1e35511e5a41ee5c2afc522d2f9b0c27cf8d382`. The main Successbox module and its default runtime remain unchanged.

## Registration and execution

Both `cmd/runsc` and `cmd/sentry` import `internal/observe`. This matters because the launcher's platform registry is not shared with the Sentry process it starts.

The registered constructor delegates to systrap. Each returned platform context wraps `Switch`; a nil error indicates a syscall trap, while signal and preemption returns pass through without being counted. The wrapper records syscall counts and emits a debug message for `uname`, without recording syscall arguments. All other platform operations delegate to systrap.

`testdata/sandboxexec` uses the unmodified upstream `sandbox.New` and `Sandbox.Exec` APIs. A launcher wrapper selects `--platform=successbox` through the existing `RUNSC_PATH` setting. There is no new sandboxexec option or RPC.

## Build and test

Run these commands from this directory on Linux amd64 or arm64:

```sh
CGO_ENABLED=0 go build -o bin/runsc ./cmd/runsc
CGO_ENABLED=0 go build -o bin/gvisor-bin/gvisor_sentry ./cmd/sentry
CGO_ENABLED=0 go build -o bin/sandboxexec-probe ./testdata/sandboxexec
go test ./internal/observe
```

Keep the two runtime binaries together in the layout above. The Sentry must contain the same platform registration as the launcher. These two binaries are an experimental runtime pair, not a complete production runsc distribution.

For the integration probe, point `RUNSC_PATH` at an executable wrapper that invokes this runsc with `--platform=successbox`. The integration executable takes an existing Linux rootfs and an output directory:

```sh
RUNSC_PATH=/absolute/path/to/wrapper \
  bin/sandboxexec-probe /absolute/path/to/rootfs /tmp/platform-result
```

When the supplied rootfs is on a read-only filesystem, the wrapper must also select a writable backing directory with runsc's existing `--overlay2=all:dir=...` option. The directory must exist before starting the probe. This was already required by the existing Successbox Linux test environment.

## Verified scope

Native Linux arm64 validation used a Linux VM hosted directly by Apple Virtualization.framework, without Docker. The launcher started the separately built `gvisor_sentry`; its logs confirmed observer initialization and five guest `uname` interceptions. Binary stdin/stdout, separate stderr, exit status 7, four concurrent Exec calls, and cleanup passed. Unit tests checked registration and preservation of registers, arguments, signal returns, interruption, preemption, and context release.

The live probe used the pinned runtime's default sidecar policy with the optional C prewarmer absent. No Sentry binaries from another release were mixed into this pair. This run establishes interception and execution behavior, not startup performance or production packaging.

Linux amd64 has compile coverage locally; Linux unit tests and builds for both architectures also run in the Platform workflow. CoW, worktree creation, checkpoint/restore of the wrapper, and non-default systrap tuning flags have not been validated by this experiment. The instrumentation remains local to these experimental binaries.

## Temporary syscall memory

The private `syscallMemory.mmap(address, size)` operation follows the existing sandbox implementation:

- Address zero allocates zeroed guest pages. A nonzero source address copies guest bytes into independent pages; partial reads return the initialized prefix together with an error.
- The returned byte slice aliases the new pages in the Sentry process. The returned address belongs to the guest. Editing the slice is immediately visible through the guest address, but does not change the original source bytes.
- The interceptor assigns guest addresses to syscall arguments and nested pointer fields explicitly. Host Go pointers must never be placed in those fields.
- The slice is borrowed for the synchronous inspection callback. The guest mapping remains until the syscall handler finishes. Restart-related and undispatched allocations are retained conservatively and cleaned up when their task exits. An extra memory-manager reference protects allocations across `execve`.

Cleanup wraps the pinned syscall table in the custom platform constructor, before tasks and tracing are enabled. Installation happens once per process and only when an inspector is installed. The upstream handlers and their results are preserved. No gVisor dependency source or C bridge is modified. The adapted memory implementation's license is in `third_party/sandbox-LICENSE`.

Build the memory integration probe on Linux:

```sh
CGO_ENABLED=0 go build -tags successbox_mmap_probe -o bin/runsc ./cmd/runsc
CGO_ENABLED=0 go build -tags successbox_mmap_probe -o bin/gvisor-bin/gvisor_sentry ./cmd/sentry
CGO_ENABLED=0 go build -o bin/memory-guest ./testdata/memory-guest
CGO_ENABLED=0 go build -o bin/memory-probe ./testdata/memory
```

The tagged policy is test-only: it rewrites fixture paths and payloads and handles reserved `getpid` arguments. Use it only with the probe, not as a general runtime. The host driver uses the Linux host root as the sandbox's read-only lower layer and requires the guest executable to be visible at the same absolute path. Select a separate writable overlay backing directory in the launcher wrapper, then run:

```sh
RUNSC_PATH=/absolute/path/to/wrapper \
  bin/memory-probe /tmp/memory-result /absolute/path/to/bin/memory-guest
```

Native arm64 tests passed zeroed allocation, direct and fragmented page aliases, a longer pathname replacement, explicit `writev` pointer replacement, 1,024 concurrent small-buffer calls, a cross-page larger replacement, invalid/protected/partially readable sources, immediate unmapping after synchronous calls, successful `execve`, interrupted `nanosleep`, task-exit cleanup, and inspection-failure cleanup. Input buffers remained unchanged. Temporary addresses are only for synchronous syscall inputs; they must not escape into retained kernel registrations or be concurrently unmapped/remapped by guest threads.
