import { app, dialog, Notification } from 'electron';
import path from 'node:path';
import {
  readFileSync,
  existsSync,
  openSync,
  closeSync,
  readdirSync,
  statSync,
} from 'node:fs';
import {
  mkdtemp,
  writeFile,
  readFile,
  rm,
  mkdir,
  rename,
  chmod,
  readdir,
} from 'node:fs/promises';
import { execFile, spawn } from 'node:child_process';
import { promisify } from 'node:util';
import { tmpdir, homedir, networkInterfaces, userInfo } from 'node:os';
import { createServer } from 'node:net';
import { parse as parseToml, stringify as stringifyToml } from 'smol-toml';
import {
  randomUUID,
  randomBytes,
  X509Certificate,
  createHmac,
} from 'node:crypto';
import * as grpc from '@grpc/grpc-js';
import { loadSync } from '@grpc/proto-loader';
import type {
  AgentId,
  AppState,
  DiffFile,
  InstallDecision,
  NewProvider,
  Provider,
} from '../shared/types';
import { AGENT_COMMANDS, AGENT_NAMES } from '../shared/agents';
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

type SecretKey = {
  sandbox_id: string;
  mcp_id: string;
  field_name: string;
  mcp_name: string;
};
type SecretInput = SecretKey & { operation_id: string };
type SecretRecord = SecretKey & {
  has_value: boolean;
  updated_at?: { seconds: string; nanos: number };
};
type InstallInput = {
  request_id: string;
  sandbox_id: string;
  mcp_name: string;
  transport: string;
  endpoint: string;
  requested_tools: string[];
  credential_fields: string[];
};
type GatewayReply = {
  operation_error?: { code: string };
  secret_list?: { records: SecretRecord[] };
  mutation_accepted?: { record: SecretRecord };
};
type GatewayMessage = {
  correlation_id: string;
  response?: GatewayReply;
  request?: { secret_input?: SecretInput; mcp_install_approval?: InstallInput };
  event?: {
    secret_changed?: { record: SecretRecord };
    mcp_install_changed?: { request_id: string; change: string };
  };
};
type GatewayClient = grpc.Client & {
  gatewaySession(
    metadata: grpc.Metadata,
  ): grpc.ClientDuplexStream<object, GatewayMessage>;
};

type NativeLLM = {
  server: { host: string; port: number };
  oauth: { 'auth-dir': string };
  access: { 'api-keys': string[] };
  'api-keys': Record<
    AgentId,
    Array<{
      name: string;
      'base-url': string;
      keys: Array<{ 'api-key': string }>;
      models?: Array<{ name: string; alias: string }>;
    }>
  >;
  plugins: {
    enabled: boolean;
    dir: string;
    configs: {
      successbox: {
        enabled: boolean;
        bindings: Record<string, Partial<Record<AgentId, string>>>;
      };
    };
  };
  [key: string]: unknown;
};
type RuntimeIdentity = {
  frontend: string;
  gateway: string;
  agent: string;
  admin: string;
  scopes: Record<AgentId, { id: string; llm: string; mcp: string }>;
};

// This implementation is local to the composition root. The renderer keeps
// its existing BackendApi; no transport credentials enter its state snapshots.
class GatewayBackend implements Backend {
  private state: AppState = {
    agents: {
      claude: { protection: false, providers: [] },
      codex: { protection: false, providers: [] },
    },
    mcps: [],
    secrets: [],
    runs: [],
    changes: [],
    unreadChanges: 0,
    requests: [],
    settings: { commandLine: false, terminalHints: false, copyDir: '' },
    services: {
      sandbox: 'unknown',
      llmGateway: 'unknown',
      mcpGateway: 'unknown',
    },
    firstRun: false,
    simulated: false,
    gatewayControl: { status: 'disconnected', message: '尚未配置本机网关连接' },
  };
  private listeners = new Set<() => void>();
  private client?: GatewayClient;
  private stream?: grpc.ClientDuplexStream<object, GatewayMessage>;
  private ready = false;
  private stopped = false;
  private retry?: ReturnType<typeof setTimeout>;
  private pending = new Map<
    string,
    { resolve: (reply: GatewayReply) => void; reject: (error: Error) => void }
  >();
  private inputs = new Map<string, SecretInput>();
  private installs = new Map<string, InstallInput>();
  private identities = new Map<
    string,
    { sandbox_id: string; mcp_id: string }
  >();
  private sandboxes = new Set<string>();
  private revision = 0;

  private initialization: Promise<void>;
  private runtime?: {
    resources: string;
    directory: string;
    identity: RuntimeIdentity;
    ports: { llm: number; mcp: number; control: number; data: number };
    gateway?: ReturnType<typeof spawn>;
    sandbox?: ReturnType<typeof spawn>;
    socket: string;
  };
  private sandboxStart?: Promise<void>;
  private providerSelectors = new Map<string, string>();
  private accountChanges: Promise<void> = Promise.resolve();
  private lifetime = new AbortController();
  private closeRuns = new Set<() => Promise<void>>();

  constructor() {
    this.initialization = this.initialize().catch(() => {
      this.state.gatewayControl = {
        status: 'disconnected',
        message: '服务启动失败，请检查 Successbox 私有日志后重新打开应用',
      };
      this.changed();
      throw new Error('Successbox 服务初始化失败，请检查私有日志');
    });
    void this.initialization.catch(() => undefined);
    app.once('before-quit', (event) => {
      event.preventDefault();
      this.stopped = true;
      this.lifetime.abort();
      clearTimeout(this.retry);
      void (async () => {
        await this.initialization.catch(() => undefined);
        await Promise.allSettled([...this.closeRuns].map((close) => close()));
        this.stream?.cancel();
        this.client?.close();
        for (const child of [this.runtime?.sandbox, this.runtime?.gateway]) {
          if (
            !child?.pid ||
            child.exitCode !== null ||
            child.signalCode !== null
          )
            continue;
          const exited = new Promise<void>((resolve) =>
            child.once('exit', () => resolve()),
          );
          child.stdin?.end();
          await exited;
        }
      })().finally(() => app.quit());
    });
  }

