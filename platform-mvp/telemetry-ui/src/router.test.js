import { describe, expect, it } from 'vitest'
import { createMemoryHistory } from 'vue-router'
import { createAppRouter } from './router'

const sessionOf = (v) => ({ get: () => v })
async function go(path, session) {
  const router = createAppRouter(session, createMemoryHistory())
  await router.push(path)
  return router.currentRoute.value
}

describe('the costs route', () => {
  const ui = (costs) => ({ ready: Promise.resolve(), state: { costs, loaded: true } })
  const goUi = async (path, u) => {
    const router = createAppRouter(sessionOf({ project: 'p', key: 'k' }), createMemoryHistory(), u)
    await router.push(path)
    return router.currentRoute.value
  }
  it('is there where this UI offers it, and an unknown address where it does not', async () => {
    expect((await goUi('/costs', ui(true))).name).toBe('costs')
    expect((await goUi('/costs', ui(false))).fullPath).toBe('/applications')
  })
  it('waits for the file to be read before deciding', async () => {
    let done
    const u = { state: { costs: false, loaded: false }, ready: new Promise((r) => (done = r)) }
    const router = createAppRouter(sessionOf({ project: 'p', key: 'k' }), createMemoryHistory(), u)
    const pushed = router.push('/costs')
    u.state.costs = true
    done()
    await pushed
    expect(router.currentRoute.value.name).toBe('costs')
  })
})

describe('routes', () => {
  it('sends "/" to the applications when connected, and to the connection page when not', async () => {
    expect((await go('/', sessionOf({ project: 'gke', key: 'k' }))).fullPath).toBe('/applications')
    expect((await go('/', sessionOf(null))).fullPath).toBe('/configuration/connection')
  })

  it('sends an unknown address the same way', async () => {
    expect((await go('/no/such/page', sessionOf({ project: 'gke', key: 'k' }))).fullPath).toBe('/applications')
    expect((await go('/no/such/page', sessionOf(null))).fullPath).toBe('/configuration/connection')
  })

  it('still understands the old /services addresses, now applications', async () => {
    const s = sessionOf({ project: 'gke', key: 'k' })
    expect((await go('/services', s)).fullPath).toBe('/applications')
    const detail = await go('/services/%2Fk8s%2Fshop%2Ffrontend', s)
    expect(detail.name).toBe('application')
    expect(detail.params.name).toBe('/k8s/shop/frontend')
    expect(detail.fullPath).toBe('/applications/%2Fk8s%2Fshop%2Ffrontend')
  })

  it('has a page for each entry of the menu, a detail page for an application and a trace, and the connection', async () => {
    const s = sessionOf(null)
    for (const [path, name] of [
      ['/applications', 'applications'], ['/recorded', 'recorded-incidents'], ['/configuration/rules', 'rules'], ['/recorded/vcxxe33p', 'recorded-incident'], ['/costs', 'costs'], ['/map', 'map'], ['/traces', 'traces'], ['/logs', 'logs'], ['/profiles', 'profiles'], ['/alerts', 'alerts'], ['/configuration/connection', 'connection'], ['/configuration/clickhouse', 'clickhouse'], ['/configuration/incidents', 'incident-rules'],
      [`/traces/${'a'.repeat(32)}`, 'trace'], ['/applications/x', 'application'],
    ]) {
      expect((await go(path, s)).name).toBe(name)
    }
  })

  it('keeps the old /connection address and the bare /configuration working: both open the connection tab', async () => {
    const s = sessionOf(null)
    expect((await go('/connection', s)).fullPath).toBe('/configuration/connection')
    expect((await go('/configuration', s)).fullPath).toBe('/configuration/connection')
  })

  it('follows the connection as it changes, without a reload', async () => {
    let value = null
    const router = createAppRouter({ get: () => value }, createMemoryHistory())
    await router.push('/')
    expect(router.currentRoute.value.fullPath).toBe('/configuration/connection')
    value = { project: 'gke', key: 'k' }
    await router.push('/')
    expect(router.currentRoute.value.fullPath).toBe('/applications')
  })
})
