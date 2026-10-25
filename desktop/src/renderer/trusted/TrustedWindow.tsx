import { useEffect, useLayoutEffect, useRef, useState } from 'react';
import { AGENT_NAMES } from '../../shared/agents';
import {
  AGENT_IDS,
  type AgentId,
  type AppState,
  type InstallRequest,
  type SecretRequest,
} from '../../shared/types';
import { api } from '../api';
import { useToast } from '../components/Toast';
import { useAppState } from '../useAppState';
import iconUrl from '../../../assets/icon.png';

/**
 * Content of the separate always-on-top window that Successbox opens for
 * sensitive input and approvals. It shows the first request in the queue.
 */
export function TrustedWindow() {
  const state = useAppState();
  const ref = useRef<HTMLDivElement>(null);
  const request = state?.requests[0];

  useLayoutEffect(() => {
    if (ref.current) api.fitTrustedWindow(ref.current.scrollHeight);
  });

  return (
    <div className="tw" ref={ref}>
      <div className="tw-bar">
        <img src={iconUrl} alt="" />
        Successbox 可信窗口
        <span className="spacer" />
        {state && state.requests.length > 1 && (
          <span>还有 {state.requests.length - 1} 个请求</span>
        )}
      </div>
      {!state || !request ? (
        <div className="tw-body muted" style={{ paddingBottom: 20 }}>
          没有待处理的请求
        </div>
      ) : request.kind === 'secret' ? (
        <SecretForm key={request.id} request={request} state={state} />
      ) : (
        <InstallForm key={request.id} request={request} />
      )}
    </div>
  );
}

function useCountdown(expiresAt: number) {
  const [now, setNow] = useState(Date.now());
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(timer);
  }, []);
  const seconds = Math.max(0, Math.round((expiresAt - now) / 1000));
  return `${Math.floor(seconds / 60)}:${String(seconds % 60).padStart(2, '0')}`;
}

function SecretForm({
  request,
  state,
}: {
  request: SecretRequest;
  state: AppState;
}) {
  const toast = useToast();
  const [value, setValue] = useState('');
  const [visible, setVisible] = useState(false);
  const [remember, setRemember] = useState(true);
  const remaining = useCountdown(request.expiresAt);
  const mcp =
    state.mcps.find((m) => m.id === request.mcpId)?.name ?? request.mcpId;

  const answer = (submitted: string | null) => {
    setValue('');
    void toast.run(api.answerSecret(request.id, submitted, remember));
  };

  return (
    <>
      <div className="tw-body">
        <h3>
          {AGENT_NAMES[request.agent]} 需要 {mcp} 的{' '}
          <code>{request.field}</code>
        </h3>
        <div className="muted">
          Agent 调用 MCP 工具时用占位符代替了这个值。提供之后，调用会继续执行。
        </div>
        <dl className="kv">
          <dt>来自</dt>
          <dd>
            {AGENT_NAMES[request.agent]} · {request.cwd}
          </dd>
          <dt>MCP 服务</dt>
          <dd>{mcp}</dd>
          <dt>工具</dt>
          <dd>{request.tool}</dd>
        </dl>
        <div className="row">
          <input
            className="input"
            type={visible ? 'text' : 'password'}
            autoFocus
            autoComplete="off"
            spellCheck={false}
            placeholder={`输入 ${request.field}`}
            value={value}
            onChange={(e) => setValue(e.target.value)}
            onKeyDown={(e) => e.key === 'Enter' && value && answer(value)}
          />
          <button className="btn" onClick={() => setVisible(!visible)}>
            {visible ? '隐藏' : '显示'}
          </button>
        </div>
        <label className="check">
          <input
            type="checkbox"
            checked={remember}
            onChange={(e) => setRemember(e.target.checked)}
          />{' '}
          记住，以后不再询问
        </label>
        <div className="note" style={{ marginBottom: 12 }}>
          这个值只交给 Successbox，不会出现在 Agent 的对话、配置或日志里。
        </div>
      </div>
      <div className="m-foot">
        <span className="muted small">{remaining} 后自动取消</span>
        <span className="spacer" />
        <button className="btn" onClick={() => answer(null)}>
          拒绝
        </button>
        <button
          className="btn shield"
          disabled={!value}
          onClick={() => answer(value)}
        >
          提交
        </button>
      </div>
    </>
  );
}

function InstallForm({ request }: { request: InstallRequest }) {
  const toast = useToast();
  const [tools, setTools] = useState(
    () => new Set(request.tools.filter((t) => !t.writes).map((t) => t.name)),
  );
  const [agents, setAgents] = useState<Record<AgentId, boolean>>({
    claude: request.agent === 'claude',
    codex: request.agent === 'codex',
  });

  const toggleTool = (name: string, on: boolean) => {
    const next = new Set(tools);
    if (on) next.add(name);
    else next.delete(name);
    setTools(next);
  };

  const approve = () =>
    toast.run(
      api.answerInstall(request.id, {
        tools: [...tools],
        agents: AGENT_IDS.filter((a) => agents[a]),
      }),
    );

  return (
    <>
      <div className="tw-body">
        <h3>
          {AGENT_NAMES[request.agent]} 想添加 MCP 服务：{request.name}
        </h3>
        <div className="muted">
          这是 Agent 发起的请求。请确认地址可信后再批准。
        </div>
        <dl className="kv">
          <dt>地址</dt>
          <dd>
            <code>{request.url}</code>
          </dd>
          <dt>认证</dt>
          <dd>
            {request.auth === 'oauth'
              ? 'OAuth · 批准后在浏览器中登录'
              : '使用时询问'}
          </dd>
        </dl>
        <div style={{ fontWeight: 600, marginTop: 4 }}>允许使用的工具</div>
        {request.tools.map((t) => (
          <label key={t.name} className="check" style={{ margin: '5px 0' }}>
            <input
              type="checkbox"
              checked={tools.has(t.name)}
              onChange={(e) => toggleTool(t.name, e.target.checked)}
            />
            <span>
              <code>{t.name}</code>{' '}
              <span className="muted">{t.description}</span>
              {t.writes && (
                <>
                  {' '}
                  <span className="tag warn">写操作</span>
                </>
              )}
            </span>
          </label>
        ))}
        <div style={{ fontWeight: 600, marginTop: 8 }}>给哪些 Agent 使用</div>
        <div className="row" style={{ marginBottom: 12 }}>
          {AGENT_IDS.map((a) => (
            <label
              key={a}
              className="check"
              style={{ margin: '4px 12px 4px 0' }}
            >
              <input
                type="checkbox"
                checked={agents[a]}
                onChange={(e) =>
                  setAgents({ ...agents, [a]: e.target.checked })
                }
              />{' '}
              {AGENT_NAMES[a]}
            </label>
          ))}
        </div>
      </div>
      <div className="m-foot">
        <span className="spacer" />
        <button
          className="btn"
          onClick={() => toast.run(api.answerInstall(request.id, null))}
        >
          拒绝
        </button>
        <button
          className="btn shield"
          disabled={tools.size === 0 || !AGENT_IDS.some((a) => agents[a])}
          onClick={approve}
        >
          批准
        </button>
      </div>
    </>
  );
}
