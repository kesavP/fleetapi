import { beforeEach, describe, expect, it } from 'vitest'
import { createSession } from './session'

const KEY = 'us-read-key-0123456789abcdef'

function memoryStorage(overrides = {}) {
  const data = new Map()
  return {
    data,
    getItem: (k) => (data.has(k) ? data.get(k) : null),
    setItem: (k, v) => data.set(k, String(v)),
    removeItem: (k) => data.delete(k),
    ...overrides,
  }
}

beforeEach(() => {
  window.sessionStorage.clear()
  window.localStorage.clear()
})

describe('the session', () => {
  it('is empty at first', () => {
    expect(createSession(memoryStorage()).get()).toBeNull()
  })

  it('keeps the project and key, and gives them back', () => {
    const s = createSession(memoryStorage())
    s.set({ project: 'gke', key: KEY })
    expect(s.get()).toEqual({ project: 'gke', key: KEY })
  })

  it('survives a reload: a new session over the same storage sees it', () => {
    const storage = memoryStorage()
    createSession(storage).set({ project: 'gke', key: KEY })
    expect(createSession(storage).get()).toEqual({ project: 'gke', key: KEY })
  })

  it('uses sessionStorage and never localStorage', () => {
    createSession().set({ project: 'gke', key: KEY })
    expect(window.sessionStorage.length).toBe(2)
    expect(window.localStorage.length).toBe(0)
    expect(JSON.stringify(Object.entries(window.localStorage))).not.toContain(KEY)
  })

  it('forgets everything on clear', () => {
    const storage = memoryStorage()
    const s = createSession(storage)
    s.set({ project: 'gke', key: KEY })
    s.clear()
    expect(s.get()).toBeNull()
    expect(storage.data.size).toBe(0)
  })

  it('treats a half-stored session as none', () => {
    const storage = memoryStorage()
    storage.setItem('fleet-ui.project', 'gke')
    expect(createSession(storage).get()).toBeNull()
  })
})

describe('when the browser refuses storage', () => {
  const throwing = () => {
    throw new DOMException('denied', 'SecurityError')
  }

  it('falls back to memory for this page', () => {
    const s = createSession(memoryStorage({ setItem: throwing, getItem: throwing, removeItem: throwing }))
    s.set({ project: 'gke', key: KEY })
    expect(s.get()).toEqual({ project: 'gke', key: KEY })
    s.clear()
    expect(s.get()).toBeNull()
  })

  it('works with no storage at all', () => {
    const s = createSession(null)
    expect(s.get()).toBeNull()
    s.set({ project: 'gke', key: KEY })
    expect(s.get()).toEqual({ project: 'gke', key: KEY })
  })

  it('does not throw when reading fails', () => {
    expect(createSession(memoryStorage({ getItem: throwing })).get()).toBeNull()
  })
})
