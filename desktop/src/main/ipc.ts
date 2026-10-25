import { BrowserWindow, ipcMain, shell } from 'electron';
import { existsSync } from 'node:fs';
import { homedir } from 'node:os';
import { API_METHODS, type ApiMethod } from '../shared/types';
import type { Backend } from './backend/backend';
import { scanImports } from './importer';
import { fitTrustedWindow, isTrustedSender } from './windows';

// Answers to sensitive requests are only accepted from the trusted window.
const TRUSTED_ONLY: ApiMethod[] = ['answerSecret', 'answerInstall'];

async function openPath(path: string) {
  const resolved = path.replace(/^~(?=\/|$)/, homedir());
  if (!existsSync(resolved)) throw new Error(`路径不存在：${path}`);
  const error = await shell.openPath(resolved);
  if (error) throw new Error(error);
}

export function registerIpc(backend: Backend) {
  ipcMain.handle(
    'sbx:call',
    async (event, method: ApiMethod, args: unknown[]) => {
      if (!API_METHODS.includes(method)) throw new Error(`未知操作：${method}`);
      if (TRUSTED_ONLY.includes(method) && !isTrustedSender(event.sender))
        throw new Error('只能在 Successbox 可信窗口中操作');
      switch (method) {
        case 'getState':
          return backend.snapshot();
        case 'scanImports':
          return scanImports();
        case 'openPath':
          return openPath(args[0] as string);
        default: {
          const fn = backend[method] as (...a: unknown[]) => Promise<unknown>;
          return fn.apply(backend, args);
        }
      }
    },
  );

  ipcMain.on('sbx:fit-trusted', (event, height: number) =>
    fitTrustedWindow(event.sender, height),
  );

  backend.subscribe(() => {
    const state = backend.snapshot();
    for (const win of BrowserWindow.getAllWindows()) {
      if (!win.isDestroyed()) win.webContents.send('sbx:state', state);
    }
  });
}
