import react from '@vitejs/plugin-react';
import {defineConfig} from 'vite';
import {readFileSync} from 'node:fs';

// The production build in dist/ is embedded into the nebulous binary by
// internal/server/web.go, so `npm run build` must precede `go build`.
export default defineConfig({
  plugins: [react()],
  build: {target: 'es2022'},
  server: {
    proxy: {
      '/v1': {
        target: 'http://127.0.0.1:8080',
        configure(proxy) {
          proxy.on('proxyReq', (request) => {
            const path = process.env.NEB_LOCAL_STATE_FILE;
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
