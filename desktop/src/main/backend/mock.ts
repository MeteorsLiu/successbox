import type {
  AgentId,
  AppState,
  ChangeRecord,
  DiffFile,
  InstallDecision,
  NewMcp,
  NewProvider,
  Settings,
  Simulation,
} from '../../shared/types';
import type { Backend, BackendEvent } from './backend';

const wait = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));
const id = () => Math.random().toString(16).slice(2, 8);
const clock = () =>
  new Date().toLocaleTimeString('zh-CN', {
    hour: '2-digit',
    minute: '2-digit',
  });
const SECRET_TTL = 5 * 60 * 1000;

function initialState(): AppState {
  return {
    agents: {
      claude: {
        protection: true,
        providers: [
          {
            id: 'cl-max',
            name: 'Claude Max',
            kind: 'subscription',
            detail: 'work@company.com · 官方登录',
            active: true,
            preset: 'claude-subscription',
          },
          {
            id: 'cl-key',
            name: 'Anthropic API',
            kind: 'apiKey',
            detail: 'sk-ant-•••• a3F9',
            active: false,
            preset: 'anthropic-api',
          },
          {
            id: 'cl-ds',
            name: 'DeepSeek',
            kind: 'apiKey',
            detail: 'api.deepseek.com/anthropic · sk-•••• 77c1',
            active: false,
            preset: 'deepseek',
          },
        ],
      },
      codex: {
        protection: true,
        providers: [
          {
            id: 'cx-gpt',
            name: 'ChatGPT Plus',
            kind: 'subscription',
            detail: 'me@example.com · 官方登录',
            active: true,
            preset: 'chatgpt-subscription',
          },
          {
            id: 'cx-key',
            name: 'OpenAI API（团队）',
            kind: 'apiKey',
            detail: 'sk-proj-•••• 9d2e',
            active: false,
            preset: 'openai-api',
            problem: '上游返回 401，请更新 Key',
          },
        ],
      },
    },
    mcps: [
      {
        id: 'github',
        name: 'GitHub',
        transport: 'http',
        target: 'https://api.githubcopilot.com/mcp',
        auth: 'oauth',
        agents: { claude: true, codex: true },
      },
      {
        id: 'context7',
        name: 'context7',
        transport: 'http',
        target: 'https://mcp.context7.com/mcp',
        auth: 'none',
        agents: { claude: true, codex: false },
      },
      {
        id: 'linear',
        name: 'Linear',
        transport: 'http',
        target: 'https://mcp.linear.app/mcp',
        auth: 'apiKey',
        agents: { claude: true, codex: true },
      },
      {
        id: 'project-tools',
        name: 'project-tools',
        transport: 'http',
        target: 'https://tools.example.internal/mcp',
        auth: 'askOnUse',
        agents: { claude: true, codex: false },
      },
      {
        id: 'postgres',
        name: 'postgres',
        transport: 'stdio',
        target: 'npx @modelcontextprotocol/server-postgres',
        auth: 'askOnUse',
        agents: { claude: false, codex: true },
      },
    ],
    secrets: [{ mcpId: 'linear', field: 'token', savedAt: '10月20日' }],
    runs: [
      {
        id: 'run-old',
        agent: 'claude',
        cwd: '~/code/infra',
        protected: false,
        startedAt: '09:12',
      },
    ],
    changes: [
      {
        id: 'sb-55aa90',
        agent: 'claude',
        cwd: '~/code/successbox',
        provider: 'Claude Max',
        endedAt: '今天 11:40',
        worktrees: [
          {
            repo: '~/code/successbox',
            path: '~/.local/share/successbox/results/sb-55aa90/repo-1/worktree',
            files: 4,
            additions: 146,
            deletions: 12,
          },
        ],
        copies: [],
        discarded: 6,
        sessionSynced: false,
      },
      {
        id: 'sb-3c7d12',
        agent: 'codex',
        cwd: '~/Documents/reports',
        provider: 'ChatGPT Plus',
        endedAt: '昨天 18:02',
        worktrees: [],
        copies: [
          {
            source: '~/Documents/reports',
            path: '~/Successbox/副本/1024-1802/Documents/reports',
            files: 6,
          },
        ],
        discarded: 0,
        sessionSynced: false,
      },
    ],
    unreadChanges: 0,
    requests: [],
    settings: {
      commandLine: true,
      terminalHints: true,
      copyDir: '~/Successbox/副本',
    },
    services: { sandbox: 'ok', llmGateway: 'ok', mcpGateway: 'ok' },
    firstRun: false,
    simulated: true,
  };
}

