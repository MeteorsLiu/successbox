#!/usr/bin/env python3
"""Prepare only the guest's disposable view, then launch in its RDP session."""
import json
import os
from pathlib import Path
import pwd
import shutil
import socket
import subprocess
import sys
import time

mode = sys.argv[1]
if mode in ('prepare', 'configure'):
    options = json.load(sys.stdin)
    home = Path(options['home'])
    home.mkdir(parents=True, exist_ok=True)
    target_uid = options['uid']
    if mode == 'prepare':
        desktop_uid = target_uid if target_uid else 1000
        try:
            desktop_user = pwd.getpwuid(desktop_uid)
        except KeyError:
            subprocess.run(['useradd', '-m', '-u', str(desktop_uid), '-s', '/bin/sh', 'successbox'], check=True)
            desktop_user = pwd.getpwuid(desktop_uid)
        # The password and all account-file changes exist only in this sandbox.
        subprocess.run(['chpasswd'], input=desktop_user.pw_name + ':' + options['password'] + '\n', text=True, check=True)
        session_home = Path(desktop_user.pw_dir)
        session_home.mkdir(parents=True, exist_ok=True)
        xsession = session_home / '.xsession'
        (session_home / '.successbox-desktop-session.json').unlink(missing_ok=True)
        xsession.write_text('exec dbus-run-session /usr/bin/python3 /opt/successbox-runtime/scripts/session.py session\n')
        xsession.chmod(0o700)
        os.chown(xsession, desktop_uid, desktop_user.pw_gid)
        openbox = session_home / '.config/openbox'
        openbox.mkdir(parents=True, exist_ok=True)
        rc = Path('/etc/xdg/openbox/rc.xml').read_text().replace('<applications>', '<applications><application class="ChatGPT"><maximized>yes</maximized></application><application class="Chatgpt"><maximized>yes</maximized></application>')
        (openbox / 'rc.xml').write_text(rc)
        for p in (session_home / '.config', openbox, openbox / 'rc.xml'):
            os.chown(p, desktop_uid, desktop_user.pw_gid)
    codex = home / '.codex'
    codex.mkdir(exist_ok=True)
    (codex / 'ipc').mkdir(exist_ok=True)
    if mode == 'prepare':
        subprocess.run(['mount', '-t', 'tmpfs', '-o', 'mode=700', 'tmpfs', str(codex / 'ipc')], check=True)
    # Unlink before replacement also avoids stale virtiofs handles retained by
    # the app. User preferences and sessions remain in the same HOME view.
    replacements = {codex / 'config.toml': options['codexConfig'], codex / 'auth.json': json.dumps({'auth_mode': 'apikey', 'OPENAI_API_KEY': options['codexToken']})}
    claude = home / '.claude'
    claude.mkdir(exist_ok=True)
    settings_path = claude / 'settings.json'
    settings = json.loads(settings_path.read_text()) if settings_path.exists() else {}
    settings['env'] = {**settings.get('env', {}), 'ANTHROPIC_BASE_URL': options['url'], 'ANTHROPIC_API_KEY': options['claudeToken']}
    settings['env'].pop('ANTHROPIC_AUTH_TOKEN', None)
    replacements[settings_path] = json.dumps(settings)
    credentials = claude / '.credentials.json'
    credentials.unlink(missing_ok=True)
    user_config = home / '.claude.json'
    config = json.loads(user_config.read_text()) if user_config.exists() else {}
    config['mcpServers'] = {'successbox': {'type': 'http', 'url': options['url'] + '/mcp', 'headers': {'Authorization': 'Bearer ' + options['claudeMcpToken']}}}
    replacements[user_config] = json.dumps(config)
    state = codex / '.codex-global-state.json'
    data = json.loads(state.read_text()) if state.exists() else {}
    data.setdefault('electron-persisted-atom-state', {}).update({'electron:onboarding-override': 'auto', 'electron:onboarding-welcome-pending': False, 'electron:onboarding-projectless-completed': True, 'electron:onboarding-hide-first-new-thread-promos': True, 'last_completed_onboarding': int(time.time())})
    data['electron-main-window-bounds'] = {'x': 0, 'y': 0, 'width': 1280, 'height': 800, 'isMaximized': True}
    replacements[state] = json.dumps(data)
    for filename, contents in replacements.items():
        filename.unlink(missing_ok=True)
        filename.write_text(contents)
        filename.chmod(0o600)
        os.chown(filename, target_uid, options['gid'])
    for filename in ('SingletonLock', 'SingletonCookie', 'SingletonSocket'):
        (home / '.config/Codex' / filename).unlink(missing_ok=True)
    if mode == 'prepare':
        os.chown(codex / 'ipc', target_uid, options['gid'])
    if mode == 'configure':
        raise SystemExit(0)
    if not Path('/usr/bin/chatgpt').exists():
        Path('/usr/bin/chatgpt').symlink_to('../lib/chatgpt/codex-launcher')
    bundled = Path('/opt/successbox-runtime/xrdp')
    xrdp = str(bundled / 'usr/sbin/xrdp') if bundled.exists() else '/usr/sbin/xrdp'
    sesman = str(bundled / 'usr/sbin/xrdp-sesman') if bundled.exists() else '/usr/sbin/xrdp-sesman'
    if bundled.exists():
        Path('/etc/xrdp').mkdir(exist_ok=True)
        for source in (bundled / 'etc/xrdp').iterdir():
            if source.is_file() and not source.is_symlink():
                shutil.copy2(source, Path('/etc/xrdp') / source.name)
        pam = bundled / 'etc/pam.d/xrdp-sesman'
        if pam.exists(): shutil.copy2(pam, '/etc/pam.d/xrdp-sesman')
        for binary in ('xrdp-chansrv',):
            target = Path('/usr/sbin') / binary
            target.unlink(missing_ok=True)
            target.symlink_to(bundled / 'usr/sbin' / binary)
        # Use the distribution's Xvnc without installing an RDP host service.
        sesman_config = Path('/etc/xrdp/sesman.ini')
        sesman_config.write_text(sesman_config.read_text().replace('param=Xvnc', 'param=/usr/bin/Xtigervnc'))
    Path('/run/xrdp').mkdir(exist_ok=True)
    Path('/run/dbus').mkdir(exist_ok=True)
    subprocess.run(['dbus-uuidgen', '--ensure=/etc/machine-id'], check=True)
    subprocess.run(['dbus-daemon', '--system', '--fork'], check=True)
    subprocess.run(['openssl', 'req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-subj', '/CN=Successbox', '-days', '1', '-keyout', '/etc/xrdp/key.pem', '-out', '/etc/xrdp/cert.pem'], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=True)
    ini = Path('/etc/xrdp/xrdp.ini')
    text = ini.read_text()
    # Keep one explicit auto-login module; RDP still authenticates the random
    # per-session password. TLS uses the certificate fingerprint read by sbx.
    text = text.replace('autorun=', 'autorun=' + ('Xvnc' if bundled.exists() else 'Xorg'))
    if bundled.exists():
        module = next((bundled / 'usr/lib').glob('**/libvnc.so'))
        text = text.replace('lib=libvnc.so', 'lib=' + str(module))
    ini.write_text(text)
    environment = dict(os.environ)
    if bundled.exists():
        environment['LD_LIBRARY_PATH'] = ':'.join(str(p) for p in (bundled / 'usr/lib').glob('**/xrdp'))
    Path('/run/successbox-desktop.json').write_text(json.dumps({**options, 'user': desktop_user.pw_name, 'sessionHome': str(session_home)}))
    services = []
    with open('/tmp/successbox-rdp.log', 'ab') as log:
        for command in ([sesman, '--nodaemon'], [xrdp, '--nodaemon']):
            services.append(subprocess.Popen(command, env=environment, stdout=log, stderr=log, start_new_session=True))
    deadline = time.monotonic() + 15
    while True:
        for process in services:
            if process.poll() is not None:
                raise RuntimeError('RDP service exited ' + str(process.returncode) + ': ' + Path('/tmp/successbox-rdp.log').read_text()[-4096:])
        try:
            with socket.create_connection(('127.0.0.1', 3389), timeout=0.2): pass
            break
        except OSError:
            if time.monotonic() > deadline: raise RuntimeError('RDP listener did not start')
            time.sleep(0.1)
    print(json.dumps({'username': desktop_user.pw_name, 'certificate': Path('/etc/xrdp/cert.pem').read_text()}))
