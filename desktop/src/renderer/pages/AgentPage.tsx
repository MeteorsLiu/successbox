import { useState } from 'react';
import { AGENT_COMMANDS, AGENT_NAMES } from '../../shared/agents';
import type { AgentId, AppState, Provider } from '../../shared/types';
import { api } from '../api';
import { ConfirmDialog, Dialog, Switch } from '../components/common';
import { useToast } from '../components/Toast';
import { PROVIDER_PRESETS, presetFor } from '../presets';

export function AgentPage({
  state,
  agent,
  onOpenSettings,
}: {
  state: AppState;
  agent: AgentId;
  onOpenSettings(): void;
}) {
  const toast = useToast();
  const { providers } = state.agents[agent];
  const [adding, setAdding] = useState(false);
  const [removing, setRemoving] = useState<Provider | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const name = AGENT_NAMES[agent];

  const relogin = async (p: Provider) => {
    setBusy(p.id);
    await toast.run(api.reloginProvider(agent, p.id), `${p.name} 已重新登录`);
    setBusy(null);
  };

  if (!state.simulated) {
    const runs = state.runs.filter((run) => run.agent === agent);
    return (
      <>
        <div className="h2">{agent === 'codex' ? 'Codex Desktop' : name}</div>
        <div className="note">
          {agent === 'codex'
            ? '在受保护的窗口中使用 Codex Desktop。关闭应用窗口后清理本次运行环境。'
            : '在终端中使用 Claude Code，程序退出后清理本次运行环境。'}
        </div>
        <div className="card row">
          <div style={{ flex: 1 }}>
            使用宿主配置指定的运行环境，不会退回宿主直接执行。
          </div>
          <button
            className="btn primary"
            disabled={busy === 'launch'}
            onClick={async () => {
              setBusy('launch');
              await toast.run(api.launchInTerminal(agent));
              setBusy(null);
            }}
          >
            {busy === 'launch'
              ? '正在打开…'
              : agent === 'codex'
                ? '打开 Codex Desktop'
                : '在终端中打开'}
          </button>
        </div>
        <div className="h2">
          模型账号
          <span className="spacer" />
          <button
            className="btn sm"
            onClick={() =>
              toast.run(
                api.importConfigs([agent], {
                  commandLine: false,
                  protection: true,
                }),
                '账号已导入',
              )
            }
          >
            导入本机账号
          </button>
          <button className="btn sm" onClick={() => setAdding(true)}>
            ＋ 添加
          </button>
        </div>
        {providers.map((provider) => (
          <div className="card row" key={provider.id}>
            <div style={{ flex: 1 }}>
              <b>{provider.name}</b>
              <div className="muted small">{provider.detail}</div>
            </div>
            {provider.active ? (
              <span className="pill blue">使用中</span>
            ) : (
              <button
                className="btn sm"
                onClick={() =>
                  toast.run(
                    api.activateProvider(agent, provider.id),
                    '账号已切换',
                  )
                }
              >
                启用
              </button>
            )}
            {provider.kind === 'subscription' && (
              <button className="btn sm" onClick={() => relogin(provider)}>
                重新登录
              </button>
            )}
            <button
              className="btn sm ghost"
              disabled={provider.active}
              onClick={() => setRemoving(provider)}
            >
              删除
            </button>
          </div>
        ))}
        {adding && (
          <AddProviderDialog agent={agent} onClose={() => setAdding(false)} />
        )}
        {removing && (
          <ConfirmDialog
            title={`删除「${removing.name}」？`}
            confirm="删除"
            onClose={() => setRemoving(null)}
            onConfirm={() =>
              toast.run(api.removeProvider(agent, removing.id), '已删除')
            }
          >
            <p>只移除 Successbox 保存的账号。</p>
          </ConfirmDialog>
        )}
        {state.terminalMessage && <p role="status">{state.terminalMessage}</p>}
        {runs.map((run) => (
          <div className="card" key={run.id}>
            <div className="row">
              <b>{run.cwd}</b>
              <span className="tag">
                {run.status === 'starting'
                  ? '正在启动'
                  : run.status === 'cleanup-failed'
                    ? '清理失败'
                    : '运行中'}
              </span>
            </div>
            <div className="muted small">
              {run.startedAt}
              {run.sandboxId && ` · ${run.sandboxId}`}
            </div>
          </div>
        ))}
      </>
    );
  }

  return (
    <>
      <ProtectionCard state={state} agent={agent} />

      <div className="h2">
        账号与供应商{' '}
        <span className="sub">切换后立即生效，运行中的 {name} 不用重启</span>
        <span className="spacer" />
        <button className="btn sm" onClick={() => setAdding(true)}>
          ＋ 添加
        </button>
      </div>
      {providers.map((p) => {
        const preset = presetFor(agent, p);
        return (
          <div key={p.id} className={`prov ${p.active ? 'active' : ''}`}>
            <div
              className="pi"
              style={{ background: preset?.color ?? '#7b8494' }}
            >
              {preset?.initial ?? p.name.slice(0, 1)}
            </div>
            <div style={{ flex: 1, minWidth: 0 }}>
              <div className="pn">
                {p.name}
                <span className="tag">
                  {p.kind === 'subscription' ? '订阅' : 'API Key'}
                </span>
                {p.active && <span className="pill blue">使用中</span>}
                {p.problem && <span className="pill err">{p.problem}</span>}
              </div>
              <div className="pd">{p.detail}</div>
            </div>
            <div className="row" style={{ gap: 4 }}>
              {!p.active && (
                <button
                  className="btn sm"
                  disabled={!!p.problem}
                  onClick={() =>
                    toast.run(
                      api.activateProvider(agent, p.id),
                      `${name} 已切换到 ${p.name}，立即生效`,
                    )
                  }
                >
                  启用
                </button>
              )}
              {p.kind === 'subscription' && (
                <button
                  className="btn sm ghost"
                  disabled={busy === p.id}
                  onClick={() => relogin(p)}
                >
                  {busy === p.id ? (
                    <>
                      <i className="spin" /> 等待浏览器登录…
                    </>
                  ) : (
                    '重新登录'
                  )}
                </button>
              )}
              <button
                className="btn sm ghost danger"
                disabled={p.active}
                title={p.active ? '使用中的账号不能删除' : undefined}
                onClick={() => setRemoving(p)}
              >
                删除
              </button>
            </div>
          </div>
        );
      })}
      <div className="note" style={{ marginTop: 10 }}>
        🔒 API Key 和登录凭据由 Successbox 保管，不会写进 {name}{' '}
        的配置文件，Agent 也读不到。
      </div>

      <div className="h2">启动</div>
      <div className="card row">
        <div style={{ flex: 1 }}>
          在终端里照常运行 <code>{AGENT_COMMANDS[agent]}</code>{' '}
          就行，不需要额外的命令。
          <div className="muted small">
            {state.settings.commandLine
              ? '命令行集成已开启'
              : `命令行集成已关闭，终端里的 ${AGENT_COMMANDS[agent]} 不会经过 Successbox`}
            {' · '}
            <a onClick={onOpenSettings}>设置</a>
          </div>
        </div>
        <button
          className="btn"
          onClick={() => toast.run(api.launchInTerminal(agent))}
        >
          在终端中打开
        </button>
      </div>

      {adding && (
        <AddProviderDialog agent={agent} onClose={() => setAdding(false)} />
      )}
      {removing && (
        <ConfirmDialog
          title={`删除「${removing.name}」？`}
          confirm="删除"
          onClose={() => setRemoving(null)}
          onConfirm={() =>
            toast.run(api.removeProvider(agent, removing.id), '已删除')
          }
        >
          <p>只会从 Successbox 中移除，不会注销提供方的账号。</p>
        </ConfirmDialog>
      )}
    </>
  );
}