const SAMPLE_DIFF: DiffFile[] = [
  {
    path: 'cmd/successbox-gvisor/server.go',
    status: 'M',
    additions: 24,
    deletions: 9,
    lines: [
      {
        kind: '@',
        text: '@@ -412,31 +412,12 @@ func (s *server) Display(stream sandboxv1.SandboxService_DisplayServer) error {',
      },
      { kind: ' ', text: '\tfirst, err := stream.Recv()' },
      { kind: ' ', text: '\tif err != nil {' },
      { kind: ' ', text: '\t\treturn err' },
      { kind: ' ', text: '\t}' },
      { kind: '-', text: '\tref := first.GetConnect()' },
      { kind: '-', text: '\tif ref == nil || ref.Id == "" {' },
      {
        kind: '-',
        text: '\t\treturn status.Error(codes.InvalidArgument, "first message must be connect")',
      },
      { kind: '-', text: '\t}' },
      { kind: '+', text: '\tbox, err := s.displayTarget(first)' },
      { kind: '+', text: '\tif err != nil {' },
      { kind: '+', text: '\t\treturn err' },
      { kind: '+', text: '\t}' },
      {
        kind: ' ',
        text: '\trelay, err := box.startRelay(stream.Context(), s.rdpPort)',
      },
    ],
  },
  {
    path: 'cmd/successbox-gvisor/display.go',
    status: 'A',
    additions: 88,
    deletions: 0,
    lines: [
      { kind: '@', text: '@@ -0,0 +1,88 @@' },
      { kind: '+', text: 'package main' },
      { kind: '+', text: '' },
      {
        kind: '+',
        text: '// displayTarget validates the first Display message and returns its sandbox.',
      },
      {
        kind: '+',
        text: 'func (s *server) displayTarget(first *sandboxv1.DisplayInput) (*box, error) {',
      },
      { kind: '+', text: '\tref := first.GetConnect()' },
    ],
  },
  {
    path: 'README.md',
    status: 'M',
    additions: 3,
    deletions: 1,
    lines: [
      { kind: '@', text: '@@ -88,7 +88,9 @@' },
      { kind: '-', text: '旧的说明' },
      { kind: '+', text: '新的说明' },
      { kind: '+', text: '补充的测试说明' },
    ],
  },
];

/** A self-contained stand-in for the sandbox service and gateways. */
export class MockBackend implements Backend {
  private state = initialState();
  private listeners = new Set<() => void>();
  private eventListeners = new Set<(event: BackendEvent) => void>();
  /** Runs started by the simulation that already did some work. */
  private touched = new Set<string>();

  constructor(firstRun: boolean) {
    this.state.firstRun = firstRun;
    setInterval(() => this.expireRequests(), 1000);
  }

  snapshot(): AppState {
    return structuredClone(this.state);
  }

  subscribe(listener: () => void) {
    this.listeners.add(listener);
    return () => this.listeners.delete(listener);
  }

  onEvent(listener: (event: BackendEvent) => void) {
    this.eventListeners.add(listener);
    return () => this.eventListeners.delete(listener);
  }

  private changed() {
    for (const listener of this.listeners) listener();
  }

  private emit(event: BackendEvent) {
    for (const listener of this.eventListeners) listener(event);
  }

  private agent(agent: AgentId) {
    return this.state.agents[agent];
  }

  async setProtection(agent: AgentId, on: boolean) {
    this.agent(agent).protection = on;
    this.changed();
  }

  async activateProvider(agent: AgentId, providerId: string) {
    const provider = this.agent(agent).providers.find(
      (p) => p.id === providerId,
    );
    if (!provider) throw new Error('账号不存在');
    if (provider.problem)
      throw new Error(`${provider.name} 不可用：${provider.problem}`);
    for (const p of this.agent(agent).providers) p.active = p === provider;
    this.changed();
  }

