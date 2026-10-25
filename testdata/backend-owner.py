#!/usr/bin/env python3
"""Verify the desktop's rootful backend contract on a native Linux runner."""
import os
from pathlib import Path
import shutil
import stat
import subprocess
import sys
import tempfile
import time

client, backend = map(lambda value: str(Path(value).resolve()), sys.argv[1:])
assert sys.platform == 'linux' and os.geteuid() == 0
with tempfile.TemporaryDirectory(prefix='successbox-owner-', dir='/tmp') as temporary:
    directory = Path(temporary)
    directory.chmod(0o711)
    # Hosted runner ancestors are private to its login UID. Test the socket
    # permissions without failing before exec on that unrelated directory.
    shutil.copy2(client, directory / 'sbx')
    shutil.copy2(backend, directory / 'backend')
    client, backend = str(directory / 'sbx'), str(directory / 'backend')
    os.chmod(client, 0o755)
    os.chmod(backend, 0o755)
    socket = directory / 'control.sock'
    args = [backend, '--socket', str(socket), '--state', str(directory / 'state'), '--runsc', '/bin/true', '--socket-uid', '1000', '--exit-on-stdin-close']
    with (directory / 'backend.log').open('wb') as log:
        service = subprocess.Popen(args, stdin=subprocess.PIPE, stdout=log, stderr=log)
        try:
            deadline = time.monotonic() + 10
            while not socket.exists():
                assert service.poll() is None, (directory / 'backend.log').read_text()
                assert time.monotonic() < deadline, 'backend did not start'
                time.sleep(0.05)
            info = socket.stat()
            assert info.st_uid == 1000 and stat.S_IMODE(info.st_mode) == 0o600, info
            command = [client, '--socket', str(socket), 'close', 'readiness-probe']
            owner = subprocess.run(command, user=1000, group=1000, extra_groups=[], capture_output=True, timeout=5)
            assert owner.returncode == 0, owner.stderr
            other = subprocess.run(command, user=1001, group=1001, extra_groups=[], capture_output=True, timeout=5)
            assert other.returncode != 0, 'other UID accessed the private socket'
            service.stdin.close()
            assert service.wait(timeout=10) == 0, (directory / 'backend.log').read_text()
            assert not socket.exists(), 'socket remained after desktop EOF'
        finally:
            if service.poll() is None:
                service.stdin.close()
                service.wait(timeout=10)
    rejected = subprocess.run(args, user=1000, group=1000, extra_groups=[], capture_output=True, timeout=5)
    assert rejected.returncode != 0 and b'rootful backend' in rejected.stderr, rejected
print('private socket ownership, cross-UID denial and desktop EOF shutdown: PASS')
