# LLM and MCP gateways

`sbx gateways` starts native CLIProxyAPI and Bifrost processes in the trusted host environment. CLIProxyAPI owns model protocols and upstream authentication; the Successbox scheduler plugin binds each accepted client token to the configured Claude/Codex account selector (`base_url#config_index`). Bifrost owns MCP connections, tool discovery and forwarding. Upstream credentials stay in their host-side configuration and authentication stores. The launcher does not rewrite those files or maintain a second credential database.

This step provides host-side gateway process management, the host HTTP/2 GatewaySession control plane, and protocol validation. It does not yet connect the sandbox network, rewrite Agent configuration copies, implement the sensitive-value dialog UI, or apply content redaction. Both sandbox backends still use their existing network configuration. A successful gateway test is not an end-to-end Claude Code/Codex session.

## Run

Validated versions: [CLIProxyAPI v8.0.42](https://github.com/router-for-me/CLIProxyAPI/releases/tag/v8.0.42) and [Bifrost transports v2.3.2](https://github.com/maximhq/bifrost/releases/tag/transports/v2.3.2). Use their native executables for your OS and architecture. `python3 testdata/gateways/install.py .tools/gateways/native` downloads these versions for Linux/macOS amd64/arm64; CLIProxyAPI archives are verified against their release checksums. Bifrost is downloaded from its official versioned binary endpoint.

```sh
go build -o bin/sbx ./cmd/sbx
mkdir -p .state/gateways/mcp
chmod 700 .state/gateways .state/gateways/mcp
cp gateways/llm.example.json .state/gateways/llm.json
cp gateways/mcp.example.json .state/gateways/mcp/config.json
chmod 600 .state/gateways/llm.json .state/gateways/mcp/config.json
```

Replace the example values in the private files with your provider configuration, remote MCP address, upstream credentials and separate gateway keys. The example URL and keys are placeholders. These files, the Bifrost database and the OAuth directory belong outside the Agent's file view.

```sh
./bin/sbx gateways \
  --cliproxy /absolute/path/to/cli-proxy-api \
  --llm-config .state/gateways/llm.json \
  --bifrost /absolute/path/to/bifrost-http \
  --mcp-dir .state/gateways/mcp \
  --mcp-port 8081 \
  --control-listen 127.0.0.1:9090 \
  --control-db .state/gateways/control.db \
  --control-frontend-token frontend-fixture-token \
  --control-gateway-token gateway-fixture-token \
  --control-agent-token agent-fixture-token
```

No sandbox backend `--socket` is needed for this command. The launcher accepts CLIProxyAPI configuration in JSON, which CLIProxyAPI also accepts as YAML input. `server.host` must be a loopback IP. Bifrost is launched on `127.0.0.1`; `--mcp-port` selects its port. LLM and MCP ports must differ. Requests retain the upstream programs' native paths: `/v1/responses`, `/v1/messages`, and Bifrost `/mcp`.

`--control-listen` starts the host-side GatewayManagementService on a TCP HTTP/2 address. This control endpoint is separate from the gVisor/Apple sandbox backend socket and is not exposed inside the guest. `--control-db` is the host SQLite state file. The three control tokens are sent as gRPC metadata (`x-successbox-client` and `authorization: Bearer ...`) by the frontend, gateway adapter, and agent adapter respectively; they are distinct from LLM API keys and Bifrost virtual keys. All three tokens are required when the control endpoint is enabled.

`GatewaySession` uses the messages in [gateway.proto](../api/gateway/v1/gateway.proto). Open a stream with `open_session`, then send requests with distinct `correlation_id` values. The host adapters must choose the sandbox from their trusted binding when opening a stream; later messages cannot change that sandbox. A frontend may open an empty sandbox scope to manage this trusted local user's sandboxes. These control credentials must not be given to a guest or used as a substitute for the sandbox-to-gateway binding.

The control database contains one business table, `sandbox_mcp_secrets`, keyed by `(sandbox_id, mcp_id, field_name)`. `CreateMCPSecret`, `UpdateMCPSecret`, `GetMCPSecret`, `ListMCPSecrets`, and `DeleteMCPSecret` are messages on the existing stream. Only explicit Get responses return values to the frontend; lists, mutation responses, and events return metadata. Updating a nonempty MCP display name updates the other fields of the same sandbox/MCP. New database files are created with mode 0600. This replaces the uncommitted seven-table implementation; there is no legacy database migration.

Missing-value resolutions share an in-memory waiter per key. The frontend can answer `secret_input` with `provide_secret`, or write the value through CRUD; successful database commit wakes the waiting gateway calls. `cancel_secret`, request deadlines, and stream closure end the applicable waits. Reconnection can replay current pending prompts through `resume_session`; it does not replay persisted event history. Stored values survive restarting the control service, while pending requests do not.

Installation requests expire five minutes after their first submission, including time spent waiting for approval or credentials. Duplicate requests, retries and frontend reconnects retain the original deadline. Expiration stops the installation context and emits `MCP_INSTALL_CANCELLED`; an active approval receives `ABORTED`. Success or rejection stops the timer. Gateway shutdown cancels pending operations and stops their timers before closing the store.

The host gateway can send `ClientRequest.cancel_sandbox_operations` (field 10) with its bound `sandbox_id`; the response is `ServerResponse.sandbox_operations_cancelled` (field 9). This is an idempotent operation on the existing `GatewaySession`, not a new RPC. It cancels that sandbox's current installation requests and missing-value waits, waits for active installation I/O to finish, and leaves other sandboxes and saved data intact. Frontend and agent clients receive `PERMISSION_DENIED`; an empty sandbox ID is invalid. This operation does not close a VM, revoke gateway credentials, or roll back changes already committed to Bifrost. The runtime lifecycle owner must stop new task submissions and revoke its bindings when closing a sandbox, then invoke this cancellation hook.

When the control service is enabled, Successbox serves the existing `/mcp` port and runs Bifrost on a private loopback port. The host configuration's active Bifrost virtual-key IDs identify the sandbox scopes used by `GatewaySession`; the matching key value authenticates the data connection. This is the current host binding configuration, not a claim that guest networking or sandbox lifecycle binding is already integrated. Bifrost revalidates data requests and owns tool authorization. The adapter never forwards management routes or guest-supplied management credentials.

The adapter adds `request_mcp_install` to the tool catalog, sends requests to the existing frontend stream, and deduplicates identical pending descriptions. Approval registers the upstream with Bifrost and reads its assigned client ID. An approval event carries that `mcp_id`; missing field values are requested through the existing secret-input flow. After values are saved, the adapter updates that sandbox's Bifrost virtual-key grants and sends `notifications/tools/list_changed` on its MCP SSE session. Agent tools continue to execute through Bifrost. A matching placeholder in a declared top-level tool argument is resolved against the stored sandbox/client/field key immediately before forwarding.

The host adapter supports the existing HTTP and SSE endpoint descriptions; a stdio endpoint is Bifrost's JSON `command`/`args`/`envs` description encoded in the installation request's string field. These credential fields describe tool arguments. Upstream connection authentication remains Bifrost's responsibility; the adapter does not infer whether an argument called `api_key` belongs in an HTTP header or a process environment.

**The native Bifrost adapter test passes the installation workflow.** It covers registration, the real client ID, frontend field storage, the same-session SSE notification, tool execution, endpoint reuse and isolation between A and B. The adapter reads `connection_string.value` and reconstructs the exposed tool name from the client name and the unprefixed management tool name.

**Live Codex 0.163.0 can discover and call the installed tool in the same process and thread after an explicit MCP reconnect.** The test disables and re-enables only its configured MCP through existing app-server configuration APIs, reloading and inspecting that thread's selected server after each change. The next model turn calls `project-search` with the placeholder and receives `successbox-hot-update-ok` from the local upstream through native Bifrost. Notification alone and a reload with unchanged configuration were insufficient in this version: Codex reused the existing connection and its cached catalog. Status queries must include both `threadId` and `serverName`; omitting `serverName` creates an independent discovery snapshot and does not verify the thread's executable catalog. This reconnect is currently a test-only integration, not an automatic production client behavior. The complete sandbox lifecycle flow remains unverified. See [proposal #14](https://github.com/MeteorsLiu/successbox/issues/14) for the workflow and client compatibility requirements.

**Live Claude Code 2.1.310 passed the same installation workflow using native notifications.** It keeps one process and conversation, calls `request_mcp_install`, receives the updated tool catalog after scripted frontend approval, and calls `project-search` without a configuration reload. Its Messages requests pass through the native CLIProxyAPI and Successbox plugin; CLIProxyAPI uses the authorized host-side Codex account for the model in this test. This validates the Claude client and Messages translation, not an Anthropic subscription login.

**The default-policy E2E passes for both clients.** The model is asked to follow the gateway's sensitive-field instructions without receiving the placeholder spelling in the test prompt. Both clients submit an installation request, the scripted frontend approves and saves the value over TCP gRPC, and exactly one subsequent search reaches the upstream with the resolved synthetic value. Codex 0.163.0 uses Code Mode and the explicit reconnect described above; Claude Code 2.1.310 consumes the MCP notification directly. Both model paths use the real CLIProxyAPI and MCP paths use native Bifrost. The installation deadline and sandbox cancellation interface are implemented. The actual sandbox lifecycle caller and production Codex controller are not wired yet; those complete-product integration steps must not be inferred from the host E2E. The frontend UI is outside the current task; its contract and scripted client are included.

Build the Successbox scheduler plugin for the host before starting the gateway pair:

```sh
go -C gateways/cliproxy build -buildmode=c-shared -o /absolute/path/to/successbox-plugin.dylib .
mkdir -p .state/gateways/plugins/darwin/arm64
cp /absolute/path/to/successbox-plugin.dylib .state/gateways/plugins/darwin/arm64/successbox.dylib
```

Use `.so` and the host's Linux architecture on Linux. The `plugins.configs.successbox.bindings` values use `base_url#config_index`, where `config_index` is the position of the provider entry in `api-keys.<provider>`.

The same plugin injects the MCP installation and `$PRIVACY_PROTECT_<field_name>` instructions by default for every bound caller's Claude Messages or Codex Responses request. Injection does not inspect tool names or require an installation tool to appear in the request. It preserves the caller's instructions, tools and history, and avoids duplicating its own marked policy block. For Responses Lite it inserts a developer message; standard Responses uses `instructions`, and Claude uses `system`. Code Mode and requests without any tool list receive the same policy. The client owns tool discovery and execution; the gateway does not construct a second capability list.

The process stays in the foreground. SIGINT/SIGTERM stops both gateway process groups. If either process exits or fails to start, its peer is stopped and the command returns an error. A ten-second shutdown deadline handles a process that does not exit after SIGTERM. The launcher does not claim readiness just because the processes started; the integration test checks the actual protocol endpoints.

Bifrost's app directory includes persistent connection configuration. Reusing a directory can retain a previous upstream address; changing a startup JSON file alone does not necessarily replace existing database state. Use Bifrost's management interface for existing installations. Tests use fresh directories and never reuse an earlier random upstream port.

## Authentication boundaries

LLM client keys go in CLIProxyAPI `access.api-keys`; upstream keys go in `api-keys.claude` or `api-keys.codex`. Bifrost virtual keys select permitted MCP clients and tools; upstream MCP headers remain in its client configuration. Keep the Bifrost administrator password distinct from the MCP virtual key.

CLIProxyAPI already provides host-side `-claude-login`, `-codex-login` and `-codex-device-login` operations using its configured `oauth.auth-dir`. Subscription login and refresh remain owned by CLIProxyAPI. The opt-in live tests below now verify routing through CLIProxyAPI using an explicitly authorized Codex subscription account. They copy the access token, account ID and ID token into a private temporary host store; they do not import the refresh token or modify the user's authentication store. Interactive login and token refresh are not exercised by these tests. The gateway-pair fixture uses synthetic credentials.

A stdio MCP can be connected through Bifrost's `stdio_config`. The fixture verifies this path with a trusted local Python process. Launching a program through Bifrost is not itself OS isolation; production placement and credential permissions for local MCP processes still need integration with the sandbox lifecycle.

## Verify

Run the same check on Linux or macOS with binaries built for that host:

```sh
python3 testdata/gateways/run.py \
  bin/sbx /absolute/path/to/cli-proxy-api /absolute/path/to/bifrost-http /absolute/path/to/successbox-plugin
```

The test launches both actual gateways and a local upstream fixture, using synthetic credentials only. It verifies Codex Responses and Claude Messages in JSON and SSE modes, authenticated remote HTTP MCP, stdio MCP through Bifrost, tool discovery/calls, rejection of incorrect client credentials and listener shutdown. It creates isolated state and local synthetic model catalogs for each run, so protocol checks need no external model/MCP services or catalog downloads. Native CI runs on Linux amd64/arm64 and macOS arm64. Lifecycle unit tests also cover cancellation, child failure and partial startup.

Run the GatewaySession and MCP adapter tests with their fixture backend:

```sh
go test -race ./cmd/sbx -run TestGatewaySession
```

Run the same installation/notification assertions against the actual Bifrost binary:

```sh
SUCCESSBOX_TEST_BIFROST="$PWD/.tools/gateways/bifrost-2.3.2/bifrost-http" go test ./cmd/sbx -run TestGatewaySessionMCPInstallApproval -count=1 -v
```

Both paths use synthetic values. The native test checks a real Bifrost-generated ID, frontend input over TCP gRPC, an SSE notification on the original MCP session, tool invocation with the resolved field, invalid placeholders, frontend reconnection, rejected installation and isolation between sandboxes. The gateway-pair script also verifies that the actual CLIProxyAPI plugin injects the MCP policy into both providers' JSON and streaming requests.

An opt-in live Codex test uses the installed CLI and an explicitly authorized authentication file. It creates a temporary `CODEX_HOME` and leaves the user's configuration and authentication store unchanged. The agent receives only a synthetic gateway key; account credentials reside in CLIProxyAPI's separate temporary host store. The temporary agent configuration preapproves only the fixture's installation and search tools; the thread retains `approvalPolicy: "never"` and the read-only sandbox.

The test keeps one app-server process and thread throughout these steps:

1. Load a catalog containing only `request_mcp_install` and complete an initial model turn.
2. Ask the model to call `request_mcp_install`, require a successful installation-tool result, approve it through the scripted frontend TCP gRPC stream, and store the synthetic field value. Duplicate requests must reuse the pending installation.
3. With `SUCCESSBOX_TEST_CODEX_RECONNECT=1`, set `mcp_servers.successbox.enabled` to `false` using `config/value/write`, reload, and query status with both `threadId` and `serverName` to confirm removal. Then set it to `true`, reload again, and confirm the new tool is present.
4. Ask the model to call `project-search` following the gateway's sensitive-field policy, without telling it the placeholder syntax. Require a real MCP call result containing `successbox-hot-update-ok` and exactly one upstream call receiving the resolved synthetic value.

```sh
SUCCESSBOX_TEST_BIFROST="$PWD/.tools/gateways/bifrost-2.3.2/bifrost-http" \
SUCCESSBOX_TEST_CODEX=/absolute/path/to/codex \
SUCCESSBOX_TEST_CODEX_AUTH=/path/to/authorized/auth.json \
SUCCESSBOX_TEST_CODEX_RECONNECT=1 \
SUCCESSBOX_TEST_CLIPROXY=/absolute/path/to/cli-proxy-api \
SUCCESSBOX_TEST_CLIPROXY_PLUGIN=/absolute/path/to/successbox.dylib \
SUCCESSBOX_TEST_CODEX_MODEL=gpt-5.6-sol \
go test ./cmd/sbx -run TestGatewaySessionMCPInstallApproval -count=1 -v
```

This opt-in case passed with Codex 0.163.0 and Bifrost 2.3.2. Omitting `SUCCESSBOX_TEST_CODEX_RECONNECT=1` retains the plain-reload path that failed to refresh the existing catalog in that Codex version; it does not silently fall back to a new thread. Diagnostics report completed item types, tool names and fixture replies, and count actual upstream calls without logging credentials or tool arguments. Teardown stops the fixture agent process group before closing its HTTP server. The ordinary adapter test does not require real credentials.

The native CLIProxyAPI executable and plugin are required for the live tests, so neither client can silently bypass the LLM gateway. Build the plugin for the host and use `.so` on Linux. `SUCCESSBOX_TEST_CODEX_MODEL` selects a model offered by the authorized account; these runs used `gpt-5.6-sol`.

For the Claude Code variant, omit `SUCCESSBOX_TEST_CODEX` and `SUCCESSBOX_TEST_CODEX_RECONNECT`, and set `SUCCESSBOX_TEST_CLAUDE` to the Claude executable. Keep the Bifrost, proxy, plugin, model and authorized Codex account variables: CLIProxyAPI translates Claude's Messages requests to the selected model. The test uses `--bare`, a private configuration, only the two fixture MCP tools, and `--permission-mode dontAsk`; it does not use the user's Claude login or skip permission checks globally.

The strengthened full-gateway tests passed for both clients, with six model requests each. The test prompt does not supply the placeholder syntax; the upstream must receive the resolved value from exactly one model-driven search. Codex still requires explicit MCP reconnect; Claude uses the native notification. CI runs the credential-free native Bifrost workflow and twelve native LLM protocol cases on the existing Linux amd64/arm64 and macOS matrix: standard Responses, Responses Lite, Code Mode, Responses without tools, Messages, and Messages without tools, each in JSON and streaming modes. Live model tests remain opt-in.

Cancellation tests exercise the real TCP gRPC interface: permissions, scope isolation, idempotence, saved-data retention, interruption of an in-flight registration HTTP request, and expiration while awaiting a credential. A virtual-time test verifies the exact five-minute deadline and proves that a duplicate submitted at four minutes does not extend it. Generated bindings are checked with Linux amd64/arm64 Go builds and a Swift `GatewayProtocol` build.