  async addProvider(input: NewProvider) {
    // A subscription opens the provider's login page in the browser; the
    // simulation just waits as if the user finished signing in.
    await wait(input.kind === 'subscription' ? 1800 : 300);
    const tail = id().slice(0, 4);
    const detail =
      input.kind === 'subscription'
        ? 'new@example.com · 官方登录'
        : `${input.endpoint ? input.endpoint.replace(/^https?:\/\//, '') + ' · ' : ''}sk-•••• ${tail}`;
    this.agent(input.agent).providers.push({
      id: `p-${id()}`,
      name: input.name,
      kind: input.kind,
      detail,
      active: false,
      preset: input.preset,
    });
    this.changed();
  }

  async reloginProvider(agent: AgentId, providerId: string) {
    await wait(1800);
    const provider = this.agent(agent).providers.find(
      (p) => p.id === providerId,
    );
    if (provider) delete provider.problem;
    this.changed();
  }

  async removeProvider(agent: AgentId, providerId: string) {
    const a = this.agent(agent);
    const provider = a.providers.find((p) => p.id === providerId);
    if (provider?.active) throw new Error('使用中的账号不能删除');
    a.providers = a.providers.filter((p) => p.id !== providerId);
    this.changed();
  }

  async addMcp(input: NewMcp) {
    await wait(input.auth === 'oauth' ? 2200 : 1000);
    this.state.mcps.push({ id: `m-${id()}`, ...input });
    this.changed();
  }

  async setMcpAgent(mcpId: string, agent: AgentId, enabled: boolean) {
    const mcp = this.state.mcps.find((m) => m.id === mcpId);
    if (mcp) mcp.agents[agent] = enabled;
    this.changed();
  }

  async removeMcp(mcpId: string) {
    this.state.mcps = this.state.mcps.filter((m) => m.id !== mcpId);
    this.state.secrets = this.state.secrets.filter((s) => s.mcpId !== mcpId);
    this.changed();
  }

  async deleteSecret(mcpId: string, field: string) {
    this.state.secrets = this.state.secrets.filter(
      (s) => !(s.mcpId === mcpId && s.field === field),
    );
    this.changed();
  }

  async deleteChange(changeId: string) {
    this.state.changes = this.state.changes.filter((c) => c.id !== changeId);
    this.changed();
  }

  async markChangesRead() {
    if (!this.state.unreadChanges) return;
    this.state.unreadChanges = 0;
    this.changed();
  }

  async getDiff(): Promise<DiffFile[]> {
    return structuredClone(SAMPLE_DIFF);
  }

  async launchInTerminal(agent: AgentId) {
    this.startRun(agent);
  }

  async importConfigs(
    _agents: AgentId[],
    options: { commandLine: boolean; protection: boolean },
  ) {
    await wait(600);
    this.state.settings.commandLine = options.commandLine;
    for (const agent of Object.keys(this.state.agents) as AgentId[])
      this.agent(agent).protection = options.protection;
    this.state.firstRun = false;
    this.changed();
  }

  async dismissWelcome() {
    this.state.firstRun = false;
    this.changed();
  }

  async updateSettings(patch: Partial<Settings>) {
    Object.assign(this.state.settings, patch);
    this.changed();
  }

  async answerSecret(
    requestId: string,
    value: string | null,
    remember: boolean,
  ) {
    const request = this.state.requests.find((r) => r.id === requestId);
    this.state.requests = this.state.requests.filter((r) => r.id !== requestId);
    // The value would be handed to the MCP gateway here. The simulation
    // drops it immediately and keeps only the metadata.
    if (
      request?.kind === 'secret' &&
      value !== null &&
      remember &&
      !this.state.secrets.some(
        (s) => s.mcpId === request.mcpId && s.field === request.field,
      )
    ) {
      this.state.secrets.push({
        mcpId: request.mcpId,
        field: request.field,
        savedAt: '今天 ' + clock(),
      });
    }
    this.changed();
  }

  async answerInstall(requestId: string, decision: InstallDecision | null) {
    const request = this.state.requests.find((r) => r.id === requestId);
    this.state.requests = this.state.requests.filter((r) => r.id !== requestId);
    this.changed();
    if (request?.kind !== 'install' || !decision) return;
    await wait(1800);
    this.state.mcps.push({
      id: `m-${id()}`,
      name: request.name,
      transport: 'http',
      target: request.url,
      auth: request.auth!,
      agents: {
        claude: decision.agents.includes('claude'),
        codex: decision.agents.includes('codex'),
      },
    });
    this.changed();
  }

