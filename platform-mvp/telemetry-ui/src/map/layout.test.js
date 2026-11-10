import { describe, expect, it } from 'vitest'
import { DIRECTIONS, NODE_H, NODE_W, edgePath, layout } from './layout'

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

describe('layout direction', () => {
  const ns = nodes('a', 'b', 'c', 'd', 'e')
  const es = [e('a', 'b'), e('a', 'c'), e('b', 'd'), e('c', 'e')]

  it('offers left to right and top to bottom, and left to right is the default', () => {
    expect(DIRECTIONS).toEqual(['lr', 'tb'])
    expect(layout(ns, es).direction).toBe('lr')
    expect(layout(ns, es, { direction: 'nonsense' }).direction).toBe('lr')
  })

  it('top to bottom runs the layers down the page, callers above what they call', () => {
    const { positions: p, width, height, direction } = layout(ns, es, { direction: 'tb' })
    expect(direction).toBe('tb')
    expect(p.get('a').y).toBeLessThan(p.get('b').y)
    expect(p.get('b').y).toBeLessThan(p.get('d').y)
    expect(p.get('b').y).toBe(p.get('c').y) // same layer, same row
    expect(p.get('b').x).not.toBe(p.get('c').x) // side by side
    expect(width).toBeGreaterThan(0)
    expect(height).toBeGreaterThan(width / 4)
  })

  it('does not overlap nodes or leave the canvas in either direction', () => {
    const ids = ['hub', ...Array.from({ length: 7 }, (_, i) => `leaf${i}`)]
    for (const direction of DIRECTIONS) {
      const { positions: p, width, height } = layout(nodes(...ids), ids.slice(1).map((id) => e('hub', id)), { direction })
      const boxes = [...p.values()]
      for (const { x, y } of boxes) {
        expect(x).toBeGreaterThanOrEqual(0)
        expect(y).toBeGreaterThanOrEqual(0)
        expect(x + NODE_W).toBeLessThanOrEqual(width)
        expect(y + NODE_H).toBeLessThanOrEqual(height)
      }
      for (let i = 0; i < boxes.length; i++) {
        for (let j = i + 1; j < boxes.length; j++) {
          const a = boxes[i]
          const b = boxes[j]
          const apart = a.x + NODE_W <= b.x || b.x + NODE_W <= a.x || a.y + NODE_H <= b.y || b.y + NODE_H <= a.y
          expect(apart).toBe(true)
        }
      }
    }
  })
})

describe('edgePath', () => {
  it('runs from the right side of the source to the left side of the target', () => {
    const { d } = edgePath({ x: 0, y: 0 }, { x: 400, y: 100 }, false)
    expect(d.startsWith(`M ${NODE_W} ${NODE_H / 2}`)).toBe(true)
    expect(d.endsWith(`400 ${100 + NODE_H / 2}`)).toBe(true)
  })
  it('draws a forward call as a straight line, and a call back as an arc', () => {
    expect(edgePath({ x: 0, y: 0 }, { x: 400, y: 100 }, false).d).toMatch(/^M [\d.]+ [\d.]+ L [\d.]+ [\d.]+$/)
    expect(edgePath({ x: 400, y: 0 }, { x: 0, y: 0 }, true).d).toContain('C')
  })
  it('top to bottom runs from the bottom of the caller to the top of the callee', () => {
    const { d } = edgePath({ x: 0, y: 0 }, { x: 0, y: 200 }, false, 'tb')
    expect(d).toBe(`M ${NODE_W / 2} ${NODE_H} L ${NODE_W / 2} 200`)
    expect(edgePath({ x: 0, y: 200 }, { x: 0, y: 0 }, true, 'tb').d).toContain('C')
  })
  it('puts a straight line\'s label in the gap before its target, clear of the nodes between, however long the line', () => {
    const a = { x: 0, y: 0 }
    const far = { x: 3 * (NODE_W + 140), y: 300 } // three layers on
    const { mx, my } = edgePath(a, far, false)
    expect(mx).toBeGreaterThan(far.x - 100)
    expect(mx).toBeLessThan(far.x) // in the gap, not on the target
    expect(my).toBeGreaterThan(0)
    expect(my).toBeLessThan(300 + NODE_H / 2)
    const short = edgePath({ x: 0, y: 0 }, { x: NODE_W + 40, y: 0 }, false) // a short line: its middle
    expect(short.mx).toBeCloseTo((NODE_W + NODE_W + 40) / 2, 6)
    const down = edgePath({ x: 0, y: 0 }, { x: 0, y: 4 * (NODE_H + 64) }, false, 'tb')
    expect(down.my).toBeGreaterThan(4 * (NODE_H + 64) - 100)
  })
  it('arcs a call that goes back', () => {
    const { d, my } = edgePath({ x: 400, y: 0 }, { x: 0, y: 0 }, true)
    expect(d).toContain('C')
    expect(my).toBeLessThan(NODE_H / 2)
  })
})
