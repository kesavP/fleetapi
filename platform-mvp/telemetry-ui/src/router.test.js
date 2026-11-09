import { describe, expect, it } from 'vitest'
import { createMemoryHistory } from 'vue-router'
import { createAppRouter } from './router'

const sessionOf = (v) => ({ get: () => v })
async function go(path, session) {
  const router = createAppRouter(session, createMemoryHistory())
  await router.push(path)
  return router.currentRoute.value
}

describe('routes', () => {
  it('sends "/" to the applications when connected, and to the connection page when not', async () => {
    expect((await go('/', sessionOf({ project: 'gke', key: 'k' }))).fullPath).toBe('/applications')
    expect((await go('/', sessionOf(null))).fullPath).toBe('/connection')
  })

  it('sends an unknown address the same way', async () => {
    expect((await go('/no/such/page', sessionOf({ project: 'gke', key: 'k' }))).fullPath).toBe('/applications')
    expect((await go('/no/such/page', sessionOf(null))).fullPath).toBe('/connection')
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
      ['/applications', 'applications'], ['/map', 'map'], ['/traces', 'traces'], ['/logs', 'logs'], ['/profiles', 'profiles'], ['/alerts', 'alerts'], ['/connection', 'connection'],
      [`/traces/${'a'.repeat(32)}`, 'trace'], ['/applications/x', 'application'],
    ]) {
      expect((await go(path, s)).name).toBe(name)
    }
  })

  it('follows the connection as it changes, without a reload', async () => {
    let value = null
    const router = createAppRouter({ get: () => value }, createMemoryHistory())
    await router.push('/')
    expect(router.currentRoute.value.fullPath).toBe('/connection')
    value = { project: 'gke', key: 'k' }
    await router.push('/')
    expect(router.currentRoute.value.fullPath).toBe('/applications')
  })
})