  private async initialize() {
    if (app.isPackaged && !process.env.SUCCESSBOX_GATEWAY_CONFIG) {
      await this.initializeRuntime();
    }
    let configuration: { address: string; token: string; sandboxIds: string[] };
    try {
      const file =
        process.env.SUCCESSBOX_GATEWAY_CONFIG ??
        path.join(
          this.runtime?.directory ?? app.getPath('userData'),
          'gateway.json',
        );
      configuration = JSON.parse(readFileSync(file, 'utf8'));
      if (
        !configuration.address ||
        !configuration.token ||
        !Array.isArray(configuration.sandboxIds) ||
        configuration.sandboxIds.some((id) => typeof id !== 'string' || !id)
      ) {
        throw new Error('invalid configuration');
      }
    } catch {
      this.state.gatewayControl!.message =
        '请先配置本机网关连接，再重新打开 Successbox';
      return;
    }
    const schema = app.isPackaged
      ? path.join(process.resourcesPath, 'gateway.proto')
      : path.resolve(app.getAppPath(), '../api/gateway/v1/gateway.proto');
    const definition = loadSync(schema, {
      keepCase: true,
      longs: String,
      enums: String,
      defaults: true,
      oneofs: true,
    });
    const loaded = grpc.loadPackageDefinition(definition) as unknown as {
      successbox: {
        gateway: {
          v1: {
            GatewayManagementService: new (
              address: string,
              credentials: grpc.ChannelCredentials,
            ) => GatewayClient;
          };
        };
      };
    };
    this.client = new loaded.successbox.gateway.v1.GatewayManagementService(
      configuration.address,
      grpc.credentials.createInsecure(),
    );
    for (const id of configuration.sandboxIds) this.sandboxes.add(id);
    const metadata = new grpc.Metadata();
    metadata.set('x-successbox-client', 'frontend');
    metadata.set('authorization', `Bearer ${configuration.token}`);
    const connect = () => {
      if (this.stopped) return;
      this.state.gatewayControl = {
        status: 'connecting',
        message: '正在连接本机网关',
      };
      this.changed();
      const stream = this.client!.gatewaySession(metadata);
      this.stream = stream;
      const disconnected = () => {
        if (this.stream !== stream) return;
        this.stream = undefined;
        this.ready = false;
        for (const call of this.pending.values())
          call.reject(new Error('网关连接已断开，操作结果可能尚未确认'));
        this.pending.clear();
        this.inputs.clear();
        this.installs.clear();
        this.state.requests = [];
        this.state.gatewayControl = {
          status: 'disconnected',
          message: '网关连接已断开，正在重新连接；列表为上次读取的记录',
        };
        this.changed();
        if (!this.stopped) this.retry = setTimeout(connect, 1000);
      };
      stream.on('error', disconnected);
      stream.on('end', disconnected);
      stream.on('close', disconnected);
      stream.on('data', (message: GatewayMessage) => {
        if (this.stream !== stream) return;
        if (message.response) {
          const call = this.pending.get(message.correlation_id);
          this.pending.delete(message.correlation_id);
          if (message.response.mutation_accepted)
            this.record(message.response.mutation_accepted.record);
          call?.resolve(message.response);
        }
        const input = message.request?.secret_input;
        if (input) {
          const id = `secret:${input.operation_id}`;
          this.inputs.set(id, input);
          if (!this.state.requests.some((request) => request.id === id)) {
            this.state.requests.push({
              id,
              kind: 'secret',
              agent: this.runtime
                ? (['codex', 'claude'] as const).find(
                    (agent) =>
                      this.runtime!.identity.scopes[agent].id ===
                      input.sandbox_id,
                  )
                : undefined,
              sandboxId: input.sandbox_id,
              mcpId: input.mcp_id,
              mcpName: input.mcp_name,
              field: input.field_name,
            });
          }
          this.discover(input.sandbox_id);
        }
        const install = message.request?.mcp_install_approval;
        if (install) {
          const id = `install:${install.request_id}`;
          this.installs.set(id, install);
          if (!this.state.requests.some((request) => request.id === id)) {
            this.state.requests.push({
              id,
              kind: 'install',
              agent: this.runtime
                ? (['codex', 'claude'] as const).find(
                    (agent) =>
                      this.runtime!.identity.scopes[agent].id ===
                      install.sandbox_id,
                  )
                : undefined,
              sandboxId: install.sandbox_id,
              name: install.mcp_name,
              url: install.endpoint,
              transport: install.transport,
              tools: install.requested_tools.map((name) => ({ name })),
              credentialFields: install.credential_fields,
            });
          }
          this.discover(install.sandbox_id);
        }
        const changed = message.event?.secret_changed;
        if (changed) {
          this.record(changed.record);
          this.discover(changed.record.sandbox_id);
          for (const [id, pending] of this.inputs) {
            if (
              changed.record.has_value &&
              pending.sandbox_id === changed.record.sandbox_id &&
              pending.mcp_id === changed.record.mcp_id &&
              pending.field_name === changed.record.field_name
            )
              this.remove(id);
          }
        }
        const installation = message.event?.mcp_install_changed;
        if (installation) {
          this.remove(`install:${installation.request_id}`);
          if (
            ['MCP_INSTALL_FAILED', 'MCP_INSTALL_CANCELLED'].includes(
              installation.change,
            )
          ) {
            this.state.gatewayControl!.message =
              '安装未完成或已取消，请重新发起请求';
          }
        }
        this.changed();
      });
      void this.call(
        {
          request: {
            open_session: {
              sandbox_id: '',
              client_version: 'successbox-desktop',
            },
          },
        },
        true,
      )
        .then((reply) => {
          if (this.stream !== stream) return;
          if (reply.operation_error) throw new Error('session rejected');
          this.ready = true;
          this.state.gatewayControl = {
            status: 'connected',
            message: '已连接本机网关控制服务',
          };
          stream.write({
            correlation_id: randomUUID(),
            request: { resume_session: { replay_pending: true } },
          });
          for (const sandbox of this.sandboxes)
            void this.refresh(sandbox).catch(() => this.listFailed());
          this.changed();
        })
        .catch(() => {
          if (this.stream === stream) {
            stream.cancel();
            disconnected();
          }
        });
    };
    connect();
  }

