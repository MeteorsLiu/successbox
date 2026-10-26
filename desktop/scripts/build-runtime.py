#!/usr/bin/env python3
"""Build only declared release resources; never copy a developer cache or HOME."""
import hashlib
import json
import os
from pathlib import Path
import platform
import shutil
import subprocess
import sys
import tempfile

repo = Path(__file__).resolve().parents[2]
destination = Path(sys.argv[1]).resolve()
system = platform.system().lower()
arch = {'arm64': 'arm64', 'aarch64': 'arm64', 'x86_64': 'amd64'}[platform.machine()]
if (system, arch) not in [('darwin', 'arm64'), ('linux', 'arm64'), ('linux', 'amd64')]:
    raise SystemExit('supported release hosts: macOS arm64; Linux amd64/arm64')
if destination.exists():
    raise SystemExit('use an empty runtime staging directory')
host = destination / 'host'
linux = destination / 'linux'
notices = destination / 'licenses'
for directory in (host, linux / 'gvisor-bin', notices):
    directory.mkdir(parents=True)
with tempfile.TemporaryDirectory(prefix='successbox-build-') as scratch:
    scratch = Path(scratch)
    env = {**os.environ, 'CGO_ENABLED': '0'}
    subprocess.run(['go', 'build', '-trimpath', '-o', host / 'sbx', './cmd/sbx'], cwd=repo, env=env, check=True)
    env.update(GOOS='linux', GOARCH=arch)
    subprocess.run(['go', 'build', '-trimpath', '-o', linux / 'successbox-gvisor', './cmd/successbox-gvisor'], cwd=repo, env=env, check=True)
    for target, package in [('runsc', 'runsc'), ('gvisor-bin/gvisor_sentry', 'sentry'), ('worktrees', 'worktrees')]:
        subprocess.run(['go', 'build', '-trimpath', '-o', linux / target, './cmd/' + package], cwd=repo / 'runtime', env=env, check=True)
    subprocess.run([sys.executable, repo / 'testdata/gateways/install.py', scratch / 'gateways'], check=True)
    for binary in ('cli-proxy-api', 'bifrost-http'):
        shutil.copy2(scratch / 'gateways' / binary, host / binary)
    plugin = host / 'plugins' / system / arch / ('successbox.dylib' if system == 'darwin' else 'successbox.so')
    plugin.parent.mkdir(parents=True)
    subprocess.run(['go', 'build', '-trimpath', '-buildmode=c-shared', '-o', plugin, '.'], cwd=repo / 'gateways/cliproxy', check=True)
    plugin.with_suffix('.h').unlink()
    shutil.copy2(repo / 'desktop/LICENSE', notices / 'Successbox-desktop.txt')
    shutil.copy2(repo / 'runtime/third_party/sandbox-LICENSE', notices / 'Sandbox.txt')
    shutil.copy2(repo / 'gateways/cliproxy/LICENSE.cliproxy', notices / 'CLIProxyAPI.txt')
    subprocess.run(['git', 'clone', '--quiet', '--depth', '1', '--branch', '3.32.2', 'https://github.com/FreeRDP/FreeRDP.git', scratch / 'freerdp'], check=True)
    revision = subprocess.check_output(['git', '-C', scratch / 'freerdp', 'rev-parse', 'HEAD'], text=True).strip()
    if revision != '97eff6ddfbbfaa8e42a126d362686bd001ff8bdc':
        raise RuntimeError('FreeRDP tag changed')
    shutil.copy2(scratch / 'freerdp/LICENSE', notices / 'FreeRDP.txt')
    cmake = ['cmake', '-S', scratch / 'freerdp', '-B', scratch / 'rdp-build', '-DCMAKE_BUILD_TYPE=Release',
        '-DCMAKE_INSTALL_PREFIX=' + str(host / 'rdp'), '-DWITH_SERVER=OFF', '-DWITH_SHADOW=OFF',
        '-DWITH_MANPAGES=OFF', '-DWITH_FFMPEG=OFF', '-DWITH_SWSCALE=OFF', '-DWITH_CUPS=OFF',
        '-DWITH_PCSC=OFF', '-DCHANNEL_URBDRC=OFF', '-DWITH_FUSE=OFF', '-DWITH_AAD=OFF', '-DWITH_SAMPLE=OFF', '-DWITH_KRB5=OFF',
        '-DWITH_CLIENT_SDL=' + ('ON' if system == 'darwin' else 'OFF'),
        '-DWITH_X11=' + ('OFF' if system == 'darwin' else 'ON'), '-DWITH_CLIENT_MAC=OFF',
        '-DWITH_CLIENT_SDL2=OFF', '-DWITH_CAIRO=OFF', '-DWITH_CLIENT_SDL3=ON', '-DWITH_CLIENT_SDL_VERSIONED=OFF', '-DWITH_BINARY_VERSIONING=OFF', '-DWITH_ABSOLUTE_PLUGIN_LOAD_PATHS=OFF']
    if system == 'darwin':
        prefix = subprocess.check_output(['brew', '--prefix'], text=True).strip()
        cmake += ['-DCMAKE_PREFIX_PATH=' + prefix, '-DCMAKE_INSTALL_RPATH=@loader_path/../lib']
    else:
        cmake += ['-DCMAKE_INSTALL_RPATH=$ORIGIN/../lib', '-DCMAKE_INSTALL_LIBDIR=lib', '-DWITH_PULSE=OFF', '-DWITH_ALSA=OFF']
    subprocess.run(cmake, check=True)
    subprocess.run(['cmake', '--build', scratch / 'rdp-build', '--parallel', '4'], check=True)
    subprocess.run(['cmake', '--install', scratch / 'rdp-build'], check=True)
    # Runtime needs the client and libraries, not headers, CMake packages or tools.
    client = 'sdl-freerdp' if system == 'darwin' else 'xfreerdp'
    shutil.copy2(host / 'rdp/bin' / client, host / 'rdp' / client)
    for child in ('include', 'share', 'bin', 'lib/cmake', 'lib/pkgconfig'):
        shutil.rmtree(host / 'rdp' / child, ignore_errors=True)
    if system == 'linux':
        subprocess.run(['patchelf', '--set-rpath', '$ORIGIN/lib', host / 'rdp' / client], check=True)
    if system == 'darwin':
        subprocess.run(['swift', 'build', '-c', 'release', '--product', 'successbox-apple', '--disable-automatic-resolution'], cwd=repo, check=True)
        shutil.copy2(repo / '.build/release/successbox-apple', host / 'successbox-apple')
        subprocess.run(['codesign', '--force', '--sign', '-', '--entitlements', repo / 'apple/entitlements.plist', host / 'successbox-apple'], check=True)
        # Copy the complete non-system dylib closure and make every edge relative
        # to this bundle. No Homebrew or build-machine path may remain.
        pending = [host / 'rdp' / client, *list((host / 'rdp/lib').glob('*.dylib'))]
        visited = set()
        origins = {}
        while pending:
            binary = pending.pop().resolve()
            if binary in visited:
                continue
            visited.add(binary)
            original = origins.get(binary, binary)
            commands = subprocess.check_output(['otool', '-l', original], text=True).splitlines()
            rpaths = [commands[i + 2].strip().split(' ', 2)[1] for i, line in enumerate(commands) if line.strip() == 'cmd LC_RPATH']
            dependencies = subprocess.check_output(['otool', '-L', original], text=True).splitlines()[1:]
            for line in dependencies:
                dependency = line.strip().split(' (', 1)[0]
                if dependency.startswith(('/usr/lib/', '/System/Library/')):
                    continue
                if dependency.startswith('@rpath/'):
                    name = dependency.removeprefix('@rpath/')
                    candidates = [host / 'rdp/lib' / name]
                    candidates += [Path(entry.replace('@loader_path', str(original.parent))) / name for entry in rpaths]
                    source = next((p for p in candidates if p.is_file()), None)
                    if source is None:
                        raise RuntimeError('Unresolved dylib ' + dependency + ' in ' + str(original))
                elif dependency.startswith('@loader_path/'):
                    source = (original.parent / dependency.removeprefix('@loader_path/')).resolve()
                else:
                    source = Path(dependency)
                target = host / 'rdp/lib' / source.name
                if not target.exists():
                    shutil.copy2(source, target)
                    origins[target.resolve()] = source.resolve()
                relative = os.path.relpath(target, binary.parent)
                replacement = '@loader_path/' + relative
                subprocess.run(['install_name_tool', '-change', dependency, replacement, binary], check=True)
                pending.append(target)
            if binary.suffix == '.dylib':
                subprocess.run(['install_name_tool', '-id', '@loader_path/' + binary.name, binary], check=True)
        for binary in visited:
            subprocess.run(['codesign', '--force', '--sign', '-', binary], check=True)
    else:
        # Copy the xrdp package without installing a host RDP service. Its other
        # dependencies are declared by the deb maker and installed normally.
        subprocess.run(['apt-get', 'download', 'xrdp'], cwd=scratch, check=True)
        archive = next(scratch.glob('xrdp_*.deb'))
        subprocess.run(['dpkg-deb', '-x', archive, linux / 'xrdp'], check=True)
        dependencies = subprocess.check_output(['dpkg-deb', '-f', archive, 'Depends'], text=True).strip()
        (destination / 'linux-dependencies.json').write_text(json.dumps(dependencies.split(', ')))
    if system == 'darwin':
        if len(sys.argv) != 3:
            raise SystemExit('macOS builds require the guest artifact directory')
        shutil.copytree(Path(sys.argv[2]), destination / 'guest')
        for name in ('rootfs.tar.gz', 'initfs.ext4', 'vmlinux-arm64'):
            if not (destination / 'guest' / name).is_file():
                raise RuntimeError('missing guest resource: ' + name)
    else:
        url = 'https://persistent.oaistatic.com/codex-app-prod/linux/deb/latest/chatgpt_' + arch + '.deb'
        package = scratch / 'chatgpt.deb'
        subprocess.run(['curl', '--fail', '--location', '--retry', '3', '--output', package, url], check=True)
        expected = {'amd64': 'eb0831c734f154f93f8ebcf0e17d118da0dea7dc1737dc61a1e71444375bf190', 'arm64': '68de1ec44066871e4177ba1ca793ebee6ba238397f13c794e45b14941c1261fb'}
        with package.open('rb') as stream:
            if hashlib.file_digest(stream, 'sha256').hexdigest() != expected[arch]:
                raise RuntimeError('Codex Desktop download changed; review the new release before updating the checksum')
        subprocess.run(['dpkg-deb', '-x', package, linux / 'agent'], check=True)
        dependencies = json.loads((destination / 'linux-dependencies.json').read_text())
        dependencies += subprocess.check_output(['dpkg-deb', '-f', package, 'Depends'], text=True).strip().split(', ')
        (destination / 'linux-dependencies.json').write_text(json.dumps(dependencies))
        with package.open('rb') as stream:
            (destination / 'chatgpt-download.sha256').write_text(hashlib.file_digest(stream, 'sha256').hexdigest() + '\n')
    (linux / 'scripts').mkdir()
    for name in ('session.py', 'reload-mcp.mjs'):
        shutil.copy2(repo / 'desktop/scripts/guest' / name, linux / 'scripts' / name)
    if system == 'linux':
        subprocess.run(['npm', 'install', '--prefix', linux / 'claude', '--omit=dev', '@anthropic-ai/claude-code@2.1.310'], check=True)
        shutil.copy2(shutil.which('node'), linux / 'node')
        subprocess.run([linux / 'claude/node_modules/.bin/claude', '--version'], check=True)

    # Include the notices from the exact source dependencies used by the build.
    for module_root in (repo, repo / 'runtime', repo / 'gateways/cliproxy'):
        data = subprocess.check_output(['go', 'list', '-m', '-json', 'all'], cwd=module_root, text=True)
        decoder = json.JSONDecoder()
        while data.strip():
            module, count = decoder.raw_decode(data.lstrip())
            data = data.lstrip()[count:]
            if not module.get('Dir'): continue
            label = module['Path'].replace('/', '_') + '@' + module.get('Version', 'local')
            for name in ('LICENSE', 'LICENSE.txt', 'LICENSE.md', 'NOTICE', 'COPYING', 'COPYRIGHT'):
                source = Path(module['Dir']) / name
                if source.is_file() and not (notices / (label + '-' + name)).exists():
                    shutil.copy2(source, notices / (label + '-' + name))
    if system == 'darwin':
        for dependency in (repo / '.build/checkouts').iterdir():
            for name in ('LICENSE', 'LICENSE.txt', 'LICENSE.md', 'NOTICE'):
                source = dependency / name
                if source.is_file(): shutil.copy2(source, notices / (dependency.name + '-' + name))
    # Build-time provenance is public; it contains no host configuration.
    versions = {'cliproxy': '8.0.42', 'bifrost': '2.3.2', 'freerdp': '3.32.2',
        'commit': subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=repo, text=True).strip()}
    files = {}
    for file in sorted(destination.rglob('*')):
        if file.is_file():
            with file.open('rb') as stream:
                files[str(file.relative_to(destination))] = hashlib.file_digest(stream, 'sha256').hexdigest()
    (destination / 'manifest.json').write_text(json.dumps({'platform': system, 'arch': 'x64' if arch == 'amd64' else arch, 'versions': versions, 'files': files}, indent=2) + '\n')
print('Runtime staged:', destination)
