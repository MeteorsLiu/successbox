import { useState } from 'react';
import { AGENT_NAMES } from '../../shared/agents';
import {
  AGENT_IDS,
  type AgentId,
  type AppState,
  type McpAuth,
  type McpServer,
} from '../../shared/types';
import { api } from '../api';
import { ConfirmDialog, Dialog, Switch } from '../components/common';
import { useToast } from '../components/Toast';
import { AUTH_LABELS, MCP_PRESETS } from '../presets';

const AUTH_SHORT: Record<McpAuth, string> = {
  oauth: 'OAuth',
  apiKey: 'API Key',
  askOnUse: '使用时询问',
  none: '无需认证',
};

export function McpPage({ state }: { state: AppState }) {
  const toast = useToast();
  const [adding, setAdding] = useState(false);
  const [removing, setRemoving] = useState<McpServer | null>(null);
  const nameOf = (id: string) =>
    state.mcps.find((m) => m.id === id)?.name ?? id;

  const setAgent = (m: McpServer, agent: AgentId, on: boolean) =>
    toast.run(
      api.setMcpAgent(m.id, agent, on),
      `${m.name} ${on ? '已对' : '已不再对'} ${AGENT_NAMES[agent]} 开放`,
    );

  return (
    <>
      <div className="h2">
        MCP 服务
        <span className="spacer" />
        <button className="btn sm primary" onClick={() => setAdding(true)}>
          ＋ 添加 MCP
        </button>
      </div>
      <div className="note" style={{ marginBottom: 12 }}>
        在这里添加一次，再选择给哪些 Agent 使用。Agent 只连接
        Successbox，拿不到服务的密钥。
      </div>
      <div className="stack">
        {state.mcps.map((m) => (
          <div key={m.id} className="card mcp">
            <div className="mi">{m.name.slice(0, 1).toUpperCase()}</div>
            <div style={{ flex: 1, minWidth: 0 }}>
              <div className="row" style={{ fontWeight: 650 }}>
                {m.name}
                <span className="tag">
                  {m.transport === 'stdio' ? '本机进程' : '远程'}
                </span>
                {m.problem && <span className="pill err">{m.problem}</span>}
              </div>
              <div className="mono" style={{ marginTop: 2 }}>
                {m.target}
              </div>
              <div className="muted small" style={{ marginTop: 2 }}>
                认证：{AUTH_SHORT[m.auth]}
              </div>
            </div>
            <div className="apps">
              {AGENT_IDS.map((agent) => (
                <label key={agent} className="app">
                  {AGENT_NAMES[agent]}
                  <Switch
                    small
                    blue
                    checked={m.agents[agent]}
                    onChange={(on) => setAgent(m, agent, on)}
                    title={`给 ${AGENT_NAMES[agent]} 使用`}
                  />
                </label>
              ))}
              <button
                className="btn sm ghost danger"
                onClick={() => setRemoving(m)}
              >
                删除
              </button>
            </div>
          </div>
        ))}
        {state.mcps.length === 0 && (
          <div className="card muted">还没有 MCP 服务</div>
        )}
      </div>

      <div className="h2">已保存的敏感信息</div>
      <div className="card">
        {state.secrets.length === 0 && (
          <span className="muted">
            还没有。Agent 第一次需要某个密钥时，Successbox 会弹窗询问。
          </span>
        )}
        {state.secrets.map((s) => (
          <div
            key={`${s.mcpId}/${s.field}`}
            className="row"
            style={{ padding: '5px 0' }}
          >
            🔑 <b>{nameOf(s.mcpId)}</b>
            <code>{s.field}</code>
            <span className="muted">保存于 {s.savedAt}</span>
            <span className="spacer" />
            <button
              className="btn sm ghost danger"
              onClick={() =>
                toast.run(
                  api.deleteSecret(s.mcpId, s.field),
                  '已删除，下次使用时会重新询问',
                )
              }
            >
              删除
            </button>
          </div>
        ))}
      </div>

      {adding && <AddMcpDialog onClose={() => setAdding(false)} />}
      {removing && (
        <ConfirmDialog
          title={`删除「${removing.name}」？`}
          confirm="删除"
          onClose={() => setRemoving(null)}
          onConfirm={() => toast.run(api.removeMcp(removing.id), '已删除')}
        >
          <p>
            所有 Agent
            都将无法使用它的工具，保存的敏感信息也会一起删除。已经在远端执行完的操作不会被撤销。
          </p>
        </ConfirmDialog>
      )}
    </>
  );
}

