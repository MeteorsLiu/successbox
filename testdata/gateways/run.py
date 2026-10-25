#!/usr/bin/env python3
"""Run the actual gateway pair against local, credential-checking fixtures."""
import argparse
import json
import os
import platform
import shutil
from pathlib import Path
import signal
import socket
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("sbx", type=Path)
parser.add_argument("cliproxy", type=Path)
parser.add_argument("bifrost", type=Path)
parser.add_argument("plugin", type=Path)
args = parser.parse_args()
client_key = "successbox-fixture-llm-client"
mcp_key = "sk-bf-successbox-fixture-client"
upstream_key = "successbox-fixture-upstream-only"
seen_llm = []
seen_mcp = []


class Fixture(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def reply(self, status, data=b"", content_type="application/json"):
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        self.reply(405)

    def do_DELETE(self):
        self.reply(200)

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        credential = self.headers.get("Authorization", "")
        authenticated = credential == "Bearer " + upstream_key or self.headers.get("x-api-key") == upstream_key
        if not authenticated:
            self.reply(401)
            return
        assert client_key not in str(self.headers)
        assert mcp_key not in str(self.headers)
        if self.path.split("?")[0] == "/mcp":
            method = body["method"]
            seen_mcp.append(method)
            if "id" not in body:
                self.reply(202)
                return
            if method == "initialize":
                result = {"protocolVersion": "2025-06-18", "capabilities": {"tools": {}}, "serverInfo": {"name": "fixture", "version": "1.0.0"}}
            elif method == "tools/list":
                result = {"tools": [{"name": "echo", "description": "Echo fixture text", "inputSchema": {"type": "object", "properties": {"text": {"type": "string"}}, "required": ["text"]}}]}
            elif method == "tools/call":
                result = {"content": [{"type": "text", "text": body["params"]["arguments"]["text"]}], "isError": False}
            else:
                self.reply(200, json.dumps({"jsonrpc": "2.0", "id": body["id"], "error": {"code": -32601, "message": "unsupported fixture method"}}).encode())
                return
            self.reply(200, json.dumps({"jsonrpc": "2.0", "id": body["id"], "result": result}).encode())
            return
        seen_llm.append(self.path)
        prompt = json.dumps(body.get("system", body.get("instructions", "")))
        if isinstance(body.get("input"), list):
            prompt += json.dumps([item for item in body["input"] if item.get("type") == "message" and item.get("role") == "developer"])
        assert "$PRIVACY_PROTECT_api_key" in prompt, "gateway did not inject the MCP placeholder rules"
        assert "mcp_id" in prompt and "request_mcp_install" in prompt, "gateway omitted installation instructions"
        if self.path.split("?")[0].endswith("/messages"):
            result = {"id": "msg_fixture", "type": "message", "role": "assistant", "model": body["model"], "content": [{"type": "text", "text": "fixture reply"}], "stop_reason": "end_turn", "stop_sequence": None, "usage": {"input_tokens": 1, "output_tokens": 2}}
            if not body.get("stream"):
                self.reply(200, json.dumps(result).encode())
                return
            events = [
                {"type": "message_start", "message": {**result, "content": [], "stop_reason": None}},
                {"type": "content_block_start", "index": 0, "content_block": {"type": "text", "text": ""}},
                {"type": "content_block_delta", "index": 0, "delta": {"type": "text_delta", "text": "fixture reply"}},
                {"type": "content_block_stop", "index": 0},
                {"type": "message_delta", "delta": {"stop_reason": "end_turn", "stop_sequence": None}, "usage": {"output_tokens": 2}},
                {"type": "message_stop"},
            ]
        else:
            result = {"id": "resp_fixture", "object": "response", "created_at": 1, "status": "completed", "model": body["model"], "output": [{"id": "msg_fixture", "type": "message", "role": "assistant", "status": "completed", "content": [{"type": "output_text", "text": "fixture reply", "annotations": []}]}], "usage": {"input_tokens": 1, "output_tokens": 2, "total_tokens": 3}}
            events = [
                {"type": "response.created", "response": {**result, "status": "in_progress", "output": []}},
                {"type": "response.output_text.delta", "output_index": 0, "content_index": 0, "item_id": "msg_fixture", "delta": "fixture reply"},
                {"type": "response.completed", "response": result},
            ]
        data = "".join("event: " + event["type"] + "\ndata: " + json.dumps(event) + "\n\n" for event in events).encode()
        self.reply(200, data, "text/event-stream")


fixture = ThreadingHTTPServer(("127.0.0.1", 0), Fixture)
threading.Thread(target=fixture.serve_forever, daemon=True).start()
# Hold both allocations together so the two gateway ports cannot be equal.
ports = []
reservations = []
for _ in range(2):
    reservation = socket.socket()
    reservation.bind(("127.0.0.1", 0))
    reservations.append(reservation)
    ports.append(reservation.getsockname()[1])
llm_port, mcp_port = ports
upstream = "http://127.0.0.1:" + str(fixture.server_port)
process = None
with tempfile.TemporaryDirectory(prefix="successbox-gateways-") as directory:
    root = Path(directory)
    mcp_dir = root / "mcp"
    mcp_dir.mkdir(mode=0o700)
    llm_config = {"config-version": 8, "server": {"host": "127.0.0.1", "port": llm_port}, "management": {"secret-key": "", "disable-control-panel": True}, "access": {"api-keys": [client_key]}, "oauth": {"auth-dir": str(root / "oauth")}, "api-keys": {}, "observability": {"logs": {"request-log": False, "debug": False}}, "routing": {"retry": {"request-retry": 0}}}
    for provider in ["codex", "claude"]:
        model = "successbox-fixture-" + provider
        llm_config["api-keys"][provider] = [{"name": "fixture-" + provider, "base-url": upstream, "proxy-url": "direct", "keys": [{"api-key": upstream_key}], "models": [{"name": model, "alias": model}]}]
    system = platform.system().lower()
    architecture = {"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64"}[platform.machine()]
    extension = ".dylib" if system == "darwin" else ".so"
    plugin_target = root / "plugins" / system / architecture / ("successbox" + extension)
    plugin_target.parent.mkdir(parents=True)
    shutil.copy2(args.plugin, plugin_target)
    llm_config["plugins"] = {"enabled": True, "dir": str(root / "plugins"), "configs": {"successbox": {"enabled": True, "bindings": {client_key: {provider: upstream for provider in ["claude", "codex"]}}}}}
    llm_path = root / "llm.json"
    llm_path.write_text(json.dumps(llm_config))
    llm_path.chmod(0o600)
    mcp_config = {
        "disable_onboarding": True,
        "client": {"enable_logging": False, "disable_content_logging": True, "enforce_governance_header": True, "mcp_disable_auto_tool_inject": True},
        "mcp": {"client_configs": [{"name": "fixture", "connection_type": "http", "connection_string": upstream + "/mcp", "auth_type": "headers", "headers": {"Authorization": "Bearer " + upstream_key}, "tools_to_execute": ["echo"], "allow_by_default": False, "require_public_target": False}]},
        "governance": {
            "auth_config": {"is_enabled": True, "admin_username": "fixture-admin", "admin_password": "successbox-fixture-admin-only"},
            "virtual_keys": [{"id": "successbox-fixture", "name": "successbox-fixture", "value": mcp_key, "is_active": True, "mcp_configs": [{"mcp_client_name": "fixture", "tools_to_execute": ["echo"]}]}],
        },
    }
    mcp_config["mcp"]["client_configs"].append({"name": "local", "connection_type": "stdio", "stdio_config": {"command": sys.executable, "args": [str(Path(__file__).with_name("stdio.py").resolve())]}, "tools_to_execute": ["echo"], "allow_by_default": False})
    mcp_config["governance"]["virtual_keys"][0]["mcp_configs"].append({"mcp_client_name": "local", "tools_to_execute": ["echo"]})
    # Bifrost initializes its model catalog even for MCP-only use. Local
    # synthetic catalog files keep this protocol test independent of DNS.
    pricing = root / "pricing.json"
    parameters = root / "parameters.json"
    pricing.write_text(json.dumps({"successbox-fixture": {"mode": "chat", "litellm_provider": "openai", "input_cost_per_token": 0, "output_cost_per_token": 0}}))
    parameters.write_text("{}")
    mcp_config["framework"] = {"pricing": {"pricing_url": pricing.as_uri(), "model_parameters_url": parameters.as_uri(), "mcp_library_sync_interval": 0, "live_models_sync_interval": 0}}
    config_path = mcp_dir / "config.json"
    config_path.write_text(json.dumps(mcp_config))
    config_path.chmod(0o600)
    log_path = root / "gateways.log"
    with log_path.open("w+") as log:
        try:
            for reservation in reservations:
                reservation.close()
            environment = {"PATH": os.environ["PATH"], "HOME": str(root), "LANG": "C.UTF-8"}
            process = subprocess.Popen([str(args.sbx.resolve()), "gateways", "--cliproxy", str(args.cliproxy.resolve()), "--llm-config", str(llm_path), "--bifrost", str(args.bifrost.resolve()), "--mcp-dir", str(mcp_dir), "--mcp-port", str(mcp_port)], stdout=log, stderr=log, env=environment)
            for port in ports:
                deadline = time.monotonic() + 30
                while True:
                    if process.poll() is not None:
                        raise AssertionError("gateway supervisor exited during startup")
                    try:
                        with socket.create_connection(("127.0.0.1", port), timeout=0.2):
                            break
                    except OSError:
                        if time.monotonic() > deadline:
                            raise AssertionError("gateway startup timed out")
                        time.sleep(0.05)
            llm_base = "http://127.0.0.1:" + str(llm_port)
            for provider, shape, endpoint in [("codex", "standard", "/v1/responses"), ("codex", "lite", "/v1/responses"), ("codex", "code-mode", "/v1/responses"), ("codex", "no-tools", "/v1/responses"), ("claude", "standard", "/v1/messages"), ("claude", "no-tools", "/v1/messages")]:
                for stream in [False, True]:
                    body = {"model": "successbox-fixture-" + provider, "stream": stream}
                    tool = {"name": "mcp__successbox__request_mcp_install", "description": "Request an MCP installation"}
                    if provider == "codex":
                        body["input"] = "fixture request"
                        body["tools"] = [{**tool, "type": "function", "parameters": {"type": "object", "properties": {}}}]
                        if shape in ("lite", "code-mode"):
                            body["input"] = [
                                {"type": "additional_tools", "role": "developer", "tools": [{"type": "namespace", "name": "mcp__successbox", "tools": [{"type": "function", "name": "request_mcp_install", "parameters": {"type": "object", "properties": {}}}]}]},
                                {"type": "message", "role": "user", "content": [{"type": "input_text", "text": "fixture request"}]},
                            ]
                            if shape == "code-mode":
                                body["input"][0]["tools"] = [{"type": "namespace", "name": "functions", "tools": [{"type": "custom", "name": "exec", "description": "Execute JavaScript against the client tool runtime"}]}]
                            del body["tools"]
                    else:
                        body.update(messages=[{"role": "user", "content": "fixture request"}], max_tokens=32)
                        body["tools"] = [{**tool, "input_schema": {"type": "object", "properties": {}}}]
                    if shape == "no-tools":
                        del body["tools"]
                    request = urllib.request.Request(llm_base + endpoint, data=json.dumps(body).encode(), headers={"Authorization": "Bearer " + client_key, "Content-Type": "application/json"})
                    with urllib.request.urlopen(request, timeout=15) as response:
                        result = response.read()
                    assert b"fixture reply" in result, result
                    assert upstream_key.encode() not in result
                    if stream:
                        assert (b"response.completed" if provider == "codex" else b"message_stop") in result
                    print(provider, shape, "SSE" if stream else "JSON", "PASS", flush=True)
            try:
                urllib.request.urlopen(urllib.request.Request(llm_base + "/v1/models", headers={"Authorization": "Bearer incorrect"}), timeout=3)
                raise AssertionError("LLM accepted incorrect client credential")
            except urllib.error.HTTPError as error:
                assert error.code in (401, 403)

            session = None
            counter = 0

            def mcp(method, params, credential=mcp_key):
                global session, counter
                counter += 1
                headers = {"Content-Type": "application/json", "Accept": "application/json, text/event-stream", "Authorization": "Bearer " + credential}
                if session:
                    headers["Mcp-Session-Id"] = session
                body = {"jsonrpc": "2.0", "method": method, "params": params}
                if not method.startswith("notifications/"):
                    body["id"] = counter
                request = urllib.request.Request("http://127.0.0.1:" + str(mcp_port) + "/mcp", data=json.dumps(body).encode(), headers=headers)
                with urllib.request.urlopen(request, timeout=15) as response:
                    session = response.headers.get("Mcp-Session-Id", session)
                    data = response.read().decode()
                    if not data:
                        return None
                    if response.headers.get("Content-Type", "").startswith("text/event-stream"):
                        data = next(line[6:] for line in data.splitlines() if line.startswith("data: "))
                    return json.loads(data)

            assert "result" in mcp("initialize", {"protocolVersion": "2025-06-18", "capabilities": {}, "clientInfo": {"name": "successbox-fixture", "version": "1.0.0"}})
            mcp("notifications/initialized", {})
            discovered = mcp("tools/list", {})
            tool = next(tool["name"] for tool in discovered["result"]["tools"] if tool["name"] == "fixture-echo")
            result = mcp("tools/call", {"name": tool, "arguments": {"text": "fixture roundtrip"}})
            assert not result.get("result", {}).get("isError"), result
            assert "fixture roundtrip" in json.dumps(result), result
            assert upstream_key not in json.dumps([discovered, result])
            local_tool = next(tool["name"] for tool in discovered["result"]["tools"] if tool["name"] == "local-echo")
            local_result = mcp("tools/call", {"name": local_tool, "arguments": {"text": "stdio roundtrip"}})
            assert not local_result.get("result", {}).get("isError"), local_result
            assert "stdio roundtrip" in json.dumps(local_result)
            print("MCP stdio through Bifrost PASS", flush=True)
            before = len(seen_mcp)
            try:
                result = mcp("tools/call", {"name": tool, "arguments": {"text": "must not run"}}, "sk-bf-incorrect-fixture")
                assert "error" in result or result.get("result", {}).get("isError"), result
            except urllib.error.HTTPError as error:
                assert error.code in (401, 403)
            assert len(seen_mcp) == before, "invalid credential reached upstream MCP"
            assert "tools/call" in seen_mcp and len(seen_llm) == 12
            print("MCP initialize/list/call and authentication separation PASS", flush=True)
            process.send_signal(signal.SIGTERM)
            assert process.wait(timeout=15) == 0
            for port in ports:
                try:
                    with socket.create_connection(("127.0.0.1", port), timeout=0.2):
                        raise AssertionError("gateway listener survived supervisor shutdown")
                except OSError:
                    pass
            print("Gateway pair shutdown PASS", flush=True)
        except BaseException:
            log.flush()
            print(log_path.read_text())  # Fixtures contain synthetic credentials only.
            raise
        finally:
            if process is not None and process.poll() is None:
                process.send_signal(signal.SIGTERM)
                process.wait(timeout=15)
            fixture.shutdown()
            fixture.server_close()
