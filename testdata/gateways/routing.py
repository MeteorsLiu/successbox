#!/usr/bin/env python3
"""Verify token-to-account routing in the official CLIProxyAPI binary."""
import argparse
import concurrent.futures
import json
from pathlib import Path
import platform
import signal
import shutil
import socket
import subprocess
import tempfile
import threading
import time
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("cliproxy", type=Path)
parser.add_argument("plugin", type=Path)
args = parser.parse_args()
args.cliproxy = args.cliproxy.resolve()
args.plugin = args.plugin.resolve()
records = []
lock = threading.Lock()


class Fixture(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def reply(self, data, status=200, mime="application/json"):
        self.send_response(status)
        self.send_header("Content-Type", mime)
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        self.reply(b"", 405)

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        identity = self.path.split("/")[1]
        expected = "fixture-upstream-" + identity
        authenticated = self.headers.get("Authorization") == "Bearer " + expected
        with lock:
            records.append({"identity": identity, "model": body.get("model"), "authenticated": authenticated, "marker": body.get("input") or body.get("messages")})
        if not authenticated:
            self.reply(b'{"error":"wrong upstream credential"}', 401)
            return
        if self.path.split("?")[0].endswith("/messages"):
            result = {"id": "msg_" + identity, "type": "message", "role": "assistant", "model": body["model"], "content": [{"type": "text", "text": "upstream-" + identity}], "stop_reason": "end_turn", "stop_sequence": None, "usage": {"input_tokens": 1, "output_tokens": 1}}
            if not body.get("stream"):
                self.reply(json.dumps(result).encode())
                return
            events = [{"type": "message_start", "message": {**result, "content": [], "stop_reason": None}}, {"type": "content_block_start", "index": 0, "content_block": {"type": "text", "text": ""}}, {"type": "content_block_delta", "index": 0, "delta": {"type": "text_delta", "text": "upstream-" + identity}}, {"type": "content_block_stop", "index": 0}, {"type": "message_delta", "delta": {"stop_reason": "end_turn", "stop_sequence": None}, "usage": {"output_tokens": 1}}, {"type": "message_stop"}]
        else:
            result = {"id": "resp_" + identity, "object": "response", "created_at": 1, "status": "completed", "model": body["model"], "output": [{"id": "msg_" + identity, "type": "message", "role": "assistant", "status": "completed", "content": [{"type": "output_text", "text": "upstream-" + identity, "annotations": []}]}], "usage": {"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}
            events = [{"type": "response.created", "response": {**result, "status": "in_progress", "output": []}}, {"type": "response.output_text.delta", "output_index": 0, "content_index": 0, "item_id": "msg_" + identity, "delta": "upstream-" + identity}, {"type": "response.completed", "response": result}]
        data = "".join("event: " + event["type"] + "\ndata: " + json.dumps(event) + "\n\n" for event in events).encode()
        self.reply(data, mime="text/event-stream")


def request(port, path, token, body):
    req = urllib.request.Request("http://127.0.0.1:" + str(port) + path, data=json.dumps(body).encode(), headers={"Authorization": "Bearer " + token, "Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=15) as response:
            return response.status, response.read().decode()
    except urllib.error.HTTPError as error:
        return error.code, error.read().decode()


fixture = ThreadingHTTPServer(("127.0.0.1", 0), Fixture)
threading.Thread(target=fixture.serve_forever, daemon=True).start()
with socket.socket() as reservation:
    reservation.bind(("127.0.0.1", 0))
    port = reservation.getsockname()[1]
process = None
try:
    with tempfile.TemporaryDirectory(prefix="successbox-token-routing-") as directory:
        root = Path(directory)
        system = platform.system().lower()
        architecture = {"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64"}[platform.machine()]
        extension = ".dylib" if system == "darwin" else ".so"
        plugin_dir = root / "plugins" / system / architecture
        plugin_dir.mkdir(parents=True)
        shutil.copy2(args.plugin, plugin_dir / ("successbox" + extension))
        upstream = "http://127.0.0.1:" + str(fixture.server_port)
        tokens = {"A": "fixture-client-A", "B": "fixture-client-B", "unmapped": "fixture-client-unmapped"}
        llm = {"config-version": 8, "server": {"host": "127.0.0.1", "port": port}, "management": {"secret-key": "", "disable-control-panel": True}, "access": {"api-keys": list(tokens.values())}, "oauth": {"auth-dir": str(root / "oauth")}, "api-keys": {}, "observability": {"logs": {"request-log": False, "debug": False}}, "routing": {"force-model-prefix": False, "session-affinity": False, "retry": {"request-retry": 0}}}
        bindings = {}
        for identity, token in tokens.items():
            if identity == "unmapped":
                continue
            bindings[token] = {}
            for provider in ["claude", "codex"]:
                key = "fixture-upstream-" + identity
                llm["api-keys"].setdefault(provider, []).append({"name": "account-" + identity, "base-url": upstream + "/" + identity, "proxy-url": "direct", "keys": [{"api-key": key}], "models": [{"name": "fixture-" + provider, "alias": "fixture-" + provider}]})
                bindings[token][provider] = upstream + "/" + identity + "#" + ("0" if identity == "A" else "1")
        llm["plugins"] = {"enabled": True, "dir": str(root / "plugins"), "configs": {"successbox": {"enabled": True, "bindings": bindings}}}
        config_path = root / "llm.json"
        config_path.write_text(json.dumps(llm))
        log = (root / "gateway.log").open("w")
        process = subprocess.Popen([str(args.cliproxy), "-config", str(config_path), "-local-model"], cwd=root, stdout=log, stderr=log, env={"PATH": "/usr/bin:/bin", "HOME": str(root), "LANG": "C.UTF-8"})
        deadline = time.monotonic() + 30
        while True:
            if process.poll() is not None:
                raise RuntimeError("CLIProxyAPI exited during startup")
            try:
                with socket.create_connection(("127.0.0.1", port), timeout=0.2):
                    break
            except OSError:
                if time.monotonic() > deadline:
                    raise RuntimeError("CLIProxyAPI startup timeout")
                time.sleep(0.05)

        def call(provider, identity, stream):
            path = "/v1/responses" if provider == "codex" else "/v1/messages"
            body = {"model": "fixture-" + provider, "stream": stream}
            if provider == "codex":
                body["input"] = "routing-" + identity
            else:
                body.update({"messages": [{"role": "user", "content": "routing-" + identity}], "max_tokens": 16})
            return request(port, path, tokens[identity], body)

        for provider in ["claude", "codex"]:
            for identity in ["A", "B"]:
                for stream in [False, True]:
                    status, body = call(provider, identity, stream)
                    assert status == 200 and "upstream-" + identity in body, (provider, identity, stream, status, body)
        def concurrent_call(number):
            provider = ["claude", "codex"][number % 2]
            identity = ["A", "B"][(number // 2) % 2]
            status, body = call(provider, identity, False)
            assert status == 200 and "upstream-" + identity in body
            return identity
        with concurrent.futures.ThreadPoolExecutor(max_workers=8) as workers:
            list(workers.map(concurrent_call, range(32)))
        status, body = request(port, "/v1/messages", tokens["unmapped"], {"model": "fixture-claude", "messages": [{"role": "user", "content": "unmapped"}], "max_tokens": 16})
        assert status >= 400 and "upstream-" not in body, (status, body)
        with lock:
            assert all(row["authenticated"] and row["identity"] in ["A", "B"] and "/" not in row["model"] for row in records), records
        print("token A/B routing, unprefixed models and 32 concurrent calls PASS", flush=True)
finally:
    if process is not None and process.poll() is None:
        process.send_signal(signal.SIGTERM)
        process.wait(timeout=15)
    fixture.shutdown()
    fixture.server_close()
