import { describe, expect, it, vi } from 'vitest'
import { buildTree, formatValue, frameHue, groupTargets, layoutRows, loadStacks, meanCores, nodeAt, parseType, topFunctions } from './profiles'

// Leaf first, as the API gives them.
const S = (value, ...frames) => ({ frames, value })

describe('groupTargets / parseType', () => {
  it('groups the types by service, sorted', () => {
    const g = groupTargets([
      { service: 'b', type: 'cpu:nanoseconds', last_seen: 't2' },
      { service: 'a', type: 'cpu:nanoseconds' },
      { service: 'a', type: 'alloc_space:bytes' },
      { service: '', type: 'x' },
      null,
    ])
    expect([...g.keys()]).toEqual(['a', 'b'])
    expect(g.get('a').map((t) => t.type)).toEqual(['alloc_space:bytes', 'cpu:nanoseconds'])
    expect(g.get('b')[0].lastSeen).toBe('t2')
    expect(groupTargets(null).size).toBe(0)
  })
  it('splits a type into its name and unit', () => {
    expect(parseType('cpu:nanoseconds')).toEqual({ name: 'cpu', unit: 'nanoseconds' })
    expect(parseType('inuse_objects:count')).toEqual({ name: 'inuse_objects', unit: 'count' })
    expect(parseType('wall')).toEqual({ name: 'wall', unit: '' })
  })
})

describe('formatValue / meanCores', () => {
  it('shows times as durations, bytes as sizes, the rest as counts', () => {
    expect(formatValue(1_500_000_000, 'nanoseconds')).toBe('1.5 s')
    expect(formatValue(2500, 'microseconds')).toBe('2.5 ms')
    expect(formatValue(64 * 1024 * 1024, 'bytes')).toBe('64 MiB')
    expect(formatValue(12345, 'count')).toBe('12,345')
    expect(formatValue(7, 'objects')).toBe('7 objects')
    expect(formatValue(7, '')).toBe('7')
  })
  it('turns CPU time into mean cores, and nothing else', () => {
    expect(meanCores(30e9, 'cpu:nanoseconds', 60)).toBeCloseTo(0.5, 9)
    expect(meanCores(5, 'alloc_space:bytes', 60)).toBeNull()
    expect(meanCores(5, 'cpu:nanoseconds', 0)).toBeNull()
  })
})

describe('loadStacks', () => {
  it('asks for the service and type over the window, and drops empty or zero stacks', async () => {
    const get = vi.fn(async () => [S(5, 'a', 'main'), S(0, 'b'), { frames: [], value: 3 }, null, S(2, 'c')])
    const out = await loadStacks({ get }, { service: 's', type: 'cpu:nanoseconds' }, { seconds: 600, limit: 500, now: () => 1_700_000_000_000 })
    expect(get).toHaveBeenCalledWith('profiles/top', { service: 's', type: 'cpu:nanoseconds', from: 1_700_000_000 - 600, to: 1_700_000_000, limit: 500 })
    expect(out.map((s) => s.value)).toEqual([5, 2])
    expect(await loadStacks({ get: async () => null }, { service: 's', type: 't' }, { seconds: 60 })).toEqual([])
  })
})

describe('buildTree', () => {
  const stacks = [S(6, 'parse', 'handle', 'main'), S(3, 'send', 'handle', 'main'), S(1, 'gc', 'main')]
  const t = buildTree(stacks)

  it('is rooted at the callers, merges common prefixes, and adds up', () => {
    expect(t.value).toBe(10)
    expect(t.children.map((c) => c.name)).toEqual(['main'])
    const main = t.children[0]
    expect(main).toMatchObject({ value: 10, self: 0 })
    expect(main.children.map((c) => [c.name, c.value])).toEqual([['handle', 9], ['gc', 1]])
    const handle = main.children[0]
    expect(handle.children.map((c) => [c.name, c.value, c.self])).toEqual([['parse', 6, 6], ['send', 3, 3]])
  })

  it('counts self where a stack ends, even in the middle of the tree', () => {
    const n = buildTree([S(4, 'handle', 'main'), S(6, 'parse', 'handle', 'main')])
    const handle = n.children[0].children[0]
    expect(handle).toMatchObject({ value: 10, self: 4 })
  })

  it('does not depend on the order the stacks came in', () => {
    const a = buildTree([S(1, 'x', 'r'), S(1, 'y', 'r')])
    const b = buildTree([S(1, 'y', 'r'), S(1, 'x', 'r')])
    expect(a.children[0].children.map((c) => c.name)).toEqual(['x', 'y'])
    expect(b.children[0].children.map((c) => c.name)).toEqual(['x', 'y'])
  })

  it('is empty for nothing and ignores non-positive values', () => {
    expect(buildTree([]).value).toBe(0)
    expect(buildTree([S(-1, 'a'), S(0, 'b')]).children).toEqual([])
  })
})

