import { describe, expect, it } from 'vitest'
import { APP, describeBuild } from './version'

describe('APP', () => {
  it('is a development build under test, with no commit and no time unless the environment gave them', () => {
    expect(APP.name).toBe('telemetry-ui')
    expect(typeof APP.version).toBe('string')
    expect(APP.version.length).toBeGreaterThan(0)
    expect(Object.isFrozen(APP)).toBe(true)
  })
})

describe('describeBuild', () => {
  const app = { name: 'telemetry-ui', version: 'v19', commit: '3f9a1c2d4e5b6789', built: '2026-11-10T07:30:00Z' }
  it('says the name and version, a short line and the whole of it', () => {
    const d = describeBuild(app)
    expect(d.name).toBe('telemetry-ui')
    expect(d.version).toBe('v19')
    expect(d.detail).toBe('3f9a1c2d4e5b · 2026-11-10')
    expect(d.title).toBe('telemetry-ui v19 · commit 3f9a1c2d4e5b6789 · built 2026-11-10 07:30 UTC')
  })
  it('says a build from a working tree with changes is one, in the short line too', () => {
    expect(describeBuild({ ...app, commit: '3f9a1c2d4e5b6789ab-dirty' }).detail).toBe('3f9a1c2d4e5b-dirty · 2026-11-10')
    expect(describeBuild({ ...app, commit: 'abc1234-dirty' }).detail).toBe('abc1234-dirty · 2026-11-10')
  })
  it('leaves out what is not known', () => {
    expect(describeBuild({ name: 'telemetry-ui', version: 'dev', commit: '', built: '' })).toEqual({ name: 'telemetry-ui', version: 'dev', detail: '', title: 'telemetry-ui dev' })
    expect(describeBuild({ ...app, commit: '' }).detail).toBe('2026-11-10')
    expect(describeBuild({ ...app, built: '' }).detail).toBe('3f9a1c2d4e5b')
    expect(describeBuild({ ...app, built: 'not a date' }).title).not.toContain('built')
  })
})
