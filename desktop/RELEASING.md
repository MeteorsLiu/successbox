# Desktop distribution

The release matrix is macOS 26 on Apple Silicon, and Ubuntu 24.04 on amd64/arm64. macOS uses Apple Containerization to host the Linux gVisor backend. Linux runs that backend natively. Docker is not used by either build script or runtime.

## Build and publish

Push a version tag such as `v0.1.0` after merging the application changes. The Desktop App workflow builds the guest payload and native runtime, tests the bundled gateways, runs frontend checks, and makes the application packages. Its final job publishes a GitHub Release only for a tag and only after all three packages succeed. Manual workflow runs and pull requests upload Actions artifacts without publishing a Release.

The release contains a macOS ZIP, two Linux deb packages, and one SHA-256 file per package. Linux packages declare their system-library, PolicyKit, desktop-session and slirp dependencies; install with `sudo apt install ./<package>.deb`, which resolves them. The RDP server itself is extracted into the application resources instead of installing a listening service on the host.

The macOS helper has an ad-hoc signature with the virtualization entitlement. A Developer ID certificate and Apple notarization are not configured, so this is not a notarized distribution: macOS may require approval in Privacy & Security before first launch. Do not describe ad-hoc signing as Gatekeeper approval.

## Bundle contents

| Resource                                                 | Purpose                                                                            |
| -------------------------------------------------------- | ---------------------------------------------------------------------------------- |
| `runtime/host/sbx`                                       | Existing sandbox client and host gateway supervisor                                |
| `runtime/host/cli-proxy-api`, `bifrost-http`, `plugins/` | Native LLM and MCP gateways and account-binding plugin                             |
| `runtime/host/rdp/`                                      | FreeRDP and its bundled libraries; macOS dylibs use bundle-relative paths          |
| `runtime/linux/`                                         | Linux gVisor backend, runsc, Sentry, worktree helper and guest preparation scripts |
| `runtime/guest/` on macOS                                | Linux kernel, Containerization init filesystem and application rootfs archive      |
| `runtime/linux/agent/` on Linux                          | Pinned Linux Codex Desktop package contents, mounted in the private guest view     |
| `runtime/manifest.json`                                  | Platform, architecture, component versions, build commit and file checksums        |

Runtime resources come from source builds and declared upstream downloads. Packaging does not scan or copy the developer's HOME, `.cache`, `.tools`, credentials, recordings or demo state. The Codex Desktop downloads are checked against recorded checksums; an upstream replacement requires review and an explicit checksum update. Debian package versions are recorded with the guest artifact. The Containerization init layer is pinned by digest.

`build-runtime.py` uses a new staging directory. For a local macOS build, first obtain the `apple-guest` Actions artifact, then run from the repository root:

```sh
python3 desktop/scripts/build-runtime.py desktop/.runtime/runtime /absolute/path/to/apple-guest
cd desktop
npm ci
SUCCESSBOX_RELEASE=1 npm run make -- --targets=zip
```

Developer frontend builds without a runtime bundle still use the explicit `SUCCESSBOX_GATEWAY_CONFIG` connection file. `SUCCESSBOX_RELEASE=1` refuses to package if the runtime manifest is absent.
