import type { AgentId } from './types';

export const AGENT_NAMES: Record<AgentId, string> = {
  claude: 'Claude Code',
  codex: 'Codex',
};
export const AGENT_COMMANDS: Record<AgentId, string> = {
  claude: 'claude',
  codex: 'codex',
};
