import type { ReactNode } from 'react';
import type { AppState, ServiceState, Simulation } from '../../shared/types';
import { api } from '../api';
import { Switch } from '../components/common';
import { useToast } from '../components/Toast';

function Row({
  title,
  description,
  children,
}: {
  title: string;
  description: ReactNode;
  children: ReactNode;
}) {
  return (
    <div
      className="row"
      style={{ padding: '12px 0', borderTop: '1px solid var(--line2)' }}
    >
      <div style={{ flex: 1 }}>
        <div style={{ fontWeight: 600 }}>{title}</div>
        <div className="muted small">{description}</div>
      </div>
      {children}
    </div>
  );
}

const status = (s: ServiceState) =>
  s === 'ok' ? (
    <span className="pill ok">正常</span>
  ) : (
    <span className="pill err">未运行</span>
  );

const SIMULATIONS: { kind: Simulation; label: string; description: string }[] =
  [
    {
      kind: 'run-start',
      label: '启动 claude',
      description: '相当于在终端里运行 claude',
    },
    {
      kind: 'run-task',
      label: '需要 MCP 密钥',
      description: '最近启动的 Agent 调用 MCP 时需要用户提供密钥',
    },
    {
      kind: 'run-end',
      label: '退出 Agent',
      description: '最近启动的 Agent 退出，受保护时生成改动记录',
    },
    {
      kind: 'install-request',
      label: 'Agent 请求安装 MCP',
      description: '弹出安装审批',
    },
    {
      kind: 'toggle-sandbox',
      label: '切换沙箱服务状态',
      description: '模拟沙箱服务停止或恢复',
    },
    { kind: 'first-run', label: '首次启动', description: '重新显示导入向导' },
  ];

export function SettingsPage({
  state,
  onReimport,
}: {
  state: AppState;
  onReimport(): void;
}) {
  const toast = useToast();
  const { settings, services } = state;

  return (
    <>
      <div className="h2">通用</div>
      <div className="card" style={{ paddingTop: 2, paddingBottom: 2 }}>
        <Row
          title="命令行集成"
          description={
            <>
              终端里的 <code>claude</code> 和 <code>codex</code> 通过 Successbox
              启动。关闭后恢复为原来的命令。
            </>
          }
        >
          <Switch
            checked={settings.commandLine}
            onChange={(v) => toast.run(api.updateSettings({ commandLine: v }))}
            title="命令行集成"
          />
        </Row>
        <Row
          title="启动时显示一行提示"
          description="受保护的 Agent 启动和结束时，在终端里显示一行说明和改动保存位置"
        >
          <Switch
            checked={settings.terminalHints}
            onChange={(v) =>
              toast.run(api.updateSettings({ terminalHints: v }))
            }
            title="启动时显示一行提示"
          />
        </Row>
        <Row
          title="副本保存位置"
          description={
            <>
              <code>{settings.copyDir}</code>（非 Git 目录的改动）
            </>
          }
        >
          <button
            className="btn sm"
            onClick={() => toast.show('更改位置需要后端支持，暂未实现')}
          >
            更改
          </button>
        </Row>
        <Row
          title="导入现有配置"
          description="从 Claude Code 和 Codex 的配置文件中导入账号和 MCP。原文件不会被修改。"
        >
          <button className="btn sm" onClick={onReimport}>
            重新导入
          </button>
        </Row>
      </div>

      <div className="h2">服务状态</div>
      <div className="card" style={{ paddingTop: 2, paddingBottom: 2 }}>
        <Row title="沙箱服务" description="运行受保护的 Agent">
          {status(services.sandbox)}
        </Row>
        <Row title="模型网关" description="保管账号凭据，转发模型请求">
          {status(services.llmGateway)}
        </Row>
        <Row title="MCP 网关" description="连接 MCP 服务，保管其密钥">
          {status(services.mcpGateway)}
        </Row>
      </div>

      {state.simulated && (
        <>
          <div className="h2">
            模拟{' '}
            <span className="sub">
              当前使用内置的模拟后端，下面的按钮用来演示各种情况
            </span>
          </div>
          <div className="card" style={{ paddingTop: 2, paddingBottom: 2 }}>
            {SIMULATIONS.map((s) => (
              <Row key={s.kind} title={s.label} description={s.description}>
                <button
                  className="btn sm"
                  onClick={() => toast.run(api.simulate(s.kind))}
                >
                  触发
                </button>
              </Row>
            ))}
          </div>
        </>
      )}

      <div
        className="faint small"
        style={{ marginTop: 20, textAlign: 'center' }}
      >
        Successbox 0.1.0
      </div>
    </>
  );
}
