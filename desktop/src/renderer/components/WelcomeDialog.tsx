import { useEffect, useState } from 'react';
import { AGENT_NAMES } from '../../shared/agents';
import type { AgentId, ImportCandidate } from '../../shared/types';
import { api } from '../api';
import { AgentIcon, Dialog } from './common';
import { useToast } from './Toast';

function summary(c: ImportCandidate) {
  if (!c.found) return '没有找到配置';
  const parts = [];
  if (c.login) parts.push('官方登录');
  if (c.apiKeys) parts.push(`${c.apiKeys} 个 API Key`);
  parts.push(`${c.mcpServers} 个 MCP`);
  return parts.join(' · ');
}

export function WelcomeDialog({ onClose }: { onClose(): void }) {
  const toast = useToast();
  const [candidates, setCandidates] = useState<ImportCandidate[] | null>(null);
  const [selected, setSelected] = useState<Record<AgentId, boolean>>({
    claude: true,
    codex: true,
  });
  const [commandLine, setCommandLine] = useState(true);
  const [protection, setProtection] = useState(true);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    api.scanImports().then(
      (list) => {
        setCandidates(list);
        setSelected({
          claude: list.some((c) => c.agent === 'claude' && c.found),
          codex: list.some((c) => c.agent === 'codex' && c.found),
        });
      },
      () => setCandidates([]),
    );
  }, []);

  const later = async () => {
    await toast.run(api.dismissWelcome());
    onClose();
  };

  const confirm = async () => {
    setBusy(true);
    const agents = (Object.keys(selected) as AgentId[]).filter(
      (a) => selected[a],
    );
    const ok = await toast.run(
      api.importConfigs(agents, { commandLine, protection }),
      '导入完成',
    );
    setBusy(false);
    if (ok) onClose();
  };

  return (
    <Dialog
      title="欢迎使用 Successbox"
      size="wide"
      onClose={later}
      footer={
        <>
          <span className="spacer" />
          <button className="btn" onClick={later} disabled={busy}>
            稍后
          </button>
          <button
            className="btn primary"
            onClick={confirm}
            disabled={busy || !candidates}
          >
            {busy ? (
              <>
                <i className="spin" /> 正在导入…
              </>
            ) : (
              '导入'
            )}
          </button>
        </>
      }
    >
      <p className="muted" style={{ marginTop: 0 }}>
        找到了以下配置。导入后，可以在这里统一管理账号和 MCP。
      </p>
      {!candidates && (
        <div className="muted">
          <i className="spin" /> 正在查找…
        </div>
      )}
      <div className="stack">
        {candidates?.map((c) => (
          <label
            key={c.agent}
            className="card row"
            style={{
              cursor: c.found ? 'pointer' : 'default',
              opacity: c.found ? 1 : 0.6,
            }}
          >
            <AgentIcon agent={c.agent} large />
            <div style={{ flex: 1 }}>
              <b>{AGENT_NAMES[c.agent]}</b>
              <div className="muted small">
                <code>{c.path}</code> · {summary(c)}
              </div>
            </div>
            <input
              type="checkbox"
              disabled={!c.found}
              checked={selected[c.agent]}
              onChange={(e) =>
                setSelected({ ...selected, [c.agent]: e.target.checked })
              }
            />
          </label>
        ))}
      </div>
      <label className="check" style={{ marginTop: 14 }}>
        <input
          type="checkbox"
          checked={commandLine}
          onChange={(e) => setCommandLine(e.target.checked)}
        />
        <span>
          <b>让终端里的 claude / codex 通过 Successbox 启动</b>
          <br />
          <span className="muted small">
            在 <code>~/.local/bin</code> 中安装同名命令，可以随时在设置里关闭
          </span>
        </span>
      </label>
      <label className="check">
        <input
          type="checkbox"
          checked={protection}
          onChange={(e) => setProtection(e.target.checked)}
        />
        <span>
          <b>默认开启沙箱保护</b>
          <br />
          <span className="muted small">
            Agent 的改动另存为 worktree 或副本，不会修改本机的文件和配置
          </span>
        </span>
      </label>
      <div className="note">
        原配置文件不会被修改。导入的 Key 由 Successbox 保管，沙箱中的 Agent
        读不到原文件里的 Key。
      </div>
    </Dialog>
  );
}
