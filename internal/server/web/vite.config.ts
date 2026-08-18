import react from '@vitejs/plugin-react';
import {defineConfig} from 'vite';
import {readFileSync} from 'node:fs';

// The production build in dist/ is embedded into the oort binary by
// internal/server/web.go, so `npm run build` must precede `go build`.
export default defineConfig({
  plugins: [react()],
  // Inlined data: assets would violate the server's CSP (font-src 'self'),
  // so always emit real files.
  build: {target: 'es2022', assetsInlineLimit: 0},
  server: {
    proxy: {
      '/v1': {
        target: 'http://127.0.0.1:8080',
        configure(proxy) {
          proxy.on('proxyReq', (request) => {
            const path = process.env.OORT_LOCAL_STATE_FILE;
            if (!path) return;
            try {
              const state = JSON.parse(readFileSync(path, 'utf8')) as {token?: string};
              if (state.token) request.setHeader('Authorization', `Bearer ${state.token}`);
            } catch {
              // The Go process atomically replaces local state during a restart;
              // the next request will retry without exposing the token to the browser.
            }
          });
        },
      },
    },
  },
});