elif mode == 'session':
    # The session owns its display/DBus environment. Reading another UID's
    # /proc/PID/environ requires ptrace rights, which the sandbox does not grant.
    session = Path.home() / '.successbox-desktop-session.json'
    temporary = session.with_suffix('.tmp')
    temporary.write_text(json.dumps({key: os.environ[key] for key in ('DISPLAY', 'DBUS_SESSION_BUS_ADDRESS', 'XDG_RUNTIME_DIR') if key in os.environ}))
    temporary.chmod(0o600)
    temporary.replace(session)
    os.execvp('openbox-session', ['openbox-session'])
else:
    options = json.loads(Path('/run/successbox-desktop.json').read_text())
    deadline = time.monotonic() + 90
    session = Path(options['sessionHome']) / '.successbox-desktop-session.json'
    while not session.exists():
        if time.monotonic() > deadline: raise RuntimeError('RDP graphical session did not start')
        time.sleep(0.2)
    os.environ.update(json.loads(session.read_text()))
    os.environ['HOME'] = options['home']
    os.environ['XAUTHORITY'] = options['sessionHome'] + '/.Xauthority'
    os.chdir(options['home'])
    if options['uid']:
        os.setgroups([])
        os.setgid(options['gid'])
        os.setuid(options['uid'])
    argv = ['/usr/bin/chatgpt', '--remote-debugging-port=9333', '--remote-debugging-address=127.0.0.1']
    if not options['uid']: argv += ['--no-sandbox']
    os.execv(argv[0], argv)
