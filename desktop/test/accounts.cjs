// Exercise the real main-process account methods with isolated native config
// files. Electron lifecycle and OS launchers are excluded; no model is called.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const vm = require('node:vm');
const { createRequire } = require('node:module');
const ts = require('typescript');
const directory = fs.mkdtempSync(
  path.join(os.tmpdir(), 'successbox-accounts-'),
);
const load = createRequire(path.resolve(__dirname, '../package.json'));
const app = {
  isPackaged: false,
  getPath: () => directory,
  requestSingleInstanceLock: () => true,
  setName() {},
  on() {},
  once() {},
  whenReady: () => new Promise(() => {}),
};
const source = fs.readFileSync(
  path.resolve(__dirname, '../src/main/main.ts'),
  'utf8',
);
const compiled = ts.transpileModule(source, {
  compilerOptions: {
    module: ts.ModuleKind.CommonJS,
    target: ts.ScriptTarget.ES2022,
    esModuleInterop: true,
  },
}).outputText;
const context = {
  exports: {},
  process: { ...process, env: {} },
  Buffer,
  URL,
  structuredClone,
  AbortController,
  AbortSignal,
  TextDecoder,
  console,
  setTimeout,
  clearTimeout,
  setInterval,
  clearInterval,
  require(name) {
    if (name === 'electron')
      return { app, Notification: { isSupported: () => false } };
    if (name.startsWith('.'))
      return {
        AGENT_COMMANDS: { codex: 'codex', claude: 'claude' },
        AGENT_NAMES: { codex: 'Codex', claude: 'Claude' },
      };
    return load(name);
  },
};
vm.runInNewContext(
  compiled + '\nexports.testBackend = GatewayBackend;',
  context,
  { filename: 'account-owner.cjs' },
);
(async () => {
  try {
    fs.mkdirSync(path.join(directory, 'oauth'));
    fs.writeFileSync(
      path.join(directory, 'llm.json'),
      JSON.stringify({
        server: { host: '127.0.0.1', port: 1 },
        oauth: { 'auth-dir': path.join(directory, 'oauth') },
        access: { 'api-keys': [] },
        'api-keys': { codex: [], claude: [] },
        plugins: {
          enabled: true,
          dir: '.',
          configs: { successbox: { enabled: true, bindings: {} } },
        },
      }),
      { mode: 0o600 },
    );
    const inode = fs.statSync(path.join(directory, 'llm.json')).ino;
    const backend = new context.exports.testBackend();
    await backend.initialization;
    backend.runtime = {
      directory,
      identity: {
        admin: 'private-id-key',
        scopes: {
          codex: { llm: 'codex-caller' },
          claude: { llm: 'claude-caller' },
        },
      },
    };
    const add = (agent, number) =>
      backend.addProvider({
        agent,
        kind: 'apiKey',
        name: agent + number,
        apiKey: 'upstream-secret-' + agent + number,
        endpoint: 'https://example.invalid/v1',
        preset: 'custom',
      });
    await Promise.all(
      Array.from({ length: 8 }, (_, index) => add('codex', index)),
    );
    await Promise.all([add('codex', 7), add('claude', 0)]);
    let config = JSON.parse(fs.readFileSync(path.join(directory, 'llm.json')));
    assert.equal(
      config['api-keys'].codex.length,
      8,
      'concurrent changes were lost or a duplicate was inserted',
    );
    assert.equal(config['api-keys'].claude.length, 1);
    let snapshot = backend.snapshot();
    assert(!JSON.stringify(snapshot).includes('upstream-secret-'));
    assert(!JSON.stringify(snapshot).includes('private-id-key'));
    const first = snapshot.agents.codex.providers.find(
      (entry) => entry.name === 'codex0',
    );
    const target = snapshot.agents.codex.providers.find(
      (entry) => entry.name === 'codex4',
    );
    await Promise.all([
      backend.removeProvider('codex', first.id),
      backend.activateProvider('codex', target.id),
    ]);
    config = JSON.parse(fs.readFileSync(path.join(directory, 'llm.json')));
    assert.equal(
      config.plugins.configs.successbox.bindings['codex-caller'].codex,
      'https://example.invalid/v1#3',
    );
    assert.equal(
      config.plugins.configs.successbox.bindings['claude-caller'].claude,
      'https://example.invalid/v1#0',
    );
    snapshot = backend.snapshot();
    assert.equal(
      snapshot.agents.codex.providers.find((entry) => entry.active).name,
      'codex4',
    );
    assert.equal(
      snapshot.agents.codex.providers.find((entry) => entry.active).id,
      target.id,
      'UI identity changed after removing another account',
    );
    await assert.rejects(backend.removeProvider('codex', target.id), /先切换/);
    await assert.rejects(
      backend.activateProvider('claude', target.id),
      /不存在/,
    );
    assert.equal(
      fs.statSync(path.join(directory, 'llm.json')).mode & 0o777,
      0o600,
    );
    assert.equal(
      fs.statSync(path.join(directory, 'llm.json')).ino,
      inode,
      'native config watch inode changed',
    );
    console.log(
      'Account concurrency, deduplication, stable identity, binding isolation and secret-free snapshots: PASS',
    );
  } finally {
    fs.rmSync(directory, { recursive: true, force: true });
  }
})().catch((error) => {
  console.error(error);
  process.exitCode = 1;
});
