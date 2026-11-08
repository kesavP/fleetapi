// A layered left-to-right layout: callers on the left, what they call to the right. Pure, so it can be tested.
//
// Layers come from the longest path through the calls, with the edges that close a cycle left out of the ranking (a service
// that calls back is common; it is still drawn, it just does not push anything a layer further). Within a layer, nodes are
// ordered by the average position of their neighbours, a few sweeps each way, so edges cross less.

export const NODE_W = 170
export const NODE_H = 44
const COL_GAP = 150
const ROW_GAP = 22
const PAD = 24

export function layout(nodes, edges) {
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

  const tallest = Math.max(1, ...columns.map((c) => c.length))
  const innerH = tallest * NODE_H + (tallest - 1) * ROW_GAP
  const pos = new Map()
  columns.forEach((c, x) => {
    const h = c.length * NODE_H + (c.length - 1) * ROW_GAP
    c.forEach((id, i) => pos.set(id, { x: PAD + x * (NODE_W + COL_GAP), y: PAD + (innerH - h) / 2 + i * (NODE_H + ROW_GAP) }))
  })
  return {
    positions: pos,
    back,
    width: PAD * 2 + (depth + 1) * NODE_W + depth * COL_GAP,
    height: PAD * 2 + innerH,
  }
}

/** An SVG path from the right side of one node to the left side of another; a call back to an earlier column arcs around. */
export function edgePath(a, b, isBack) {
  const x1 = a.x + NODE_W
  const y1 = a.y + NODE_H / 2
  const x2 = b.x
  const y2 = b.y + NODE_H / 2
  if (isBack || x2 <= x1) {
    const lift = 40 + Math.abs(y2 - y1) * 0.1
    return { d: `M ${x1} ${y1} C ${x1 + 60} ${y1 - lift}, ${x2 - 60} ${y2 - lift}, ${x2} ${y2}`, mx: (x1 + x2) / 2, my: Math.min(y1, y2) - lift * 0.75 }
  }
  const dx = Math.max(40, (x2 - x1) / 2)
  return { d: `M ${x1} ${y1} C ${x1 + dx} ${y1}, ${x2 - dx} ${y2}, ${x2} ${y2}`, mx: (x1 + x2) / 2, my: (y1 + y2) / 2 }
}
