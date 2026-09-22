import { defineConfig } from 'vitest/config';
import react from '@vitejs/plugin-react';
import tailwindcss from '@tailwindcss/vite';

export default defineConfig({
  plugins: [react(), tailwindcss()],
  build: {
    outDir: 'dist',
    emptyOutDir: true,
  },
  server: {
    strictPort: true,
    port: 5173,
    headers: {
      'Content-Security-Policy':
        "default-src 'self'; base-uri 'none'; frame-ancestors 'self'; form-action 'self'; object-src 'none'; script-src 'self' https://static.cloudflareinsights.com; script-src-elem 'self' https://static.cloudflareinsights.com 'unsafe-inline'; script-src-attr 'none'; connect-src 'self' ws: wss: https://cloudflareinsights.com; img-src 'self' data: blob: https:; font-src 'self' data:; style-src 'self' 'unsafe-inline'",
    },
    proxy: {
      '/api': 'http://127.0.0.1:18081',
      '/health': 'http://127.0.0.1:18081',
      '/healthz': 'http://127.0.0.1:18081',
      '/ready': 'http://127.0.0.1:18081',
      '/readyz': 'http://127.0.0.1:18081',
    },
  },
  test: {
    environment: 'node',
    include: ['tests/**/*.{test,spec}.{ts,tsx}', 'src/**/*.{test,spec}.{ts,tsx}'],
  },
});