  async simulate(kind: Simulation) {
    const latest = this.state.runs.filter((r) => r.id !== 'run-old').at(-1);
    switch (kind) {
      case 'run-start':
        this.startRun('claude');
        break;
      case 'run-task': {
        if (!latest) throw new Error('先模拟启动一个 Agent');
        this.touched.add(latest.id);
        const field =
          latest.agent === 'claude'
            ? { mcpId: 'project-tools', tool: 'connect', field: 'api_key' }
            : { mcpId: 'postgres', tool: 'query', field: 'connection_string' };
        if (
          this.state.secrets.some(
            (s) => s.mcpId === field.mcpId && s.field === field.field,
          )
        )
          break;
        this.state.requests.push({
          id: `req-${id()}`,
          kind: 'secret',
          agent: latest.agent,
          cwd: latest.cwd,
          expiresAt: Date.now() + SECRET_TTL,
          ...field,
        });
        this.changed();
        break;
      }
      case 'run-end':
        if (!latest) throw new Error('没有正在运行的 Agent');
        this.endRun(latest.id);
        break;
      case 'install-request':
        this.state.requests.push({
          id: `req-${id()}`,
          kind: 'install',
          agent: latest?.agent ?? 'claude',
          name: 'notion',
          url: 'https://mcp.notion.com/mcp',
          auth: 'oauth',
          tools: [
            { name: 'search', description: '搜索页面', writes: false },
            { name: 'fetch', description: '读取页面', writes: false },
            { name: 'create_page', description: '创建页面', writes: true },
            { name: 'delete_block', description: '删除内容', writes: true },
          ],
        });
        this.changed();
        break;
      case 'toggle-sandbox':
        this.state.services.sandbox =
          this.state.services.sandbox === 'ok' ? 'down' : 'ok';
        this.changed();
        break;
      case 'first-run':
        this.state.firstRun = true;
        this.changed();
        break;
    }
  }

  private startRun(agent: AgentId) {
    const wantsProtection =
      this.state.settings.commandLine && this.agent(agent).protection;
    if (wantsProtection && this.state.services.sandbox !== 'ok') {
      // Fail closed: never fall back to running unprotected.
      this.emit({ kind: 'run-blocked', agent });
      return;
    }
    this.state.runs.push({
      id: `run-${id()}`,
      agent,
      cwd: '~/code/successbox',
      protected: wantsProtection,
      startedAt: clock(),
    });
    this.changed();
  }

  private endRun(runId: string) {
    const run = this.state.runs.find((r) => r.id === runId);
    if (!run) return;
    this.state.runs = this.state.runs.filter((r) => r.id !== runId);
    this.state.requests = this.state.requests.filter(
      (r) =>
        !(r.kind === 'secret' && r.cwd === run.cwd && r.agent === run.agent),
    );
    if (run.protected && this.touched.has(run.id)) {
      const changeId = `sb-${id()}`;
      const change: ChangeRecord = {
        id: changeId,
        agent: run.agent,
        cwd: run.cwd,
        endedAt: '今天 ' + clock(),
        provider:
          this.agent(run.agent).providers.find((p) => p.active)?.name ?? '',
        worktrees: [
          {
            repo: run.cwd,
            path: `~/.local/share/successbox/results/${changeId}/repo-1/worktree`,
            files: 3,
            additions: 115,
            deletions: 10,
          },
        ],
        copies: [],
        discarded: 12,
        sessionSynced: false,
      };
      this.state.changes.unshift(change);
      this.state.unreadChanges++;
      this.emit({ kind: 'run-ended', change });
    }
    this.touched.delete(run.id);
    this.changed();
  }

  private expireRequests() {
    const now = Date.now();
    const before = this.state.requests.length;
    this.state.requests = this.state.requests.filter(
      (r) =>
        r.kind !== 'secret' || r.expiresAt === undefined || r.expiresAt > now,
    );
    if (this.state.requests.length !== before) this.changed();
  }
}