  snapshot(): AppState {
    return structuredClone(this.state);
  }
  subscribe(listener: () => void) {
    this.listeners.add(listener);
    return () => {
      this.listeners.delete(listener);
    };
  }
  onEvent(_listener: (event: BackendEvent) => void) {
    return () => {};
  }
  private changed() {
    for (const listener of this.listeners) listener();
  }
  private remove(id: string) {
    this.inputs.delete(id);
    this.installs.delete(id);
    this.state.requests = this.state.requests.filter(
      (request) => request.id !== id,
    );
  }
  private call(body: object, opening = false): Promise<GatewayReply> {
    if (!this.stream || (!this.ready && !opening))
      return Promise.reject(new Error('网关未连接'));
    const correlation_id = randomUUID();
    return new Promise((resolve, reject) => {
      this.pending.set(correlation_id, { resolve, reject });
      this.stream!.write({ correlation_id, ...body });
    });
  }
  private check(reply: GatewayReply) {
    if (reply.operation_error)
      throw new Error(`网关操作失败（${reply.operation_error.code}）`);
  }
  private discover(sandbox: string) {
    if (this.sandboxes.has(sandbox)) return;
    this.sandboxes.add(sandbox);
    void this.refresh(sandbox).catch(() => this.listFailed());
  }
  private listFailed() {
    if (this.ready) {
      this.state.gatewayControl!.message =
        '密钥列表读取失败，重新连接后会再次读取';
      this.changed();
    }
  }
  private record(record: SecretRecord) {
    this.revision++;
    // The UI ID includes the sandbox. Existing deleteSecret(id, field) stays
    // unambiguous even when A and B use the same Bifrost client and field.
    const id = JSON.stringify([record.sandbox_id, record.mcp_id]);
    this.identities.set(id, {
      sandbox_id: record.sandbox_id,
      mcp_id: record.mcp_id,
    });
    this.state.secrets = this.state.secrets.filter(
      (secret) => !(secret.mcpId === id && secret.field === record.field_name),
    );
    if (record.has_value) {
      this.state.secrets.push({
        mcpId: id,
        sandboxId: record.sandbox_id,
        mcpName: record.mcp_name || record.mcp_id,
        field: record.field_name,
        savedAt: record.updated_at
          ? new Date(
              Number(record.updated_at.seconds) * 1000 +
                record.updated_at.nanos / 1e6,
            ).toLocaleString()
          : '—',
      });
    }
  }
  private async refresh(sandbox: string): Promise<void> {
    const revision = this.revision;
    const reply = await this.call({
      request: { list_mcp_secrets: { sandbox_id: sandbox } },
    });
    this.check(reply);
    // Other peers can mutate while this list response is in flight.
    if (revision !== this.revision) return this.refresh(sandbox);
    this.state.secrets = this.state.secrets.filter(
      (secret) => secret.sandboxId !== sandbox,
    );
    for (const record of reply.secret_list!.records) this.record(record);
    this.changed();
  }
  async deleteSecret(id: string, field: string) {
    const identity = this.identities.get(id);
    if (!identity) throw new Error('密钥记录不存在，请重新读取列表');
    this.check(
      await this.call({
        request: { delete_mcp_secret: { ...identity, field_name: field } },
      }),
    );
  }
  async answerSecret(id: string, value: string | null, remember: boolean) {
    const input = this.inputs.get(id);
    if (!input) throw new Error('请求已经结束');
    if (value !== null && !remember)
      throw new Error('当前网关要求保存输入的密钥');
    const reply = await this.call({
      response:
        value === null
          ? {
              cancel_secret: {
                operation_id: input.operation_id,
                reason: 'user declined',
              },
            }
          : {
              provide_secret: {
                ...input,
                secret_value: Buffer.from(value, 'utf8'),
              },
            },
    });
    if (reply.operation_error?.code === 'NotFound') {
      this.remove(id);
      this.changed();
    }
    if (!(value === null && reply.operation_error?.code === 'Aborted'))
      this.check(reply);
    this.remove(id);
    this.changed();
  }
  async answerInstall(id: string, decision: InstallDecision | null) {
    const install = this.installs.get(id);
    if (!install) throw new Error('安装请求已经结束');
    if (
      decision &&
      (decision.agents.length ||
        JSON.stringify([...decision.tools].sort()) !==
          JSON.stringify([...install.requested_tools].sort()))
    )
      throw new Error('当前接口只支持批准本任务请求的完整工具范围');
    // Approval can wait for credentials on this same stream. Let its input
    // window proceed instead of retaining the approval at the queue head.
    const operation = this.call({
      response: {
        decide_mcp_install: {
          operation_id: randomUUID(),
          request_id: install.request_id,
          approve: decision !== null,
          decision_reason: decision ? 'user approved' : 'user declined',
        },
      },
    });
    this.remove(id);
    this.changed();
    const reply = await operation;
    if (reply.operation_error) {
      this.state.gatewayControl!.message = `安装未完成（${reply.operation_error.code}）`;
      this.changed();
    }
    this.check(reply);
  }
  private async initializeRuntime() {
    const resources = path.join(process.resourcesPath, 'runtime');
    const manifest = JSON.parse(
      await readFile(path.join(resources, 'manifest.json'), 'utf8'),
    );
    if (
      manifest.platform !== process.platform ||
      manifest.arch !== process.arch
    )
      throw new Error('运行时与当前系统不匹配');
    // Keep every credential under the backend state mask, including the
    // frontend's control token. Sharing HOME must not reveal these files.
    const directory = path.join(app.getPath('userData'), 'private', 'gvisor');
    await mkdir(directory, { recursive: true, mode: 0o700 });
    await chmod(directory, 0o700);
    const identityPath = path.join(directory, 'identity.json');
    let identity: RuntimeIdentity;
    if (existsSync(identityPath))
      identity = JSON.parse(await readFile(identityPath, 'utf8'));
    else {
      const token = () => randomBytes(32).toString('hex');
      identity = {
        frontend: token(),
        gateway: token(),
        agent: token(),
        admin: token(),
        scopes: {
          codex: { id: randomUUID(), llm: token(), mcp: 'sk-bf-' + token() },
          claude: { id: randomUUID(), llm: token(), mcp: 'sk-bf-' + token() },
        },
      };
      await writeFile(identityPath, JSON.stringify(identity), {
        mode: 0o600,
        flag: 'wx',
      });
    }
    const listeners = [];
    const ports: number[] = [];
    try {
      for (let i = 0; i < 4; i++) {
        const listener = createServer();
        listeners.push(listener);
        await new Promise<void>((resolve, reject) => {
          listener.once('error', reject);
          listener.listen(0, '127.0.0.1', resolve);
        });
        ports.push((listener.address() as { port: number }).port);
      }
    } finally {
      await Promise.all(
        listeners.map(
          (listener) =>
            new Promise<void>((resolve) => listener.close(() => resolve())),
        ),
      );
    }
    this.runtime = {
      resources,
      directory,
      identity,
      ports: {
        llm: ports[0],
        mcp: ports[1],
        control: ports[2],
        data: ports[3],
      },
      socket: path.join(directory, 'control.sock'),
    };
    const r = this.runtime;
    await mkdir(path.join(directory, 'oauth'), {
      recursive: true,
      mode: 0o700,
    });
    await mkdir(path.join(directory, 'mcp'), { recursive: true, mode: 0o700 });
    const llmPath = path.join(directory, 'llm.json');
    const llm: NativeLLM = existsSync(llmPath)
      ? JSON.parse(await readFile(llmPath, 'utf8'))
      : {
          'config-version': 8,
          server: { host: '127.0.0.1', port: ports[0] },
          management: { 'secret-key': '', 'disable-control-panel': true },
          access: { 'api-keys': [] },
          oauth: { 'auth-dir': '' },
          'api-keys': { codex: [], claude: [] },
          plugins: {
            enabled: true,
            dir: '',
            configs: { successbox: { enabled: true, bindings: {} } },
          },
          observability: { logs: { 'request-log': false, debug: false } },
        };
    // Preserve the client's declared tools. The proxy's default image tool
    // injection makes ordinary text requests fail on text-only API accounts.
    llm.multimedia = { 'disable-image-generation': 'passthrough' };
    llm.server = { host: '127.0.0.1', port: ports[0] };
    llm.access['api-keys'] = Object.values(identity.scopes).map(
      (scope) => scope.llm,
    );
    llm.oauth['auth-dir'] = path.join(directory, 'oauth');
    llm.plugins.dir = path.join(resources, 'host', 'plugins');
    await this.writeLLM(llm);
    const mcpConfig = path.join(directory, 'mcp', 'config.json');
    // Bifrost owns installed clients in its database. Never replace that DB.
    if (!existsSync(mcpConfig))
      await writeFile(
        mcpConfig,
        JSON.stringify({
          disable_onboarding: true,
          client: {
            enable_logging: false,
            disable_content_logging: true,
            enforce_governance_header: true,
            mcp_disable_auto_tool_inject: true,
          },
          mcp: { client_configs: [] },
          governance: {
            auth_config: {
              is_enabled: true,
              admin_username: 'successbox',
              admin_password: identity.admin,
            },
            virtual_keys: Object.values(identity.scopes).map((scope) => ({
              id: scope.id,
              name: scope.id,
              value: scope.mcp,
              is_active: true,
            })),
          },
        }),
        { mode: 0o600, flag: 'wx' },
      );
    const address = Object.entries(networkInterfaces())
      .filter(([name]) => !/^(utun|tun|docker|veth)/.test(name))
      .flatMap(([, addresses]) => addresses ?? [])
      .find(
        (entry) =>
          entry.family === 'IPv4' &&
          !entry.internal &&
          !entry.address.startsWith('169.254.'),
      )?.address;
    if (!address) throw new Error('需要一个可供沙箱访问的本机 IPv4 网络地址');
    await writeFile(
      path.join(directory, 'gateway.json'),
      JSON.stringify({
        address: `127.0.0.1:${ports[2]}`,
        token: identity.frontend,
        sandboxIds: Object.values(identity.scopes).map((scope) => scope.id),
        sandbox: {
          client: path.join(resources, 'host', 'sbx'),
          socket: r.socket,
          create: path.join(directory, 'create.json'),
          terminal:
            process.platform === 'darwin'
              ? ['/usr/bin/open', '-a', 'Terminal']
              : ['/usr/bin/x-terminal-emulator', '-e'],
          commands: {
            claude:
              process.platform === 'darwin'
                ? ['/usr/local/bin/claude']
                : ['/opt/successbox-runtime/claude/node_modules/.bin/claude'],
          },
          gateways: Object.fromEntries(
            Object.entries(identity.scopes).map(([agent, scope]) => [
              agent,
              {
                url: `http://${address}:${ports[3]}`,
                model: '',
                llmToken: scope.llm,
                mcpToken: scope.mcp,
              },
            ]),
          ),
          desktop: {
            viewer: [
              path.join(
                resources,
                'host',
                'rdp',
                process.platform === 'darwin' ? 'sdl-freerdp' : 'xfreerdp',
              ),
              '/v:{address}',
            ],
            command: [
              '/usr/bin/python3',
              '/opt/successbox-runtime/scripts/session.py',
              'codex',
            ],
          },
        },
      }),
      { mode: 0o600 },
    );
    const log = openSync(path.join(directory, 'gateway.log'), 'a', 0o600);
    try {
      r.gateway = spawn(
        path.join(resources, 'host', 'sbx'),
        [
          'gateways',
          '--exit-on-stdin-close',
          '--cliproxy',
          path.join(resources, 'host', 'cli-proxy-api'),
          '--bifrost',
          path.join(resources, 'host', 'bifrost-http'),
          '--llm-config',
          llmPath,
          '--mcp-dir',
          path.join(directory, 'mcp'),
          '--mcp-port',
          String(ports[1]),
          '--control-listen',
          `127.0.0.1:${ports[2]}`,
          '--data-listen',
          `${address}:${ports[3]}`,
          '--control-db',
          path.join(directory, 'control.db'),
          '--control-frontend-token',
          identity.frontend,
          '--control-gateway-token',
          identity.gateway,
          '--control-agent-token',
          identity.agent,
        ],
        { stdio: ['pipe', log, log] },
      );
    } finally {
      closeSync(log);
    }
    let failure = false;
    r.gateway.once('error', () => {
      failure = true;
    });
    r.gateway.once('exit', () => {
      failure = true;
      this.state.services.llmGateway = this.state.services.mcpGateway = 'down';
      this.changed();
    });
    const deadline = Date.now() + 60_000;
    while (true) {
      if (failure || this.stopped) throw new Error('网关启动已停止');
      try {
        const response = await fetch(`http://127.0.0.1:${ports[0]}/v1/models`, {
          headers: { Authorization: `Bearer ${identity.scopes.codex.llm}` },
          signal: AbortSignal.timeout(1000),
        });
        if (response.ok) break;
      } catch {
        /* Native gateway is still starting. */
      }
      if (Date.now() > deadline) throw new Error('网关启动超时');
      await new Promise((resolve) => setTimeout(resolve, 200));
    }
    this.state.services.llmGateway = 'ok';
    void this.watchCodexCatalog();
    this.loadProviders();
    this.state.firstRun =
      !this.state.agents.codex.providers.length &&
      !this.state.agents.claude.providers.length;
    this.state.agents.codex.protection =
      this.state.agents.claude.protection = true;
  }

