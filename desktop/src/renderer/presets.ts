import type { AgentId, McpAuth, Provider } from '../shared/types';

export interface ProviderPreset {
  id: string;
  name: string;
  description: string;
  kind: Provider['kind'];
  /** Present when the preset needs an endpoint field; empty for user input. */
  endpoint?: string;
  color: string;
  initial: string;
}

export const PROVIDER_PRESETS: Record<AgentId, ProviderPreset[]> = {
  claude: [
    {
      id: 'claude-subscription',
      name: 'Claude 订阅',
      description: '登录 Pro / Max 账号',
      kind: 'subscription',
      color: '#d97757',
      initial: 'A',
    },
    {
      id: 'anthropic-api',
      name: 'Anthropic API',
      description: 'API Key',
      kind: 'apiKey',
      color: '#c46a4b',
      initial: 'A',
    },
    {
      id: 'deepseek',
      name: 'DeepSeek',
      description: 'Anthropic 兼容接口',
      kind: 'apiKey',
      endpoint: 'https://api.deepseek.com/anthropic',
      color: '#4d6bfe',
      initial: 'D',
    },
    {
      id: 'kimi',
      name: 'Kimi',
      description: 'Anthropic 兼容接口',
      kind: 'apiKey',
      endpoint: 'https://api.moonshot.cn/anthropic',
      color: '#1f1f1f',
      initial: 'K',
    },
    {
      id: 'glm',
      name: '智谱 GLM',
      description: 'Anthropic 兼容接口',
      kind: 'apiKey',
      endpoint: 'https://open.bigmodel.cn/api/anthropic',
      color: '#3859ff',
      initial: 'Z',
    },
    {
      id: 'claude-custom',
      name: '自定义',
      description: '填写地址和 Key',
      kind: 'apiKey',
      endpoint: '',
      color: '#7b8494',
      initial: '+',
    },
  ],
  codex: [
    {
      id: 'chatgpt-subscription',
      name: 'ChatGPT 订阅',
      description: '登录 ChatGPT 账号',
      kind: 'subscription',
      color: '#111111',
      initial: '◎',
    },
    {
      id: 'openai-api',
      name: 'OpenAI API',
      description: 'API Key',
      kind: 'apiKey',
      color: '#10a37f',
      initial: 'O',
    },
    {
      id: 'azure-openai',
      name: 'Azure OpenAI',
      description: '部署地址 + Key',
      kind: 'apiKey',
      endpoint: '',
      color: '#0078d4',
      initial: 'Az',
    },
    {
      id: 'codex-custom',
      name: '自定义',
      description: 'OpenAI 兼容接口',
      kind: 'apiKey',
      endpoint: '',
      color: '#7b8494',
      initial: '+',
    },
  ],
};

export function presetFor(agent: AgentId, provider: Provider) {
  return PROVIDER_PRESETS[agent].find((p) => p.id === provider.preset);
}

export interface McpPreset {
  name: string;
  target: string;
  auth: McpAuth;
}

export const MCP_PRESETS: McpPreset[] = [
  {
    name: 'GitHub',
    target: 'https://api.githubcopilot.com/mcp',
    auth: 'oauth',
  },
  { name: 'Notion', target: 'https://mcp.notion.com/mcp', auth: 'oauth' },
  { name: 'Sentry', target: 'https://mcp.sentry.dev/mcp', auth: 'oauth' },
  { name: '自定义', target: '', auth: 'askOnUse' },
];

export const AUTH_LABELS: Record<McpAuth, string> = {
  oauth: 'OAuth（在浏览器中登录）',
  apiKey: 'API Key',
  askOnUse: '使用时询问',
  none: '无需认证',
};
