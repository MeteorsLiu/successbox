import type {
  AgentId,
  AppState,
  BackendApi,
  ChangeRecord,
} from '../../shared/types';

export type BackendEvent =
  | { kind: 'run-ended'; change: ChangeRecord }
  | { kind: 'run-blocked'; agent: AgentId };

/**
 * The main process reaches the sandbox service and the gateways only through
 * this interface. `MockBackend` simulates everything today; a real
 * implementation replaces it once the RPCs are settled.
 */
export interface Backend extends BackendApi {
  snapshot(): AppState;
  subscribe(listener: () => void): () => void;
  onEvent(listener: (event: BackendEvent) => void): () => void;
}
