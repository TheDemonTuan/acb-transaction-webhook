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
    proxy: {
      '/api': 'http://127.0.0.1:8090',
      '/health': 'http://127.0.0.1:8090',
      '/healthz': 'http://127.0.0.1:8090',
      '/ready': 'http://127.0.0.1:8090',
      '/readyz': 'http://127.0.0.1:8090',
    },
  },
  test: {
    environment: 'node',
    include: ['tests/**/*.{test,spec}.{ts,tsx}', 'src/**/*.{test,spec}.{ts,tsx}'],
  },
});
