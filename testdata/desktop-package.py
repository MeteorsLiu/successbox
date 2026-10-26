#!/usr/bin/env python3
"""Exercise the installed Linux payload through real gVisor, RDP and Codex."""
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import ssl
import subprocess
import sys
import tempfile
import time

runtime = Path(sys.argv[1]).resolve()
work = Path(tempfile.mkdtemp(prefix='successbox-package-', dir='/var/tmp'))
work.chmod(0o755)
print('desktop validation logs:', work, flush=True)
home = work / 'home'
home.mkdir(mode=0o755)
os.chown(home, 1000, 1000)
socket = work / 'control.sock'
# Hosted runners do not delegate cgroup controllers to workflow processes.
wrapper = work / 'runsc'
wrapper.write_text('#!/bin/sh\nexec "' + str(runtime / 'linux/runsc') + '" --ignore-cgroups "$@"\n')
wrapper.chmod(0o755)
backend_log = (work / 'backend.log').open('w')
backend = subprocess.Popen([str(runtime / 'linux/successbox-gvisor'), '--socket', str(socket), '--state', str(work / ('state-' + 'long-profile-' * 8)), '--runsc', str(wrapper), '--worktrees', str(runtime / 'linux/worktrees'), '--network', 'slirp4netns', '--slirp4netns', '/usr/bin/slirp4netns', '--exit-on-stdin-close'], stdin=subprocess.PIPE, stdout=backend_log, stderr=backend_log)
client = [str(runtime / 'host/sbx'), '--socket', str(socket)]
box = None
children = []
try:
    deadline = time.monotonic() + 30
    while subprocess.run(client + ['close', 'readiness-probe'], capture_output=True).returncode:
        assert backend.poll() is None, 'backend exited'
        assert time.monotonic() < deadline, 'backend readiness timeout'
        time.sleep(0.2)
    request = {'rootfs': '/', 'desktop': True, 'workingDir': str(home), 'env': ['HOME=' + str(home), 'LANG=C.UTF-8', 'SUCCESSBOX_LLM_TOKEN=package-test'], 'mounts': [{'source': str(runtime / 'linux'), 'destination': '/opt/successbox-runtime', 'readOnly': True}, {'source': str(runtime / 'linux/agent/usr/lib/chatgpt'), 'destination': '/usr/lib/chatgpt', 'readOnly': True}]}
    (work / 'create.json').write_text(json.dumps(request))
    box = subprocess.check_output(client + ['create', str(work / 'create.json')], text=True, timeout=60).strip()
    execute = client + ['exec', '--sandbox', box, '--']
    subprocess.run(execute + ['python3', '-c', 'import os,socket;os.setgid(1000);os.setuid(1000);socket.getaddrinfo("github.com",443);print("ordinary-user DNS: PASS")'], check=True, timeout=20)
    options = {'home': str(home), 'uid': 1000, 'gid': 1000, 'password': secrets.token_hex(24), 'url': 'http://127.0.0.1:9', 'codexToken': 'package-test', 'claudeToken': 'package-test', 'claudeMcpToken': 'package-test', 'codexConfig': 'model = "gpt-5"\nmodel_provider = "successbox"\n[model_providers.successbox]\nname = "Successbox"\nbase_url = "http://127.0.0.1:9/v1"\nwire_api = "responses"\nrequires_openai_auth = false\nenv_key = "SUCCESSBOX_LLM_TOKEN"\n'}
    prepare = subprocess.run(execute + ['python3', '/opt/successbox-runtime/scripts/session.py', 'prepare'], input=json.dumps(options), capture_output=True, text=True, timeout=30)
    (work / 'prepare.log').write_text(prepare.stderr)
    assert prepare.returncode == 0, prepare.stderr
    rdp = json.loads(prepare.stdout.strip().splitlines()[-1])
    proxy = subprocess.Popen(client + ['display', '--sandbox', box, '--listen', '127.0.0.1:0'], stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, text=True)
    children.append(proxy)
    address = re.search(r'127\.0\.0\.1:\d+', proxy.stderr.readline())[0]
    args = work / 'viewer.args'
    args.write_text('\n'.join(['/v:' + address, '/u:' + rdp['username'], '/p:' + options['password'], '/sec:tls', '/cert:fingerprint:sha256:' + hashlib.sha256(ssl.PEM_cert_to_DER_cert(rdp['certificate'])).hexdigest(), '/size:1280x800', '-gfx']) + '\n')
    args.chmod(0o600)
    with (work / 'viewer.log').open('w') as log:
        viewer = subprocess.Popen([str(runtime / 'host/rdp/xfreerdp'), '/args-from:file:' + str(args)], stdout=log, stderr=log)
        children.append(viewer)
    with (work / 'application.log').open('w') as log:
        application = subprocess.Popen(execute + ['python3', '/opt/successbox-runtime/scripts/session.py', 'launch'], stdout=log, stderr=log)
        children.append(application)
    deadline = time.monotonic() + 100
    while True:
        assert viewer.poll() is None, 'RDP viewer exited'
        assert application.poll() is None, 'Codex Desktop exited'
        probe = subprocess.run(execute + ['python3', '-c', 'import urllib.request,json;pages=json.load(urllib.request.urlopen("http://127.0.0.1:9333/json",timeout=2));assert any(p.get("type")=="page" for p in pages)'], capture_output=True, timeout=8)
        if probe.returncode == 0:
            break
        assert time.monotonic() < deadline, 'Codex Desktop renderer readiness timeout'
        time.sleep(1)
    print('RDP authentication, graphical session and Codex Desktop renderer: PASS', flush=True)
    subprocess.run(execute + ['/opt/successbox-runtime/claude/node_modules/.bin/claude', '--version'], check=True, timeout=20)
finally:
    if box:
        logs = subprocess.run(client + ['exec', '--sandbox', box, '--', '/bin/sh', '-c', 'cat /tmp/successbox-rdp.log /var/log/xrdp.log /var/log/xrdp-sesman.log'], capture_output=True, timeout=15)
        (work / 'guest.log').write_bytes(logs.stdout + logs.stderr)
        for name in ('prepare.log', 'application.log', 'viewer.log', 'guest.log', 'backend.log'):
            p = work / name
            if p.exists(): print(name, p.read_text()[-6000:], flush=True)
        closed = subprocess.run(client + ['close', box], timeout=30)
        runtime_log = work / ('state-' + 'long-profile-' * 8) / box / 'runtime.log'
        if runtime_log.exists(): print('runtime.log', runtime_log.read_text()[-10000:], flush=True)
    for child in reversed(children):
        if child.poll() is None: child.terminate()
        child.wait(timeout=15)
    backend.stdin.close()
    assert backend.wait(timeout=30) == 0, 'backend EOF cleanup failed'
    backend_log.close()
    if sys.exc_info()[0]:
        for name in ('prepare.log', 'application.log', 'viewer.log', 'guest.log', 'backend.log'):
            p = work / name
            if p.exists(): print(name, p.read_text()[-6000:], flush=True)
assert closed.returncode == 0, 'sandbox Close failed'
print('Close and backend EOF cleanup: PASS', flush=True)
