import { useEffect, useState } from 'react';
import type { AppState } from '../shared/types';
import { api } from './api';

/**
 * The main process owns all state. Components read the latest snapshot from
 * this hook and change it only by calling `api` methods; the main process then
 * pushes the new snapshot to every window.
 */
export function useAppState() {
  const [state, setState] = useState<AppState | null>(null);
  useEffect(() => {
    void api.getState().then(setState);
    return api.onState(setState);
  }, []);
  return state;
}
