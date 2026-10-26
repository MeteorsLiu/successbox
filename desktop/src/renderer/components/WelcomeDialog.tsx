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
  const commandLine = false;
  const protection = true;
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
        导入现有模型账号。MCP 可以在启动 Agent 后通过安装工具添加。
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
      <div className="note">
        原配置文件不会被修改。导入的 Key 由 Successbox 保管，沙箱中的 Agent
        读不到原文件里的 Key。
      </div>
    </Dialog>
  );
}
