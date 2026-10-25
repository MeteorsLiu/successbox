import { defineConfig, type Plugin } from 'vite';
import react from '@vitejs/plugin-react';

// The development server injects inline scripts for hot reload, so the
// strict Content-Security-Policy is only added to packaged builds.
const csp: Plugin = {
  name: 'successbox-csp',
  apply: 'build',
  transformIndexHtml: (html) =>
    html.replace(
      '<head>',
      `<head>\n    <meta http-equiv="Content-Security-Policy" content="default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:">`,
    ),
};

// https://vitejs.dev/config
export default defineConfig({ plugins: [react(), csp] });
