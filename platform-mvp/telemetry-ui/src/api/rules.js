// The rules the hub holds (GET .../rules, PUT .../rules/{name}): the settings that decide when its detectors speak, kept on the hub so that a person can
// change them without a deploy. Reading takes the project's read key. Changing one takes its MANAGE key, which the page asks for each time and keeps
// nowhere (see RulesTab.vue), and the hub refuses a change that was based on an older version than it has.
//
// What the hub checks is checked here too, so that a person is told before they press Save and not after; the hub still has the last word.

export const RULE_NAMES = ['rightsize.memory', 'rightsize.cpu', 'rightsize.storage']
export const LIMITS = { percent: { above: 0, atMost: 100 }, days: { min: 1, max: 30 }, percentile: { min: 50, max: 100 } }

const isObject = (v) => v !== null && typeof v === 'object' && !Array.isArray(v)

/** A number from what a form holds: a number, or text that is one; NaN for anything else (an empty field is not zero). */
function num(v) {
  if (typeof v === 'number') return v
  if (typeof v === 'string' && v.trim() !== '') return Number(v)
  return NaN
}

const settingsOf = (o) =>
  isObject(o) && typeof o.enabled === 'boolean' && Number.isFinite(o.percent) && Number.isFinite(o.days) && Number.isFinite(o.percentile)
    ? { enabled: o.enabled, percent: o.percent, days: o.days, percentile: o.percentile }
    : null

/** One rule as the page uses it, or null for a row that is not one the page knows. */
export function parseRule(r) {
  if (!isObject(r) || !RULE_NAMES.includes(r.name)) return null
  const settings = settingsOf(r)
  const def = settingsOf(r.default)
  if (!settings || !def || !Number.isInteger(r.version) || r.version < 0) return null
  return {
    name: r.name,
    settings,
    default: def,
    version: r.version,
    saved: r.saved === true,
    updatedBy: typeof r.updated_by === 'string' ? r.updated_by : '',
    updatedAt: typeof r.updated_at === 'string' ? r.updated_at : '',
  }
}

/** Every rule the hub has for the project, in the order the page lists them. */
export async function loadRules(client) {
  const data = await client.get('rules')
  const rows = (Array.isArray(data) ? data : []).map(parseRule).filter(Boolean)
  return RULE_NAMES.map((n) => rows.find((r) => r.name === n)).filter(Boolean)
}

/** What is wrong with the settings, by field: {} when nothing is. The same limits the hub holds to. */
export function settingsProblems(s) {
  const out = {}
  const percent = num(s.percent)
  if (!(Number.isFinite(percent) && percent > LIMITS.percent.above && percent <= LIMITS.percent.atMost)) out.percent = 'A number above 0 and at most 100.'
  const days = num(s.days)
  if (!(Number.isInteger(days) && days >= LIMITS.days.min && days <= LIMITS.days.max)) out.days = 'A whole number from 1 to 30.'
  const percentile = num(s.percentile)
  if (!(Number.isFinite(percentile) && percentile >= LIMITS.percentile.min && percentile <= LIMITS.percentile.max)) out.percentile = 'A number from 50 to 100.'
  return out
}

/** Whether two sets of settings are the same, a number typed as text being the number. */
export function sameSettings(a, b) {
  return !!a.enabled === !!b.enabled && num(a.percent) === num(b.percent) && num(a.days) === num(b.days) && num(a.percentile) === num(b.percentile)
}

/**
 * Saves a new version of `rule` with `settings`, based on the version the page read; returns the rule as it is then. `key` is the project's manage key,
 * for this one call. A rule the page does not know, or settings the hub would refuse, are refused here before anything is sent.
 */
export async function saveRule(client, rule, settings, { key, actor } = {}) {
  if (!RULE_NAMES.includes(rule.name)) throw new Error(`"${String(rule.name).slice(0, 40)}" is not a rule.`)
  const problems = settingsProblems(settings)
  const first = Object.keys(problems)[0]
  if (first) throw new Error(`${first}: ${problems[first]}`)
  const body = { enabled: !!settings.enabled, percent: num(settings.percent), days: num(settings.days), percentile: num(settings.percentile), expected_version: rule.version }
  const saved = parseRule(await client.send('PUT', `rules/${rule.name}`, { key, body, actor }))
  if (!saved) throw new Error('The service answered with something that is not a rule.')
  return saved
}
