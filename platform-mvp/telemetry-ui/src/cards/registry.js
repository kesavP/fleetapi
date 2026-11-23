import { gauges, gaugeMeans, rates } from '../map/load'
import { workloadOf } from '../map/workload'

// Cards for an application's page, as plugins. A card is data: the metrics it reads, and a function that turns them into rows of
// text. It brings no markup and no code to the page, so a card cannot break the page's layout or reach beyond the page's own
// styles; the page draws every card the same way (see components/PluginCards.vue). A plugin lives in its own file, calls defineCard
// and is listed in ./index.js; the page needs no change for it.
//
//   defineCard({
//     id: 'connections',                       // lower-case letters, digits and dashes: it is an element id on the page
//     title: 'Connections',
//     metrics: { connects: { metric: 'container_net_..._total', kind: 'rate' }, ... },
//         kind 'rate'  : [{labels, rate}]  mean per-second rate over the window (a counter)
//         kind 'gauge' : [{labels, value}] the latest value of each series still reporting
//         kind 'mean'  : [{labels, value}] the mean value of each series over the window
//         optional: true : the card stands without it, so a failure to read it is a warning, not the end of the card
//     build: (series, { id, seconds }) => ({ rows: [{ label, value, level? }], note? }) | null
//         `series` has a list for every key of `metrics` (empty when the project has no such metric); `level` is 'bad' or 'caution';
//         null means there is nothing to show for this application.
//   })

const KINDS = {
  rate: (client, metric, start, end) => rates(client, metric, start, end),
  gauge: (client, metric, start, end) => gauges(client, metric, start, end),
  mean: (client, metric, start, end) => gaugeMeans(client, metric, start, end),
}
const LEVELS = new Set([undefined, 'bad', 'caution'])

export function defineCard(card) {
  if (!card || typeof card.id !== 'string' || !/^[a-z][a-z0-9-]*$/.test(card.id)) throw new Error('a card needs an id of lower-case letters, digits and dashes')
  if (typeof card.title !== 'string' || !card.title.trim()) throw new Error(`card ${card.id} needs a title`)
  if (typeof card.build !== 'function') throw new Error(`card ${card.id} needs a build function`)
  for (const [key, m] of Object.entries(card.metrics || {})) {
    if (!m || typeof m.metric !== 'string' || !m.metric) throw new Error(`card ${card.id}: metric ${key} needs a metric name`)
    if (!Object.hasOwn(KINDS, m.kind)) throw new Error(`card ${card.id}: metric ${key} has an unknown kind ${JSON.stringify(m.kind)}`)
  }
  return Object.freeze({ metrics: {}, ...card })
}

/** The series of one application: those whose container id belongs to it. */
export function mine(series, id) {
  return (series || []).filter((s) => {
    const w = workloadOf(s.labels && s.labels.container_id)
    return !!w && w.id === id
  })
}

function checkRows(out) {
  const ok = out && Array.isArray(out.rows) && out.rows.every((r) => r && typeof r.label === 'string' && typeof r.value === 'string' && LEVELS.has(r.level))
  if (!ok || (out.note != null && typeof out.note !== 'string')) throw new Error('its rows are not text')
  return out
}

async function loadCard(card, client, id, { start, end, seconds, present }) {
  const warnings = []
  const entries = Object.entries(card.metrics).filter(([, m]) => present.has(m.metric))
  if (!entries.length) return { warnings }
  const settled = await Promise.allSettled(entries.map(([, m]) => KINDS[m.kind](client, m.metric, start, end)))
  const series = Object.fromEntries(Object.keys(card.metrics).map((k) => [k, []]))
  for (let i = 0; i < entries.length; i++) {
    const [key, m] = entries[i]
    const r = settled[i]
    if (r.status !== 'fulfilled') {
      const why = `${card.title}: ${m.metric}: ${r.reason && r.reason.message ? r.reason.message : 'failed'}`
      if (!m.optional) return { warnings: [why] }
      warnings.push(why)
      continue
    }
    if (r.value.truncated) warnings.push(`${card.title}: ${m.metric} returned more series than one call allows, so its figures may be partial`)
    series[key] = r.value.series
  }
  try {
    const out = card.build(series, { id, seconds })
    if (!out) return { warnings }
    const { rows, note } = checkRows(out)
    return { card: { id: card.id, title: card.title, rows, note: note ?? null }, warnings }
  } catch (e) {
    return { warnings: [`${card.title}: ${e && e.message ? e.message : 'failed'}`] }
  }
}

/**
 * Loads the cards for one application: {cards, warnings}. Every card reads at the same time, only the metrics the project has, and a
 * card that cannot be read or built is left out with a warning while the others stay.
 */
export async function loadCards(client, id, { seconds, now = () => Date.now(), present, cards }) {
  const end = now() / 1000
  const start = end - seconds
  const done = await Promise.all(cards.map((c) => loadCard(c, client, id, { start, end, seconds, present })))
  return { cards: done.filter((d) => d.card).map((d) => d.card), warnings: done.flatMap((d) => d.warnings) }
}
