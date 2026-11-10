import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { createMemoryHistory, createRouter } from 'vue-router'
import { reactive } from 'vue'
import SideNav from './SideNav.vue'
import { createSession } from '../api/session'

const stub = { template: '<i/>' }
const PATHS = ['/mapping', '/applications', '/applications/:name', '/map', '/traces', '/traces/:id', '/logs', '/profiles', '/alerts', '/incidents', '/incidents/:id', '/configuration/connection', '/configuration/clickhouse']

function memoryStorage() {
  const m = new Map()
  return { getItem: (k) => (m.has(k) ? m.get(k) : null), setItem: (k, v) => m.set(k, String(v)), removeItem: (k) => m.delete(k) }
}

async function mountNav({ at = '/applications', session = createSession(memoryStorage()), alerts = null, build = null } = {}) {
  const router = createRouter({ history: createMemoryHistory(), routes: PATHS.map((path) => ({ path, component: stub })) })
  router.push(at)
  await router.isReady()
  const w = mount(SideNav, { props: { session, alerts, build }, global: { plugins: [router] } })
  await flushPromises()
  return { w, router, session }
}
const items = (w) => w.findAll('.menu .item')

beforeEach(() => window.localStorage.clear())
afterEach(() => window.localStorage.clear())

describe('SideNav', () => {
  it('has the pages as menu entries, in order, each a link', async () => {
    const { w } = await mountNav()
    expect(items(w).map((i) => [i.text(), i.attributes('href')])).toEqual([
      ['Applications', '/applications'],
      ['Map', '/map'],
      ['Traces', '/traces'],
      ['Logs', '/logs'],
      ['Profiles', '/profiles'],
      ['Alerts', '/alerts'],
      ['Incidents', '/incidents'],
    ])
    expect(w.get('nav').attributes('aria-label')).toBe('Main menu')
  })

  it('has no entry called Services: it is Applications now', async () => {
    const { w } = await mountNav()
    expect(w.text()).not.toMatch(/\bServices\b/)
  })

  it('highlights the page being shown, and keeps its menu entry lit on a page beneath it', async () => {
    const list = await mountNav({ at: '/applications' })
    expect(items(list.w).filter((i) => i.classes().includes('active')).map((i) => i.text())).toEqual(['Applications'])
    const detail = await mountNav({ at: '/applications/%2Fk8s%2Fshop%2Ffrontend' })
    expect(items(detail.w).filter((i) => i.classes().includes('active')).map((i) => i.text())).toEqual(['Applications'])
    const trace = await mountNav({ at: `/traces/${'a'.repeat(32)}` })
    expect(items(trace.w).filter((i) => i.classes().includes('active')).map((i) => i.text())).toEqual(['Traces'])
    expect(items(trace.w).filter((i) => i.attributes('aria-current') === 'page').map((i) => i.text())).toEqual(['Traces'])
    const inc = await mountNav({ at: '/incidents/abc12345' })
    expect(items(inc.w).filter((i) => i.classes().includes('active')).map((i) => i.text())).toEqual(['Incidents'])
    const conn = await mountNav({ at: '/configuration/connection' })
    expect(items(conn.w).some((i) => i.classes().includes('active'))).toBe(false) // the project link below is the one lit
    expect(conn.w.get('.foot .project').classes()).toContain('active')
    const ch = await mountNav({ at: '/configuration/clickhouse' })
    expect(ch.w.get('.foot .project').classes()).toContain('active') // the ClickHouse tab is under the project too
    const lookalike = await mountNav({ at: '/mapping' })
    expect(items(lookalike.w).some((i) => i.classes().includes('active'))).toBe(false) // "/map" is not a prefix of "/mapping"
    void lookalike
  })

  it('shows the connected project, and "Not connected" without one, and follows a connect and a disconnect', async () => {
    const { w, session } = await mountNav()
    expect(w.get('[data-testid=project]').text()).toBe('Not connected')
    session.set({ project: 'gke', key: 'k' })
    await flushPromises()
    expect(w.get('[data-testid=project]').text()).toBe('gke')
    session.clear()
    await flushPromises()
    expect(w.get('[data-testid=project]').text()).toBe('Not connected')
  })

  it('starts with the project of a session that already exists, and links it to the connection page', async () => {
    const s = createSession(memoryStorage())
    s.set({ project: 'dc1', key: 'k' })
    const { w } = await mountNav({ session: s })
    expect(w.get('[data-testid=project]').text()).toBe('dc1')
    expect(w.get('.foot .project').attributes('href')).toBe('/configuration/connection')
  })

  it('never shows the key', async () => {
    const s = createSession(memoryStorage())
    s.set({ project: 'gke', key: 'super-secret-read-key' })
    const { w } = await mountNav({ session: s })
    expect(w.html()).not.toContain('super-secret-read-key')
  })

  it('shows how many alerts are firing on Alerts, red when one is critical, and nothing when none are', async () => {
    const alerts = { state: reactive({ alerts: [] }) }
    const { w } = await mountNav({ alerts })
    expect(w.find('.badge').exists()).toBe(false)
    alerts.state.alerts = [{ status: 'firing', severity: 'warning' }, { status: 'pending', severity: 'critical' }, { status: 'resolved', severity: 'critical' }]
    await flushPromises()
    expect(w.get('.badge').text()).toBe('1')
    expect(w.get('.badge').classes()).not.toContain('critical')
    expect(w.get('.badge').attributes('aria-label')).toBe('1 firing')
    alerts.state.alerts.push({ status: 'firing', severity: 'critical' })
    await flushPromises()
    expect(w.get('.badge').text()).toBe('2')
    expect(w.get('.badge').classes()).toContain('critical')
    expect(w.get('.badge').element.closest('a').getAttribute('href')).toBe('/alerts')
  })

  it('collapses to icons, says so for assistive technology, labels the icons, and remembers', async () => {
    const { w } = await mountNav()
    const btn = w.get('button.collapse')
    expect(btn.attributes('aria-expanded')).toBe('true')
    expect(w.get('aside').classes()).not.toContain('collapsed')
    expect(items(w)[0].attributes('title')).toBeUndefined()
    await btn.trigger('click')
    expect(w.get('aside').classes()).toContain('collapsed')
    expect(btn.attributes('aria-expanded')).toBe('false')
    expect(btn.attributes('aria-label')).toBe('Expand the menu')
    expect(items(w)[0].attributes('title')).toBe('Applications') // the text is hidden, so the icon says what it is
    expect(window.localStorage.getItem('fleet-ui.sidebar')).toBe('collapsed')
    const again = await mountNav()
    expect(again.w.get('aside').classes()).toContain('collapsed') // remembered
    await again.w.get('button.collapse').trigger('click')
    expect(window.localStorage.getItem('fleet-ui.sidebar')).toBe('open')
  })

  it('draws its icons itself and hides them from assistive technology', async () => {
    const { w } = await mountNav()
    const icons = w.findAll('svg.icon')
    expect(icons.length).toBeGreaterThanOrEqual(8)
    expect(icons.every((i) => i.attributes('aria-hidden') === 'true')).toBe(true)
  })

  it('says which build it is at the foot of the menu, with the commit and the day, and the whole of it as a tooltip', async () => {
    const { w } = await mountNav({ build: { name: 'telemetry-ui', version: 'v19', commit: '3f9a1c2d4e5b6789', built: '2026-11-10T07:30:00Z' } })
    const v = w.get('.foot [data-testid=version]')
    expect(v.text()).toContain('telemetry-ui')
    expect(v.get('strong').text()).toBe('v19')
    expect(v.get('small').text()).toBe('3f9a1c2d4e5b · 2026-11-10')
    expect(v.attributes('title')).toBe('telemetry-ui v19 · commit 3f9a1c2d4e5b6789 · built 2026-11-10 07:30 UTC')
  })

  it('says it is a development build when it was built without a version, and shows no second line it cannot fill', async () => {
    const { w } = await mountNav({ build: { name: 'telemetry-ui', version: 'dev', commit: '', built: '' } })
    const v = w.get('[data-testid=version]')
    expect(v.get('strong').text()).toBe('dev')
    expect(v.find('small').exists()).toBe(false)
  })

  it('shows the build the test run was built as when none is given, and keeps it out of the menu entries', async () => {
    const { w } = await mountNav()
    expect(w.get('[data-testid=version] strong').text().length).toBeGreaterThan(0)
    expect(items(w).map((i) => i.text())).not.toContain('dev')
    expect(w.findAll('.item').every((i) => !i.find('[data-testid=version]').exists())).toBe(true)
  })

  it('puts the version in as text, never as markup', async () => {
    const { w } = await mountNav({ build: { name: 'telemetry-ui', version: '<img src=x onerror=alert(1)>', commit: '', built: '' } })
    expect(w.find('img').exists()).toBe(false)
    expect(w.get('[data-testid=version] strong').text()).toBe('<img src=x onerror=alert(1)>')
  })
})
