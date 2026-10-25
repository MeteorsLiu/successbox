import {
  Menu,
  nativeImage,
  Tray,
  type MenuItemConstructorOptions,
} from 'electron';
import { AGENT_IDS, type AgentId, type Page } from '../shared/types';
import { AGENT_NAMES } from '../shared/agents';
import type { Backend } from './backend/backend';
import { assetPath } from './windows';

interface TrayActions {
  showMain(page?: Page): void;
  focusTrusted(): void;
  quit(): void;
  error(message: string): void;
}

export function createTray(backend: Backend, actions: TrayActions) {
  const tray = new Tray(nativeImage.createFromPath(assetPath('tray.png')));
  tray.setToolTip('Successbox');

  const run = (task: Promise<unknown>) =>
    task.catch((err: Error) => actions.error(err.message));

  const agentSection = (agent: AgentId): MenuItemConstructorOptions[] => {
    const { protection, providers } = backend.snapshot().agents[agent];
    return [
      { label: AGENT_NAMES[agent], enabled: false },
      ...providers.map(
        (p): MenuItemConstructorOptions => ({
          label: p.problem ? `${p.name}（不可用）` : p.name,
          type: 'radio',
          checked: p.active,
          enabled: !p.problem,
          click: () => run(backend.activateProvider(agent, p.id)),
        }),
      ),
      {
        label: '沙箱保护',
        type: 'checkbox',
        checked: protection,
        click: () => run(backend.setProtection(agent, !protection)),
      },
      { type: 'separator' },
    ];
  };

  const rebuild = () => {
    const state = backend.snapshot();
    const pending = state.requests.length;
    const template: MenuItemConstructorOptions[] = [
      ...AGENT_IDS.flatMap(agentSection),
      ...(pending
        ? [{ label: `待处理请求（${pending}）`, click: actions.focusTrusted }]
        : []),
      {
        label: state.unreadChanges
          ? `改动记录（${state.unreadChanges} 条新）`
          : '改动记录',
        click: () => actions.showMain('changes'),
      },
      { label: '打开 Successbox', click: () => actions.showMain() },
      { type: 'separator' },
      { label: '退出', click: actions.quit },
    ];
    tray.setContextMenu(Menu.buildFromTemplate(template));
    tray.setToolTip(
      pending ? `Successbox · ${pending} 个待处理请求` : 'Successbox',
    );
  };

  backend.subscribe(rebuild);
  rebuild();
  tray.on('click', () => actions.showMain());
  return tray;
}
