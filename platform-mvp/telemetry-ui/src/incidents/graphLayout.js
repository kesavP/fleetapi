// Where to draw the affected applications of an incident: one column per number of calls from the hurt application (the hurt one on
// the left, what it depends on to the right), each card as tall as the issues it lists. Pure.

export const CARD_W = 250
export const COLUMN_GAP = 90
export const ROW_GAP = 28
const PAD = 20
const HEAD = 34 // the name
const ISSUE = 18 // one line of issues
const FOOT = 12

export const cardHeight = (issueCount) => HEAD + (issueCount ? 22 + issueCount * ISSUE : 0) + FOOT

/**
 * The column of each node: the longest chain of calls from a node nothing calls, so a callee is always to the right of every caller. A call
 * that closes a cycle is left out of the count (it is still drawn). Without calls, the node's own `depth`.
 */
export function columnsOf(nodes, edges = []) {
  const ids = nodes.map((n) => n.id).sort()
  const known = new Set(ids)
  const out = new Map(ids.map((id) => [id, []]))
  for (const e of edges) if (known.has(e.from) && known.has(e.to) && e.from !== e.to) out.get(e.from).push(e.to)
  const back = new Set()
  const state = new Map()
  const visit = (id) => {
    state.set(id, 1)
    for (const to of [...out.get(id)].sort()) {
      if (state.get(to) === 1) back.add(`${id}\n${to}`)
      else if (!state.get(to)) visit(to)
    }
    state.set(id, 2)
  }
  // the hurt application (depth 0) first, so that it is the root of the walk and a cycle through it is closed at its far end
  for (const id of [...ids].sort((a, b) => (nodes.find((n) => n.id === a).depth || 0) - (nodes.find((n) => n.id === b).depth || 0) || a.localeCompare(b))) if (!state.get(id)) visit(id)
  const col = new Map(ids.map((id) => [id, 0]))
  const forward = edges.filter((e) => known.has(e.from) && known.has(e.to) && e.from !== e.to && !back.has(`${e.from}\n${e.to}`))
  const indeg = new Map(ids.map((id) => [id, 0]))
  for (const e of forward) indeg.set(e.to, indeg.get(e.to) + 1)
  const queue = ids.filter((id) => indeg.get(id) === 0)
  while (queue.length) {
    const id = queue.shift()
    for (const e of forward.filter((x) => x.from === id)) {
      col.set(e.to, Math.max(col.get(e.to), col.get(id) + 1))
      indeg.set(e.to, indeg.get(e.to) - 1)
      if (indeg.get(e.to) === 0) queue.push(e.to)
    }
  }
  return edges.length ? col : new Map(nodes.map((n) => [n.id, Math.max(0, n.depth || 0)]))
}

/**
 * -> {cards: Map(id -> {x, y, w, h}), width, height}. Nodes are {id, depth, issues[]}; `edges` ([{from, to}]) decide the columns. Cards in a column are stacked in id order and the
 * columns are centred on one another, so a single dependency sits level with the middle of the column before it.
 */
export function placeCards(nodes, edges = []) {
  const columns = new Map()
  const at = columnsOf(nodes, edges)
  for (const n of [...nodes].sort((a, b) => a.id.localeCompare(b.id))) {
    const d = at.get(n.id)
    if (!columns.has(d)) columns.set(d, [])
    columns.get(d).push(n)
  }
  const heights = new Map([...columns].map(([d, list]) => [d, list.reduce((s, n) => s + cardHeight((n.issues || []).length), 0) + ROW_GAP * (list.length - 1)]))
  const tallest = Math.max(0, ...heights.values())
  const cards = new Map()
  for (const [d, list] of [...columns].sort((a, b) => a[0] - b[0])) {
    let y = PAD + (tallest - heights.get(d)) / 2
    for (const n of list) {
      const h = cardHeight((n.issues || []).length)
      cards.set(n.id, { x: PAD + d * (CARD_W + COLUMN_GAP), y, w: CARD_W, h })
      y += h + ROW_GAP
    }
  }
  const last = Math.max(-1, ...columns.keys())
  return { cards, width: last < 0 ? 0 : PAD * 2 + (last + 1) * CARD_W + last * COLUMN_GAP, height: nodes.length ? tallest + PAD * 2 : 0 }
}

/** An SVG path from the right side of one card to the left side of the next (or, for a call that goes back, around the top), with the point for an arrow head's direction. */
export function linkPath(a, b) {
  const x1 = a.x + a.w
  const y1 = a.y + a.h / 2
  const x2 = b.x
  const y2 = b.y + b.h / 2
  if (x2 <= x1) {
    const top = Math.min(a.y, b.y) - 14
    return `M ${a.x + a.w / 2} ${a.y} C ${a.x + a.w / 2} ${top}, ${b.x + b.w / 2} ${top}, ${b.x + b.w / 2} ${b.y}`
  }
  const mid = (x1 + x2) / 2
  return `M ${x1} ${y1} C ${mid} ${y1}, ${mid} ${y2}, ${x2} ${y2}`
}

/** How far to shrink a drawing `width` wide to fit `available` pixels: never above 1, and never so small that the text cannot be read. */
export function fitScale(available, width, min = 0.6) {
  if (!(available > 0) || !(width > 0)) return 1
  return Math.min(1, Math.max(min, available / width))
}
