// @vitest-environment node
import { describe, expect, it } from 'vitest'
import { keyMatches, loadProjects } from './projects.js'

const file = (obj) => () => JSON.stringify(obj)

describe('loadProjects', () => {
  it('takes names and read keys, and nothing else from the file', () => {
    const out = loadProjects('x', file({ projects: [{ name: 'gke', ingestKeys: ['INGEST'], readKeys: ['READ1', 'READ2'] }] }))
    expect(out).toEqual([{ name: 'gke', readKeys: ['READ1', 'READ2'] }])
    expect(JSON.stringify(out)).not.toContain('INGEST')
  })

  it('skips what cannot be used: a bad name, no read key, junk', () => {
    const out = loadProjects('x', file({ projects: [
      { name: 'ok', readKeys: ['k'] },
      { name: 'Bad_Name', readKeys: ['k'] },
      { name: '../etc', readKeys: ['k'] },
      { name: 'nokeys', readKeys: [] },
      { name: 'emptykey', readKeys: [''] },
      { name: 7, readKeys: ['k'] },
      null,
    ] }))
    expect(out.map((p) => p.name)).toEqual(['ok'])
  })

  it('is empty for a file with no projects, and throws for one that is not JSON', () => {
    expect(loadProjects('x', file({}))).toEqual([])
    expect(loadProjects('x', file({ projects: 'nope' }))).toEqual([])
    expect(() => loadProjects('x', () => '{oops')).toThrow()
  })
})

describe('keyMatches', () => {
  const p = { name: 'gke', readKeys: ['key-one-aaaaaaaa', 'key-two-bbbbbbbb'] }
  it('accepts any of the project\'s read keys', () => {
    expect(keyMatches(p, 'key-one-aaaaaaaa')).toBe(true)
    expect(keyMatches(p, 'key-two-bbbbbbbb')).toBe(true)
  })
  it('refuses everything else, including near misses, prefixes, and nothing', () => {
    for (const k of ['key-one-aaaaaaab', 'key-one', 'key-one-aaaaaaaa ', 'KEY-ONE-AAAAAAAA', '', undefined, null]) expect(keyMatches(p, k)).toBe(false)
  })
})
