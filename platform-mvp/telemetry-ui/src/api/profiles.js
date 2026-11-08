import { formatDuration } from './serviceStats'
import { formatBytes } from '../format'
import { windowRange } from './search'

// Reading profiles: which service/type pairs exist, the stacks of one pair over a window, and the tree, the flame layout and
// the table of hottest functions made from them. Everything but the two calls is pure.
//
// The API gives up to `limit` stacks (at most 5000), the heaviest first, each as frames with the LEAF FIRST and a value in the
// profile's unit. When the limit is reached the lighter stacks are missing, so every total here is of the stacks returned and
// the views say when that is not all of them.

export const STACK_LIMITS = [500, 2000, 5000]
export const MAX_NODES = 2500 // what the flame graph draws at most; narrower frames are folded away
export const MIN_FRACTION = 0.001 // a frame narrower than this share of the view is not drawn, to start with

/** {service -> [{type, lastSeen}]} from the `profiles` answer, with the types in order. */
export function groupTargets(rows) {
  const map = new Map()
  for (const r of Array.isArray(rows) ? rows : []) {
    if (!r || !r.service || !r.type) continue
    if (!map.has(r.service)) map.set(r.service, [])
    map.get(r.service).push({ type: r.type, lastSeen: r.last_seen })
  }
  for (const types of map.values()) types.sort((a, b) => a.type.localeCompare(b.type))
  return new Map([...map].sort(([a], [b]) => a.localeCompare(b)))
}

/** "cpu:nanoseconds" -> {name: "cpu", unit: "nanoseconds"}; a type with no unit has the empty one. */
export function parseType(type) {
  const i = String(type || '').indexOf(':')
  return i < 0 ? { name: String(type || ''), unit: '' } : { name: type.slice(0, i), unit: type.slice(i + 1) }
}

const UNIT_NS = { nanoseconds: 1, microseconds: 1e3, milliseconds: 1e6, seconds: 1e9 }

/** A value in the profile's unit, for people: times as durations, bytes as sizes, anything else as a count. */
export function formatValue(v, unit) {
  const n = Number(v) || 0
  if (UNIT_NS[unit]) return formatDuration(n * UNIT_NS[unit])
  if (unit === 'bytes') return formatBytes(n)
  const text = n.toLocaleString('en-US')
  return unit && unit !== 'count' ? `${text} ${unit}` : text
}

/** Mean CPU cores a CPU profile's total represents over the window; null for any other profile. */
export function meanCores(total, type, seconds) {
  const { name, unit } = parseType(type)
  if (name !== 'cpu' || !UNIT_NS[unit] || !(seconds > 0)) return null
  return (total * UNIT_NS[unit]) / 1e9 / seconds
}

export async function loadTargets(client) {
  return groupTargets(await client.get('profiles'))
}

export async function loadStacks(client, { service, type }, { seconds, limit = 2000, now } = {}) {
  const rows = await client.get('profiles/top', { service, type, ...windowRange(seconds, now), limit })
  return (Array.isArray(rows) ? rows : []).filter((s) => s && Array.isArray(s.frames) && s.frames.length && Number(s.value) > 0)
}

/**
 * The stacks as a tree, root first: {name, value, self, children}. A node's value is everything that passed through it, its
 * self is what ended in it, and children with the same name under one parent are one node (that is what makes it a flame
 * graph). Children are heaviest first, ties by name, so the picture does not depend on the order the API answered in.
 */
export function buildTree(stacks) {
  const root = { name: 'all', value: 0, self: 0, kids: new Map() }
  for (const s of stacks) {
    const v = Number(s.value)
    if (!(v > 0)) continue
    root.value += v
    let node = root
    for (let i = s.frames.length - 1; i >= 0; i--) {
      const name = String(s.frames[i])
      let child = node.kids.get(name)
      if (!child) node.kids.set(name, (child = { name, value: 0, self: 0, kids: new Map() }))
      child.value += v
      node = child
    }
    node.self += v
  }
  const finish = (n) => {
    n.children = [...n.kids.values()].sort((a, b) => b.value - a.value || a.name.localeCompare(b.name))
    delete n.kids
    n.children.forEach(finish)
    return n
  }
  return finish(root)
}

/** The node reached by following child names from the root; the root itself for an empty path or one that no longer exists. */
export function nodeAt(root, path) {
  let node = root
  const walked = []
  for (const name of path) {
    const next = node.children.find((c) => c.name === name)
    if (!next) break
    node = next
    walked.push(name)
  }
  return { node, path: walked }
}

/**
 * Rows of boxes for a view rooted at `focus`: [{depth, nodes: [{name, value, self, left, width, path}]}], where left and
 * width are percentages of the focus node's value. A box narrower than `minFraction` of the view is not drawn, and if that
 * leaves more than `maxNodes`, the threshold doubles until it does not. Returns the rows and the threshold used.
 */
export function layoutRows(focus, focusPath = [], { minFraction = MIN_FRACTION, maxNodes = MAX_NODES } = {}) {
  const total = focus.value
  if (!(total > 0)) return { rows: [], minFraction, drawn: 0 }
  for (let min = minFraction; ; min *= 2) {
    const rows = []
    let drawn = 0
    const walk = (node, left, depth, path) => {
      if (!rows[depth]) rows[depth] = { depth, nodes: [] }
      rows[depth].nodes.push({ name: node.name, value: node.value, self: node.self, left, width: (node.value / total) * 100, path })
      drawn++
      // Children are heaviest first, so the ones folded away (the narrowest) come after every one that is drawn: stopping at
      // the first thin one leaves the others exactly where they would have been.
      let at = left
      for (const c of node.children) {
        if (c.value / total < min) break
        walk(c, at, depth + 1, [...path, c.name])
        at += (c.value / total) * 100
      }
    }
    walk(focus, 0, 0, focusPath)
    if (drawn <= maxNodes || min >= 1) return { rows, minFraction: min, drawn }
  }
}

/** The hottest functions: `self` is the value that ended in the function, `total` the value of every stack it appears in (once per stack, so recursion is not counted twice). */
export function topFunctions(stacks, n = 25) {
  const by = new Map()
  let all = 0
  for (const s of stacks) {
    const v = Number(s.value)
    if (!(v > 0)) continue
    all += v
    const seen = new Set()
    s.frames.forEach((f, i) => {
      const name = String(f)
      if (!by.has(name)) by.set(name, { name, self: 0, total: 0 })
      const e = by.get(name)
      if (i === 0) e.self += v // leaf first: frame 0 is where the time was spent
      if (!seen.has(name)) {
        seen.add(name)
        e.total += v
      }
    })
  }
  const list = [...by.values()].sort((a, b) => b.self - a.self || b.total - a.total || a.name.localeCompare(b.name))
  return { functions: list.slice(0, n), all, count: list.length }
}

/** A stable colour for a frame name, in the warm range flame graphs use. */
export function frameHue(name) {
  let h = 0
  for (let i = 0; i < name.length; i++) h = (h * 31 + name.charCodeAt(i)) >>> 0
  return 8 + (h % 48)
}
