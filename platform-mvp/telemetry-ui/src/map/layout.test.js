import { describe, expect, it } from 'vitest'
import { NODE_H, NODE_W, edgePath, layout } from './layout'

const nodes = (...ids) => ids.map((id) => ({ id }))
const e = (from, to) => ({ from, to })

describe('layout', () => {
  it('puts callers left of what they call, in layers', () => {
    const { positions: p } = layout(nodes('a', 'b', 'c', 'd'), [e('a', 'b'), e('b', 'c'), e('a', 'd')])
    expect(p.get('a').x).toBeLessThan(p.get('b').x)
    expect(p.get('b').x).toBeLessThan(p.get('c').x)
    expect(p.get('d').x).toBe(p.get('b').x) // d is one call from a, like b
  })

  it('ranks by the longest path, so a node called at two depths sits after both', () => {
    const { positions: p } = layout(nodes('a', 'b', 'c'), [e('a', 'b'), e('b', 'c'), e('a', 'c')])
    expect(p.get('c').x).toBeGreaterThan(p.get('b').x)
  })

  it('does not overlap nodes in a column and keeps all of them inside the canvas', () => {
    const ids = ['hub', ...Array.from({ length: 9 }, (_, i) => `leaf${i}`)]
    const { positions: p, width, height } = layout(nodes(...ids), ids.slice(1).map((id) => e('hub', id)))
    const col = ids.slice(1).map((id) => p.get(id)).sort((a, b) => a.y - b.y)
    for (let i = 1; i < col.length; i++) expect(col[i].y - col[i - 1].y).toBeGreaterThanOrEqual(NODE_H)
    for (const { x, y } of p.values()) {
      expect(x).toBeGreaterThanOrEqual(0)
      expect(y).toBeGreaterThanOrEqual(0)
      expect(x + NODE_W).toBeLessThanOrEqual(width)
      expect(y + NODE_H).toBeLessThanOrEqual(height)
    }
  })

  it('survives a cycle: both nodes are placed, the closing edge is a back edge', () => {
    const r = layout(nodes('a', 'b', 'c'), [e('a', 'b'), e('b', 'c'), e('c', 'a')])
    expect(r.positions.size).toBe(3)
    expect(r.back.size).toBe(1)
    expect(r.positions.get('a').x).toBeLessThan(r.positions.get('c').x)
  })

  it('does not loop on a self call, and ignores edges to unknown nodes', () => {
    const r = layout(nodes('a'), [e('a', 'a'), e('a', 'ghost')])
    expect(r.positions.size).toBe(1)
  })

  it('gives the same answer whatever order it is given', () => {
    const edges = [e('a', 'b'), e('a', 'c'), e('b', 'd'), e('c', 'd')]
    const one = layout(nodes('a', 'b', 'c', 'd'), edges)
    const two = layout(nodes('d', 'c', 'b', 'a'), [...edges].reverse())
    expect([...one.positions].sort()).toEqual([...two.positions].sort())
  })

  it('handles nothing', () => {
    const r = layout([], [])
    expect(r.positions.size).toBe(0)
    expect(r.width).toBeGreaterThan(0)
  })
})

describe('edgePath', () => {
  it('runs from the right side of the source to the left side of the target', () => {
    const { d } = edgePath({ x: 0, y: 0 }, { x: 400, y: 100 }, false)
    expect(d.startsWith(`M ${NODE_W} ${NODE_H / 2}`)).toBe(true)
    expect(d.endsWith(`400 ${100 + NODE_H / 2}`)).toBe(true)
  })
  it('arcs a call that goes back', () => {
    const { d, my } = edgePath({ x: 400, y: 0 }, { x: 0, y: 0 }, true)
    expect(d).toContain('C')
    expect(my).toBeLessThan(NODE_H / 2)
  })
})
