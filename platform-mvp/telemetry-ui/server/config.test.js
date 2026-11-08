// @vitest-environment node
import { describe, expect, it } from 'vitest'
import { loadConfig } from './config.js'

describe('loadConfig', () => {
  it('has defaults that suit the chart', () => {
    const c = loadConfig({})
    expect(c).toMatchObject({
      listen: 8081,
      readApi: 'http://telemetry-ingest.monitoring.svc.cluster.local',
      projectsFile: '/etc/telemetry/projects/projects.json',
      stateFile: '',
      intervalSeconds: 60,
      alertmanagerUrl: '',
    })
    expect(c.thresholds.errorShare).toBe(5)
  })

  it('reads the settings, trimming a trailing slash from a URL', () => {
    const c = loadConfig({ PORT: '9000', READ_API: 'http://api:8080//', INTERVAL_SECONDS: '30', ALERTMANAGER_URL: 'http://am:9093/', STATE_FILE: '/var/x.json', PROJECTS_FILE: '/p.json' })
    expect(c).toMatchObject({ listen: 9000, readApi: 'http://api:8080', intervalSeconds: 30, alertmanagerUrl: 'http://am:9093', stateFile: '/var/x.json', projectsFile: '/p.json' })
  })

  it('takes thresholds from RULES_JSON over the defaults', () => {
    const c = loadConfig({ RULES_JSON: '{"errorShare": 20, "baselineSeconds": 7200}' })
    expect(c.thresholds).toMatchObject({ errorShare: 20, baselineSeconds: 7200, recentSeconds: 300 })
  })

  it.each([
    [{ PORT: 'abc' }, 'PORT'],
    [{ PORT: '70000' }, 'PORT'],
    [{ INTERVAL_SECONDS: '5' }, 'INTERVAL_SECONDS'],
    [{ INTERVAL_SECONDS: '1.5' }, 'INTERVAL_SECONDS'],
    [{ READ_API: 'not a url' }, 'READ_API is not a URL'],
    [{ READ_API: 'ftp://x' }, 'http or https'],
    [{ ALERTMANAGER_URL: '::' }, 'ALERTMANAGER_URL'],
    [{ RULES_JSON: '{nope' }, 'RULES_JSON is not valid JSON'],
    [{ RULES_JSON: '[1]' }, 'must be an object'],
    [{ RULES_JSON: '{"errrorShare": 3}' }, 'unknown threshold(s): errrorShare'],
  ])('refuses %j', (env, text) => {
    expect(() => loadConfig(env)).toThrow(text)
  })
})
