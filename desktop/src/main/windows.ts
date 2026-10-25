import { app, BrowserWindow, type WebContents } from 'electron';
import path from 'node:path';
import type { Page } from '../shared/types';

export function assetPath(name: string) {
  return app.isPackaged
    ? path.join(process.resourcesPath, 'assets', name)
    : path.join(app.getAppPath(), 'assets', name);
}

const webPreferences = {
  preload: path.join(__dirname, 'preload.cjs'),
  contextIsolation: true,
  sandbox: true,
  nodeIntegration: false,
};

function load(win: BrowserWindow, hash?: string) {
  if (MAIN_WINDOW_VITE_DEV_SERVER_URL) {
    void win.loadURL(
      MAIN_WINDOW_VITE_DEV_SERVER_URL + (hash ? `#${hash}` : ''),
    );
  } else {
    void win.loadFile(
      path.join(__dirname, `../renderer/${MAIN_WINDOW_VITE_NAME}/index.html`),
      hash ? { hash } : undefined,
    );
  }
}

let quitting = false;
export function setQuitting() {
  quitting = true;
}

/* ---------- Main window ---------- */

let mainWindow: BrowserWindow | null = null;

function createMainWindow() {
  const win = new BrowserWindow({
    width: 980,
    height: 740,
    minWidth: 760,
    minHeight: 560,
    title: 'Successbox',
    icon: assetPath('icon.png'),
    show: false,
    autoHideMenuBar: true,
    webPreferences,
  });
  win.once('ready-to-show', () => win.show());
  // Closing the window keeps Successbox in the tray; quitting is explicit.
  win.on('close', (event) => {
    if (quitting) return;
    event.preventDefault();
    win.hide();
  });
  win.on('closed', () => {
    mainWindow = null;
  });
  load(win);
  return win;
}

export function showMainWindow(page?: Page) {
  if (!mainWindow) mainWindow = createMainWindow();
  const win = mainWindow;
  win.show();
  win.focus();
  if (!page) return;
  if (win.webContents.isLoading())
    win.webContents.once('did-finish-load', () =>
      win.webContents.send('sbx:navigate', page),
    );
  else win.webContents.send('sbx:navigate', page);
}

/* ---------- Trusted window ---------- */

// A separate, always-on-top window owned by Successbox. Sensitive input and
// approvals only ever appear here, never in a terminal or Agent UI.
let trustedWindow: BrowserWindow | null = null;
const closedBySync = new WeakSet<BrowserWindow>();

export function isTrustedSender(sender: WebContents) {
  return (
    trustedWindow !== null &&
    !trustedWindow.isDestroyed() &&
    trustedWindow.webContents === sender
  );
}

export function fitTrustedWindow(sender: WebContents, height: number) {
  if (!isTrustedSender(sender) || !trustedWindow) return;
  const [width] = trustedWindow.getContentSize();
  trustedWindow.setContentSize(
    width,
    Math.min(Math.max(Math.round(height), 240), 760),
  );
}

export function focusTrustedWindow() {
  trustedWindow?.show();
  trustedWindow?.focus();
}

/**
 * Opens the trusted window while requests are pending and closes it when the
 * queue is empty. `onUserClose` answers the current request as declined.
 */
export function syncTrustedWindow(pending: boolean, onUserClose: () => void) {
  if (pending && !trustedWindow) {
    const win = new BrowserWindow({
      width: 480,
      height: 460,
      resizable: false,
      minimizable: false,
      maximizable: false,
      fullscreenable: false,
      alwaysOnTop: true,
      title: 'Successbox',
      icon: assetPath('icon.png'),
      show: false,
      autoHideMenuBar: true,
      webPreferences,
    });
    win.once('ready-to-show', () => {
      win.show();
      win.focus();
    });
    // However the window goes away (title bar, keyboard shortcut, script),
    // closing it declines the current request. If more requests are queued,
    // the next state change opens a fresh window.
    win.on('closed', () => {
      if (trustedWindow === win) trustedWindow = null;
      if (!closedBySync.has(win) && !quitting) onUserClose();
    });
    load(win, '/trusted');
    trustedWindow = win;
  } else if (!pending && trustedWindow) {
    closedBySync.add(trustedWindow);
    trustedWindow.destroy();
    trustedWindow = null;
  }
}
