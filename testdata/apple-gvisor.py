#!/usr/bin/env python3
"""Run against a real Apple VM and Linux arm64 runtime, without Docker."""

import concurrent.futures
import json
import pathlib
import shutil
import signal
import stat
import subprocess
import sys
import tempfile
import time

if len(sys.argv) != 7:
    raise SystemExit("usage: apple-gvisor.py SBX APPLE KERNEL INITFS LINUX_ROOTFS RUNTIME_DIRECTORY")

client, apple, kernel, initfs, rootfs, runtime = map(lambda p: str(pathlib.Path(p).resolve()), sys.argv[1:])
work = pathlib.Path(tempfile.mkdtemp(prefix="successbox-apple-gvisor-", dir="/tmp"))
print("retained test directory:", work, flush=True)
# Keep the inspected cgroup mount outside the sandbox's rootfs. Using the
# runtime's own / as rootfs can hide that source during gofer mount setup.
assets = work / "runtime"
shutil.copytree(runtime, assets, symlinks=True)
shutil.copytree(rootfs, assets / "rootfs", symlinks=True)
(assets / "rootfs" / "host-cgroups").mkdir()
runtime = str(assets)
control = work / "control.sock"
state = work / "state"
arguments = [apple, "--socket", str(control), "--state", str(state), "--kernel", kernel, "--initfs", initfs]
invalid = subprocess.run(arguments + ["--linux-rootfs", rootfs], capture_output=True, text=True, timeout=15)
assert invalid.returncode != 0 and "must be provided together" in invalid.stderr, invalid
log = (work / "backend.log").open("wb")
server = subprocess.Popen(arguments + ["--linux-rootfs", rootfs, "--runtime-directory", runtime], stdout=log, stderr=log)
prefix = [client, "--socket", str(control)]
boxes = []
try:
    deadline = time.monotonic() + 90
    while True:
        assert server.poll() is None, (work / "backend.log").read_text()
        if control.exists():
            # The SDK creates its host listener before the guest daemon starts.
            ready = subprocess.run(prefix + ["close", "readiness-probe"], capture_output=True, timeout=5)
            if ready.returncode == 0:
                break
        assert time.monotonic() < deadline, "Linux backend did not become ready"
        time.sleep(0.1)
    assert stat.S_IMODE(control.stat().st_mode) == 0o600
    assert len([path for path in state.iterdir() if path.is_dir()]) == 1
    print("Linux host VM, socket relay and owner-only permissions: PASS", flush=True)

    config = work / "box.json"
    config.write_text(json.dumps({
        "rootfs": "/opt/successbox-runtime/rootfs", "cpus": 1, "memoryBytes": 268435456,
        "env": ["SUCCESSBOX_TEST=from-macos"],
        "mounts": [{"source": "/sys/fs/cgroup", "destination": "/host-cgroups", "readOnly": True}],
    }))
    for _ in range(2):
        boxes.append(subprocess.check_output(prefix + ["create", str(config)], text=True, timeout=60).strip())
    assert boxes[0] != boxes[1]
    first = prefix + ["exec", "--sandbox", boxes[0], "--"]
    second = prefix + ["exec", "--sandbox", boxes[1], "--"]
    marker = "/tmp/" + work.name
    result = subprocess.run(first + ["/bin/sh", "-ec", 'printf "%s\\n" "$SUCCESSBOX_TEST"; uname -s; printf private > "$1"; cat "$1"', "probe", marker], capture_output=True, timeout=30)
    assert (result.returncode, result.stdout) == (0, b"from-macos\nLinux\nprivate"), result
    result = subprocess.run(second + ["/bin/sh", "-ec", 'test ! -e "$1"', "probe", marker], capture_output=True, timeout=30)
    assert result.returncode == 0, result
    assert not (pathlib.Path(rootfs) / marker.lstrip("/")).exists()
    assert not (assets / "rootfs" / marker.lstrip("/")).exists()
    assert len([path for path in state.iterdir() if path.is_dir()]) == 1
    print("Two gVisor sandboxes in one VM, environment and write isolation: PASS", flush=True)

    result = subprocess.run(first + ["/bin/sh", "-ec", 'cat "/host-cgroups/$1/cpu.max"; cat "/host-cgroups/$1/memory.max"', "probe", boxes[0]], capture_output=True, timeout=30)
    assert result.returncode == 0, result
    quota, period, memory = map(int, result.stdout.split())
    assert quota == period and memory == 268435456, result.stdout
    print("Actual outer cgroup CPU and memory limits: PASS", flush=True)

    payload = bytes(range(256)) * 2048
    result = subprocess.run(first + ["/bin/cat"], input=payload, capture_output=True, timeout=30)
    assert result.returncode == 0 and result.stdout == payload, (result.returncode, len(result.stdout), result.stderr)
    result = subprocess.run(first + ["/bin/sh", "-c", "printf output; printf error >&2; exit 7"], capture_output=True, timeout=30)
    assert (result.returncode, result.stdout, result.stderr) == (7, b"output", b"error"), result
    with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
        pending = [pool.submit(subprocess.run, command + ["/bin/sh", "-c", "sleep 0.1; printf concurrent"], capture_output=True, timeout=30) for command in [first, second]]
        for future in pending:
            result = future.result()
            assert result.returncode == 0 and result.stdout == b"concurrent", result
    print("512 KiB binary IO, stderr, exit status and concurrent Exec: PASS", flush=True)

    for box in boxes:
        subprocess.run(prefix + ["close", box], check=True, timeout=30)
        subprocess.run(prefix + ["close", box], check=True, timeout=30)
    boxes.clear()
    # Also exercise the documented mode that uses the Linux VM's own root.
    config.write_text(json.dumps({"rootfs": "/"}))
    box = subprocess.check_output(prefix + ["create", str(config)], text=True, timeout=60).strip()
    boxes.append(box)
    subprocess.run(prefix + ["exec", "--sandbox", box, "--", "/bin/true"], check=True, timeout=30)
    # Shutdown owns any sandboxes that have not received Close yet.
    server.send_signal(signal.SIGTERM)
    assert server.wait(timeout=30) == 0, (work / "backend.log").read_text()
    boxes.clear()
    assert not control.exists(), "socket remained after shutdown"
    assert not any(path.is_dir() for path in state.iterdir()), "VM state remained after shutdown"
    print("Repeated Close, subsequent Create and shutdown with an active sandbox: PASS", flush=True)
finally:
    if server.poll() is None:
        for box in boxes:
            subprocess.run(prefix + ["close", box], capture_output=True, timeout=30)
        server.send_signal(signal.SIGTERM)
        server.wait(timeout=30)
    log.close()
    if server.returncode != 0:
        print((work / "backend.log").read_text(), file=sys.stderr)

# An init that exits unsuccessfully must still release the VM, disk and relay,
# and its exit code must reach the caller. Only the test's runtime copy changes.
(assets / "successbox-gvisor").write_text("#!/bin/sh\nprintf 'intentional startup failure\\n' >&2\nexit 19\n")
result = subprocess.run(arguments + ["--linux-rootfs", rootfs, "--runtime-directory", runtime], capture_output=True, timeout=30)
assert result.returncode == 19, result
assert b"intentional startup failure" in result.stderr, result
assert not control.exists()
assert not any(path.is_dir() for path in state.iterdir())
print("Backend startup failure: exit status and VM cleanup: PASS", flush=True)