  private async watchCodexCatalog() {
    const r = this.runtime!;
    const endpoint = `http://127.0.0.1:${r.ports.mcp}/mcp`;
    const authorization = `Bearer ${r.identity.scopes.codex.mcp}`;
    while (!this.stopped) {
      let session = '';
      try {
        const initialized = await fetch(endpoint, {
          method: 'POST',
          signal: AbortSignal.any([
            this.lifetime.signal,
            AbortSignal.timeout(5000),
          ]),
          headers: {
            Authorization: authorization,
            'Content-Type': 'application/json',
            Accept: 'application/json, text/event-stream',
          },
          body: JSON.stringify({
            jsonrpc: '2.0',
            id: 1,
            method: 'initialize',
            params: {
              protocolVersion: '2025-06-18',
              capabilities: {},
              clientInfo: {
                name: 'successbox-desktop',
                version: app.getVersion(),
              },
            },
          }),
        });
        if (!initialized.ok || (await initialized.json()).error)
          throw new Error('MCP initialization failed');
        session = initialized.headers.get('Mcp-Session-Id') ?? '';
        if (!session) throw new Error('MCP session missing');
        const response = await fetch(endpoint, {
          signal: this.lifetime.signal,
          headers: {
            Authorization: authorization,
            'Mcp-Session-Id': session,
            Accept: 'text/event-stream',
          },
        });
        if (!response.ok || !response.body)
          throw new Error('MCP notifications unavailable');
        this.state.services.mcpGateway = 'ok';
        this.changed();
        const reader = response.body.getReader();
        const decoder = new TextDecoder();
        let pending = '';
        while (!this.stopped) {
          const next = await reader.read();
          if (next.done) break;
          pending += decoder.decode(next.value, { stream: true });
          const events = pending.split(/\r?\n\r?\n/);
          pending = events.pop()!;
          for (const event of events) {
            const data = event
              .split(/\r?\n/)
              .filter((line) => line.startsWith('data:'))
              .map((line) => line.slice(5).trimStart())
              .join('\n');
            if (
              !data ||
              JSON.parse(data).method !== 'notifications/tools/list_changed'
            )
              continue;
            // This notification follows credential resolution and Bifrost grants.
            // An APPROVED control event arrives earlier and is insufficient.
            for (const run of this.state.runs.filter(
              (run) =>
                run.agent === 'codex' &&
                run.status === 'running' &&
                run.sandboxId,
            )) {
              try {
                await promisify(execFile)(
                  path.join(r.resources, 'host', 'sbx'),
                  [
                    '--socket',
                    r.socket,
                    'exec',
                    '--sandbox',
                    run.sandboxId!,
                    '--',
                    process.platform === 'darwin'
                      ? '/usr/local/bin/node'
                      : '/opt/successbox-runtime/node',
                    '/opt/successbox-runtime/scripts/reload-mcp.mjs',
                  ],
                  { timeout: 20000 },
                );
                this.state.terminalMessage = 'MCP 工具目录已更新';
              } catch {
                this.state.terminalMessage =
                  'MCP 已安装，但 Codex 工具目录刷新失败；请重新打开本次桌面';
              }
              this.changed();
            }
          }
        }
      } catch {
        if (!this.stopped) {
          this.state.services.mcpGateway = 'down';
          this.changed();
        }
      } finally {
        if (session)
          await fetch(endpoint, {
            method: 'DELETE',
            signal: AbortSignal.timeout(1000),
            headers: {
              Authorization: authorization,
              'Mcp-Session-Id': session,
            },
          }).catch(() => undefined);
      }
      if (!this.stopped)
        await new Promise((resolve) => setTimeout(resolve, 1000));
    }
  }

  private readLLM(): NativeLLM {
    if (!this.runtime) throw new Error('账号管理需要内置运行时');
    return JSON.parse(
      readFileSync(path.join(this.runtime.directory, 'llm.json'), 'utf8'),
    );
  }

  private async writeLLM(configuration: NativeLLM) {
    // CLIProxyAPI watches the config inode. Keep it in place; replacing the
    // inode can detach subsequent updates from its fsnotify watch on Linux.
    // Account changes are serialized and its watcher ignores partial writes.
    const filename = path.join(this.runtime!.directory, 'llm.json');
    await writeFile(filename, JSON.stringify(configuration, null, 2), {
      mode: 0o600,
      flush: true,
    });
  }

  private changeAccounts(update: (config: NativeLLM) => Promise<void> | void) {
    const operation = this.accountChanges.then(async () => {
      const configuration = this.readLLM();
      await update(configuration);
      await this.writeLLM(configuration);
      this.loadProviders();
      this.state.firstRun = false;
      this.changed();
    });
    this.accountChanges = operation.catch(() => undefined);
    return operation;
  }

  private loadProviders() {
    const config = this.readLLM();
    this.providerSelectors.clear();
    for (const agent of ['codex', 'claude'] as const) {
      const selected =
        config.plugins.configs.successbox.bindings[
          this.runtime!.identity.scopes[agent].llm
        ]?.[agent];
      const providers: Provider[] = config['api-keys'][agent].map(
        (entry, index) => {
          // UI identities must survive deletion of an earlier native config entry.
          // The private HMAC key prevents exposing a digest of an upstream key.
          const id =
            'api-' +
            createHmac('sha256', this.runtime!.identity.admin)
              .update(JSON.stringify([agent, entry['base-url'], entry.keys]))
              .digest('hex');
          const selector = `${entry['base-url']}#${index}`;
          this.providerSelectors.set(id, selector);
          return {
            id,
            name: entry.name,
            kind: 'apiKey',
            detail: entry['base-url'],
            active: selected === selector,
            preset: `${agent}-custom`,
          };
        },
      );
      for (const filename of readdirSync(config.oauth['auth-dir']).filter(
        (name) => name.endsWith('.json'),
      )) {
        const auth = JSON.parse(
          readFileSync(path.join(config.oauth['auth-dir'], filename), 'utf8'),
        );
        if (auth.type !== agent) continue;
        providers.push({
          id: filename,
          name: agent === 'codex' ? 'ChatGPT 订阅' : 'Claude 订阅',
          kind: 'subscription',
          detail: auth.refresh_token
            ? 'Successbox 登录'
            : '导入的当前登录；过期后可重新登录',
          active: selected === filename,
          preset:
            agent === 'codex' ? 'chatgpt-subscription' : 'claude-subscription',
        });
      }
      this.state.agents[agent].providers = providers;
    }
    this.changed();
  }

  private async startSandbox() {
    const r = this.runtime!;
    if (r.sandbox) {
      if (r.sandbox.exitCode !== null || r.sandbox.signalCode !== null)
        throw new Error('沙箱服务已退出，请重新打开 Successbox');
      return;
    }
    this.state.terminalMessage = '正在启动沙箱服务';
    this.changed();
    const assets = path.join(
      app.getPath('userData'),
      'runtime',
      app.getVersion() +
        '-' +
        JSON.parse(
          readFileSync(path.join(r.resources, 'manifest.json'), 'utf8'),
        ).versions.commit.slice(0, 12),
    );
    await mkdir(assets, { recursive: true, mode: 0o700 });
    if (
      process.platform === 'darwin' &&
      !existsSync(path.join(assets, 'rootfs', 'etc', 'os-release'))
    ) {
      const unpacking = await mkdtemp(path.join(assets, '.extract-'));
      try {
        await promisify(execFile)('/usr/bin/tar', [
          '-xzpf',
          path.join(r.resources, 'guest', 'rootfs.tar.gz'),
          '-C',
          unpacking,
        ]);
        await rename(
          path.join(unpacking, 'rootfs'),
          path.join(assets, 'rootfs'),
        );
      } finally {
        await rm(unpacking, { recursive: true, force: true });
      }
    }
    // A stale socket can only belong to this app's private state. Never unlink
    // a live service socket: probe it before creating a replacement.
    if (existsSync(r.socket)) {
      try {
        await promisify(execFile)(
          path.join(r.resources, 'host', 'sbx'),
          ['--socket', r.socket, 'close', 'readiness-probe'],
          { timeout: 2000 },
        );
        throw new Error('另一个沙箱服务仍在使用此配置');
      } catch (error) {
        if (
          error instanceof Error &&
          error.message === '另一个沙箱服务仍在使用此配置'
        )
          throw error;
        await rm(r.socket);
      }
    }
    const log = openSync(path.join(r.directory, 'sandbox.log'), 'a', 0o600);
    const linux = path.join(r.resources, 'linux');
    try {
      if (process.platform === 'darwin') {
        r.sandbox = spawn(
          path.join(r.resources, 'host', 'successbox-apple'),
          [
            '--exit-on-stdin-close',
            '--socket',
            r.socket,
            '--state',
            path.dirname(r.directory),
            '--kernel',
            path.join(r.resources, 'guest', 'vmlinux-arm64'),
            '--initfs',
            path.join(r.resources, 'guest', 'initfs.ext4'),
            '--linux-rootfs',
            path.join(assets, 'rootfs'),
            '--runtime-directory',
            linux,
            '--runtime-memory-mib',
            '8192',
            '--host-home',
            homedir(),
            '--network',
            'slirp4netns',
          ],
          { stdio: ['pipe', log, log] },
        );
      } else {
        r.sandbox = spawn(
          '/usr/bin/pkexec',
          [
            path.join(linux, 'successbox-gvisor'),
            '--socket',
            r.socket,
            '--socket-uid',
            String(userInfo().uid),
            '--exit-on-stdin-close',
            '--state',
            r.directory,
            '--runsc',
            path.join(linux, 'runsc'),
            '--worktrees',
            path.join(linux, 'worktrees'),
            '--network',
            'slirp4netns',
            '--slirp4netns',
            '/usr/bin/slirp4netns',
          ],
          { stdio: ['pipe', log, log] },
        );
      }
    } finally {
      closeSync(log);
    }
    let failure = false;
    r.sandbox.once('error', () => {
      failure = true;
    });
    r.sandbox.once('exit', () => {
      failure = true;
      this.state.services.sandbox = 'down';
      this.changed();
    });
    const deadline = Date.now() + 120_000;
    while (true) {
      if (failure || this.stopped)
        throw new Error('沙箱启动已停止或系统授权被取消');
      try {
        await promisify(execFile)(
          path.join(r.resources, 'host', 'sbx'),
          ['--socket', r.socket, 'close', 'readiness-probe'],
          { timeout: 2000 },
        );
        break;
      } catch {
        /* The private control socket is not ready yet. */
      }
      if (Date.now() > deadline) throw new Error('沙箱启动超时');
      await new Promise((resolve) => setTimeout(resolve, 200));
    }
    this.state.services.sandbox = 'ok';
    this.changed();
  }

