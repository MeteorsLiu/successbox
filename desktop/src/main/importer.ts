import { existsSync, readFileSync } from 'node:fs';
import { homedir } from 'node:os';
import { join } from 'node:path';
import type { ImportCandidate } from '../shared/types';

// Read-only discovery of existing Agent configuration. Only counts leave this
// module; key values are never returned to the renderer.

function readText(path: string): string | null {
  try {
    return readFileSync(path, 'utf8');
  } catch {
    return null;
  }
}

function readJson(path: string): Record<string, unknown> | null {
  const text = readText(path);
  if (text === null) return null;
  try {
    return JSON.parse(text) as Record<string, unknown>;
  } catch {
    return null;
  }
}

const tilde = (path: string) => path.replace(homedir(), '~');

function scanClaude(home: string): ImportCandidate {
  const settingsPath = join(home, '.claude', 'settings.json');
  const settings = readJson(settingsPath);
  const env = (settings?.env ?? {}) as Record<string, unknown>;
  const apiKeys = ['ANTHROPIC_API_KEY', 'ANTHROPIC_AUTH_TOKEN'].filter(
    (k) => typeof env[k] === 'string' && env[k],
  ).length;
  // User-scoped MCP servers live in ~/.claude.json.
  const userConfig = readJson(join(home, '.claude.json'));
  const mcpServers = Object.keys(
    (userConfig?.mcpServers ?? {}) as object,
  ).length;
  const login = existsSync(join(home, '.claude', '.credentials.json'));
  return {
    agent: 'claude',
    path: tilde(settingsPath),
    found: settings !== null || userConfig !== null,
    apiKeys,
    login,
    mcpServers,
  };
}

function scanCodex(home: string): ImportCandidate {
  const configPath = join(home, '.codex', 'config.toml');
  const config = readText(configPath);
  const servers = new Set<string>();
  for (const match of (config ?? '').matchAll(
    /^\s*\[mcp_servers\.("?)([^\]".]+)\1/gm,
  ))
    servers.add(match[2]);
  const auth = readJson(join(home, '.codex', 'auth.json'));
  const apiKeys =
    typeof auth?.OPENAI_API_KEY === 'string' && auth.OPENAI_API_KEY ? 1 : 0;
  const login = auth !== null && auth.tokens != null;
  return {
    agent: 'codex',
    path: tilde(configPath),
    found: config !== null || auth !== null,
    apiKeys,
    login,
    mcpServers: servers.size,
  };
}

export function scanImports(home = homedir()): ImportCandidate[] {
  return [scanClaude(home), scanCodex(home)];
}
