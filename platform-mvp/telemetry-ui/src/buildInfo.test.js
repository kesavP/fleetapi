import { describe, expect, it } from 'vitest'
import { buildInfo, versionFile } from './buildInfo'

const NOW = () => new Date('2026-11-10T08:00:00Z')

describe('buildInfo', () => {
  it('is a development build, built now, when nothing is given', () => {
    expect(buildInfo({}, NOW)).toEqual({ version: 'dev', commit: '', built: '2026-11-10T08:00:00.000Z' })
    expect(buildInfo(undefined, NOW).version).toBe('dev')
    expect(buildInfo({ APP_VERSION: '  ', APP_COMMIT: '', APP_BUILT: ' ' }, NOW)).toEqual({ version: 'dev', commit: '', built: '2026-11-10T08:00:00.000Z' })
  })

  it('takes a version, a commit and a time that look like one', () => {
    expect(buildInfo({ APP_VERSION: 'v19', APP_COMMIT: '3f9a1c2d4e5b', APP_BUILT: '2026-11-10T07:30:00Z' }, NOW)).toEqual({ version: 'v19', commit: '3f9a1c2d4e5b', built: '2026-11-10T07:30:00Z' })
    for (const v of ['1.2.3', 'v1.2.3-rc.1', '2026.11.10', 'release_7', 'a1b2c3d', 'v19+build.5']) expect(buildInfo({ APP_VERSION: v }, NOW).version).toBe(v)
    expect(buildInfo({ APP_COMMIT: 'abcdef1-dirty' }, NOW).commit).toBe('abcdef1-dirty') // built from a working tree with changes in it
    expect(buildInfo({ APP_BUILT: '2026-11-10T07:30:00.123Z' }, NOW).built).toBe('2026-11-10T07:30:00.123Z')
  })

  it('says "unknown" for a version that was given and is not usable, never showing it', () => {
    for (const v of ['<script>alert(1)</script>', 'has space', 'a/b', '-v1', 'x'.repeat(65), 'v1;rm -rf', 'v1"', "v1'", 'v1\nv2', '../../etc']) {
      expect(buildInfo({ APP_VERSION: v }, NOW).version, v).toBe('unknown')
    }
  })

  it('drops a commit or a time that is not the shape of one', () => {
    for (const c of ['HEAD', 'xyz1234', 'abc', 'ABCDEF1', 'abcdef1-clean', 'abcdef1 ', '<b>', 'a'.repeat(41)]) {
      if (c === 'abcdef1 ') expect(buildInfo({ APP_COMMIT: c }, NOW).commit).toBe('abcdef1') // trailing white space from a shell is trimmed
      else expect(buildInfo({ APP_COMMIT: c }, NOW).commit, c).toBe('')
    }
    for (const b of ['yesterday', '2026-13-45T99:99:99Z', '2026-11-10', '2026-11-10T07:30:00+02:00', '1700000000', '<i>']) {
      expect(buildInfo({ APP_BUILT: b }, NOW).built, b).toBe('')
    }
  })

  it('ignores a value that is not a string', () => {
    expect(buildInfo({ APP_VERSION: 19, APP_COMMIT: {}, APP_BUILT: [] }, NOW)).toEqual({ version: 'dev', commit: '', built: '2026-11-10T08:00:00.000Z' })
  })
})

describe('versionFile', () => {
  it('is JSON that names the app and has the three facts', () => {
    const doc = JSON.parse(versionFile({ version: 'v19', commit: 'abc1234', built: '2026-11-10T07:30:00Z' }))
    expect(doc).toEqual({ name: 'telemetry-ui', version: 'v19', commit: 'abc1234', built: '2026-11-10T07:30:00Z' })
    expect(versionFile({ version: 'dev', commit: '', built: '' }).endsWith('\n')).toBe(true)
  })
})