function AddMcpDialog({ onClose }: { onClose(): void }) {
  const toast = useToast();
  const [index, setIndex] = useState(0);
  const [name, setName] = useState(MCP_PRESETS[0].name.toLowerCase());
  const [transport, setTransport] = useState<McpServer['transport']>('http');
  const [target, setTarget] = useState(MCP_PRESETS[0].target);
  const [auth, setAuth] = useState<McpAuth>(MCP_PRESETS[0].auth);
  const [agents, setAgents] = useState<Record<AgentId, boolean>>({
    claude: true,
    codex: true,
  });
  const [busy, setBusy] = useState(false);

  const pick = (i: number) => {
    const p = MCP_PRESETS[i];
    setIndex(i);
    setName(p.name === '自定义' ? '' : p.name.toLowerCase());
    setTarget(p.target);
    setAuth(p.auth);
    setTransport('http');
  };

  const submit = async () => {
    setBusy(true);
    const ok = await toast.run(
      api.addMcp({
        name: name.trim(),
        transport,
        target: target.trim(),
        auth,
        agents,
      }),
      `已添加 ${name.trim()}`,
    );
    setBusy(false);
    if (ok) onClose();
  };

  return (
    <Dialog
      title="添加 MCP 服务"
      size="wide"
      onClose={busy ? () => undefined : onClose}
      footer={
        <>
          {busy && (
            <span className="muted small">
              <i className="spin" />{' '}
              {auth === 'oauth'
                ? '已在浏览器中打开授权页，等待你完成授权…'
                : '正在连接并发现工具…'}
            </span>
          )}
          <span className="spacer" />
          <button className="btn" onClick={onClose} disabled={busy}>
            取消
          </button>
          <button
            className="btn primary"
            onClick={submit}
            disabled={busy || !name.trim() || !target.trim()}
          >
            添加
          </button>
        </>
      }
    >
      <div className="presets four">
        {MCP_PRESETS.map((p, i) => (
          <button
            key={p.name}
            className={`preset ${i === index ? 'on' : ''}`}
            onClick={() => pick(i)}
            disabled={busy}
          >
            <b>{p.name}</b>
          </button>
        ))}
      </div>
      <div className="row" style={{ gap: 14 }}>
        <div className="field" style={{ flex: 1 }}>
          <label>名称</label>
          <input
            className="input"
            value={name}
            placeholder="例如：my-tools"
            onChange={(e) => setName(e.target.value)}
          />
        </div>
        <div className="field" style={{ flex: 1 }}>
          <label>类型</label>
          <select
            className="input"
            value={transport}
            onChange={(e) =>
              setTransport(e.target.value as McpServer['transport'])
            }
          >
            <option value="http">远程（HTTP / SSE）</option>
            <option value="stdio">本机进程（stdio）</option>
          </select>
        </div>
      </div>
      <div className="field">
        <label>{transport === 'http' ? '地址' : '命令'}</label>
        <input
          className="input"
          value={target}
          placeholder={
            transport === 'http' ? 'https://…/mcp' : 'npx some-mcp-server'
          }
          onChange={(e) => setTarget(e.target.value)}
        />
      </div>
      <div className="field">
        <label>认证</label>
        <select
          className="input"
          value={auth}
          onChange={(e) => setAuth(e.target.value as McpAuth)}
        >
          {(Object.keys(AUTH_LABELS) as McpAuth[]).map((a) => (
            <option key={a} value={a}>
              {AUTH_LABELS[a]}
            </option>
          ))}
        </select>
        <div className="hint">
          密钥由 Successbox 保管。选“使用时询问”时，Agent
          第一次需要时会弹窗让你填写。
        </div>
      </div>
      <div className="field">
        <label>给哪些 Agent 使用</label>
        <div className="row">
          {AGENT_IDS.map((agent) => (
            <label
              key={agent}
              className="check"
              style={{ margin: '0 14px 0 0' }}
            >
              <input
                type="checkbox"
                checked={agents[agent]}
                onChange={(e) =>
                  setAgents({ ...agents, [agent]: e.target.checked })
                }
              />{' '}
              {AGENT_NAMES[agent]}
            </label>
          ))}
        </div>
      </div>
    </Dialog>
  );
}
