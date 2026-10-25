import { useEffect, useState } from 'react';
import { AGENT_NAMES } from '../shared/agents';
import { AGENT_IDS, type AgentId, type Page } from '../shared/types';
import { api } from './api';
import { AgentIcon } from './components/common';
import { WelcomeDialog } from './components/WelcomeDialog';
import { AgentPage } from './pages/AgentPage';
import { ChangesPage } from './pages/ChangesPage';
import { McpPage } from './pages/McpPage';
import { SettingsPage } from './pages/SettingsPage';
import { useAppState } from './useAppState';
import iconUrl from '../../assets/icon.png';

export function App() {
  const state = useAppState();
  const [page, setPage] = useState<Page>('mcp');
  const [agent, setAgent] = useState<AgentId>('claude');
  const [welcome, setWelcome] = useState(false);

  // The tray and system notifications can ask the window to show a page.
  useEffect(() => api.onNavigate(setPage), []);

  // Opening the changes page clears its badge.
  const unread = state?.unreadChanges ?? 0;
  useEffect(() => {
    if (page === 'changes' && unread > 0) void api.markChangesRead();
  }, [page, unread]);

  if (!state) return null;

  return (
    <div className="app">
      <header className="top">
        <div className="brand">
          <img src={iconUrl} alt="" />
          Successbox
        </div>
        <nav className="seg">
          {AGENT_IDS.map((id) => (
            <button
              key={id}
              className={page === 'agent' && agent === id ? 'on' : ''}
              onClick={() => {
                setAgent(id);
                setPage('agent');
              }}
            >
              <AgentIcon agent={id} />
              {AGENT_NAMES[id]}
              {state.agents[id].protection && (
                <span className="shield-mark" title="沙箱保护已开启">
                  🛡
                </span>
              )}
            </button>
          ))}
        </nav>
        <span className="spacer" />
        {state.requests.length > 0 && (
          <span className="pill warn" title="请在 Successbox 可信窗口中处理">
            🔑 {state.requests.length} 个请求待处理
          </span>
        )}
        <button
          className={`ibtn ${page === 'mcp' ? 'on' : ''}`}
          onClick={() => setPage('mcp')}
        >
          MCP
        </button>
        <button
          className={`ibtn ${page === 'changes' ? 'on' : ''}`}
          onClick={() => setPage('changes')}
        >
          改动{unread > 0 && <span className="dotn">{unread}</span>}
        </button>
        <button
          className={`ibtn ${page === 'settings' ? 'on' : ''}`}
          onClick={() => setPage('settings')}
        >
          设置
        </button>
      </header>
      <main className="scroll">
        <div className="inner">
          {state.gatewayControl && (
            <div className="note" role="status">
              {state.gatewayControl.message}
            </div>
          )}
          {page === 'agent' && (
            <AgentPage
              state={state}
              agent={agent}
              onOpenSettings={() => setPage('settings')}
            />
          )}
          {page === 'mcp' && <McpPage state={state} />}
          {page === 'changes' && <ChangesPage state={state} />}
          {page === 'settings' && (
            <SettingsPage state={state} onReimport={() => setWelcome(true)} />
          )}
        </div>
      </main>
      {(state.firstRun || welcome) && (
        <WelcomeDialog onClose={() => setWelcome(false)} />
      )}
    </div>
  );
}
