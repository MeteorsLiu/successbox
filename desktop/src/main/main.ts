import { app, dialog, Notification } from 'electron';
import path from 'node:path';
import { readFileSync } from 'node:fs';
import { randomUUID } from 'node:crypto';
import * as grpc from '@grpc/grpc-js';
import { loadSync } from '@grpc/proto-loader';
import type { AppState, DiffFile, InstallDecision } from '../shared/types';
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

  constructor() {
    app.once('before-quit', () => {
      this.stopped = true;
      clearTimeout(this.retry);
      this.stream?.cancel();
      this.client?.close();
    });
    let configuration: { address: string; token: string; sandboxIds: string[] };
    try {
      const file =
        process.env.SUCCESSBOX_GATEWAY_CONFIG ??
        path.join(app.getPath('userData'), 'gateway.json');
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
  private unavailable(): never {
    throw new Error('此功能尚未接入真实后端');
  }
  async setProtection() {
    this.unavailable();
  }
  async activateProvider() {
    this.unavailable();
  }
  async addProvider() {
    this.unavailable();
  }
  async reloginProvider() {
    this.unavailable();
  }
  async removeProvider() {
    this.unavailable();
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
  async launchInTerminal() {
    this.unavailable();
  }
  async importConfigs() {
    this.unavailable();
  }
  async dismissWelcome() {}
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