describe('nodeAt', () => {
  const t = buildTree([S(1, 'c', 'b', 'a')])
  it('follows a path of names, and falls back to where it can get to', () => {
    expect(nodeAt(t, ['a', 'b']).node.name).toBe('b')
    expect(nodeAt(t, ['a', 'zzz', 'c'])).toMatchObject({ path: ['a'] })
    expect(nodeAt(t, []).node).toBe(t)
  })
})

describe('layoutRows', () => {
  it('places boxes as shares of the view, children inside their parent', () => {
    const t = buildTree([S(6, 'parse', 'handle', 'main'), S(3, 'send', 'handle', 'main'), S(1, 'gc', 'main')])
    const { rows } = layoutRows(t)
    expect(rows.map((r) => r.nodes.map((n) => n.name))).toEqual([['all'], ['main'], ['handle', 'gc'], ['parse', 'send']])
    const byName = Object.fromEntries(rows.flatMap((r) => r.nodes).map((n) => [n.name, n]))
    expect(byName.main).toMatchObject({ left: 0, width: 100 })
    expect(byName.handle).toMatchObject({ left: 0, width: 90 })
    expect(byName.gc.left).toBeCloseTo(90, 9)
    expect(byName.send.left).toBeCloseTo(60, 9)
    expect(byName.send.width).toBeCloseTo(30, 9)
  })

  it('zooms: a focus node becomes the whole width, with its path kept', () => {
    const t = buildTree([S(6, 'parse', 'handle', 'main'), S(3, 'send', 'handle', 'main'), S(1, 'gc', 'main')])
    const { node, path } = nodeAt(t, ['main', 'handle'])
    const { rows } = layoutRows(node, path)
    expect(rows[0].nodes[0]).toMatchObject({ name: 'handle', left: 0, width: 100, path: ['main', 'handle'] })
    expect(rows[1].nodes.map((n) => [n.name, Math.round(n.width)])).toEqual([['parse', 67], ['send', 33]])
    expect(rows[1].nodes[0].path).toEqual(['main', 'handle', 'parse'])
  })

  it('folds away thin boxes and leaves the others where they were', () => {
    const t = buildTree([S(990, 'big', 'r'), S(1, 'thin', 'r'), S(9, 'mid', 'r')])
    const { rows, minFraction } = layoutRows(t, [], { minFraction: 0.005 })
    const names = rows.flatMap((r) => r.nodes.map((n) => n.name))
    expect(names).toContain('big')
    expect(names).toContain('mid')
    expect(names).not.toContain('thin')
    expect(minFraction).toBe(0.005)
    const mid = rows[2].nodes.find((n) => n.name === 'mid')
    expect(mid.left).toBeCloseTo(99, 9) // right after big (99%); the thin box would have come after mid
  })

  it('raises the threshold until it draws no more than the limit', () => {
    const stacks = Array.from({ length: 300 }, (_, i) => S(1 + (i % 7), `f${i}`, 'r'))
    const { drawn, minFraction } = layoutRows(buildTree(stacks), [], { minFraction: 0.0001, maxNodes: 50 })
    expect(drawn).toBeLessThanOrEqual(50)
    expect(minFraction).toBeGreaterThan(0.0001)
  })

  it('draws nothing for an empty view', () => {
    expect(layoutRows(buildTree([])).rows).toEqual([])
  })
})

describe('topFunctions', () => {
  const stacks = [S(6, 'parse', 'handle', 'main'), S(3, 'send', 'handle', 'main'), S(1, 'gc', 'main')]
  it('separates self from total', () => {
    const { functions, all, count } = topFunctions(stacks)
    expect(all).toBe(10)
    expect(count).toBe(5)
    expect(functions.map((f) => [f.name, f.self, f.total])).toEqual([
      ['parse', 6, 6],
      ['send', 3, 3],
      ['gc', 1, 1],
      ['main', 0, 10],
      ['handle', 0, 9],
    ])
  })
  it('counts a recursive function once per stack, and its self only where the time was spent', () => {
    const { functions } = topFunctions([S(5, 'f', 'f', 'f', 'main')])
    const f = functions.find((x) => x.name === 'f')
    expect(f).toMatchObject({ self: 5, total: 5 }) // not 15
  })
  it('keeps only the n hottest', () => {
    expect(topFunctions(stacks, 2).functions).toHaveLength(2)
  })
})

describe('frameHue', () => {
  it('is stable and stays in the warm range', () => {
    expect(frameHue('main')).toBe(frameHue('main'))
    for (const n of ['a', 'main.handle', 'runtime.mallocgc', '']) {
      const h = frameHue(n)
      expect(h).toBeGreaterThanOrEqual(8)
      expect(h).toBeLessThan(56)
    }
  })
})
