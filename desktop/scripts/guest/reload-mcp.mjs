// The pinned desktop's existing app-server bridge reloads the current catalog.
// CDP is bound only to loopback inside this individual guest network namespace.
import { randomUUID } from 'node:crypto';
const pages = await (
  await fetch('http://127.0.0.1:9333/json/list', {
    signal: AbortSignal.timeout(5000),
  })
).json();
const page = pages.find(
  (entry) => entry.type === 'page' && !entry.url.startsWith('devtools:'),
);
if (!page) throw new Error('Codex Desktop window is not ready');
const socket = new WebSocket(page.webSocketDebuggerUrl);
const requestId = randomUUID();
const expression = `new Promise((resolve, reject) => {
  const id = ${JSON.stringify(requestId)};
  const timer = setTimeout(() => { window.removeEventListener('message', receive); reject(new Error('MCP reload timed out')); }, 10000);
  function receive(event) {
    const response = event.data;
    if (response?.type !== 'mcp-response' || response.message?.id !== id) return;
    clearTimeout(timer);
    window.removeEventListener('message', receive);
    if (response.message.error) reject(new Error('MCP reload was rejected'));
    else resolve(true);
  }
  window.addEventListener('message', receive);
  window.electronBridge.sendMessageFromView({ type: 'mcp-request', hostId: 'local', request: { id, method: 'config/mcpServer/reload' } });
})`;
try {
  await new Promise((resolve, reject) => {
    const timer = setTimeout(
      () => reject(new Error('Desktop connection timed out')),
      15000,
    );
    socket.addEventListener('open', () =>
      socket.send(
        JSON.stringify({
          id: 1,
          method: 'Runtime.evaluate',
          params: { expression, awaitPromise: true, returnByValue: true },
        }),
      ),
    );
    socket.addEventListener('error', () => {
      clearTimeout(timer);
      reject(new Error('Desktop connection failed'));
    });
    socket.addEventListener('message', (event) => {
      const response = JSON.parse(event.data);
      if (response.id !== 1) return;
      clearTimeout(timer);
      if (
        response.error ||
        response.result?.exceptionDetails ||
        response.result?.result?.value !== true
      )
        reject(new Error('Desktop did not refresh its MCP catalog'));
      else resolve();
    });
  });
  console.log('Codex MCP catalog reloaded');
} finally {
  socket.close();
}
