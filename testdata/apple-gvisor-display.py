#!/usr/bin/env python3
"""Validate a real xrdp listener through the macOS -> gVisor Display RPC."""

import hashlib
import json
import pathlib
import re
import signal
import socket
import ssl
import struct
import subprocess
import sys
import tempfile
import time

if len(sys.argv) != 8:
    raise SystemExit("usage: apple-gvisor-display.py SBX APPLE KERNEL INITFS DESKTOP_ROOTFS RUNTIME_DIRECTORY TEST_CERTIFICATE")

client, apple, kernel, initfs, rootfs, runtime, certificate_file = map(lambda p: str(pathlib.Path(p).resolve()), sys.argv[1:])
work = pathlib.Path(tempfile.mkdtemp(prefix="successbox-apple-display-", dir="/tmp"))
print("retained test directory:", work, flush=True)
control = work / "control.sock"
state = work / "state"
log = (work / "backend.log").open("wb")
server = subprocess.Popen([apple, "--socket", str(control), "--state", str(state), "--kernel", kernel, "--initfs", initfs, "--linux-rootfs", rootfs, "--runtime-directory", runtime], stdout=log, stderr=log)
client_args = [client, "--socket", str(control)]
box = None
proxy = None
try:
    deadline = time.monotonic() + 90
    while True:
        assert server.poll() is None, (work / "backend.log").read_text()
        if control.exists():
            ready = subprocess.run(client_args + ["close", "readiness-probe"], capture_output=True, timeout=5)
            if ready.returncode == 0:
                break
        assert time.monotonic() < deadline, "Linux backend did not become ready"
        time.sleep(0.1)

    config = work / "box.json"
    config.write_text(json.dumps({"rootfs": "/", "desktop": True, "initArgv": ["/usr/local/bin/start-desktop"]}))
    box = subprocess.check_output(client_args + ["create", str(config)], text=True, timeout=60).strip()
    execute = client_args + ["exec", "--sandbox", box, "--"]
    ready = subprocess.run(execute + ["/bin/sh", "-c", "for i in $(seq 1 100); do /usr/bin/socat -u OPEN:/dev/null TCP:127.0.0.1:3389 2>/dev/null && exit 0; sleep 0.1; done; cat /var/log/xrdp.log >&2; exit 1"], capture_output=True, timeout=30)
    assert ready.returncode == 0, ready
    print("gVisor desktop startup and RDP listener: PASS", flush=True)

    proxy = subprocess.Popen(client_args + ["display", "--sandbox", box, "--listen", "127.0.0.1:0"], stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, text=True)
    line = proxy.stderr.readline()
    match = re.search(r"display byte-stream proxy: (127\.0\.0\.1):(\d+)", line)
    assert match, line
    address = (match.group(1), int(match.group(2)))
    certificate = ssl.PEM_cert_to_DER_cert(pathlib.Path(certificate_file).read_text())
    expected = hashlib.sha256(certificate).digest()
    tls = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
    # The tunnel selects a sandbox ID, not the image's build-time hostname.
    # Validate the chain against the fixture certificate and pin its DER hash.
    tls.check_hostname = False
    tls.load_verify_locations(cafile=certificate_file)
    for attempt in range(2):
        with socket.create_connection(address, timeout=15) as connection:
            connection.sendall(bytes.fromhex("030000130ee000000000000100080001000000"))
            header = b""
            while len(header) < 4:
                chunk = connection.recv(4 - len(header))
                assert chunk, "RDP connection closed before response"
                header += chunk
            length = struct.unpack("!H", header[2:4])[0]
            assert header[:2] == b"\x03\x00" and 19 <= length <= 65535, header.hex()
            body = b""
            while len(body) < length - 4:
                chunk = connection.recv(length - 4 - len(body))
                assert chunk, "truncated RDP response"
                body += chunk
            assert body[1] == 0xD0 and body[-8] == 2 and int.from_bytes(body[-4:], "little") == 1, (header + body).hex()
            with tls.wrap_socket(connection, server_hostname=None) as secured:
                assert hashlib.sha256(secured.getpeercert(binary_form=True)).digest() == expected
                print("RDP negotiation and verified TLS handshake: PASS", attempt + 1, secured.version(), flush=True)
        subprocess.run(execute + ["/bin/true"], check=True, timeout=30)
    print("Display disconnect/reconnect and sandbox survival: PASS", flush=True)

    subprocess.run(client_args + ["close", box], check=True, timeout=30)
    box = None
    server.send_signal(signal.SIGTERM)
    assert server.wait(timeout=30) == 0, (work / "backend.log").read_text()
    assert not control.exists()
    assert not any(path.is_dir() for path in state.iterdir())
    print("Desktop Close and VM cleanup: PASS", flush=True)
finally:
    if proxy is not None:
        if proxy.poll() is None:
            proxy.send_signal(signal.SIGTERM)
        proxy.wait(timeout=15)
        (work / "proxy.log").write_text(proxy.stderr.read())
    if server.poll() is None:
        if box:
            subprocess.run(client_args + ["close", box], capture_output=True, timeout=30)
        server.send_signal(signal.SIGTERM)
        server.wait(timeout=30)
    log.close()