function ProtectionCard({ state, agent }: { state: AppState; agent: AgentId }) {
  const toast = useToast();
  const on = state.agents[agent].protection;
  const name = AGENT_NAMES[agent];
  const runs = state.runs.filter((r) => r.agent === agent);

  const toggle = (next: boolean) => {
    const unaffected = runs.filter((r) => r.protected !== next).length;
    const message = next
      ? `${name} 沙箱保护已开启，对新启动的 ${AGENT_COMMANDS[agent]} 生效${unaffected ? `（已在运行的 ${unaffected} 个不受影响）` : ''}`
      : `${name} 沙箱保护已关闭，新启动的 ${AGENT_COMMANDS[agent]} 将直接在本机运行`;
    void toast.run(api.setProtection(agent, next), message);
  };

  return (
    <div className={`guard ${on ? 'on' : ''}`}>
      <div className="gi">🛡</div>
      <div style={{ flex: 1, minWidth: 0 }}>
        <div className="gt">
          沙箱保护{' '}
          {on ? (
            <span className="pill shield">已开启</span>
          ) : (
            <span className="pill mute">已关闭</span>
          )}
        </div>
        <div className="muted" style={{ marginTop: 3 }}>
          {on
            ? `新启动的 ${name} 会在隔离环境中运行，用法和平时完全一样。`
            : `${name} 直接在本机运行，可以修改本机的任何文件和配置。`}
        </div>
        {on && (
          <ul>
            <li>Git 仓库里的改动，自动保存为新的 worktree，原目录不变</li>
            <li>其他目录的改动，保存为副本文件夹</li>
            <li>系统目录和环境配置不会被修改</li>
          </ul>
        )}
        {on && state.services.sandbox !== 'ok' && (
          <div className="note warn" style={{ marginTop: 10 }}>
            ⚠ 沙箱服务没有运行。为了安全，受保护的 {name}{' '}
            暂时无法启动。可以稍后重试，或临时关闭保护。
          </div>
        )}
        {on && (
          <div className="note" style={{ marginTop: 10 }}>
            ⓘ 已有的会话可以在沙箱中继续；沙箱中产生的<b>新会话</b>
            暂时不会同步回本机 <span className="tag todo">开发中</span>
          </div>
        )}
        {runs.length > 0 && (
          <div className="runs">
            <div className="muted small">正在运行的 {name}</div>
            {runs.map((r) => (
              <div key={r.id} className="row small">
                {r.protected ? (
                  <span className="pill shield">受保护</span>
                ) : (
                  <span className="pill warn">未受保护</span>
                )}
                <code>{r.cwd}</code>
                <span className="faint">{r.startedAt} 启动</span>
                <span className="spacer" />
                {on && !r.protected && (
                  <>
                    <span className="muted">重新打开后受保护</span>
                    <button
                      className="btn sm"
                      disabled
                      title="后续通过 CRIU 迁移正在运行的进程"
                    >
                      迁移到沙箱
                    </button>
                    <span className="tag todo">后续</span>
                  </>
                )}
                {!on && r.protected && (
                  <span className="muted">保持受保护，直到退出</span>
                )}
              </div>
            ))}
          </div>
        )}
      </div>
      <Switch checked={on} onChange={toggle} title="沙箱保护" />
    </div>
  );
}

