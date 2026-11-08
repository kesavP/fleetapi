import { createServer } from 'node:http'
import { loadConfig } from './config.js'
import { loadProjects } from './projects.js'
import { createEvaluator } from './evaluator.js'
import { createHandler } from './app.js'

// The alert service: evaluates every project on a timer and serves the result. See docs/platform-mvp/21-telemetry-ui.md.

const log = (o) => console.log(JSON.stringify({ time: new Date().toISOString(), ...o }))

let config
try {
  config = loadConfig()
} catch (e) {
  log({ level: 'error', msg: `bad configuration: ${e.message}` })
  process.exit(2)
}

const evaluator = createEvaluator({ config, loadProjects: () => loadProjects(config.projectsFile), log })
await evaluator.restore()

const server = createServer(createHandler({ evaluator, log }))
server.listen(config.listen, () =>
  log({ level: 'info', msg: 'listening', port: config.listen, readApi: config.readApi, intervalSeconds: config.intervalSeconds, alertmanager: config.alertmanagerUrl ? 'on' : 'off', state: config.stateFile || 'memory only' }),
)

// A timer chain, not setInterval: the next check starts a full interval after the last one ended, so a slow one never overlaps.
let timer = null
let stopping = false
async function loop() {
  try {
    await evaluator.tick()
  } catch (e) {
    log({ level: 'error', msg: 'check crashed', error: e && e.message })
  }
  if (!stopping) timer = setTimeout(loop, config.intervalSeconds * 1000)
}
loop()

function stop(signal) {
  if (stopping) return
  stopping = true
  log({ level: 'info', msg: `stopping on ${signal}` })
  clearTimeout(timer)
  server.close(() => process.exit(0))
  setTimeout(() => process.exit(0), 3000).unref()
}
process.on('SIGTERM', () => stop('SIGTERM'))
process.on('SIGINT', () => stop('SIGINT'))
