import { defineConfig } from 'vitest/config'
import vue from '@vitejs/plugin-vue'
import { buildInfo, versionFile } from './src/buildInfo.js'

// In development the API is reached through Vite's proxy, so the browser sees one origin, as it does in
// the nginx image. Point API_TARGET at telemetry-ingest, for example a `kubectl port-forward` on :8080.
const target = process.env.API_TARGET || 'http://127.0.0.1:8080'

// Which build this is. Taken from the environment at build time (see src/buildInfo.js), shown in the sidebar and served as /version.json, so
// "which version is deployed?" can be answered from the running page or with curl, without cluster access.
const build = buildInfo(process.env)
const versionJson = {
  name: 'version-json',
  generateBundle() {
    this.emitFile({ type: 'asset', fileName: 'version.json', source: versionFile(build) })
  },
}

export default defineConfig({
  plugins: [vue(), versionJson],
  define: {
    __APP_VERSION__: JSON.stringify(build.version),
    __APP_COMMIT__: JSON.stringify(build.commit),
    __APP_BUILT__: JSON.stringify(build.built),
  },
  server: { port: 5173, proxy: { '/api': { target, changeOrigin: false } } },
  build: { sourcemap: false, chunkSizeWarningLimit: 600 },
  // The service's tests (server/) need a real Node, not a browser stand-in: each says so in its first line.
  // Three workers, not one per CPU. With the default many component tests run at once, and the ones that assert right after an interaction
  // (a click, then a route change) failed at random under that contention: 3 of 6 full runs, a different test each time, all passing alone.
  // With three workers 6 of 6 runs were clean. It costs a little speed and nothing else.
  test: { environment: 'jsdom', include: ['src/**/*.test.js', 'server/**/*.test.js'], maxWorkers: 3 },
})
