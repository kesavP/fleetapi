import { describe, expect, it, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import TelemetryNotice from './TelemetryNotice.vue'

const NOW = Date.now()
function sessionOf(project = 'gke') {
  let value = project ? { project, key: 'super-secret-read-key' } : null
  const subs = []
  return { get: () => value, subscribe: (f) => { subs.push(f); return () => subs.splice(subs.indexOf(f), 1) }, set: (v) => { value = v; subs.forEach((f) => f()) }, subs }
}
const clientOf = (names, { fresh = true } = {}) => ({
  get: vi.fn(async (path) => {
    if (path === 'metrics') return typeof names === 'function' ? names() : names
    if (path === 'metrics/query_range') return { series: fresh ? [{ labels: {}, points: [{ t: NOW, v: 1 }] }] : [] }
    throw new Error('unexpected ' + path)
  }),
})
async function mountIt({ client, session = sessionOf() } = {}) {
  const w = mount(TelemetryNotice, { props: { client, session } })
  await flushPromises()
  return { w, session }
}

describe('TelemetryNotice', () => {
  it('says nothing has arrived, naming the project, with the steps to take', async () => {
    const { w } = await mountIt({ client: clientOf([]) })
    const n = w.get('[data-testid=telemetry-notice]')
    expect(n.attributes('role')).toBe('alert')
    expect(n.attributes('data-state')).toBe('none')
    expect(n.get('h2').text()).toContain('No telemetry has arrived for the project “gke”')
    expect(n.text()).toContain('node-agent is most likely not installed')
    const steps = n.findAll('.tn-steps li')
    expect(steps).toHaveLength(3)
    expect(steps.map((s) => s.get('.tn-cmd').text())).toEqual([
      'kubectl -n telemetry-agent get daemonset node-agent',
      'kubectl -n telemetry-agent get pods -o wide',
      'kubectl -n telemetry-agent logs ds/node-agent --tail=50',
    ])
    expect(steps[0].text()).toContain('Is the node-agent installed?')
    expect(steps[2].text()).toContain('401')
  })

  it('says the node-agent is missing when other metrics arrive without its own', async () => {
    const { w } = await mountIt({ client: clientOf(['up', 'http_requests_total']) })
    expect(w.get('[data-testid=telemetry-notice]').attributes('data-state')).toBe('other')
    expect(w.get('h2').text()).toContain('No node-agent metrics for the project “gke”')
    expect(w.text()).toContain('container_')
  })

  it('says the node-agent has stopped when its metrics are not recent', async () => {
    const { w } = await mountIt({ client: clientOf(['container_resources_memory_rss_bytes'], { fresh: false }) })
    expect(w.get('[data-testid=telemetry-notice]').attributes('data-state')).toBe('stale')
    expect(w.get('h2').text()).toContain('has stopped reporting')
  })

  it('shows nothing when the node-agent is reporting', async () => {
    const { w } = await mountIt({ client: clientOf(['container_resources_memory_rss_bytes']) })
    expect(w.find('[data-testid=telemetry-notice]').exists()).toBe(false)
    expect(w.html()).toBe('<!--v-if-->')
  })

  it('shows nothing, and asks nothing, without a client or a session or a connection', async () => {
    const c = clientOf([])
    expect((await mountIt({ client: null })).w.find('[data-testid=telemetry-notice]').exists()).toBe(false)
    expect((await mountIt({ client: c, session: sessionOf(null) })).w.find('[data-testid=telemetry-notice]').exists()).toBe(false)
    expect(c.get).not.toHaveBeenCalled()
    const bare = mount(TelemetryNotice)
    await flushPromises()
    expect(bare.find('[data-testid=telemetry-notice]').exists()).toBe(false)
  })

  it('stays quiet when the metrics cannot be read: the page reports that failure itself', async () => {
    const { w } = await mountIt({ client: clientOf(() => { throw new Error('The read key was not accepted.') }) })
    expect(w.find('[data-testid=telemetry-notice]').exists()).toBe(false)
    expect(w.text()).not.toContain('not accepted')
  })

  it('asks again on request, and goes away when the agent has started reporting', async () => {
    let names = []
    const client = clientOf(() => names)
    const { w } = await mountIt({ client })
    expect(w.find('[data-testid=telemetry-notice]').exists()).toBe(true)
    names = ['container_resources_memory_rss_bytes']
    await w.get('.tn-foot button').trigger('click')
    await flushPromises()
    expect(w.find('[data-testid=telemetry-notice]').exists()).toBe(false)
  })

  it('checks again when the project changes, and shows the new project\'s name', async () => {
    const byProject = { gke: [], dc1: ['container_resources_memory_rss_bytes'] }
    const session = sessionOf('gke')
    const client = { get: vi.fn(async (path) => (path === 'metrics' ? byProject[session.get().project] : { series: [{ points: [{ t: 1, v: 1 }] }] })) }
    const { w } = await mountIt({ client, session })
    expect(w.get('h2').text()).toContain('“gke”')
    session.set({ project: 'dc1', key: 'k' })
    await flushPromises()
    expect(w.find('[data-testid=telemetry-notice]').exists()).toBe(false)
    session.set({ project: 'eu', key: 'k' })
    byProject.eu = []
    await flushPromises()
    expect(w.get('h2').text()).toContain('“eu”')
  })

  it('puts the project name in as text, never as markup, and never shows the key', async () => {
    const { w } = await mountIt({ client: clientOf([]), session: sessionOf('<img src=x onerror=alert(1)>') })
    expect(w.find('img').exists()).toBe(false)
    expect(w.get('h2').text()).toContain('<img src=x onerror=alert(1)>')
    expect(w.html()).not.toContain('super-secret-read-key')
  })

  it('stops listening to the session when it is removed', async () => {
    const { w, session } = await mountIt({ client: clientOf([]) })
    expect(session.subs).toHaveLength(1)
    w.unmount()
    expect(session.subs).toHaveLength(0)
  })

  it('does not let an old answer overwrite a newer one', async () => {
    let release
    const slow = new Promise((r) => (release = r))
    let call = 0
    const client = { get: vi.fn(async (path) => {
      if (path !== 'metrics') return { series: [{ points: [{ t: 1, v: 1 }] }] }
      call++
      if (call === 1) { await slow; return [] } // the first check is slow and would say "none"
      return ['container_resources_memory_rss_bytes'] // the second says it is fine
    }) }
    const { w } = await mountIt({ client })
    // a second check (the project is announced again) finishes first
    const session = w.vm.$props.session
    session.set({ project: 'gke', key: 'k' })
    await flushPromises()
    release()
    await flushPromises()
    expect(w.find('[data-testid=telemetry-notice]').exists()).toBe(false)
  })
})
