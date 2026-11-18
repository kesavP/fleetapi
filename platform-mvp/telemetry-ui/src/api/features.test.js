import { describe, expect, it, vi } from 'vitest'
import { createFeatureStore, loadFeatures } from './features.js'

describe('loadFeatures', () => {
  it('reads which optional features the hub has switched on, as true or false and nothing else', async () => {
    const client = { get: vi.fn(async () => ({ costs: true })) }
    expect(await loadFeatures(client)).toEqual({ costs: true })
    expect(client.get).toHaveBeenCalledWith('features')
    expect(await loadFeatures({ get: async () => ({ costs: false }) })).toEqual({ costs: false })
  })
  it('takes anything that is not exactly true as off, so a feature is never shown on a guess', async () => {
    for (const v of [{ costs: 'true' }, { costs: 1 }, { costs: null }, {}, null, undefined, 'x', 5, [], [true]]) {
      expect(await loadFeatures({ get: async () => v })).toEqual({ costs: false })
    }
  })
  it('lets a failure through, for the store to turn into "off"', async () => {
    await expect(loadFeatures({ get: async () => { throw new Error('not found') } })).rejects.toThrow('not found')
  })
})

describe('createFeatureStore', () => {
  const sessionOf = (initial) => {
    let value = initial
    const listeners = new Set()
    return {
      get: () => value,
      subscribe: (fn) => (listeners.add(fn), () => listeners.delete(fn)),
      set: (v) => { value = v; listeners.forEach((f) => f()) },
    }
  }
  const tick = () => new Promise((r) => setTimeout(r, 0))

  it('asks the hub when it is connected, and says costs are on when the hub does', async () => {
    const load = vi.fn(async () => ({ costs: true }))
    const store = createFeatureStore({ client: {}, session: sessionOf({ project: 'us', key: 'k' }), load })
    expect(store.state.costs).toBe(false) // until it has heard
    await tick()
    expect(store.state.costs).toBe(true)
    expect(store.state.loaded).toBe(true)
    expect(load).toHaveBeenCalledTimes(1)
  })
  it('asks nothing while it is not connected, and everything is off', async () => {
    const load = vi.fn(async () => ({ costs: true }))
    const store = createFeatureStore({ client: {}, session: sessionOf(null), load })
    await tick()
    expect(load).not.toHaveBeenCalled()
    expect(store.state).toMatchObject({ costs: false, loaded: false })
  })
  it('asks again when the project is changed, and turns everything off when it is disconnected', async () => {
    const session = sessionOf(null)
    const load = vi.fn(async () => ({ costs: true }))
    const store = createFeatureStore({ client: {}, session, load })
    session.set({ project: 'us', key: 'k' })
    await tick()
    expect(store.state.costs).toBe(true)
    session.set(null)
    await tick()
    expect(store.state).toMatchObject({ costs: false, loaded: false })
    expect(load).toHaveBeenCalledTimes(1)
    session.set({ project: 'eu', key: 'k2' })
    await tick()
    expect(load).toHaveBeenCalledTimes(2)
  })
  it('says off when the hub cannot say, as it does not have the feature or cannot be reached', async () => {
    const store = createFeatureStore({ client: {}, session: sessionOf({ project: 'us', key: 'k' }), load: async () => { throw new Error('Not found.') } })
    await tick()
    expect(store.state).toMatchObject({ costs: false, loaded: true })
  })
  it('keeps the answer for the project it is connected to now, when an older question comes back last', async () => {
    const session = sessionOf({ project: 'us', key: 'k' })
    const resolvers = []
    const load = vi.fn(() => new Promise((resolve) => resolvers.push(resolve)))
    const store = createFeatureStore({ client: {}, session, load })
    session.set({ project: 'eu', key: 'k2' })
    resolvers[1]({ costs: false }) // eu answers first
    await tick()
    resolvers[0]({ costs: true }) // us answers late
    await tick()
    expect(store.state.costs).toBe(false)
  })
  it('can be asked again', async () => {
    const load = vi.fn().mockResolvedValueOnce({ costs: false }).mockResolvedValueOnce({ costs: true })
    const store = createFeatureStore({ client: {}, session: sessionOf({ project: 'us', key: 'k' }), load })
    await tick()
    expect(store.state.costs).toBe(false)
    await store.refresh()
    expect(store.state.costs).toBe(true)
  })
})
