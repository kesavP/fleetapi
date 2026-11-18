// The cost report the hub makes (GET .../costs?window=N): what the infrastructure costs, what the money is for, and where it is wasted. A feature the hub
// switches on (see features.js): a hub that has not answers 404.
//
// Everything that comes back is checked on the way in. A number that is not a finite number is zero, and "not known" is kept as null and never as zero (a price
// that is missing, requests that are not known): a page that shows a missing price as $0 would be telling a person something false.

export const COST_WINDOWS = [
  { label: 'hour', seconds: 3600 },
  { label: '6 hours', seconds: 6 * 3600 },
  { label: '24 hours', seconds: 24 * 3600 },
  { label: '7 days', seconds: 7 * 24 * 3600 },
]

const isObject = (v) => v !== null && typeof v === 'object' && !Array.isArray(v)
const num = (v) => (typeof v === 'number' && Number.isFinite(v) ? v : 0)
const numOrNull = (v) => (typeof v === 'number' && Number.isFinite(v) ? v : null)
const text = (v) => (typeof v === 'string' ? v : '')
const list = (v) => (Array.isArray(v) ? v : [])

const resource = (r) => {
  const o = isObject(r) ? r : {}
  return { capacity: num(o.capacity), usage: num(o.usage), requests: numOrNull(o.requests) }
}

function parseNode(n) {
  if (!isObject(n) || !text(n.name)) return null
  return {
    name: n.name, machineId: text(n.machine_id), provider: text(n.provider), region: text(n.region), zone: text(n.zone), instanceType: text(n.instance_type),
    lifecycle: text(n.lifecycle), lifecycleAssumed: n.lifecycle_assumed === true, cpu: resource(n.cpu), memory: resource(n.memory), cpuShare: num(n.cpu_share),
    priceMonthly: numOrNull(n.price_monthly), priceSource: text(n.price_source),
    usedMonthly: num(n.used_monthly), overprovisionedMonthly: num(n.overprovisioned_monthly), idleMonthly: num(n.idle_monthly),
  }
}

function parseApp(a) {
  if (!isObject(a) || !text(a.app)) return null
  return {
    app: a.app, label: text(a.label) || a.app, namespace: text(a.namespace), cpu: resource(a.cpu), memory: resource(a.memory),
    usageMonthly: num(a.usage_monthly), allocationMonthly: numOrNull(a.allocation_monthly), overprovisioningMonthly: numOrNull(a.overprovisioning_monthly),
  }
}

const KINDS = ['saving', 'info', 'warning']

function parseFinding(f) {
  if (!isObject(f) || !text(f.id) || !text(f.title)) return null
  return {
    id: f.id, kind: KINDS.includes(f.kind) ? f.kind : 'info', title: f.title, detail: text(f.detail), monthlySaving: numOrNull(f.monthly_saving),
    items: list(f.items).filter((i) => isObject(i) && text(i.name)).map((i) => ({ name: i.name, monthly: num(i.monthly) })),
  }
}

/** The report as the page uses it. */
export function parseReport(d) {
  const t = isObject(d && d.totals) ? d.totals : {}
  return {
    currency: text(d && d.currency) || 'USD',
    windowSeconds: num(d && d.window_seconds),
    generatedAt: text(d && d.generated_at),
    hasRequests: !!d && d.has_requests === true,
    nodes: list(d && d.nodes).map(parseNode).filter(Boolean),
    applications: list(d && d.applications).map(parseApp).filter(Boolean),
    totals: {
      priceMonthly: num(t.price_monthly), usedMonthly: num(t.used_monthly), overprovisionedMonthly: num(t.overprovisioned_monthly), idleMonthly: num(t.idle_monthly),
      unattributedMonthly: num(t.unattributed_monthly), nodesWithoutPrice: num(t.nodes_without_price), appUsageMonthly: num(t.app_usage_monthly),
      appAllocationMonthly: numOrNull(t.app_allocation_monthly), appOverprovisioningMonthly: numOrNull(t.app_overprovisioning_monthly),
    },
    analysis: list(d && d.analysis).map(parseFinding).filter(Boolean),
    warnings: list(d && d.warnings).filter((w) => typeof w === 'string' && w.trim() !== ''),
  }
}

/** The report of the last `seconds` seconds. Lets a failure through, as the client words it (a hub that has not switched the costs on says so). */
export async function loadCosts(client, seconds) {
  const data = await client.get('costs', { window: seconds })
  if (!isObject(data)) throw new Error('The service answered with something that is not a cost report.')
  return parseReport(data)
}

/** Dollars: cents below a hundred, whole dollars above, a thousands separator; a dash for what is not a number. */
export function money(v) {
  if (typeof v !== 'number' || !Number.isFinite(v)) return '—'
  const n = Math.abs(v) < 0.005 ? 0 : v
  if (Math.abs(n) >= 100) return '$' + Math.round(n).toLocaleString('en-US')
  return '$' + n.toFixed(2)
}

const trim = (n) => String(Math.round(n * 10) / 10)

export const vcpuText = (cores) => `${trim(cores)} vCPU`

export function memoryText(bytes) {
  const gib = bytes / 1024 ** 3
  return gib >= 1 ? `${trim(gib)} GiB` : `${trim(bytes / 1024 ** 2)} MiB`
}

/** A share of a whole, as a whole percent; a dash when there is no whole. */
export const shareText = (part, whole) => (whole > 0 ? `${Math.round((100 * part) / whole)}%` : '—')

/** How wide a bar is: use as a share of capacity, from 0 to 100. */
export function barPercent(value, capacity) {
  if (!(capacity > 0) || !Number.isFinite(value)) return 0
  return Math.min(100, Math.max(0, (100 * value) / capacity))
}

export const sourceText = (s) => ({ override: 'your price', 'azure-retail': 'list price' })[s] || ''

export const lifecycleText = (n) => (n.lifecycle ? (n.lifecycleAssumed ? `${n.lifecycle} (assumed)` : n.lifecycle) : '')

/** The applications added up by category (`categoryFor(app)` names it), the one that costs most first, each with its applications by what they cost. */
export function groupByCategory(apps, categoryFor) {
  const by = new Map()
  for (const a of apps || []) {
    const c = categoryFor(a)
    if (!by.has(c)) by.set(c, { category: c, usage: 0, allocation: null, overprovisioning: null, apps: [] })
    const g = by.get(c)
    g.usage += a.usageMonthly
    if (a.allocationMonthly !== null) g.allocation = (g.allocation || 0) + a.allocationMonthly
    if (a.overprovisioningMonthly !== null) g.overprovisioning = (g.overprovisioning || 0) + a.overprovisioningMonthly
    g.apps.push(a)
  }
  const out = [...by.values()]
  for (const g of out) g.apps.sort((x, y) => y.usageMonthly - x.usageMonthly || x.label.localeCompare(y.label))
  return out.sort((x, y) => y.usage - x.usage || x.category.localeCompare(y.category))
}
