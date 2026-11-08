import { defineConfig } from 'vitest/config'
import vue from '@vitejs/plugin-vue'

// In development the API is reached through Vite's proxy, so the browser sees one origin, as it does in
// the nginx image. Point API_TARGET at telemetry-ingest, for example a `kubectl port-forward` on :8080.
const target = process.env.API_TARGET || 'http://127.0.0.1:8080'

export default defineConfig({
  plugins: [vue()],
  server: { port: 5173, proxy: { '/api': { target, changeOrigin: false } } },
  build: { sourcemap: false, chunkSizeWarningLimit: 600 },
  // The service's tests (server/) need a real Node, not a browser stand-in: each says so in its first line.
  test: { environment: 'jsdom', include: ['src/**/*.test.js', 'server/**/*.test.js'] },
})
