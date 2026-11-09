// @vitest-environment node
import { mkdtemp, readFile, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { afterEach, describe, expect, it } from 'vitest'
import { createEvaluator } from './evaluator.js'
import { loadConfig } from './config.js'

const P = (name) => ({ name, readKeys: [`${name}-read-key`] })
const cond = (service = '/k8s/shop/cart') => ({ id: `errors|${service}`, rule: 'errors', service, label: service.split('/').slice(2).join('/'), severity: 'critical', summary: 's', evidence: {} })

const dirs = []
afterEach(async () => {
  while (dirs.length) await rm(dirs.pop(), { recursive: true, force: true })
})

/**
 * An evaluator with a stub read API and a stub check. The stub API answers the metric list with the name of the project asked
 * about (taken from the URL, so each project's own client reports who it is), and the stub check turns that into what the test
 * has set for the project: conditions to report, or an error.
 */
function setup({ projects = [P('gke')], stateFile = '' } = {}) {
  let t = 1_700_000_000_000
  const ctx = { projects, conditions: {}, fail: {}, posts: [], logs: [], keysSeen: [], notifyResult: null, projectsError: '' }
  const fetchImpl = async (url, init) => {
    ctx.keysSeen.push(init.headers['X-API-Key'])
    const name = /\/projects\/([^/]+)\//.exec(url)[1]
    return { ok: true, status: 200, json: async () => ({ data: { project: name } }) }
  }
  ctx.ev = createEvaluator({
    config: loadConfig({ INTERVAL_SECONDS: '60' }),
    loadProjects: () => {
      if (ctx.projectsError) throw new Error(ctx.projectsError)
      return ctx.projects
    },
    now: () => t,
    fetchImpl,
    check: async (client) => {
      const { project } = await client.get('metrics')
      if (ctx.fail[project]) throw new Error(ctx.fail[project])
      return { alerts: ctx.conditions[project] || [], checked: 3, skipped: [] }
    },
    notify: async (url, payload) => {
      ctx.posts.push(payload)
      return ctx.notifyResult || { sent: payload.length, error: '' }
    },
    log: (o) => ctx.logs.push(o),
    stateFile,
  })
  ctx.step = async () => {
    t += 60_000
    await ctx.ev.tick()
  }
  return ctx
}
const statuses = (c, p = 'gke') => c.ev.snapshot(p).alerts.map((a) => a.status)

describe('evaluator', () => {
  it('is not ready until its first check has run, and then is, whatever it found', async () => {
    const c = setup()
    expect(c.ev.ready()).toBe(false)
    await c.step()
    expect(c.ev.ready()).toBe(true)
  })

  it('judges each project with its own read key', async () => {
    const c = setup({ projects: [P('gke'), P('dc1')] })
    await c.step()
    expect(new Set(c.keysSeen)).toEqual(new Set(['gke-read-key', 'dc1-read-key']))
  })

  it('holds a new problem as pending and fires it on the second check', async () => {
    const c = setup()
    c.conditions.gke = [cond()]
    await c.step()
    expect(statuses(c)).toEqual(['pending'])
    await c.step()
    expect(statuses(c)).toEqual(['firing'])
    expect(c.logs.some((l) => l.msg === 'alert firing' && l.rule === 'errors' && l.project === 'gke')).toBe(true)
  })

  it('says once that it is checking, and again after a failure is over, not on every check', async () => {
    const c = setup()
    const oks = () => c.logs.filter((l) => l.msg === 'check ok')
    await c.step()
    expect(oks()).toEqual([{ level: 'info', msg: 'check ok', project: 'gke', services: 3, firing: 0, skipped: 0 }])
    await c.step()
    await c.step()
    expect(oks()).toHaveLength(1) // a healthy service stays quiet
    c.fail.gke = 'down'
    await c.step()
    await c.step()
    expect(oks()).toHaveLength(1)
    delete c.fail.gke
    await c.step()
    expect(oks()).toHaveLength(2) // it is back
  })

  it('resolves after two clear checks', async () => {
    const c = setup()
    c.conditions.gke = [cond()]
    await c.step()
    await c.step()
    c.conditions.gke = []
    await c.step()
    expect(statuses(c)).toEqual(['firing'])
    await c.step()
    expect(statuses(c)).toEqual(['resolved'])
    expect(c.logs.some((l) => l.msg === 'alert resolved')).toBe(true)
  })

  it('keeps what was firing, and reports it could not look, when a check fails', async () => {
    const c = setup()
    c.conditions.gke = [cond()]
    await c.step()
    await c.step()
    c.fail.gke = 'The telemetry service is not reachable right now.'
    for (let i = 0; i < 4; i++) await c.step()
    const s = c.ev.snapshot('gke')
    expect(s.alerts.map((a) => a.status)).toEqual(['firing'])
    expect(s.lastError).toContain('not reachable')
    expect(s.errorSince).toBeGreaterThan(0)
    delete c.fail.gke
    await c.step()
    expect(c.ev.snapshot('gke').lastError).toBe('')
  })

  it('keeps telling Alertmanager about what is firing even when the check fails', async () => {
    const c = setup()
    c.conditions.gke = [cond()]
    await c.step()
    await c.step()
    const before = c.posts.length
    c.fail.gke = 'down'
    await c.step()
    expect(c.posts.length).toBe(before + 1)
    expect(c.posts.at(-1)).toHaveLength(1)
    expect(c.posts.at(-1)[0].labels.alertname).toBe('ServiceCallsFailing')
  })

  it('one project failing does not touch another', async () => {
    const c = setup({ projects: [P('gke'), P('dc1')] })
    c.conditions.gke = [cond()]
    c.conditions.dc1 = [cond('/k8s/data/db')]
    c.fail.gke = 'denied'
    await c.step()
    await c.step()
    expect(c.ev.snapshot('gke').lastError).toBe('denied')
    expect(c.ev.snapshot('dc1')).toMatchObject({ lastError: '' })
    expect(statuses(c, 'dc1')).toEqual(['firing'])
  })

  it('picks up a project added to the file, and forgets one removed', async () => {
    const c = setup()
    c.conditions.gke = [cond()]
    await c.step()
    c.projects = [P('gke'), P('new')]
    await c.step()
    expect(c.ev.snapshot('new')).not.toBeNull()
    c.projects = [P('new')]
    await c.step()
    expect(c.ev.snapshot('gke')).toBeNull()
  })

  it('keeps the projects it knew when the file cannot be read for a moment', async () => {
    const c = setup()
    await c.step()
    c.projectsError = 'EAGAIN'
    await c.step()
    expect(c.ev.projects().map((p) => p.name)).toEqual(['gke'])
    expect(c.ev.projectsError()).toBe('EAGAIN')
    c.projectsError = ''
    await c.step()
    expect(c.ev.projectsError()).toBe('')
  })

  it('does not run two checks at once', async () => {
    const c = setup()
    await Promise.all([c.ev.tick(), c.ev.tick(), c.ev.tick()])
    expect(c.keysSeen).toHaveLength(1)
  })

  it('does not know a project that is not in the file, but gives an empty answer to one that is not yet checked', async () => {
    const c = setup()
    await c.ev.restore() // start-up: the projects are read before the first check
    expect(c.ev.snapshot('nope')).toBeNull()
    expect(c.ev.snapshot('gke')).toMatchObject({ alerts: [], lastRun: 0, evaluatedBy: 'server' })
  })

  it('records a failed post and says so in the logs', async () => {
    const c = setup()
    c.conditions.gke = [cond()]
    c.notifyResult = { sent: 0, error: 'cannot reach Alertmanager (ECONNREFUSED)' }
    await c.step()
    expect(c.logs.some((l) => l.msg === 'could not notify' && l.error.includes('ECONNREFUSED'))).toBe(true)
    expect(c.ev.metrics()).toContain('telemetry_alerts_notify_ok{project="gke"} 0')
  })

  it('never writes a key to the logs', async () => {
    const c = setup()
    c.conditions.gke = [cond()]
    c.fail.gke = 'boom'
    await c.step()
    await c.step()
    expect(JSON.stringify(c.logs)).not.toContain('gke-read-key')
  })
})

describe('metrics', () => {
  it('counts what is firing by rule and severity, and how the checking is going', async () => {
    const c = setup({ projects: [P('gke'), P('dc1')] })
    c.conditions.gke = [cond('/k8s/shop/a'), cond('/k8s/shop/b')]
    c.fail.dc1 = 'denied'
    await c.step()
    await c.step()
    const m = c.ev.metrics()
    expect(m).toContain('telemetry_alerts_firing{project="gke",rule="errors",severity="critical"} 2')
    expect(m).toContain('telemetry_alerts_last_check_ok{project="gke"} 1')
    expect(m).toContain('telemetry_alerts_last_check_ok{project="dc1"} 0')
    expect(m).toContain('telemetry_alerts_checks_total{project="gke",result="ok"} 2')
    expect(m).toContain('telemetry_alerts_checks_total{project="dc1",result="error"} 2')
    expect(m).not.toContain('read-key')
    expect(m.endsWith('\n')).toBe(true)
  })
})

describe('state file', () => {
  it('survives a restart: what was firing is still firing, with its original start', async () => {
    const dir = await mkdtemp(join(tmpdir(), 'alerts-'))
    dirs.push(dir)
    const file = join(dir, 'sub', 'state.json') // the directory does not exist yet
    const a = setup({ stateFile: file })
    a.conditions.gke = [cond()]
    await a.step()
    await a.step()
    const started = a.ev.snapshot('gke').alerts[0].firstSeen

    const b = setup({ stateFile: file })
    await b.ev.restore()
    const s = b.ev.snapshot('gke')
    expect(s.alerts).toHaveLength(1)
    expect(s.alerts[0]).toMatchObject({ status: 'firing', firstSeen: started })
    b.conditions.gke = [cond()]
    await b.step()
    expect(b.ev.snapshot('gke').alerts[0].firstSeen).toBe(started) // the episode went on across the restart
  })

  it('starts clean from a missing or corrupt file, and saves no keys', async () => {
    const dir = await mkdtemp(join(tmpdir(), 'alerts-'))
    dirs.push(dir)
    const file = join(dir, 'state.json')
    await writeFile(file, '{ this is not json')
    const c = setup({ stateFile: file })
    await c.ev.restore()
    c.conditions.gke = [cond()]
    await c.step()
    const saved = await readFile(file, 'utf8')
    expect(JSON.parse(saved).projects.gke.alerts).toHaveLength(1)
    expect(saved).not.toContain('read-key')
    const none = setup({ stateFile: join(dir, 'missing.json') })
    await none.ev.restore()
    expect(none.ev.snapshot('gke')).toMatchObject({ alerts: [] })
  })
})
