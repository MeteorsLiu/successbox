# LLM and MCP gateways

`sbx gateways` starts native CLIProxyAPI and Bifrost processes in the trusted host environment. CLIProxyAPI owns model protocols and upstream authentication; the Successbox scheduler plugin binds each accepted client token to the configured Claude/Codex account selector (`base_url#config_index`). Bifrost owns MCP connections, tool discovery and forwarding. Upstream credentials stay in their host-side configuration and authentication stores. The launcher does not rewrite those files or maintain a second credential database.

This step provides gateway process management and protocol validation. It does not yet connect the sandbox network, rewrite Agent configuration copies, implement the sensitive-value dialog, or apply content redaction. Both sandbox backends still use their existing network configuration. A successful gateway test is not an end-to-end Claude Code/Codex session.

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
  --mcp-port 8081
```

No backend `--socket` is needed for this command. The launcher accepts CLIProxyAPI configuration in JSON, which CLIProxyAPI also accepts as YAML input. `server.host` must be a loopback IP. Bifrost is launched on `127.0.0.1`; `--mcp-port` selects its port. LLM and MCP ports must differ. Requests retain the upstream programs' native paths: `/v1/responses`, `/v1/messages`, and Bifrost `/mcp`.

Build the Successbox scheduler plugin for the host before starting the gateway pair:

```sh
go -C gateways/cliproxy build -buildmode=c-shared -o /absolute/path/to/successbox-plugin.dylib .
mkdir -p .state/gateways/plugins/darwin/arm64
cp /absolute/path/to/successbox-plugin.dylib .state/gateways/plugins/darwin/arm64/successbox.dylib
```

Use `.so` and the host's Linux architecture on Linux. The `plugins.configs.successbox.bindings` values use `base_url#config_index`, where `config_index` is the position of the provider entry in `api-keys.<provider>`.

The process stays in the foreground. SIGINT/SIGTERM stops both gateway process groups. If either process exits or fails to start, its peer is stopped and the command returns an error. A ten-second shutdown deadline handles a process that does not exit after SIGTERM. The launcher does not claim readiness just because the processes started; the integration test checks the actual protocol endpoints.

Bifrost's app directory includes persistent connection configuration. Reusing a directory can retain a previous upstream address; changing a startup JSON file alone does not necessarily replace existing database state. Use Bifrost's management interface for existing installations. Tests use fresh directories and never reuse an earlier random upstream port.

## Authentication boundaries

LLM client keys go in CLIProxyAPI `access.api-keys`; upstream keys go in `api-keys.claude` or `api-keys.codex`. Bifrost virtual keys select permitted MCP clients and tools; upstream MCP headers remain in its client configuration. Keep the Bifrost administrator password distinct from the MCP virtual key.

CLIProxyAPI already provides host-side `-claude-login`, `-codex-login` and `-codex-device-login` operations using its configured `oauth.auth-dir`. Subscription login and refresh remain owned by CLIProxyAPI. Real subscription accounts have not been tested in this step; no user credentials were used for the fixture runs.

A stdio MCP can be connected through Bifrost's `stdio_config`. The fixture verifies this path with a trusted local Python process. Launching a program through Bifrost is not itself OS isolation; production placement and credential permissions for local MCP processes still need integration with the sandbox lifecycle.

## Verify

Run the same check on Linux or macOS with binaries built for that host:

```sh
python3 testdata/gateways/run.py \
  bin/sbx /absolute/path/to/cli-proxy-api /absolute/path/to/bifrost-http /absolute/path/to/successbox-plugin
```

The test launches both actual gateways and a local upstream fixture, using synthetic credentials only. It verifies Codex Responses and Claude Messages in JSON and SSE modes, authenticated remote HTTP MCP, stdio MCP through Bifrost, tool discovery/calls, rejection of incorrect client credentials and listener shutdown. It creates isolated state and local synthetic model catalogs for each run, so protocol checks need no external model/MCP services or catalog downloads. Native CI runs on Linux amd64/arm64 and macOS arm64. Lifecycle unit tests also cover cancellation, child failure and partial startup.

## MCP instructions

The Successbox plugin injects sensitive-field and MCP installation instructions by default for each authenticated bound caller. It does not inspect tool names or require an MCP tool to appear in the request. Claude Messages receives system instructions, standard Responses receives instructions, and Responses Lite receives a developer message. Code Mode and requests without tools receive the same policy. Existing tools, history and caller instructions are preserved; retries do not duplicate the marked policy block.

The policy explains `$PRIVACY_PROTECT_<field_name>` and directs installation through `request_mcp_install`. Actual MCP registration, credential resolution and client catalog refresh are provided by the subsequent gateway-service change; these instructions do not create callable tools or perform content redaction.

The native fixture checks twelve JSON/streaming cases covering Messages, Responses, Responses Lite, Code Mode, and requests without tools.

## GatewaySession contract

The host control protocol is defined in [gateway.proto](../api/gateway/v1/gateway.proto). One bidirectional `GatewaySession` RPC carries correlated requests, replies and events for secret CRUD, missing-value input and MCP installation approval. Authentication determines the caller identity; messages do not declare a role. Installation requests contain no MCP ID or secret value.

This change adds Go bindings and the Swift `GatewayProtocol` generation target. The service implementation follows in the next PR.
