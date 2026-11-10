// A layered layout: callers first, what they call after them, left to right or top to bottom. Pure, so it can be tested.
//
// Layers come from the longest path through the calls, with the edges that close a cycle left out of the ranking (a service
// that calls back is common; it is still drawn, it just does not push anything a layer further). Within a layer, nodes are
// ordered by the average position of their neighbours, a few sweeps each way, so edges cross less.

export const NODE_W = 200
export const NODE_H = 54
// Space between one layer and the next ("layer"), and between neighbours inside a layer ("within"). Left to right needs room for
// the lines between columns; top to bottom stacks wide cards, so it needs less between layers and more between neighbours.
const GAP = { lr: { layer: 140, within: 22 }, tb: { layer: 64, within: 26 } }
const PAD = 24

export const DIRECTIONS = ['lr', 'tb']

export function layout(nodes, edges, { direction = 'lr' } = {}) {
  const dir = direction === 'tb' ? 'tb' : 'lr'
  const ids = nodes.map((n) => n.id)
  const known = new Set(ids)
  const live = edges.filter((e) => known.has(e.from) && known.has(e.to) && e.from !== e.to)

  const out = new Map(ids.map((id) => [id, []]))
  for (const e of live) out.get(e.from).push(e.to)

  // Back edges by depth-first search (callers visited in id order, so the result does not depend on input order).
  const back = new Set()
  const state = new Map() // 1 on the stack, 2 done
  const visit = (id) => {
    state.set(id, 1)
    for (const to of [...out.get(id)].sort()) {
      if (state.get(to) === 1) back.add(`${id}\n${to}`)
      else if (!state.get(to)) visit(to)
    }
    state.set(id, 2)
  }
  for (const id of [...ids].sort()) if (!state.get(id)) visit(id)
  const forward = live.filter((e) => !back.has(`${e.from}\n${e.to}`))

  // Longest path from the roots, over the acyclic edges.
  const layer = new Map(ids.map((id) => [id, 0]))
  const indeg = new Map(ids.map((id) => [id, 0]))
  for (const e of forward) indeg.set(e.to, indeg.get(e.to) + 1)
  const queue = ids.filter((id) => indeg.get(id) === 0).sort()
  const outF = new Map(ids.map((id) => [id, []]))
  for (const e of forward) outF.get(e.from).push(e.to)
  while (queue.length) {
    const id = queue.shift()
    for (const to of outF.get(id)) {
      layer.set(to, Math.max(layer.get(to), layer.get(id) + 1))
      indeg.set(to, indeg.get(to) - 1)
      if (indeg.get(to) === 0) queue.push(to)
    }
  }

  const depth = Math.max(0, ...layer.values())
  const columns = Array.from({ length: depth + 1 }, () => [])
  for (const id of [...ids].sort()) columns[layer.get(id)].push(id)

  const neighbours = new Map(ids.map((id) => [id, []]))
  for (const e of live) {
    neighbours.get(e.from).push(e.to)
    neighbours.get(e.to).push(e.from)
  }
  const index = () => {
    const m = new Map()
    columns.forEach((c) => c.forEach((id, i) => m.set(id, i)))
    return m
  }
  const sweep = (order) => {
    for (const c of order) {
      const pos = index()
      const bary = new Map(
        c.map((id) => {
          const ns = neighbours.get(id).filter((n) => layer.get(n) !== layer.get(id))
          return [id, ns.length ? ns.reduce((a, n) => a + pos.get(n), 0) / ns.length : pos.get(id)]
        }),
      )
      c.sort((a, b) => bary.get(a) - bary.get(b) || a.localeCompare(b))
    }
  }
  for (let i = 0; i < 4; i++) {
    sweep(columns.slice(1))
    sweep(columns.slice(0, -1).reverse())
  }

  // `along` is the way layers run (x when left to right), `across` the way a layer's nodes are stacked.
  const along = dir === 'lr' ? NODE_W : NODE_H
  const across = dir === 'lr' ? NODE_H : NODE_W
  const gap = GAP[dir]
  const tallest = Math.max(1, ...columns.map((c) => c.length))
  const innerAcross = tallest * across + (tallest - 1) * gap.within
  const pos = new Map()
  columns.forEach((c, layerIndex) => {
    const extent = c.length * across + (c.length - 1) * gap.within
    c.forEach((id, i) => {
      const a = PAD + layerIndex * (along + gap.layer) // along the layers
      const b = PAD + (innerAcross - extent) / 2 + i * (across + gap.within) // across them, centred
      pos.set(id, dir === 'lr' ? { x: a, y: b } : { x: b, y: a })
    })
  })
  const alongSize = PAD * 2 + (depth + 1) * along + depth * gap.layer
  const acrossSize = PAD * 2 + innerAcross
  return {
    positions: pos,
    back,
    direction: dir,
    width: dir === 'lr' ? alongSize : acrossSize,
    height: dir === 'lr' ? acrossSize : alongSize,
  }
}

// Where a straight line's label goes: this far before the end (the target's edge), which is in the gap between the last two layers, clear
// of the nodes, however long the line. Never before the line's middle, so a short line is labelled at its middle.
const LABEL_BEFORE_END = 78

function pointOnLine(x1, y1, x2, y2, span) {
  const length = Math.abs(span)
  const t = length === 0 ? 0.5 : Math.min(0.95, Math.max(0.5, 1 - LABEL_BEFORE_END / length))
  return { mx: x1 + (x2 - x1) * t, my: y1 + (y2 - y1) * t }
}

/**
 * An SVG path from one node to another: from the right side to the left (left to right) or the bottom to the top (top to bottom).
 * A call that goes forward is a straight line, which stays readable when many cross; a call back to an earlier layer arcs around so
 * it does not run along the forward ones. `mx`, `my` is a point on the line for a label.
 */
export function edgePath(a, b, isBack, direction = 'lr') {
  if (direction === 'tb') {
    const x1 = a.x + NODE_W / 2
    const y1 = a.y + NODE_H
    const x2 = b.x + NODE_W / 2
    const y2 = b.y
    if (isBack || y2 <= y1) {
      // out of the top of the caller and into the bottom of the callee, bulging to the side
      const bulge = 70 + Math.abs(x2 - x1) * 0.1
      const sx = a.x + NODE_W / 2
      const sy = a.y
      const ex = b.x + NODE_W / 2
      const ey = b.y + NODE_H
      return { d: `M ${sx} ${sy} C ${sx + bulge} ${sy - 40}, ${ex + bulge} ${ey + 40}, ${ex} ${ey}`, mx: Math.max(sx, ex) + bulge * 0.75, my: (sy + ey) / 2 }
    }
    return { d: `M ${x1} ${y1} L ${x2} ${y2}`, ...pointOnLine(x1, y1, x2, y2, y2 - y1) }
  }
  const x1 = a.x + NODE_W
  const y1 = a.y + NODE_H / 2
  const x2 = b.x
  const y2 = b.y + NODE_H / 2
  if (isBack || x2 <= x1) {
    const lift = 40 + Math.abs(y2 - y1) * 0.1
    return { d: `M ${x1} ${y1} C ${x1 + 60} ${y1 - lift}, ${x2 - 60} ${y2 - lift}, ${x2} ${y2}`, mx: (x1 + x2) / 2, my: Math.min(y1, y2) - lift * 0.75 }
  }
  return { d: `M ${x1} ${y1} L ${x2} ${y2}`, ...pointOnLine(x1, y1, x2, y2, x2 - x1) }
}
