#!/usr/bin/env python3
"""Start the real Go control service and native gateways with synthetic data."""
import json
import pathlib
import signal
import socket
import subprocess
import sys
import tempfile
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

if len(sys.argv) != 4:
    raise SystemExit("usage: gateway-fixture.py SBX CLIPROXY BIFROST")
work = pathlib.Path(tempfile.mkdtemp(prefix="successbox-desktop-e2e-", dir="/tmp"))


class Fixture(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def do_GET(self):
        self.send_error(405)

    def do_DELETE(self):
        self.send_response(200)
        self.end_headers()

    def do_POST(self):
        message = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        if "id" not in message and "method" in message:
            self.send_response(202)
            self.end_headers()
            return
        if self.path.startswith('/v1/') or self.path.startswith('/responses') or self.path.startswith('/messages'):
            assert self.headers.get('Authorization') == 'Bearer fixture-upstream' or self.headers.get('x-api-key') == 'fixture-upstream'
            result = {"id": "resp_fixture", "object": "response", "created_at": 1, "status": "completed", "model": message["model"], "output": [{"id": "msg_fixture", "type": "message", "role": "assistant", "status": "completed", "content": [{"type": "output_text", "text": "desktop-model-ok", "annotations": []}]}], "usage": {"input_tokens": 1, "output_tokens": 2, "total_tokens": 3}}
            if self.path.split('?')[0].endswith('/messages'):
                result = {"id": "msg_fixture", "type": "message", "role": "assistant", "model": message["model"], "content": [{"type": "text", "text": "desktop-model-ok"}], "stop_reason": "end_turn", "stop_sequence": None, "usage": {"input_tokens": 1, "output_tokens": 2}}
                events = [{"type":"message_start","message":{**result,"content":[],"stop_reason":None}}, {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}, {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"desktop-model-ok"}}, {"type":"content_block_stop","index":0}, {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":None},"usage":{"output_tokens":2}}, {"type":"message_stop"}]
            else:
                item = result['output'][0]
                events = [{"type":"response.created","response":{**result,"status":"in_progress","output":[]}}, {"type":"response.output_item.added","output_index":0,"item":{**item,"status":"in_progress","content":[]}}, {"type":"response.content_part.added","output_index":0,"content_index":0,"item_id":"msg_fixture","part":{"type":"output_text","text":"","annotations":[]}}, {"type":"response.output_text.delta","output_index":0,"content_index":0,"item_id":"msg_fixture","delta":"desktop-model-ok"}, {"type":"response.output_item.done","output_index":0,"item":item}, {"type":"response.completed","response":result}]
            body = (''.join('event: '+e['type']+'\n'+'data: '+json.dumps(e)+'\n\n' for e in events).encode() if message.get('stream') else json.dumps(result).encode())
            self.send_response(200)
            self.send_header('Content-Type', 'text/event-stream' if message.get('stream') else 'application/json')
            self.send_header('Content-Length', str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            (work / 'model-called').write_text(self.path)
            return
        method = message["method"]
        if method == "initialize":
            result = {"protocolVersion": "2025-06-18", "capabilities": {"tools": {}}, "serverInfo": {"name": "desktop-fixture", "version": "1.0"}}
        elif method == "tools/list":
            result = {"tools": [{"name": "search", "description": "Search the fixture", "inputSchema": {"type": "object", "properties": {"api_key": {"type": "string"}, "query": {"type": "string"}}, "required": ["api_key"]}}]}
        elif method == "tools/call":
            assert message["params"]["arguments"]["api_key"] == "fixture-installed-secret"
            result = {"content": [{"type": "text", "text": "desktop-install-ok"}]}
            (work / "called").write_text("resolved synthetic value")
        else:
            self.send_error(400)
            return
        body = json.dumps({"jsonrpc": "2.0", "id": message["id"], "result": result}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


upstream = ThreadingHTTPServer(("127.0.0.1", 0), Fixture)
threading.Thread(target=upstream.serve_forever, daemon=True).start()
reservations = [socket.socket() for _ in range(4)]
for reservation in reservations:
    reservation.bind(("127.0.0.1", 0))
llm, mcp, control, data_port = [reservation.getsockname()[1] for reservation in reservations]
(work / "mcp").mkdir()
(work / "pricing.json").write_text('{"fixture":{"mode":"chat","litellm_provider":"openai","input_cost_per_token":0,"output_cost_per_token":0}}')
(work / "parameters.json").write_text("{}")
(work / "llm.json").write_text(json.dumps({"config-version": 8, "server": {"host": "127.0.0.1", "port": llm}, "management": {"secret-key": "", "disable-control-panel": True}, "access": {"api-keys": ["fixture-llm"]}, "oauth": {"auth-dir": str(work / "oauth")}, "api-keys": {provider: [{"name": "fixture-" + provider, "base-url": f"http://127.0.0.1:{upstream.server_port}", "proxy-url": "direct", "keys": [{"api-key": "fixture-upstream"}], "models": [{"name": "fixture-" + provider, "alias": "fixture-" + provider}]}] for provider in ['codex', 'claude']}, "plugins": {"enabled": True, "dir": str(pathlib.Path(sys.argv[2]).resolve().parent.parent / "plugins"), "configs": {"successbox": {"enabled": True, "bindings": {"fixture-llm": {provider: f"http://127.0.0.1:{upstream.server_port}" for provider in ["codex", "claude"]}}}}}, "observability": {"logs": {"request-log": False, "debug": False}}}))
(work / "mcp/config.json").write_text(json.dumps({"disable_onboarding": True, "client": {"enable_logging": False, "disable_content_logging": True, "enforce_governance_header": True, "mcp_disable_auto_tool_inject": True}, "mcp": {"client_configs": []}, "governance": {"auth_config": {"is_enabled": True, "admin_username": "fixture", "admin_password": "fixture-admin"}, "virtual_keys": [{"id": scope, "name": scope, "value": "sk-bf-fixture-" + scope, "is_active": True} for scope in ["A", "B"]]}, "framework": {"pricing": {"pricing_url": (work / "pricing.json").as_uri(), "model_parameters_url": (work / "parameters.json").as_uri(), "mcp_library_sync_interval": 0, "live_models_sync_interval": 0}}}))
configuration = {"address": f"127.0.0.1:{control}", "token": "fixture-frontend", "sandboxIds": ["A", "B"]}
(work / "gateway.json").write_text(json.dumps(configuration))
(work / "fixture.json").write_text(json.dumps({"address": configuration["address"], "mcp": f"http://127.0.0.1:{data_port}/mcp", "dataPort": data_port, "upstream": f"http://127.0.0.1:{upstream.server_port}/mcp"}))
for file in [work / "gateway.json", work / "llm.json", work / "mcp/config.json"]:
    file.chmod(0o600)
log = (work / "service.log").open("wb")
for reservation in reservations:
    reservation.close()
command = [str(pathlib.Path(sys.argv[1]).resolve()), "gateways", "--cliproxy", str(pathlib.Path(sys.argv[2]).resolve()), "--llm-config", str(work / "llm.json"), "--bifrost", str(pathlib.Path(sys.argv[3]).resolve()), "--mcp-dir", str(work / "mcp"), "--mcp-port", str(mcp), "--control-listen", configuration["address"], "--control-db", str(work / "control.db"), "--control-frontend-token", "fixture-frontend", "--control-gateway-token", "fixture-gateway", "--control-agent-token", "fixture-agent", "--data-listen", f"0.0.0.0:{data_port}"]
process = subprocess.Popen(command, stdout=log, stderr=log)
stop = threading.Event()
signal.signal(signal.SIGTERM, lambda *_: stop.set())
signal.signal(signal.SIGINT, lambda *_: stop.set())
try:
    deadline = time.monotonic() + 30
    for port in [llm, mcp, control, data_port]:
        while True:
            assert process.poll() is None, (work / "service.log").read_text()
            try:
                with socket.create_connection(("127.0.0.1", port), timeout=0.2):
                    break
            except OSError:
                assert time.monotonic() < deadline, "gateway startup timed out"
                time.sleep(0.05)
    print(work, flush=True)
    while not stop.wait(0.5):
        assert process.poll() is None, "gateway exited"
finally:
    if process.poll() is None:
        process.send_signal(signal.SIGTERM)
    process.wait(timeout=15)
    upstream.shutdown()
    log.close()