function AddProviderDialog({
  agent,
  onClose,
}: {
  agent: AgentId;
  onClose(): void;
}) {
  const toast = useToast();
  const presets = PROVIDER_PRESETS[agent];
  const [index, setIndex] = useState(0);
  const preset = presets[index];
  const [name, setName] = useState(preset.name);
  const [endpoint, setEndpoint] = useState(preset.endpoint ?? '');
  const [apiKey, setApiKey] = useState('');
  const [busy, setBusy] = useState(false);

  const pick = (i: number) => {
    setIndex(i);
    setName(presets[i].name);
    setEndpoint(presets[i].endpoint ?? '');
  };

  const needsKey = preset.kind === 'apiKey';
  const valid =
    name.trim() &&
    (!needsKey ||
      (apiKey.trim() && (preset.endpoint === undefined || endpoint.trim())));

  const submit = async () => {
    setBusy(true);
    const ok = await toast.run(
      api.addProvider({
        agent,
        preset: preset.id,
        name: name.trim(),
        kind: preset.kind,
        endpoint: preset.endpoint === undefined ? undefined : endpoint.trim(),
        apiKey: needsKey ? apiKey : undefined,
      }),
      '已添加，点击“启用”即可切换',
    );
    setApiKey('');
    setBusy(false);
    if (ok) onClose();
  };

  return (
    <Dialog
      title={`添加 ${AGENT_NAMES[agent]} 账号`}
      size="wide"
      onClose={busy ? () => undefined : onClose}
      footer={
        <>
          {busy && preset.kind === 'subscription' && (
            <span className="muted small">
              <i className="spin" /> 已在浏览器中打开登录页，等待你完成登录…
            </span>
          )}
          <span className="spacer" />
          <button className="btn" onClick={onClose} disabled={busy}>
            取消
          </button>
          <button
            className="btn primary"
            onClick={submit}
            disabled={busy || !valid}
          >
            添加
          </button>
        </>
      }
    >
      <div className="presets">
        {presets.map((p, i) => (
          <button
            key={p.id}
            className={`preset ${i === index ? 'on' : ''}`}
            onClick={() => pick(i)}
            disabled={busy}
          >
            <b>{p.name}</b>
            <span>{p.description}</span>
          </button>
        ))}
      </div>
      <div className="field">
        <label>名称</label>
        <input
          className="input"
          value={name}
          onChange={(e) => setName(e.target.value)}
        />
      </div>
      {preset.kind === 'subscription' ? (
        <div className="note">
          点“添加”后会在浏览器中打开登录页。登录完成后，凭据由 Successbox 保管。
        </div>
      ) : (
        <>
          {preset.endpoint !== undefined && (
            <div className="field">
              <label>接口地址</label>
              <input
                className="input"
                value={endpoint}
                placeholder="https://"
                onChange={(e) => setEndpoint(e.target.value)}
              />
            </div>
          )}
          <div className="field">
            <label>API Key</label>
            <input
              className="input"
              type="password"
              autoComplete="off"
              value={apiKey}
              placeholder="粘贴 API Key"
              onChange={(e) => setApiKey(e.target.value)}
            />
            <div className="hint">
              由 Successbox 保管，不会写进 {AGENT_NAMES[agent]} 的配置文件。
            </div>
          </div>
        </>
      )}
    </Dialog>
  );
}
