// Types shared by the main process, the preload script and the renderer.
// The renderer only ever sees these shapes; it never talks to the sandbox
// service or the gateways directly.

export type AgentId = 'claude' | 'codex';
export const AGENT_IDS: AgentId[] = ['claude', 'codex'];

export type Page = 'agent' | 'mcp' | 'changes' | 'settings';

export interface Provider {
  id: string;
  name: string;
  kind: 'subscription' | 'apiKey';
  /** Non-secret description, e.g. account e-mail, endpoint, last key characters. */
  detail: string;
  active: boolean;
  /** Set when the gateway reports the account as unusable. */
  problem?: string;
  preset: string;
}

export type McpAuth = 'oauth' | 'apiKey' | 'askOnUse' | 'none';

export interface McpServer {
  id: string;
  name: string;
  transport: 'http' | 'stdio';
  /** URL for remote servers, command line for local processes. */
  target: string;
  auth: McpAuth;
  agents: Record<AgentId, boolean>;
  problem?: string;
}

/** Metadata of a value the user saved for an MCP field. The value itself never leaves the backend. */
export interface SavedSecret {
  mcpId: string;
  field: string;
  savedAt: string;
}

/** An Agent process started through the command-line integration. */
export interface Run {
  id: string;
  agent: AgentId;
  cwd: string;
  protected: boolean;
  startedAt: string;
}

export interface ChangeRecord {
  id: string;
  agent: AgentId;
  cwd: string;
  provider: string;
  endedAt: string;
  worktrees: {
    repo: string;
    path: string;
    files: number;
    additions: number;
    deletions: number;
  }[];
  copies: { source: string; path: string; files: number }[];
  /** Number of discarded changes in system directories. */
  discarded: number;
  sessionSynced: boolean;
}

export interface SecretRequest {
  id: string;
  kind: 'secret';
  agent: AgentId;
  cwd: string;
  mcpId: string;
  tool: string;
  field: string;
  expiresAt: number;
}

export interface InstallRequest {
  id: string;
  kind: 'install';
  agent: AgentId;
  name: string;
  url: string;
  auth: McpAuth;
  tools: { name: string; description: string; writes: boolean }[];
}

export type TrustedRequest = SecretRequest | InstallRequest;

export interface Settings {
  /** Route `claude` / `codex` in terminals through Successbox. */
  commandLine: boolean;
  /** Print one line when a protected Agent starts and ends. */
  terminalHints: boolean;
  copyDir: string;
}

export type ServiceState = 'ok' | 'down';

export interface AppState {
  agents: Record<AgentId, { protection: boolean; providers: Provider[] }>;
  mcps: McpServer[];
  secrets: SavedSecret[];
  runs: Run[];
  changes: ChangeRecord[];
  unreadChanges: number;
  /** Queue of requests for the trusted window; the first one is shown. */
  requests: TrustedRequest[];
  settings: Settings;
  services: {
    sandbox: ServiceState;
    llmGateway: ServiceState;
    mcpGateway: ServiceState;
  };
  firstRun: boolean;
  /** True while the backend is the built-in simulation. */
  simulated: boolean;
}

export interface NewProvider {
  agent: AgentId;
  preset: string;
  name: string;
  kind: Provider['kind'];
  endpoint?: string;
  apiKey?: string;
}

export interface NewMcp {
  name: string;
  transport: McpServer['transport'];
  target: string;
  auth: McpAuth;
  agents: Record<AgentId, boolean>;
}

export interface DiffFile {
  path: string;
  status: 'M' | 'A' | 'D';
  additions: number;
  deletions: number;
  lines: { kind: ' ' | '+' | '-' | '@'; text: string }[];
}

export interface ImportCandidate {
  agent: AgentId;
  path: string;
  found: boolean;
  apiKeys: number;
  login: boolean;
  mcpServers: number;
}

export interface InstallDecision {
  tools: string[];
  agents: AgentId[];
}

export type Simulation =
  | 'run-start'
  | 'run-task'
  | 'run-end'
  | 'install-request'
  | 'toggle-sandbox'
  | 'first-run';

/** Operations both the simulation and a real backend implement. */
export interface BackendApi {
  setProtection(agent: AgentId, on: boolean): Promise<void>;
  activateProvider(agent: AgentId, id: string): Promise<void>;
  addProvider(input: NewProvider): Promise<void>;
  reloginProvider(agent: AgentId, id: string): Promise<void>;
  removeProvider(agent: AgentId, id: string): Promise<void>;
  addMcp(input: NewMcp): Promise<void>;
  setMcpAgent(id: string, agent: AgentId, enabled: boolean): Promise<void>;
  removeMcp(id: string): Promise<void>;
  deleteSecret(mcpId: string, field: string): Promise<void>;
  deleteChange(id: string): Promise<void>;
  markChangesRead(): Promise<void>;
  getDiff(changeId: string): Promise<DiffFile[]>;
  launchInTerminal(agent: AgentId): Promise<void>;
  importConfigs(
    agents: AgentId[],
    options: { commandLine: boolean; protection: boolean },
  ): Promise<void>;
  dismissWelcome(): Promise<void>;
  updateSettings(patch: Partial<Settings>): Promise<void>;
  answerSecret(
    id: string,
    value: string | null,
    remember: boolean,
  ): Promise<void>;
  answerInstall(id: string, decision: InstallDecision | null): Promise<void>;
  simulate(kind: Simulation): Promise<void>;
}

/** What the preload script exposes to the renderer as `window.successbox`. */
export interface SuccessboxApi extends BackendApi {
  getState(): Promise<AppState>;
  onState(listener: (state: AppState) => void): () => void;
  onNavigate(listener: (page: Page) => void): () => void;
  scanImports(): Promise<ImportCandidate[]>;
  openPath(path: string): Promise<void>;
  /** Resize the trusted window to its content. Ignored for other windows. */
  fitTrustedWindow(height: number): void;
}

/** Methods the renderer may call through the generic IPC channel. */
export const API_METHODS = [
  'setProtection',
  'activateProvider',
  'addProvider',
  'reloginProvider',
  'removeProvider',
  'addMcp',
  'setMcpAgent',
  'removeMcp',
  'deleteSecret',
  'deleteChange',
  'markChangesRead',
  'getDiff',
  'launchInTerminal',
  'importConfigs',
  'dismissWelcome',
  'updateSettings',
  'answerSecret',
  'answerInstall',
  'simulate',
  'getState',
  'scanImports',
  'openPath',
] as const;

export type ApiMethod = (typeof API_METHODS)[number];