  private unavailable(): never {
    throw new Error('此功能尚未接入真实后端');
  }
  async setProtection() {
    this.unavailable();
  }
  async activateProvider(agent: AgentId, id: string) {
    await this.initialization;
    return this.changeAccounts(async (config) => {
      if (
        !this.state.agents[agent]?.providers.some(
          (provider) => provider.id === id,
        )
      )
        throw new Error('账号不存在');
      config.plugins.configs.successbox.bindings[
        this.runtime!.identity.scopes[agent].llm
      ] = {
        [agent]: this.providerSelectors.get(id) ?? id,
      };
    });
  }
  async addProvider(input: NewProvider) {
    await this.initialization;
    if (!['codex', 'claude'].includes(input.agent))
      throw new Error('未知 Agent');
    return this.changeAccounts(async (config) => {
      if (input.kind === 'subscription') {
        const previous = new Map(
          (await readdir(config.oauth['auth-dir'])).map((name) => [
            name,
            statSync(path.join(config.oauth['auth-dir'], name)).mtimeMs,
          ]),
        );
        try {
          await promisify(execFile)(
            path.join(this.runtime!.resources, 'host', 'cli-proxy-api'),
            [
              '-config',
              path.join(this.runtime!.directory, 'llm.json'),
              `-${input.agent}-login`,
            ],
            { timeout: 300_000, signal: this.lifetime.signal },
          );
        } catch {
          throw new Error('登录未完成，请重试');
        }
        const files = (await readdir(config.oauth['auth-dir'])).filter(
          (file) =>
            file.endsWith('.json') &&
            previous.get(file) !==
              statSync(path.join(config.oauth['auth-dir'], file)).mtimeMs,
        );
        const candidate = files.find(
          (file) =>
            JSON.parse(
              readFileSync(path.join(config.oauth['auth-dir'], file), 'utf8'),
            ).type === input.agent,
        );
        if (!candidate) throw new Error('登录程序没有保存新账号');
        await chmod(path.join(config.oauth['auth-dir'], candidate), 0o600);
        config.plugins.configs.successbox.bindings[
          this.runtime!.identity.scopes[input.agent].llm
        ] = {
          [input.agent]: candidate,
        };
      } else {
        if (!input.apiKey?.trim() || !input.name?.trim())
          throw new Error('请填写账号名称和 API Key');
        const endpoint = new URL(
          input.endpoint ||
            (input.agent === 'codex'
              ? 'https://api.openai.com/v1'
              : 'https://api.anthropic.com'),
        );
        if (
          !['https:', 'http:'].includes(endpoint.protocol) ||
          endpoint.username ||
          endpoint.password ||
          endpoint.search ||
          endpoint.hash
        )
          throw new Error('请填写不包含账号或参数的 HTTP(S) 服务地址');
        const base = endpoint.toString().replace(/\/$/, '');
        const entries = config['api-keys'][input.agent];
        let index = entries.findIndex(
          (entry) =>
            entry['base-url'] === base &&
            entry.keys.some((key) => key['api-key'] === input.apiKey),
        );
        if (index < 0) {
          index = entries.length;
          entries.push({
            name: input.name.trim(),
            'base-url': base,
            keys: [{ 'api-key': input.apiKey }],
          });
        }
        config.plugins.configs.successbox.bindings[
          this.runtime!.identity.scopes[input.agent].llm
        ] = {
          [input.agent]: `${base}#${index}`,
        };
      }
    });
  }
  async reloginProvider(agent: AgentId, id: string) {
    const provider = this.state.agents[agent]?.providers.find(
      (candidate) => candidate.id === id,
    );
    if (!provider || provider.kind !== 'subscription')
      throw new Error('请选择订阅账号');
    await this.addProvider({
      agent,
      kind: 'subscription',
      name: provider.name,
      preset: provider.preset,
    });
  }
  async removeProvider(agent: AgentId, id: string) {
    await this.initialization;
    return this.changeAccounts(async (config) => {
      const provider = this.state.agents[agent]?.providers.find(
        (candidate) => candidate.id === id,
      );
      if (!provider || provider.active) throw new Error('请先切换到另一个账号');
      if (provider.kind === 'subscription') {
        // IDs are obtained from our own provider list, never used as arbitrary paths.
        await rm(path.join(config.oauth['auth-dir'], id));
      } else {
        const selector = this.providerSelectors.get(id)!;
        const index = Number(selector.slice(selector.lastIndexOf('#') + 1));
        const binding =
          config.plugins.configs.successbox.bindings[
            this.runtime!.identity.scopes[agent].llm
          ];
        const selected = binding?.[agent];
        const selectedIndex = selected?.includes('#')
          ? Number(selected.slice(selected.lastIndexOf('#') + 1))
          : -1;
        config['api-keys'][agent].splice(index, 1);
        if (selectedIndex > index)
          binding[agent] =
            `${config['api-keys'][agent][selectedIndex - 1]['base-url']}#${selectedIndex - 1}`;
      }
    });
  }
  async addMcp() {
    this.unavailable();
  }
  async setMcpAgent() {
    this.unavailable();
  }
  async removeMcp() {
    this.unavailable();
  }
  async deleteChange() {
    this.unavailable();
  }
  async markChangesRead() {}
  async getDiff(): Promise<DiffFile[]> {
    return this.unavailable();
  }
  async launchInTerminal(agent: AgentId) {
    await this.initialization;
    if (agent !== 'claude' && agent !== 'codex') throw new Error('未知 Agent');
    if (process.platform !== 'darwin' && process.platform !== 'linux')
      throw new Error('当前仅支持 macOS 和 Linux');
    const file =
      process.env.SUCCESSBOX_GATEWAY_CONFIG ??
      path.join(
        this.runtime?.directory ?? app.getPath('userData'),
        'gateway.json',
      );
    const configuration = JSON.parse(await readFile(file, 'utf8')) as {
      sandbox?: {
        client: string;
        socket: string;
        create: string;
        terminal: string[];
        desktop?: {
          viewer: string[];
          prepare?: string[];
          command?: string[];
        };
        commands?: Partial<Record<AgentId, string[]>>;
        environment?: Partial<Record<AgentId, string[]>>;
        gateways?: Partial<
          Record<
            AgentId,
            {
              url: string;
              model: string;
              llmToken: string;
              mcpToken: string;
            }
          >
        >;
      };
    };
    let desktopPreparation: object | undefined;
    let managedCreate: Record<string, unknown> | undefined;
    if (this.runtime) {
      if (
        !this.state.agents[agent].providers.some((provider) => provider.active)
      )
        throw new Error('请先导入或添加一个模型账号');
      await (this.sandboxStart ??= this.startSandbox());
      const r = this.runtime;
      const sandbox = configuration.sandbox!;
      const gateways = sandbox.gateways!;
      let codexSettings = existsSync(
        path.join(homedir(), '.codex', 'config.toml'),
      )
        ? parseToml(
            await readFile(
              path.join(homedir(), '.codex', 'config.toml'),
              'utf8',
            ),
          )
        : {};
      const response = await fetch(
        `http://127.0.0.1:${r.ports.llm}/v1/models`,
        {
          headers: { Authorization: `Bearer ${r.identity.scopes[agent].llm}` },
          signal: AbortSignal.timeout(5000),
        },
      );
      if (!response.ok) throw new Error('模型网关暂不可用');
      const catalog = (await response.json()) as {
        data?: Array<{ id: string }>;
      };
      const claudeSettingsPath = path.join(
        homedir(),
        '.claude',
        'settings.json',
      );
      const claudeSettings =
        agent === 'claude' && existsSync(claudeSettingsPath)
          ? JSON.parse(await readFile(claudeSettingsPath, 'utf8'))
          : {};
      const originalModel =
        agent === 'codex'
          ? typeof codexSettings.model === 'string'
            ? codexSettings.model
            : undefined
          : (claudeSettings.env?.ANTHROPIC_MODEL ?? claudeSettings.model);
      const model =
        originalModel ||
        catalog.data?.find((entry) =>
          agent === 'codex'
            ? entry.id.startsWith('gpt-')
            : entry.id.startsWith('claude-'),
        )?.id ||
        catalog.data?.[0]?.id;
      if (!model) throw new Error('当前账号没有可用模型');
      gateways[agent]!.model = model;
      const gateway = gateways.codex!;
      codexSettings = {
        ...codexSettings,
        model,
        model_provider: 'successbox',
        model_providers: {
          successbox: {
            name: 'Successbox',
            base_url: gateway.url + '/v1',
            wire_api: 'responses',
            requires_openai_auth: false,
            env_key: 'SUCCESSBOX_LLM_TOKEN',
          },
        },
        mcp_servers: {
          successbox: {
            url: gateway.url + '/mcp',
            bearer_token_env_var: 'SUCCESSBOX_MCP_TOKEN',
          },
        },
      };
      const uid = process.platform === 'darwin' ? 0 : userInfo().uid;
      const gid = process.platform === 'darwin' ? 0 : userInfo().gid;
      const mounts =
        process.platform === 'darwin'
          ? [{ source: homedir(), destination: homedir() }]
          : [
              {
                source: path.join(r.resources, 'linux'),
                destination: '/opt/successbox-runtime',
                readOnly: true,
              },
              {
                source: path.join(
                  r.resources,
                  'linux',
                  'agent',
                  'usr',
                  'lib',
                  'chatgpt',
                ),
                destination: '/usr/lib/chatgpt',
                readOnly: true,
              },
            ];
      managedCreate = {
        rootfs: '/',
        workingDir: homedir(),
        desktop: agent === 'codex',
        mounts,
        env: [
          `HOME=${homedir()}`,
          `USER=${userInfo().username}`,
          'LANG=C.UTF-8',
          'TERM=xterm-256color',
          `SUCCESSBOX_LLM_TOKEN=${gateway.llmToken}`,
          `SUCCESSBOX_MCP_TOKEN=${gateway.mcpToken}`,
        ],
      };
      desktopPreparation = {
        home: homedir(),
        uid,
        gid,
        password: randomBytes(24).toString('hex'),
        url: gateway.url,
        codexConfig: stringifyToml(codexSettings),
        codexToken: gateway.llmToken,
        claudeToken: gateways.claude!.llmToken,
        claudeMcpToken: gateways.claude!.mcpToken,
      };
    }
    const sandbox = configuration.sandbox;
    if (agent === 'codex' && !sandbox?.desktop)
      throw new Error('Codex Desktop 尚未配置桌面连接，无法打开应用');
    if (agent === 'codex' && sandbox?.desktop) {
      if (
        ![sandbox.client, sandbox.socket, sandbox.create].every(
          (value) => typeof value === 'string' && path.isAbsolute(value),
        )
      )
        throw new Error('请配置沙箱客户端、控制 socket 和桌面 Create 文件');
      const desktop = sandbox.desktop;
      for (const argv of [desktop.viewer, desktop.prepare, desktop.command]) {
        if (
          argv !== undefined &&
          (!Array.isArray(argv) ||
            !argv.length ||
            argv.some(
              (value) => typeof value !== 'string' || value.includes('\0'),
            ) ||
            !argv[0])
        )
          throw new Error('桌面启动命令必须是参数数组');
      }
      if (!desktop.viewer?.some((arg) => arg.includes('{address}')))
        throw new Error('RDP 客户端参数需要包含 {address}');
      const request =
        managedCreate ?? JSON.parse(await readFile(sandbox.create, 'utf8'));
      if (request.desktop !== true)
        throw new Error('桌面 Create 配置需要 desktop=true');
      const id = randomUUID();
      const directory = await mkdtemp(
        path.join(tmpdir(), 'successbox-display-'),
      );
      const createPath = path.join(directory, 'create.json');
      const call = promisify(execFile);
      const prefix = ['--socket', sandbox.socket];
      let sandboxId = '';
      let proxy: ReturnType<typeof spawn> | undefined;
      let viewer: ReturnType<typeof spawn> | undefined;
      let application: ReturnType<typeof spawn> | undefined;
      let closing: Promise<void> | undefined;
      const close = () =>
        (closing ??= (async () => {
          viewer?.kill('SIGTERM');
          proxy?.kill('SIGTERM');
          try {
            if (sandboxId)
              await call(sandbox.client, [...prefix, 'close', sandboxId]);
            this.state.runs = this.state.runs.filter((run) => run.id !== id);
            this.state.terminalMessage = '桌面已关闭，沙箱已清理';
            await rm(directory, { recursive: true, force: true });
          } catch {
            const run = this.state.runs.find((run) => run.id === id);
            if (run) run.status = 'cleanup-failed';
            this.state.terminalMessage = `桌面清理失败，沙箱 ${sandboxId}；请检查后端`;
          } finally {
            application?.kill('SIGTERM');
            this.changed();
            this.closeRuns.delete(close);
          }
        })());
      this.closeRuns.add(close);
      this.state.runs.push({
        id,
        agent,
        cwd: request.workingDir ?? request.working_dir ?? '/',
        protected: true,
        startedAt: new Date().toLocaleString(),
        status: 'starting',
      });
      this.state.terminalMessage = '正在准备 RDP 桌面';
      this.changed();
      try {
        await writeFile(createPath, JSON.stringify(request), { mode: 0o600 });
        sandboxId = (
          await call(sandbox.client, [...prefix, 'create', createPath])
        ).stdout.trim();
        const run = this.state.runs.find((run) => run.id === id);
        if (run) run.sandboxId = sandboxId;
        let rdpCredentials:
          | { username: string; certificate: string }
          | undefined;
        if (desktopPreparation) {
          const preparation = spawn(
            sandbox.client,
            [
              ...prefix,
              'exec',
              '--sandbox',
              sandboxId,
              '--',
              '/usr/bin/python3',
              '/opt/successbox-runtime/scripts/session.py',
              'prepare',
            ],
            { stdio: ['pipe', 'pipe', 'pipe'] },
          );
          let output = '';
          preparation.stdout.on('data', (data: Buffer) => {
            output = (output + data.toString()).slice(-65536);
          });
          const prepared = new Promise<void>((resolve, reject) => {
            preparation.once('error', reject);
            preparation.once('exit', (code) =>
              code === 0 ? resolve() : reject(new Error('桌面环境准备失败')),
            );
          });
          preparation.stderr.resume();
          preparation.stdin.end(JSON.stringify(desktopPreparation));
          await prepared;
          rdpCredentials = JSON.parse(output.trim().split('\n').at(-1)!);
        }
        if (desktop.prepare) {
          this.state.terminalMessage = '正在准备桌面应用';
          this.changed();
          await call(
            sandbox.client,
            [
              ...prefix,
              'exec',
              '--sandbox',
              sandboxId,
              '--',
              ...desktop.prepare,
            ],
            { maxBuffer: 4 * 1024 * 1024 },
          );
        }
        proxy = spawn(
          sandbox.client,
          [
            ...prefix,
            'display',
            '--sandbox',
            sandboxId,
            '--listen',
            '127.0.0.1:0',
          ],
          { stdio: ['ignore', 'ignore', 'pipe'] },
        );
        const address = await new Promise<string>((resolve, reject) => {
          let output = '';
          proxy!.once('error', reject);
          proxy!.once('exit', () => reject(new Error('RDP 通道未能启动')));
          proxy!.stderr!.on('data', (chunk: Buffer) => {
            output = (output + chunk.toString()).slice(-8192);
            const match = output.match(
              /display byte-stream proxy: (127\.0\.0\.1:\d+)/,
            );
            if (match) resolve(match[1]);
          });
        });
        if (desktop.command) {
          application = spawn(
            sandbox.client,
            [
              ...prefix,
              'exec',
              '--sandbox',
              sandboxId,
              '--',
              ...desktop.command,
            ],
            { stdio: 'ignore' },
          );
          application.once('error', () => {
            this.state.terminalMessage = '桌面应用启动失败';
            this.changed();
          });
          application.once('exit', (code) => {
            if (!closing && code !== 0) {
              this.state.terminalMessage = `桌面应用退出（${code}），RDP 桌面仍保留`;
              this.changed();
            }
          });
        }
        let viewerArguments = desktop.viewer.map((arg) =>
          arg.replaceAll('{address}', address),
        );
        if (rdpCredentials && desktopPreparation) {
          const certificate = new X509Certificate(rdpCredentials.certificate);
          const argsFile = path.join(directory, 'viewer.args');
          await writeFile(
            argsFile,
            [
              '/v:' + address,
              '/u:' + rdpCredentials.username,
              '/p:' + (desktopPreparation as { password: string }).password,
              '/sec:tls',
              '/cert:fingerprint:sha256:' +
                certificate.fingerprint256.replaceAll(':', ''),
              '/size:2560x1600',
              '+dynamic-resolution',
              '-gfx',
              '/title:Successbox Codex Desktop',
            ].join('\n') + '\n',
            { mode: 0o600 },
          );
          viewerArguments = [desktop.viewer[0], '/args-from:file:' + argsFile];
        }
        const [executable, ...args] = viewerArguments;
        viewer = spawn(executable, args, { stdio: 'ignore' });
        viewer.once('exit', () => {
          void close();
        });
        await new Promise<void>((resolve, reject) => {
          viewer!.once('spawn', resolve);
          viewer!.once('error', reject);
        });
        if (run) run.status = 'running';
        this.state.terminalMessage = 'RDP 客户端已打开，正在连接沙箱桌面';
        this.changed();
        proxy.once('exit', () => {
          if (!closing) void close();
        });
      } catch {
        await close();
        throw new Error('桌面启动失败，请检查运行环境、RDP 客户端和后端日志');
      }
      return;
    }
    if (
      !sandbox ||
      ![sandbox.client, sandbox.socket, sandbox.create].every(
        (value) => typeof value === 'string' && path.isAbsolute(value),
      ) ||
      !Array.isArray(sandbox.terminal) ||
      sandbox.terminal.length === 0 ||
      sandbox.terminal.some((value) => typeof value !== 'string' || !value)
    )
      throw new Error(
        '请在连接配置中填写 sandbox 的 client、socket、create 和 terminal',
      );
    let argv = sandbox.commands?.[agent] ?? [AGENT_COMMANDS[agent]];
    if (
      !Array.isArray(argv) ||
      argv.length === 0 ||
      argv.some((value) => typeof value !== 'string' || !value)
    )
      throw new Error('Agent 启动参数必须是非空字符串数组');
    const createRequest = (managedCreate ??
      JSON.parse(await readFile(sandbox.create, 'utf8'))) as {
      workingDir?: string;
      working_dir?: string;
      env?: string[];
    };
    if (
      createRequest.env !== undefined &&
      (!Array.isArray(createRequest.env) ||
        createRequest.env.some(
          (value) =>
            typeof value !== 'string' ||
            value.indexOf('=') <= 0 ||
            value.includes('\0'),
        ))
    )
      throw new Error('基础运行环境必须是 KEY=VALUE 字符串数组');
    const agentEnvironment = sandbox.environment?.[agent] ?? [];
    if (
      !Array.isArray(agentEnvironment) ||
      agentEnvironment.some((value) => {
        const separator = typeof value === 'string' ? value.indexOf('=') : -1;
        return (
          typeof value !== 'string' ||
          separator <= 0 ||
          !value.slice(0, separator).trim() ||
          value.includes('\0')
        );
      })
    )
      throw new Error('Agent 网关环境变量必须是 KEY=VALUE 字符串');
    if (!Array.isArray(createRequest.env)) createRequest.env = [];
    const environment = new Map<string, string>();
    for (const value of createRequest.env) {
      const separator = typeof value === 'string' ? value.indexOf('=') : -1;
      if (separator > 0) environment.set(value.slice(0, separator), value);
    }
    for (const value of agentEnvironment) {
      environment.set(value.slice(0, value.indexOf('=')), value);
    }
    const id = randomUUID();
    const gateway = sandbox.gateways?.[agent];
    let agentConfiguration:
      | { directory: string; name: string; content: string }
      | undefined;
    if (gateway) {
      if (
        ![gateway.url, gateway.model, gateway.llmToken, gateway.mcpToken].every(
          (value) =>
            typeof value === 'string' &&
            value.length > 0 &&
            !value.includes('\0'),
        )
      )
        throw new Error('网关配置需要 url、model、llmToken 和 mcpToken');
      const endpoint = new URL(gateway.url);
      if (
        !['http:', 'https:'].includes(endpoint.protocol) ||
        endpoint.username ||
        endpoint.password ||
        endpoint.search ||
        endpoint.hash ||
        endpoint.pathname !== '/'
      )
        throw new Error('网关 url 必须是无路径、无账号信息的 HTTP(S) 地址');
      const directory = `/tmp/successbox-agent-${id}`;
      // Only gateway client keys enter the guest. Host control credentials are
      // never copied. Each launch gets its own config, including on macOS where
      // host paths cannot be passed directly to the Linux VM.
      for (const key of [
        'OPENAI_API_KEY',
        'OPENAI_BASE_URL',
        'ANTHROPIC_API_KEY',
        'ANTHROPIC_AUTH_TOKEN',
        'ANTHROPIC_BASE_URL',
      ])
        environment.delete(key);
      environment.set(
        'SUCCESSBOX_LLM_TOKEN',
        `SUCCESSBOX_LLM_TOKEN=${gateway.llmToken}`,
      );
      environment.set(
        'SUCCESSBOX_MCP_TOKEN',
        `SUCCESSBOX_MCP_TOKEN=${gateway.mcpToken}`,
      );
      const string = (value: string) => JSON.stringify(value);
      if (agent === 'codex') {
        environment.set('CODEX_HOME', `CODEX_HOME=${directory}`);
        agentConfiguration = {
          directory,
          name: 'config.toml',
          content: [
            `model = ${string(gateway.model)}`,
            'model_provider = "successbox"',
            '[model_providers.successbox]',
            'name = "Successbox"',
            `base_url = ${string(endpoint.origin + '/v1')}`,
            'env_key = "SUCCESSBOX_LLM_TOKEN"',
            'wire_api = "responses"',
            'requires_openai_auth = false',
            '[mcp_servers.successbox]',
            `url = ${string(endpoint.origin + '/mcp')}`,
            'bearer_token_env_var = "SUCCESSBOX_MCP_TOKEN"',
            '',
          ].join('\n'),
        };
      } else {
        environment.set('CLAUDE_CONFIG_DIR', `CLAUDE_CONFIG_DIR=${directory}`);
        environment.set(
          'ANTHROPIC_BASE_URL',
          `ANTHROPIC_BASE_URL=${endpoint.origin}`,
        );
        environment.set(
          'ANTHROPIC_API_KEY',
          `ANTHROPIC_API_KEY=${gateway.llmToken}`,
        );
        agentConfiguration = {
          directory,
          name: 'mcp.json',
          content: JSON.stringify({
            mcpServers: {
              successbox: {
                type: 'http',
                url: endpoint.origin + '/mcp',
                headers: { Authorization: `Bearer ${gateway.mcpToken}` },
              },
            },
          }),
        };
        argv = [
          ...argv,
          '--strict-mcp-config',
          '--mcp-config',
          `${directory}/mcp.json`,
          '--model',
          gateway.model,
        ];
      }
    }
    if (this.runtime && agent === 'claude') {
      environment.set(
        'CLAUDE_CONFIG_DIR',
        `CLAUDE_CONFIG_DIR=${path.join(homedir(), '.claude')}`,
      );
      if (process.platform === 'linux')
        argv = [
          '/usr/bin/setpriv',
          '--reuid',
          String(userInfo().uid),
          '--regid',
          String(userInfo().gid),
          '--clear-groups',
          '--',
          ...argv,
        ];
    }
    createRequest.env = [...environment.values()];
    const requestWithAgentEnvironment = JSON.stringify(createRequest, null, 2);
    const directory = await mkdtemp(
      path.join(tmpdir(), 'successbox-terminal-'),
    );
    const script = path.join(directory, 'launch.command');
    const result = path.join(directory, 'result');
    const identity = path.join(directory, 'sandbox-id');
    const cleanupLog = path.join(directory, 'close.log');
    const quote = (value: string) => "'" + value.replaceAll("'", "'\\''") + "'";
    const command = [sandbox.client, '--socket', sandbox.socket]
      .map(quote)
      .join(' ');
    let observed = false;
    let timer: ReturnType<typeof setInterval> | undefined;
    try {
      await writeFile(
        path.join(directory, 'create.json'),
        requestWithAgentEnvironment,
        {
          mode: 0o600,
        },
      );
      // The terminal owns Create/Exec/Close independently of the desktop UI.
      // A shell defers traps while waiting for a foreground command. Use its
      // interruptible wait builtin so terminal HUP can Close a running guest.
      const cleanup = [
        'code=$?',
        'trap - EXIT HUP INT TERM',
        'closed=0',
        `if [ -n "$sandbox_id" ]; then ${command} close "$sandbox_id" > ${quote(cleanupLog)} 2>&1; closed=$?; fi`,
        'if [ -n "$worker" ] && [ "$closed" -eq 0 ]; then wait "$worker"; fi',
        `printf '%s %s\\n' "$code" "$closed" > ${quote(result + '.tmp')}`,
        `mv ${quote(result + '.tmp')} ${quote(result)}`,
        'exit "$code"',
      ].join('; ');
      await writeFile(
        script,
        [
          '#!/bin/sh',
          'umask 077',
          "sandbox_id=''",
          "worker=''",
          `trap ${quote(cleanup)} EXIT`,
          "trap 'exit 129' HUP",
          "trap 'exit 130' INT",
          "trap 'exit 143' TERM",
          `sandbox_id=$(${command} create ${quote(path.join(directory, 'create.json'))}) || exit $?`,
          `printf '%s\\n' "$sandbox_id" > ${quote(identity)}`,
          ...(desktopPreparation
            ? [
                `printf '%s' ${quote(Buffer.from(JSON.stringify(desktopPreparation)).toString('base64'))} | ${command} exec --sandbox "$sandbox_id" -- /bin/sh -ec ${quote('base64 -d | /usr/bin/python3 /opt/successbox-runtime/scripts/session.py configure')} || exit $?`,
              ]
            : []),
          ...(agentConfiguration
            ? [
                `printf '%s' ${quote(Buffer.from(agentConfiguration.content, 'utf8').toString('base64'))} | ${command} exec --sandbox "$sandbox_id" -- /bin/sh -ec ${quote('umask 077; mkdir -m 700 "$1"; base64 -d > "$1/$2"' + (this.runtime && process.platform === 'linux' ? `; chown ${userInfo().uid}:${userInfo().gid} "$1" "$1/$2"` : ''))} successbox-config ${quote(agentConfiguration.directory)} ${quote(agentConfiguration.name)} || exit $?`,
              ]
            : []),
          `printf '%s\\n' ${quote('Successbox · ' + AGENT_NAMES[agent] + ' · 沙箱终端')}`,
          `${command} exec --sandbox "$sandbox_id" --tty -- ${argv.map(quote).join(' ')} < /dev/tty &`,
          'worker=$!',
          'wait "$worker"',
          '',
        ].join('\n'),
        { mode: 0o700 },
      );
      this.state.runs.push({
        id,
        agent,
        cwd: createRequest.workingDir ?? createRequest.working_dir ?? '/',
        protected: true,
        startedAt: new Date().toLocaleString(),
        status: 'starting',
      });
      this.state.terminalMessage = '正在打开沙箱终端';
      this.changed();
      // Poll only this invocation's private files. The shell atomically
      // publishes result after Close, independently of the launcher's lifetime.
      timer = setInterval(() => {
        const run = this.state.runs.find((item) => item.id === id);
        if (!observed && existsSync(identity)) {
          observed = true;
          if (run) {
            run.sandboxId = readFileSync(identity, 'utf8').trim();
            run.status = 'running';
          }
          this.state.terminalMessage = gateway
            ? '终端已连接沙箱，已配置模型和 MCP 网关'
            : agentEnvironment.length
              ? '终端已连接沙箱，已加载本次 Agent 的网关环境'
              : '终端已连接沙箱；未配置 Agent 网关环境变量';
          this.changed();
        }
        if (!existsSync(result)) return;
        clearInterval(timer);
        const [exitCode, closeCode] = readFileSync(result, 'utf8')
          .trim()
          .split(/\s+/)
          .map(Number);
        if (closeCode !== 0) {
          if (run) run.status = 'cleanup-failed';
          this.state.terminalMessage = `程序已退出，但沙箱关闭失败。沙箱 ${run?.sandboxId ?? id}；详情：${cleanupLog}`;
        } else {
          this.state.runs = this.state.runs.filter((item) => item.id !== id);
          this.state.terminalMessage = `${AGENT_NAMES[agent]} 终端已结束（退出码 ${exitCode}），沙箱已清理`;
          void rm(directory, { recursive: true, force: true }).catch(() => {});
        }
        this.changed();
      }, 250);
      timer.unref();
      const [terminal, ...args] = sandbox.terminal;
      await promisify(execFile)(terminal, [...args, script]);
    } catch (error) {
      // A terminal may return nonzero after running the script. Its result
      // remains authoritative; do not remove files while its cleanup is active.
      if (observed || existsSync(identity) || existsSync(result)) throw error;
      clearInterval(timer);
      this.state.runs = this.state.runs.filter((item) => item.id !== id);
      this.state.terminalMessage = '终端未能启动';
      this.changed();
      await rm(directory, { recursive: true, force: true });
      throw error;
    }
  }
  async importConfigs(
    agents: AgentId[],
    options: { commandLine: boolean; protection: boolean },
  ) {
    await this.initialization;
    if (!this.runtime) throw new Error('配置导入需要内置运行时');
    // Validate and prepare all selected inputs before changing either gateway.
    const inputs: Array<{
      agent: AgentId;
      key?: string;
      endpoint?: string;
      name: string;
      auth?: Record<string, unknown>;
    }> = [];
    for (const agent of new Set(agents)) {
      if (agent === 'codex') {
        const source = path.join(homedir(), '.codex');
        const settings = existsSync(path.join(source, 'config.toml'))
          ? parseToml(await readFile(path.join(source, 'config.toml'), 'utf8'))
          : {};
        const auth = existsSync(path.join(source, 'auth.json'))
          ? JSON.parse(await readFile(path.join(source, 'auth.json'), 'utf8'))
          : {};
        const providerName =
          typeof settings.model_provider === 'string'
            ? settings.model_provider
            : 'openai';
        const providers = settings.model_providers as
          | Record<string, Record<string, unknown>>
          | undefined;
        const provider = providers?.[providerName] ?? {};
        const key =
          (typeof provider.env_key === 'string'
            ? process.env[provider.env_key]
            : undefined) || auth.OPENAI_API_KEY;
        if (typeof key === 'string' && key)
          inputs.push({
            agent,
            key,
            endpoint:
              typeof provider.base_url === 'string'
                ? provider.base_url
                : undefined,
            name:
              typeof provider.name === 'string' ? provider.name : 'Codex API',
          });
        else if (auth.tokens?.access_token && auth.tokens?.account_id)
          inputs.push({
            agent,
            name: 'ChatGPT 订阅',
            auth: {
              type: 'codex',
              access_token: auth.tokens.access_token,
              account_id: auth.tokens.account_id,
              id_token: auth.tokens.id_token,
              // The host agent owns its refresh token. Importing it into a second
              // refresher could rotate the host login; a fresh login has its own store.
            },
          });
        else
          throw new Error(
            'Codex 配置中没有可用账号，请添加 API Key 或登录订阅',
          );
      } else if (agent === 'claude') {
        const source = path.join(homedir(), '.claude');
        const settings = existsSync(path.join(source, 'settings.json'))
          ? JSON.parse(
              await readFile(path.join(source, 'settings.json'), 'utf8'),
            )
          : {};
        const env = settings.env ?? {};
        const key =
          env.ANTHROPIC_API_KEY ||
          env.ANTHROPIC_AUTH_TOKEN ||
          process.env.ANTHROPIC_API_KEY;
        if (typeof key === 'string' && key)
          inputs.push({
            agent,
            key,
            endpoint: env.ANTHROPIC_BASE_URL,
            name: 'Claude API',
          });
        else {
          const credentials = existsSync(path.join(source, '.credentials.json'))
            ? JSON.parse(
                await readFile(path.join(source, '.credentials.json'), 'utf8'),
              ).claudeAiOauth
            : undefined;
          if (!credentials?.accessToken)
            throw new Error(
              'Claude 配置中没有可导入账号，请添加 API Key 或登录订阅',
            );
          inputs.push({
            agent,
            name: 'Claude 订阅',
            auth: {
              type: 'claude',
              access_token: credentials.accessToken,
              expired: new Date(credentials.expiresAt).toISOString(),
            },
          });
        }
      } else throw new Error('未知 Agent');
    }
    for (const input of inputs) {
      if (input.auth) {
        await this.changeAccounts(async (config) => {
          const filename = `${input.agent}.json`;
          await writeFile(
            path.join(config.oauth['auth-dir'], filename),
            JSON.stringify(input.auth),
            {
              mode: 0o600,
            },
          );
          config.plugins.configs.successbox.bindings[
            this.runtime!.identity.scopes[input.agent].llm
          ] = { [input.agent]: filename };
        });
      } else
        await this.addProvider({
          agent: input.agent,
          kind: 'apiKey',
          name: input.name,
          preset: `${input.agent}-custom`,
          apiKey: input.key,
          endpoint: input.endpoint,
        });
    }
    this.loadProviders();
    this.state.firstRun = false;
    // This desktop release launches from the UI. Importing accounts does not
    // install host shell wrappers or turn off sandbox isolation.
    this.state.settings.commandLine = false;
    this.changed();
    void options;
  }
  async dismissWelcome() {
    this.state.firstRun = false;
    this.changed();
  }
  async updateSettings() {
    this.unavailable();
  }
  async simulate() {
    this.unavailable();
  }
}

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
  const backend: Backend =
    process.env.SUCCESSBOX_DEMO === '1'
      ? new MockBackend(!store.get('welcomed'))
      : new GatewayBackend();

  // The first one in the queue is the request the trusted window shows.
  const declineCurrent = () => {
    const [current] = backend.snapshot().requests;
    if (!current) return;
    const answer =
      current.kind === 'secret'
        ? backend.answerSecret(current.id, null, false)
        : backend.answerInstall(current.id, null);
    void answer.catch((error: Error) =>
      dialog.showErrorBox('Successbox', error.message),
    );
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
