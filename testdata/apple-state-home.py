#!/usr/bin/env python3
"""Verify packaged state under shared HOME against a real Apple-hosted Sentry."""
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import time

client, apple, kernel, initfs, rootfs, runtime = [str(Path(value).resolve()) for value in sys.argv[1:]]
with tempfile.TemporaryDirectory(prefix='successbox-home-state-', dir='/tmp') as temporary:
    stage = Path(temporary)
    home = stage / 'home'
    state = home / 'application/private'
    backend_state = state / 'gvisor'
    backend_state.mkdir(parents=True)
    (home / 'user-file').write_text('host original')
    (backend_state / 'private-account').write_text('host-only-test-credential')
    control = stage / 'control.sock'
    with (stage / 'service.log').open('wb') as log:
        server = subprocess.Popen([apple, '--socket', str(control), '--state', str(state), '--kernel', kernel, '--initfs', initfs,
            '--linux-rootfs', rootfs, '--runtime-directory', runtime, '--host-home', str(home)], stdout=log, stderr=log)
        box = ''
        prefix = [client, '--socket', str(control)]
        try:
            deadline = time.monotonic() + 90
            while True:
                assert server.poll() is None, (stage / 'service.log').read_text()
                if control.exists():
                    probe = subprocess.run(prefix + ['close', 'readiness-probe'], capture_output=True, timeout=3)
                    if probe.returncode == 0: break
                assert time.monotonic() < deadline, 'backend startup deadline exceeded'
                time.sleep(0.1)
            request = stage / 'create.json'
            request.write_text(json.dumps({'rootfs': '/', 'mounts': [{'source': str(home), 'destination': str(home)}]}))
            box = subprocess.check_output(prefix + ['create', str(request)], text=True, timeout=45).strip()
            result = subprocess.run(prefix + ['exec', '--sandbox', box, '--', '/bin/sh', '-ec',
                'test "$(cat "$1/user-file")" = "host original"; test ! -e "$2/private-account"; printf private > "$1/user-file"; cat "$1/user-file"',
                'probe', str(home), str(backend_state)], capture_output=True, timeout=30)
            assert result.returncode == 0 and result.stdout == b'private', result
            subprocess.run(prefix + ['close', box], check=True, timeout=30)
            box = ''
            assert (home / 'user-file').read_text() == 'host original'
            assert (backend_state / 'private-account').read_text() == 'host-only-test-credential'
            assert (backend_state / 'results').is_dir()
        finally:
            if server.poll() is None:
                if box: subprocess.run(prefix + ['close', box], timeout=30, check=True)
                server.terminate()
                assert server.wait(timeout=30) == 0, (stage / 'service.log').read_text()
print('shared HOME, native private state, credential masking and disposable writes: PASS')
