import { describe, expect, it } from 'vitest'
import { BUILT_IN, MAX_CUSTOM, createCategoryStore, normalizeCategory } from './categories'
import { createSession } from './api/session'

function memoryStorage() {
  const m = new Map()
  return { getItem: (k) => (m.has(k) ? m.get(k) : null), setItem: (k, v) => m.set(k, String(v)), removeItem: (k) => m.delete(k), m }
}
const connected = (project = 'gke') => {
  const session = createSession(memoryStorage())
  if (project) session.set({ project, key: 'k' })
  return session
}
const make = (o = {}) => {
  const storage = o.storage || memoryStorage()
  const session = o.session || connected()
  return { store: createCategoryStore({ session, storage }), storage, session }
}

describe('normalizeCategory', () => {
  it('lower-cases, trims and collapses spaces', () => {
    expect(normalizeCategory('  Payments   Tier ')).toBe('payments tier')
    expect(normalizeCategory('a')).toBe('a')
    expect(normalizeCategory('team-1')).toBe('team-1')
  })
  it('refuses what cannot travel in the address or is not a name', () => {
    for (const bad of ['', '   ', 'a,b', '-x', 'x-', 'has/slash', 'x'.repeat(33), 'émile', undefined, null]) expect(normalizeCategory(bad)).toBe('')
    expect(normalizeCategory('x'.repeat(32))).toBe('x'.repeat(32))
  })
})

describe('category store', () => {
  it('has the three built-in categories, and an application is where its namespace puts it until it is moved', () => {
    const { store } = make()
    expect(store.all()).toEqual(BUILT_IN)
    expect(store.categoryFor('/k8s/shop/a', 'application')).toBe('application')
    expect(store.moved('/k8s/shop/a')).toBe(false)
  })

  it('moves an application to a category, and a move overrides the namespace', () => {
    const { store } = make()
    expect(store.move('/k8s/shop/a', 'monitoring', 'application')).toBe(true)
    expect(store.categoryFor('/k8s/shop/a', 'application')).toBe('monitoring')
    expect(store.moved('/k8s/shop/a')).toBe(true)
    expect(store.categoryFor('/k8s/shop/b', 'application')).toBe('application') // nothing else moves
  })

  it('forgets a move back to the category its namespace gives, so the application is "not moved" again', () => {
    const { store } = make()
    store.move('x', 'monitoring', 'application')
    expect(store.move('x', 'application', 'application')).toBe(true)
    expect(store.moved('x')).toBe(false)
  })

  it('refuses a move to a category that does not exist', () => {
    const { store } = make()
    expect(store.move('x', 'nope', 'application')).toBe(false)
    expect(store.moved('x')).toBe(false)
  })

  it('makes a category of its own, once, whatever the case, and moves applications to it', () => {
    const { store } = make()
    expect(store.add('  Payments ')).toBe('payments')
    expect(store.add('payments')).toBe('payments')
    expect(store.all()).toEqual([...BUILT_IN, 'payments'])
    expect(store.move('x', 'payments', 'application')).toBe(true)
    expect(store.categoryFor('x', 'application')).toBe('payments')
  })

  it('does not make a built-in category again, a bad name, or more than the limit', () => {
    const { store } = make()
    expect(store.add('Monitoring')).toBe('monitoring')
    expect(store.all()).toEqual(BUILT_IN)
    expect(store.add('a,b')).toBe('')
    for (let i = 0; i < MAX_CUSTOM; i++) expect(store.add(`team ${i}`)).toBe(`team ${i}`)
    expect(store.add('one too many')).toBe('')
    expect(store.add('team 3')).toBe('team 3') // an existing one is still fine
    expect(store.problem('one too many')).toContain('At most')
  })

  it('says why a name cannot be used', () => {
    const { store } = make()
    expect(store.problem('')).toBe('Give the category a name.')
    expect(store.problem('A,B')).toContain('lowercase letters')
    expect(store.problem('ok name')).toBe('')
  })

  it('keeps the choices for the next visit, per project', () => {
    const storage = memoryStorage()
    const a = make({ storage })
    a.store.add('payments')
    a.store.move('x', 'payments', 'application')
    const again = make({ storage })
    expect(again.store.all()).toContain('payments')
    expect(again.store.categoryFor('x', 'application')).toBe('payments')
    const other = make({ storage, session: connected('dc1') })
    expect(other.store.all()).toEqual(BUILT_IN) // another project has its own
    expect(other.store.categoryFor('x', 'application')).toBe('application')
  })

  it('follows the connection: another project is loaded, and none shows nothing', () => {
    const storage = memoryStorage()
    const session = connected('gke')
    const { store } = make({ storage, session })
    store.move('x', 'monitoring', 'application')
    session.set({ project: 'dc1', key: 'k' })
    expect(store.categoryFor('x', 'application')).toBe('application')
    session.set({ project: 'gke', key: 'k' })
    expect(store.categoryFor('x', 'application')).toBe('monitoring') // and back
    session.clear()
    expect(store.categoryFor('x', 'application')).toBe('application')
  })

  it('drops a move to a category that is gone, and tolerates a damaged record', () => {
    const storage = memoryStorage()
    storage.setItem('fleet-ui.categories.gke', JSON.stringify({ custom: ['ok', 'a,b', 'monitoring', 5], assignments: { x: 'ok', y: 'vanished', z: 7 } }))
    const { store } = make({ storage })
    expect(store.all()).toEqual([...BUILT_IN, 'ok'])
    expect(store.categoryFor('x', 'application')).toBe('ok')
    expect(store.categoryFor('y', 'application')).toBe('application')
    const broken = memoryStorage()
    broken.setItem('fleet-ui.categories.gke', '{nope')
    expect(make({ storage: broken }).store.all()).toEqual(BUILT_IN)
  })

  it('works when storage is refused, for this visit', () => {
    const refusing = { getItem: () => { throw new Error('no') }, setItem: () => { throw new Error('no') } }
    const { store } = make({ storage: refusing })
    store.add('payments')
    store.move('x', 'payments', 'application')
    expect(store.categoryFor('x', 'application')).toBe('payments')
    expect(createCategoryStore({ session: connected(), storage: null }).all()).toEqual(BUILT_IN)
  })

  it('keeps nothing without a project', () => {
    const storage = memoryStorage()
    const { store } = make({ storage, session: connected('') })
    store.add('payments')
    expect(storage.m.size).toBe(0)
  })
})
