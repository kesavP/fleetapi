import { describe, expect, it } from 'vitest'
import { CARD_W, COLUMN_GAP, ROW_GAP, cardHeight, columnsOf, fitScale, linkPath, placeCards } from './graphLayout'

const n = (id, depth, issues = 0) => ({ id, depth, issues: Array.from({ length: issues }, () => ({})) })

describe('placeCards', () => {
  it('puts the hurt application on the left and each step of dependency one column to the right', () => {
    const { cards } = placeCards([n('b', 1), n('a', 0), n('c', 2)])
    expect(cards.get('a').x).toBeLessThan(cards.get('b').x)
    expect(cards.get('b').x - cards.get('a').x).toBe(CARD_W + COLUMN_GAP)
    expect(cards.get('c').x - cards.get('b').x).toBe(CARD_W + COLUMN_GAP)
  })
  it('makes a card as tall as the issues it lists', () => {
    const { cards } = placeCards([n('a', 0, 0), n('b', 1, 3)])
    expect(cards.get('a').h).toBe(cardHeight(0))
    expect(cards.get('b').h).toBe(cardHeight(3))
    expect(cardHeight(3)).toBeGreaterThan(cardHeight(0))
  })
  it('stacks cards of one column without overlap, and centres a lone card level with a taller column', () => {
    const { cards, height } = placeCards([n('a', 0), n('b', 1, 2), n('c', 1, 2)])
    const b = cards.get('b')
    const c = cards.get('c')
    expect(c.y - (b.y + b.h)).toBe(ROW_GAP)
    const a = cards.get('a')
    expect(a.y + a.h / 2).toBeCloseTo((b.y + c.y + c.h) / 2, 6)
    expect(height).toBeGreaterThanOrEqual(c.y + c.h)
  })
  it('gives the same placement whatever the order of the nodes', () => {
    const one = placeCards([n('a', 0), n('b', 1), n('c', 1)])
    const two = placeCards([n('c', 1), n('b', 1), n('a', 0)])
    expect([...one.cards]).toEqual([...two.cards].sort((x, y) => x[0].localeCompare(y[0])))
  })
  it('copes with no nodes', () => {
    expect(placeCards([])).toMatchObject({ width: 0, height: 0 })
  })
  it('keeps every card inside the width and height it reports', () => {
    const { cards, width, height } = placeCards([n('a', 0, 1), n('b', 1, 4), n('c', 2), n('d', 2, 2)])
    for (const c of cards.values()) {
      expect(c.x + c.w).toBeLessThanOrEqual(width)
      expect(c.y + c.h).toBeLessThanOrEqual(height)
    }
  })
})

describe('linkPath', () => {
  const a = { x: 20, y: 20, w: 200, h: 50 }
  it('runs from the right side of the caller to the left side of the callee', () => {
    expect(linkPath(a, { x: 320, y: 100, w: 200, h: 50 })).toMatch(/^M 220 45 C .* 320 125$/)
  })
  it('arcs over the top for a call that goes back to the same or an earlier column', () => {
    expect(linkPath({ x: 320, y: 100, w: 200, h: 50 }, a)).toMatch(/^M 420 100 C 420 /)
  })
})

describe('columnsOf', () => {
  const e = (from, to) => ({ from, to })
  it('puts a callee to the right of every caller, by the longest chain, not the shortest', () => {
    // a calls b and c, and b calls c: c is two calls from a by way of b, and sits after b
    const col = columnsOf([n('a', 0), n('b', 1), n('c', 1)], [e('a', 'b'), e('a', 'c'), e('b', 'c')])
    expect([col.get('a'), col.get('b'), col.get('c')]).toEqual([0, 1, 2])
  })
  it('puts the calls of a cycle in order, leaving the call that closes it out of the count', () => {
    const col = columnsOf([n('a', 0), n('b', 1), n('c', 2)], [e('a', 'b'), e('b', 'c'), e('c', 'a')])
    expect([col.get('a'), col.get('b'), col.get('c')]).toEqual([0, 1, 2])
  })
  it('falls back on the depth of each node when there are no calls to go by', () => {
    const col = columnsOf([n('a', 0), n('b', 2)], [])
    expect([col.get('a'), col.get('b')]).toEqual([0, 2])
  })
  it('ignores a call to a node that is not shown', () => {
    expect(columnsOf([n('a', 0)], [e('a', 'zzz')]).get('a')).toBe(0)
  })
})

describe('placeCards with calls', () => {
  it('draws no call sideways: every call goes from one column to a later one', () => {
    const nodes = [n('a', 0), n('b', 1), n('c', 1)]
    const edges = [{ from: 'a', to: 'b' }, { from: 'a', to: 'c' }, { from: 'b', to: 'c' }]
    const { cards } = placeCards(nodes, edges)
    for (const x of edges) expect(cards.get(x.to).x).toBeGreaterThan(cards.get(x.from).x)
  })
})

describe('fitScale', () => {
  it('shrinks a wide drawing to the room there is, and never enlarges one', () => {
    expect(fitScale(500, 1000)).toBe(0.6) // would be 0.5: held at the smallest that can be read
    expect(fitScale(800, 1000)).toBe(0.8)
    expect(fitScale(2000, 1000)).toBe(1)
  })
  it('leaves the drawing alone when the room or the width is not known', () => {
    expect([fitScale(0, 1000), fitScale(undefined, 1000), fitScale(800, 0), fitScale(NaN, NaN)]).toEqual([1, 1, 1, 1])
  })
})
