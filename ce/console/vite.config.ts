import { fileURLToPath, URL } from 'node:url'
import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// Dev proxy: all API traffic goes through the unified gateway (ADR-19).
// The console never talks to backend services directly.
export default defineConfig({
  plugins: [vue()],
  resolve: {
    alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) },
  },
  server: {
    port: 5173,
    proxy: {
      '/v1': { target: 'http://127.0.0.1:18080', changeOrigin: true, ws: true },
      '/v2': { target: 'http://127.0.0.1:18080', changeOrigin: true },
      '/metrics': { target: 'http://127.0.0.1:18080', changeOrigin: true },
    },
  },
  build: {
    outDir: 'dist',
    chunkSizeWarningLimit: 1024,
  },
})
