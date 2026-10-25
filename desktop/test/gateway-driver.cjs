// Synthetic data only. Talks to the real service started by gateway-fixture.py.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const grpc = require('@grpc/grpc-js');
const loader = require('@grpc/proto-loader');
const [directory, action] = process.argv.slice(2);
const fixture = JSON.parse(
  fs.readFileSync(path.join(directory, 'fixture.json')),
);
const definition = loader.loadSync(
  path.resolve(__dirname, '../../api/gateway/v1/gateway.proto'),
  { keepCase: true, longs: String, enums: String, defaults: true },
);
const Service =
  grpc.loadPackageDefinition(definition).successbox.gateway.v1
    .GatewayManagementService;
const client = new Service(fixture.address, grpc.credentials.createInsecure());
const pending = new Map();
const metadata = new grpc.Metadata();
const role =
  action === 'resolve' || action === 'cancel' ? 'gateway' : 'frontend';
metadata.set('x-successbox-client', role);
metadata.set('authorization', `Bearer fixture-${role}`);
const stream = client.gatewaySession(metadata);
let sequence = 0;
stream.on('data', (message) => {
  if (message.response && pending.has(message.correlation_id)) {
    pending.get(message.correlation_id)(message.response);
    pending.delete(message.correlation_id);
  }
});
stream.on('error', (error) => {
  if (error.code !== grpc.status.CANCELLED) {
    console.error(error.message);
    process.exitCode = 1;
  }
});
const call = (body) =>
  new Promise((resolve) => {
    const correlation_id = `test-${++sequence}`;
    pending.set(correlation_id, resolve);
    stream.write({ correlation_id, ...body });
  });
const request = async (body) => {
  const response = await call({ request: body });
  assert.equal(
    response.operation_error,
    undefined,
    JSON.stringify(response.operation_error),
  );
  return response;
};
const rpc = async (scope, method, params, session = '') => {
  const response = await fetch(fixture.mcp, {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      Accept: 'application/json, text/event-stream',
      Authorization: `Bearer sk-bf-fixture-${scope}`,
      ...(session ? { 'Mcp-Session-Id': session } : {}),
    },
    body: JSON.stringify({ jsonrpc: '2.0', id: ++sequence, method, params }),
  });
  assert.equal(response.status, 200, await response.clone().text());
  const body = await response.json();
  assert.equal(body.error, undefined, JSON.stringify(body.error));
  return { body, session: response.headers.get('Mcp-Session-Id') };
};
const timer = setTimeout(() => {
  console.error('fixture operation timed out');
  process.exit(1);
}, 120000);
(async () => {
  await request({
    open_session: {
      sandbox_id: role === 'frontend' ? '' : 'A',
      client_version: 'desktop-fixture',
    },
  });
  if (action === 'seed') {
    for (const sandbox_id of ['A', 'B'])
      await request({
        create_mcp_secret: {
          sandbox_id,
          mcp_id: 'shared-client',
          mcp_name: 'Fixture MCP',
          field_name: 'api_key',
          secret_value: Buffer.from(`fixture-${sandbox_id}`),
        },
      });
    console.log('seeded A and B with the same MCP ID and field');
  } else if (action === 'resolve' || action === 'cancel') {
    console.log('waiting for trusted frontend answer');
    const result = await call({
      request: {
        resolve_secret: {
          operation_id: `desktop-${action}`,
          sandbox_id: 'A',
          mcp_id: 'prompt-client',
          mcp_name: 'Prompt MCP',
          field_name: action === 'resolve' ? 'api_key' : 'cancel_key',
          placeholder: `$PRIVACY_PROTECT_${action === 'resolve' ? 'api_key' : 'cancel_key'}`,
          timeout_ms: 110000,
        },
      },
    });
    if (action === 'resolve')
      assert.equal(
        result.secret_value?.value.toString(),
        'fixture-prompt-secret',
      );
    else assert.equal(result.operation_error?.code, 'Aborted');
    console.log(`${action}: verified`);
  } else if (action === 'install') {
    const { session } = await rpc('A', 'initialize', {
      protocolVersion: '2025-06-18',
      capabilities: {},
      clientInfo: { name: 'desktop-fixture', version: '1' },
    });
    await rpc(
      'A',
      'tools/call',
      {
        name: 'request_mcp_install',
        arguments: {
          mcp_name: 'desktop',
          transport: 'http',
          endpoint: fixture.upstream,
          requested_tools: ['search'],
          credential_fields: ['api_key'],
        },
      },
      session,
    );
    fs.writeFileSync(path.join(directory, 'session'), session);
    console.log('installation submitted through MCP gateway');
  } else if (action === 'verify') {
    const a = await request({ list_mcp_secrets: { sandbox_id: 'A' } });
    const b = await request({ list_mcp_secrets: { sandbox_id: 'B' } });
    assert(!a.secret_list.records.some((r) => r.mcp_id === 'shared-client'));
    assert(b.secret_list.records.some((r) => r.mcp_id === 'shared-client'));
    const session = fs.readFileSync(path.join(directory, 'session'), 'utf8');
    const { body: tools } = await rpc('A', 'tools/list', {}, session);
    const tool = tools.result.tools.find((t) => t.name === 'desktop-search');
    assert(tool, JSON.stringify(tools));
    const { body: result } = await rpc(
      'A',
      'tools/call',
      {
        name: tool.name,
        arguments: { api_key: '$PRIVACY_PROTECT_api_key', query: 'hello' },
      },
      session,
    );
    assert(JSON.stringify(result).includes('desktop-install-ok'));
    assert(!JSON.stringify(result).includes('fixture-installed-secret'));
    const other = await rpc('B', 'initialize', {
      protocolVersion: '2025-06-18',
      capabilities: {},
      clientInfo: { name: 'desktop-fixture', version: '1' },
    });
    const { body: otherTools } = await rpc(
      'B',
      'tools/list',
      {},
      other.session,
    );
    assert(!otherTools.result.tools.some((t) => t.name === tool.name));
    assert(fs.existsSync(path.join(directory, 'called')));
    console.log(
      'verified: scoped deletion, real MCP invocation, placeholder resolution, B cannot discover A tool',
    );
  } else throw new Error(`unknown action: ${action}`);
})()
  .then(() => {
    clearTimeout(timer);
    stream.cancel();
    client.close();
  })
  .catch((error) => {
    console.error(error);
    process.exit(1);
  });
