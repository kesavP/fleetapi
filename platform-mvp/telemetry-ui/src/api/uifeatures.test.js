import { describe, expect, it } from 'vitest'
import { createUiFeatures, loadUiFeatures } from './uifeatures.js'

const answer = (body, ok = true) => async () => ({ ok, status: ok ? 200 : 404, json: async () => body })

describe('loadUiFeatures', () => {
  it('reads costs as true or false, from the file the UI serves', async () => {
    let asked
    const f = async (url, opts) => { asked = [url, opts]; return { ok: true, json: async () => ({ costs: true }) } }
    expect(await loadUiFeatures(f, '/ui-features.json')).toEqual({ costs: true })
    expect(asked[0]).toBe('/ui-features.json')
    expect(asked[1].cache).toBe('no-store')
    expect(await loadUiFeatures(answer({ costs: false }))).toEqual({ costs: false })
  })
  it('takes anything but exactly true as off', async () => {
    for (const v of [{ costs: 'true' }, { costs: 1 }, {}, null, [], [true], 'x']) expect(await loadUiFeatures(answer(v))).toEqual({ costs: false })
  })
  it('lets a bad answer through', async () => {
    await expect(loadUiFeatures(answer({}, false))).rejects.toThrow('404')
  })
})

describe('createUiFeatures', () => {
  it('is on once the file says so, and ready after', async () => {
    const u = createUiFeatures({ load: async () => ({ costs: true }) })
    expect(u.state.loaded).toBe(false)
    await u.ready
    expect(u.state).toMatchObject({ costs: true, loaded: true })
  })
  it('is off, but loaded, when the file cannot be read', async () => {
    const u = createUiFeatures({ load: async () => { throw new Error('x') } })
    await u.ready
    expect(u.state).toMatchObject({ costs: false, loaded: true })
  })
})
