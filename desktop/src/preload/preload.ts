import { contextBridge, ipcRenderer, type IpcRendererEvent } from 'electron';
import {
  API_METHODS,
  type ApiMethod,
  type AppState,
  type Page,
  type SuccessboxApi,
} from '../shared/types';

// Remove Electron's "Error invoking remote method ..." prefix so the
// renderer can show the original message.
async function call(method: ApiMethod, args: unknown[]) {
  try {
    return await ipcRenderer.invoke('sbx:call', method, args);
  } catch (err) {
    const message = err instanceof Error ? err.message : String(err);
    throw new Error(
      message.replace(/^Error invoking remote method '[^']+': (Error: )?/, ''),
    );
  }
}

function listen<T>(channel: string, listener: (value: T) => void) {
  const handler = (_event: IpcRendererEvent, value: T) => listener(value);
  ipcRenderer.on(channel, handler);
  return () => {
    ipcRenderer.removeListener(channel, handler);
  };
}

const methods = Object.fromEntries(
  API_METHODS.map((method) => [
    method,
    (...args: unknown[]) => call(method, args),
  ]),
);

const api = {
  ...methods,
  onState: (listener: (state: AppState) => void) =>
    listen('sbx:state', listener),
  onNavigate: (listener: (page: Page) => void) =>
    listen('sbx:navigate', listener),
  fitTrustedWindow: (height: number) =>
    ipcRenderer.send('sbx:fit-trusted', height),
} as SuccessboxApi;

contextBridge.exposeInMainWorld('successbox', api);
