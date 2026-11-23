// The build that is running, as the build said it (see buildInfo.js: vite.config.js defines these at build time). Without them (a tool that
// does not use the Vite config) it is a development build.
const text = (v) => (typeof v === 'string' ? v : '')

export const APP = Object.freeze({
  name: 'telemetry-ui',
  version: (typeof __APP_VERSION__ !== 'undefined' && text(__APP_VERSION__)) || 'dev',
  commit: typeof __APP_COMMIT__ !== 'undefined' ? text(__APP_COMMIT__) : '',
  built: typeof __APP_BUILT__ !== 'undefined' ? text(__APP_BUILT__) : '',
})

/** What to show of a build: the name and version, a short second line (commit and day built), and the whole of it for a tooltip. */
export function describeBuild(app = APP) {
  const t = app.built ? Date.parse(app.built) : NaN
  const day = Number.isFinite(t) ? new Date(t).toISOString().slice(0, 10) : ''
  const hash = app.commit.replace(/-dirty$/, '')
  const commit = hash ? hash.slice(0, 12) + (app.commit.endsWith('-dirty') ? '-dirty' : '') : ''
  const detail = [commit, day].filter(Boolean).join(' · ')
  const parts = [`${app.name} ${app.version}`]
  if (app.commit) parts.push(`commit ${app.commit}`)
  if (Number.isFinite(t)) parts.push(`built ${new Date(t).toISOString().replace('T', ' ').slice(0, 16)} UTC`)
  return { name: app.name, version: app.version, detail, title: parts.join(' · ') }
}
