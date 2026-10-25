import type { SuccessboxApi } from '../shared/types';

declare global {
  interface Window {
    successbox: SuccessboxApi;
  }
}

/** Everything the UI does goes through this object (see src/preload/preload.ts). */
export const api = window.successbox;
