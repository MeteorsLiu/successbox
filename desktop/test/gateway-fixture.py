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
        if "id" not in message:
            self.send_response(202)
            self.end_headers()
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
reservations = [socket.socket() for _ in range(3)]
for reservation in reservations:
    reservation.bind(("127.0.0.1", 0))
llm, mcp, control = [reservation.getsockname()[1] for reservation in reservations]
(work / "mcp").mkdir()
(work / "pricing.json").write_text('{"fixture":{"mode":"chat","litellm_provider":"openai","input_cost_per_token":0,"output_cost_per_token":0}}')
(work / "parameters.json").write_text("{}")
(work / "llm.json").write_text(json.dumps({"config-version": 8, "server": {"host": "127.0.0.1", "port": llm}, "management": {"secret-key": "", "disable-control-panel": True}, "access": {"api-keys": ["fixture-llm"]}, "oauth": {"auth-dir": str(work / "oauth")}, "api-keys": {}, "plugins": {"enabled": True, "dir": str(pathlib.Path(sys.argv[2]).resolve().parent.parent / "plugins"), "configs": {"successbox": {"enabled": True, "config": {"bindings": {}}}}}, "observability": {"logs": {"request-log": False, "debug": False}}}))
(work / "mcp/config.json").write_text(json.dumps({"disable_onboarding": True, "client": {"enable_logging": False, "disable_content_logging": True, "enforce_governance_header": True, "mcp_disable_auto_tool_inject": True}, "mcp": {"client_configs": []}, "governance": {"auth_config": {"is_enabled": True, "admin_username": "fixture", "admin_password": "fixture-admin"}, "virtual_keys": [{"id": scope, "name": scope, "value": "sk-bf-fixture-" + scope, "is_active": True} for scope in ["A", "B"]]}, "framework": {"pricing": {"pricing_url": (work / "pricing.json").as_uri(), "model_parameters_url": (work / "parameters.json").as_uri(), "mcp_library_sync_interval": 0, "live_models_sync_interval": 0}}}))
configuration = {"address": f"127.0.0.1:{control}", "token": "fixture-frontend", "sandboxIds": ["A", "B"]}
(work / "gateway.json").write_text(json.dumps(configuration))
(work / "fixture.json").write_text(json.dumps({"address": configuration["address"], "mcp": f"http://127.0.0.1:{mcp}/mcp", "upstream": f"http://127.0.0.1:{upstream.server_port}/mcp"}))
for file in [work / "gateway.json", work / "llm.json", work / "mcp/config.json"]:
    file.chmod(0o600)
log = (work / "service.log").open("wb")
for reservation in reservations:
    reservation.close()
command = [str(pathlib.Path(sys.argv[1]).resolve()), "gateways", "--cliproxy", str(pathlib.Path(sys.argv[2]).resolve()), "--llm-config", str(work / "llm.json"), "--bifrost", str(pathlib.Path(sys.argv[3]).resolve()), "--mcp-dir", str(work / "mcp"), "--mcp-port", str(mcp), "--control-listen", configuration["address"], "--control-db", str(work / "control.db"), "--control-frontend-token", "fixture-frontend", "--control-gateway-token", "fixture-gateway", "--control-agent-token", "fixture-agent"]
process = subprocess.Popen(command, stdout=log, stderr=log)
stop = threading.Event()
signal.signal(signal.SIGTERM, lambda *_: stop.set())
signal.signal(signal.SIGINT, lambda *_: stop.set())
try:
    deadline = time.monotonic() + 30
    for port in [llm, mcp, control]:
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
