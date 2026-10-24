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

Linux amd64 has compile coverage locally; Linux unit tests and builds for both architectures also run in the Platform workflow. Checkpoint/restore of the wrapper and non-default systrap tuning flags have not been validated by this experiment. The instrumentation remains local to these experimental binaries.

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

## Lazy private repositories

The optional `cmd/worktrees` helper extends the same platform without changing gVisor, sandboxexec's RPC server, or the public Successbox protocol. Supplying runsc's existing `--platform_device_path` connects the platform to a private host socket; the descriptor is donated to Sentry and never installed into the guest FD table. With no device path, the observer retains its previous behavior.

On a pathname syscall, Sentry resolves the path with the task's credentials and discovers the repository through the helper. The helper prepares one independent working directory per source repository, including staged, unstaged, untracked and ignored files. It uses reflinks for regular working files where supported and copies them otherwise. Git objects are independent too. This is lazy per repository, not a whole-system snapshot and not per-file lazy CoW.

The helper serves that directory through the stock LISAFS gofer. Sentry mounts it at the original guest path using existing VFS APIs, so relative paths, directory FDs, symlinks and subsequent ordinary file syscalls use the private tree. Concurrent discovery is serialized to create one copy. Bootstrap cwd references are refreshed after mounting. VFS work uses the kernel's supervisor context because a platform `Switch` still runs in the task's application accounting state; path permission checks explicitly use the task credentials.

During execution, `.git` is private as well. After the sandbox has stopped, terminating the helper with SIGTERM closes its gofers and registers each finished directory as a detached worktree in the original repository. It imports object data and preserves the index, without checking out over either working directory or changing the original branch. Guest-controlled Git configuration and hooks are not executed during publication. The old private metadata remains alongside the result as `worktree.git`. `repositories.json` records original and result paths. A publication failure leaves the result in place and exits with an error.

Build and run the native integration probe as root on Linux:

```sh
CGO_ENABLED=0 go build -o bin/worktrees ./cmd/worktrees
CGO_ENABLED=0 go build -o bin/worktree-probe ./testdata/worktrees
sudo "$(pwd)/bin/worktree-probe" /tmp/successbox-worktrees "$(pwd)/bin/worktrees" "$(pwd)/bin/runsc"
```

Build the normal `runsc`/`gvisor_sentry` pair as above, without the MMap probe tag. The driver creates its own Git fixtures, uses the host root through the unchanged sandboxexec API, starts the helper and checks lazy discovery, dirty/staged/untracked state, guest Git, renames/deletes, directory FD access, symlink/`..` semantics, eight simultaneous first accesses and host-file preservation. CI runs this directly on Linux amd64 and arm64, without Docker. Local arm64 validation used a directly hosted Linux VM.

This is an opt-in prototype. It currently requires a rootful backend; preparation and publication workers run as the guest UID/GID/groups. Read-only bind mounts remain read-only and are not promoted. Writable repositories containing the runtime state directory are rejected. The service integration must hide runtime state and socket paths from the guest. Standalone callers must provide equivalent mounts; this low-level probe is for controlled fixtures.

Copying is not an atomic snapshot against simultaneous host edits. Submodules, nested repository publication, externally stored Git metadata, changing guest roots/mount namespaces and checkpoint/restore are not covered. Repositories without a commit are retained as standalone results if registration cannot complete. Files outside discovered repositories remain in the ordinary disposable overlay; separate non-Git result retention is not part of this step. Mounting a repository also gives its root normal mount-point restrictions, including `EBUSY` for renaming that root.
