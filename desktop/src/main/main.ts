import { app, dialog, Notification } from 'electron';
import path from 'node:path';
import { AGENT_NAMES } from '../shared/agents';
import type { Backend, BackendEvent } from './backend/backend';
import { MockBackend } from './backend/mock';
import { registerIpc } from './ipc';
import { Store } from './store';
import { createTray } from './tray';
import {
  assetPath,
  focusTrustedWindow,
  setQuitting,
  showMainWindow,
  syncTrustedWindow,
} from './windows';

if (!app.requestSingleInstanceLock()) app.quit();
app.setName('Successbox');

function notify(title: string, body: string, onClick?: () => void) {
  if (!Notification.isSupported()) return;
  const notification = new Notification({
    title,
    body,
    icon: assetPath('icon.png'),
  });
  if (onClick) notification.on('click', onClick);
  notification.show();
}

function handleEvent(event: BackendEvent) {
  switch (event.kind) {
    case 'run-ended': {
      const { change } = event;
      const saved = change.worktrees.length
        ? `已保存为 ${change.worktrees.length} 个 worktree`
        : '已保存';
      notify(
        `${AGENT_NAMES[change.agent]} 已结束`,
        `${change.cwd} 的改动${saved}`,
        () => showMainWindow('changes'),
      );
      break;
    }
    case 'run-blocked':
      notify(
        `${AGENT_NAMES[event.agent]} 没有启动`,
        '沙箱服务没有运行。开启沙箱保护时不会改为直接在本机运行。',
        () => showMainWindow('agent'),
      );
      break;
  }
}

app.whenReady().then(() => {
  const store = new Store<{ welcomed: boolean }>(
    path.join(app.getPath('userData'), 'preferences.json'),
  );
  const backend: Backend = new MockBackend(!store.get('welcomed'));

  // The first one in the queue is the request the trusted window shows.
  const declineCurrent = () => {
    const [current] = backend.snapshot().requests;
    if (!current) return;
    const answer =
      current.kind === 'secret'
        ? backend.answerSecret(current.id, null, false)
        : backend.answerInstall(current.id, null);
    void answer;
  };

  backend.subscribe(() => {
    const state = backend.snapshot();
    if (!state.firstRun && !store.get('welcomed')) store.set('welcomed', true);
    syncTrustedWindow(state.requests.length > 0, declineCurrent);
  });
  backend.onEvent(handleEvent);

  registerIpc(backend);
  createTray(backend, {
    showMain: showMainWindow,
    focusTrusted: focusTrustedWindow,
    quit: () => app.quit(),
    error: (message) => dialog.showErrorBox('Successbox', message),
  });
  showMainWindow();
});

app.on('second-instance', () => showMainWindow());
app.on('activate', () => showMainWindow());
app.on('before-quit', setQuitting);
// Successbox stays in the tray after its windows close.
app.on('window-all-closed', () => undefined);

app.on('web-contents-created', (_event, contents) => {
  contents.setWindowOpenHandler(() => ({ action: 'deny' }));
  contents.on('will-navigate', (event, url) => {
    const allowed = MAIN_WINDOW_VITE_DEV_SERVER_URL
      ? url.startsWith(MAIN_WINDOW_VITE_DEV_SERVER_URL)
      : url.startsWith('file://');
    if (!allowed) event.preventDefault();
  });
});
