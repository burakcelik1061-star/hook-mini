import { defineConfig } from 'vitest/config';
import react from '@vitejs/plugin-react';

// The build goes straight into the Go package that embeds it
// (internal/webui), so `go build` after `npm run build` ships the console.
// In development, /v1 is proxied to a locally running api.
export default defineConfig({
  plugins: [react()],
  build: {
    outDir: '../internal/webui/dist',
    emptyOutDir: true,
    assetsDir: 'assets',
    // Never inline assets as data: URIs. The console's Content-Security-Policy
    // allows fonts and images from its own origin only, and keeping it that
    // strict is worth a few more small requests.
    assetsInlineLimit: 0,
    sourcemap: false,
  },
  server: {
    port: 5173,
    proxy: { '/v1': 'http://localhost:8080' },
  },
  test: {
    environment: 'node',
  },
});
