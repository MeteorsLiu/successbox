#!/usr/bin/env python3
"""PTY launcher for desktop launch tests; accepts the generated command file."""
import errno
import fcntl
import os
import pathlib
import pty
import select
import struct
import termios
import sys
import time

if len(sys.argv) != 4 or sys.argv[1] not in ['input', 'interrupt', 'hangup']:
    raise SystemExit('usage: terminal-driver.py input|interrupt|hangup OUTPUT COMMAND_FILE')
mode, output, script = sys.argv[1:]
pid, terminal = pty.fork()
if pid == 0:
    fcntl.ioctl(0, termios.TIOCSWINSZ, struct.pack('HHHH', 24, 80, 0, 0))
    os.execv('/bin/sh', ['/bin/sh', script])
data = bytearray()
sent = False
deadline = time.monotonic() + 45
try:
    while True:
        assert time.monotonic() < deadline, 'terminal test timed out'
        if terminal is not None:
            readable, _, _ = select.select([terminal], [], [], 0.1)
            if readable:
                try:
                    chunk = os.read(terminal, 65536)
                except OSError as error:
                    if error.errno != errno.EIO:
                        raise
                    chunk = b''
                data.extend(chunk)
                if not chunk:
                    os.close(terminal)
                    terminal = None
            if not sent and b'TERMINAL_READY' in data:
                sent = True
                if mode == 'input':
                    os.write(terminal, b'hello from desktop\n')
                elif mode == 'interrupt':
                    os.write(terminal, b'\x03')
                else:
                    os.close(terminal)
                    terminal = None
        else:
            time.sleep(0.1)
        ended, status = os.waitpid(pid, os.WNOHANG)
        if ended:
            assert sent, data.decode(errors='replace')
            if mode == 'input':
                assert b'INPUT_OK' in data, data.decode(errors='replace')
            pathlib.Path(output).write_bytes(data)
            expected = {'input': 0, 'interrupt': 130, 'hangup': 129}[mode]
            assert os.waitstatus_to_exitcode(status) == expected, (status, data.decode(errors='replace'))
            raise SystemExit(0)
finally:
    if terminal is not None:
        os.close(terminal)
