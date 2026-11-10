import { describe, expect, it, vi } from 'vitest'
import { AGENT_PREFIX, FRESH_SECONDS, NOTICES, STEPS, loadTelemetryStatus } from './telemetry'

const NOW = 1_800_000_000_000
const clientOf = ({ names, range = { series: [{ labels: {}, points: [{ t: 1, v: 1 }] }] }, rangeError = null, listError = null } = {}) => ({
  get: vi.fn(async (path) => {
    if (path === 'metrics') {
      if (listError) throw listError
      return names
    }
    if (path === 'metrics/query_range') {
      if (rangeError) throw rangeError
      return range
    }
    throw new Error('unexpected ' + path)
  }),
})
const status = (c) => loadTelemetryStatus(c, { now: () => NOW })

describe('loadTelemetryStatus', () => {
  it('says nothing has arrived when the project has no metrics at all', async () => {
    const c = clientOf({ names: [] })
    expect(await status(c)).toEqual({ state: 'none', metrics: 0, agentMetrics: 0 })
    expect(c.get).toHaveBeenCalledTimes(1) // nothing else to ask
  })

  it('treats an answer that is not a list as no metrics', async () => {
    for (const bad of [null, undefined, {}, 'x', 5]) expect((await status(clientOf({ names: bad }))).state).toBe('none')
  })

  it('says the node-agent is missing when there are metrics but none of its own', async () => {
    const r = await status(clientOf({ names: ['up', 'node_cpu_seconds_total', 'http_requests_total'] }))
    expect(r).toEqual({ state: 'other', metrics: 3, agentMetrics: 0 })
  })

  it('ignores names that are not strings', async () => {
    expect((await status(clientOf({ names: [null, 3, {}, 'container_x'] }))).metrics).toBe(1)
  })

  it('says the node-agent is reporting when its newest metric has points in the last ten minutes', async () => {
    const c = clientOf({ names: ['container_resources_memory_rss_bytes', 'up'] })
    expect(await status(c)).toEqual({ state: 'ok', metrics: 2, agentMetrics: 1 })
    const [, q] = c.get.mock.calls.find(([p]) => p === 'metrics/query_range')
    expect(q).toEqual({ name: 'container_resources_memory_rss_bytes', from: NOW / 1000 - FRESH_SECONDS, to: NOW / 1000, step: 60 })
  })

  it('says the node-agent has stopped when it has metrics but no recent points', async () => {
    for (const range of [{ series: [] }, { series: [{ labels: {}, points: [] }] }, {}, null]) {
      expect((await status(clientOf({ names: ['container_resources_memory_rss_bytes'], range }))).state).toBe('stale')
    }
  })

  it('does not say it has stopped when the answer was cut short, because then points may only be missing from it', async () => {
    expect((await status(clientOf({ names: ['container_resources_memory_rss_bytes'], range: { series: [], truncated: true } }))).state).toBe('ok')
  })

  it('does not say anything is wrong when the freshness probe itself fails', async () => {
    expect((await status(clientOf({ names: ['container_resources_memory_rss_bytes'], rangeError: new Error('boom') }))).state).toBe('ok')
  })

  it('lets a failure to list the metrics through, for the page to report', async () => {
    await expect(status(clientOf({ listError: new Error('The read key was not accepted.') }))).rejects.toThrow('not accepted')
  })

  it('probes a metric the agent reports on every scrape, and falls back to any of its metrics', async () => {
    const pick = async (names) => {
      const c = clientOf({ names })
      await status(c)
      return c.get.mock.calls.find(([p]) => p === 'metrics/query_range')[1].name
    }
    expect(await pick(['container_resources_cpu_usage_seconds_total', 'container_net_tcp_listen_info'])).toBe('container_net_tcp_listen_info')
    expect(await pick(['container_resources_cpu_usage_seconds_total'])).toBe('container_resources_cpu_usage_seconds_total')
    expect(await pick(['container_oddity_total'])).toBe('container_oddity_total')
  })

  it('names the node-agent by the prefix its metrics have', () => {
    expect(AGENT_PREFIX).toBe('container_')
  })
})

describe('what the notice says', () => {
  it('has a title and a lead for each state, naming the project', () => {
    for (const s of ['none', 'other', 'stale']) {
      expect(NOTICES[s].title('gke')).toContain('“gke”')
      expect(NOTICES[s].lead.length).toBeGreaterThan(40)
    }
    expect(NOTICES.stale.lead).toContain('10 minutes')
  })

  it('has steps that ask a question, give a command, and say how to read it', () => {
    expect(STEPS).toHaveLength(3)
    for (const s of STEPS) {
      expect(s.ask.endsWith('?')).toBe(true)
      expect(s.where).toMatch(/^kubectl -n telemetry-agent /)
      expect(s.then.length).toBeGreaterThan(30)
    }
    expect(STEPS.map((s) => s.where).join('\n')).not.toMatch(/apiKey|--from-literal|secret/i) // a command to look at, never one that handles the key
  })
})
