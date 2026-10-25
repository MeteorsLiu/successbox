#!/usr/bin/env python3
"""Fetch the gateway versions validated by the integration fixture."""
import argparse
import hashlib
from pathlib import Path
import platform
import tarfile
import urllib.request
import shutil

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("directory", type=Path)
args = parser.parse_args()
system = platform.system().lower()
architecture = {"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64"}.get(platform.machine())
if system not in ("linux", "darwin") or architecture is None:
    parser.error("supported hosts: Linux amd64/arm64 and macOS arm64/amd64")
args.directory.mkdir(parents=True, exist_ok=True)


def fetch(url, destination):
    request = urllib.request.Request(url, headers={"User-Agent": "successbox-gateway-tests"})
    with urllib.request.urlopen(request, timeout=60) as source, destination.open("wb") as target:
        shutil.copyfileobj(source, target)


release_base = "https://github.com/router-for-me/CLIProxyAPI/releases/download/v8.0.42/"
archive_name = "CLIProxyAPI_8.0.42_" + system + "_" + ("aarch64" if architecture == "arm64" else architecture) + ".tar.gz"
archive = args.directory / archive_name
checksums = args.directory / "checksums.txt"
fetch(release_base + archive_name, archive)
fetch(release_base + "checksums.txt", checksums)
expected = next(line.split()[0] for line in checksums.read_text().splitlines() if line.split()[-1] == archive_name)
if hashlib.sha256(archive.read_bytes()).hexdigest() != expected:
    raise RuntimeError("CLIProxyAPI archive checksum mismatch")
with tarfile.open(archive) as package:
    executable = next(member for member in package.getmembers() if Path(member.name).name == "cli-proxy-api")
    if not executable.isfile():
        raise RuntimeError("CLIProxyAPI archive entry is not a regular file")
    with package.extractfile(executable) as source, (args.directory / "cli-proxy-api").open("wb") as target:
        shutil.copyfileobj(source, target)
(args.directory / "cli-proxy-api").chmod(0o755)
# This is the versioned download layout used by Bifrost's official launcher.
# Unlike CLIProxyAPI's release, it supplies a raw executable without a checksum manifest.
fetch("https://downloads.getmaxim.ai/bifrost/v2.3.2/" + system + "/" + architecture + "/bifrost-http", args.directory / "bifrost-http")
(args.directory / "bifrost-http").chmod(0o755)
print("Installed CLIProxyAPI 8.0.42 and Bifrost 2.3.2 into", args.directory)
