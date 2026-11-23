import { describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import RulesTab from './RulesTab.vue'
import { ApiError } from '../api/client'
import { RULE_NAMES } from '../api/rules'

const session = (v = { project: 'us', key: 'read-key' }) => ({ get: () => v })
const memory = (init = {}) => {
  const m = new Map(Object.entries(init))
  return { getItem: (k) => (m.has(k) ? m.get(k) : null), setItem: (k, v) => m.set(k, String(v)), removeItem: (k) => m.delete(k) }
}
const DEF = { enabled: true, percent: 50, days: 7, percentile: 95 }
const rule = (name, over = {}) => ({
  name, settings: { ...DEF, ...(name === 'rightsize.storage' ? { enabled: false, percentile: 100 } : {}) }, default: { ...DEF, ...(name === 'rightsize.storage' ? { enabled: false, percentile: 100 } : {}) },
  version: 0, saved: false, updatedBy: '', updatedAt: '', ...over,
})
const RULES = () => RULE_NAMES.map((n) => rule(n))

function make({ rules = RULES, load, save, sess = session(), storage = memory() } = {}) {
  const loadRules = load || vi.fn(async () => rules())
  const saveRule = save || vi.fn(async (client, r, s) => ({ ...r, settings: { enabled: s.enabled, percent: Number(s.percent), days: Number(s.days), percentile: Number(s.percentile) }, version: r.version + 1, saved: true, updatedBy: 'sam', updatedAt: '2026-11-17T10:00:00Z' }))
  const w = mount(RulesTab, { props: { client: {}, session: sess, storage, loaders: { loadRules, saveRule } } })
  return { w, loadRules, saveRule }
}
async function ready(opts) {
  const m = make(opts)
  await flushPromises()
  return m
}
const form = (w, name) => w.get(`[data-testid="rule-${name}"]`)
const field = (w, name, f) => form(w, name).get(`[name=${f}]`)
const key = (w) => w.get('[data-testid=rules-key]')
const save = (w, name) => form(w, name).get('[data-testid=save]')

describe('RulesTab', () => {
  it('shows the three rules in order, each with its settings, and says what these rules are', async () => {
    const { w, loadRules } = await ready()
    expect(loadRules).toHaveBeenCalledTimes(1)
    expect(w.findAll('form[data-testid^="rule-"]').map((f) => f.get('h3').text())).toEqual(['Memory', 'CPU', 'Storage'])
    const mem = form(w, 'rightsize.memory')
    expect(mem.get('[name=enabled]').element.checked).toBe(true)
    expect(mem.get('[name=percent]').element.value).toBe('50')
    expect(mem.get('[name=days]').element.value).toBe('7')
    expect(mem.get('[name=percentile]').element.value).toBe('95')
    expect(form(w, 'rightsize.storage').get('[name=enabled]').element.checked).toBe(false)
    const intro = w.get('[data-testid=rules-intro]').text()
    for (const want of ['kept on the hub', 'detector', 'next look', 'Incidents tab']) expect(intro).toContain(want)
  })

  it('says whether a rule is the default or was saved, and by whom and when', async () => {
    const { w } = await ready({ rules: () => [rule('rightsize.memory', { saved: true, version: 3, updatedBy: 'sam', updatedAt: '2026-11-17T10:00:00Z', settings: { ...DEF, percent: 40 } }), rule('rightsize.cpu'), rule('rightsize.storage')] })
    const saved = form(w, 'rightsize.memory').get('[data-testid=status]').text()
    expect(saved).toContain('version 3')
    expect(saved).toContain('sam')
    expect(form(w, 'rightsize.cpu').get('[data-testid=status]').text()).toContain('default')
    expect(form(w, 'rightsize.memory').get('[name=percent]').element.value).toBe('40')
    expect(form(w, 'rightsize.memory').get('[data-testid=default]').text()).toContain('50%')
  })

  it('reads nothing without a connection', async () => {
    const { w, loadRules } = await ready({ sess: session(null) })
    expect(w.text()).toContain('Not connected')
    expect(loadRules).not.toHaveBeenCalled()
    expect(w.find('form').exists()).toBe(false)
  })

  it('says why when the rules cannot be read, and what it means when the hub has no rules API', async () => {
    const down = await ready({ load: async () => { throw new ApiError('The telemetry service is not reachable right now.', { status: 502, kind: 'unavailable' }) } })
    expect(down.w.get('[data-testid=rules-error]').text()).toContain('not reachable')
    expect(down.w.find('form').exists()).toBe(false)
    const old = await ready({ load: async () => { throw new ApiError('Not found.', { status: 404, kind: 'request' }) } })
    expect(old.w.get('[data-testid=rules-error]').text()).toContain('Rules API')
    expect(old.w.get('[data-testid=rules-error]').text()).toContain('telemetry-ingest')
  })

  it('keeps Save off until something is changed, and on again only for a change', async () => {
    const { w } = await ready()
    expect(save(w, 'rightsize.memory').attributes('disabled')).toBeDefined()
    await field(w, 'rightsize.memory', 'percent').setValue('40')
    expect(save(w, 'rightsize.memory').attributes('disabled')).toBeUndefined()
    await field(w, 'rightsize.memory', 'percent').setValue('50')
    expect(save(w, 'rightsize.memory').attributes('disabled')).toBeDefined()
    await field(w, 'rightsize.cpu', 'enabled').setValue(false)
    expect(save(w, 'rightsize.cpu').attributes('disabled')).toBeUndefined()
    expect(save(w, 'rightsize.memory').attributes('disabled')).toBeDefined() // one rule's edit does not make another's savable
  })

  it('says what is wrong with a value as it is typed, and will not save it', async () => {
    const { w, saveRule } = await ready()
    for (const [f, v] of [['percent', '0'], ['percent', '150'], ['percent', ''], ['days', '0'], ['days', '31'], ['days', '7.5'], ['percentile', '10'], ['percentile', '101']]) {
      await field(w, 'rightsize.memory', f).setValue(v)
      expect(form(w, 'rightsize.memory').get(`[data-testid="err-${f}"]`).text(), `${f}=${v}`).not.toBe('')
      expect(save(w, 'rightsize.memory').attributes('disabled')).toBeDefined()
      await field(w, 'rightsize.memory', f).setValue(f === 'percent' ? '50' : f === 'days' ? '7' : '95')
    }
    await key(w).setValue('k')
    await save(w, 'rightsize.memory').trigger('click')
    expect(saveRule).not.toHaveBeenCalled()
  })

  it('asks for the manage key and saves nothing without one', async () => {
    const { w, saveRule } = await ready()
    await field(w, 'rightsize.memory', 'percent').setValue('40')
    await save(w, 'rightsize.memory').trigger('click')
    await flushPromises()
    expect(form(w, 'rightsize.memory').get('[data-testid=rule-error]').text()).toContain('manage key')
    expect(saveRule).not.toHaveBeenCalled()
    await key(w).setValue('   ')
    await save(w, 'rightsize.memory').trigger('click')
    expect(saveRule).not.toHaveBeenCalled()
  })

  it('saves the rule with the key given for this call, then shows the new version, forgets the key and has nothing left to save', async () => {
    const { w, saveRule } = await ready()
    expect(key(w).attributes('type')).toBe('password')
    await key(w).setValue('the-manage-key')
    await field(w, 'rightsize.memory', 'percent').setValue('40')
    await save(w, 'rightsize.memory').trigger('click')
    await flushPromises()
    expect(saveRule).toHaveBeenCalledTimes(1)
    const [, r, s, o] = saveRule.mock.calls[0]
    expect(r.name).toBe('rightsize.memory')
    expect(s).toMatchObject({ enabled: true, percent: 40, days: 7, percentile: 95 })
    expect(o.key).toBe('the-manage-key')
    expect(form(w, 'rightsize.memory').get('[data-testid=status]').text()).toContain('version 1')
    expect(form(w, 'rightsize.memory').get('[data-testid=rule-ok]').text()).toContain('Saved')
    expect(form(w, 'rightsize.memory').get('[name=percent]').element.value).toBe('40')
    expect(key(w).element.value).toBe('') // used, and not kept
    expect(save(w, 'rightsize.memory').attributes('disabled')).toBeDefined()
  })

  it('does not save twice while it is saving', async () => {
    let done
    const save1 = vi.fn(() => new Promise((resolve) => { done = resolve }))
    const { w } = await ready({ save: save1 })
    await key(w).setValue('k')
    await field(w, 'rightsize.memory', 'percent').setValue('40')
    await save(w, 'rightsize.memory').trigger('click')
    expect(save(w, 'rightsize.memory').attributes('disabled')).toBeDefined()
    await form(w, 'rightsize.memory').trigger('submit')
    await save(w, 'rightsize.memory').trigger('click')
    expect(save1).toHaveBeenCalledTimes(1)
    done({ ...rule('rightsize.memory'), version: 1, saved: true, settings: { ...DEF, percent: 40 } })
    await flushPromises()
    expect(save(w, 'rightsize.memory').attributes('disabled')).toBeDefined()
  })

  it('says why a save failed, keeps what was typed, forgets the key, and lets the person try again', async () => {
    const save1 = vi.fn().mockRejectedValueOnce(new ApiError('This key cannot change project "us". It must be a manage key of that project, not the read key.', { status: 403, kind: 'auth' })).mockImplementationOnce(async (c, r, s) => ({ ...r, settings: s, version: 1, saved: true }))
    const { w } = await ready({ save: save1 })
    await key(w).setValue('wrong')
    await field(w, 'rightsize.memory', 'percent').setValue('40')
    await save(w, 'rightsize.memory').trigger('click')
    await flushPromises()
    expect(form(w, 'rightsize.memory').get('[data-testid=rule-error]').text()).toContain('manage key')
    expect(field(w, 'rightsize.memory', 'percent').element.value).toBe('40')
    expect(key(w).element.value).toBe('')
    expect(save(w, 'rightsize.memory').attributes('disabled')).toBeUndefined()
    await key(w).setValue('right')
    await save(w, 'rightsize.memory').trigger('click')
    await flushPromises()
    expect(form(w, 'rightsize.memory').find('[data-testid=rule-error]').exists()).toBe(false)
    expect(form(w, 'rightsize.memory').get('[data-testid=status]').text()).toContain('version 1')
  })

  it('offers to read the rules again when someone else changed one, and then shows what is there', async () => {
    const conflict = new ApiError('the rule has changed since it was read: it is now at version 4', { status: 409, kind: 'request' })
    const save1 = vi.fn().mockRejectedValue(conflict)
    const load = vi.fn().mockResolvedValueOnce(RULES()).mockResolvedValueOnce([rule('rightsize.memory', { saved: true, version: 4, updatedBy: 'ana', settings: { ...DEF, percent: 30 } }), rule('rightsize.cpu'), rule('rightsize.storage')])
    const { w } = await ready({ save: save1, load })
    await key(w).setValue('k')
    await field(w, 'rightsize.memory', 'percent').setValue('40')
    await save(w, 'rightsize.memory').trigger('click')
    await flushPromises()
    const err = form(w, 'rightsize.memory').get('[data-testid=rule-error]')
    expect(err.text()).toContain('version 4')
    await form(w, 'rightsize.memory').get('[data-testid=reload]').trigger('click')
    await flushPromises()
    expect(load).toHaveBeenCalledTimes(2)
    expect(field(w, 'rightsize.memory', 'percent').element.value).toBe('30')
    expect(form(w, 'rightsize.memory').get('[data-testid=status]').text()).toContain('ana')
    expect(form(w, 'rightsize.memory').find('[data-testid=rule-error]').exists()).toBe(false)
  })

  it('has no reload button for a failure that is not a conflict', async () => {
    const { w } = await ready({ save: vi.fn().mockRejectedValue(new ApiError('boom', { status: 500 })) })
    await key(w).setValue('k')
    await field(w, 'rightsize.memory', 'percent').setValue('40')
    await save(w, 'rightsize.memory').trigger('click')
    await flushPromises()
    expect(form(w, 'rightsize.memory').find('[data-testid=reload]').exists()).toBe(false)
  })

  it('puts the default values back in the form without saving, so they can be looked at first', async () => {
    const { w, saveRule } = await ready({ rules: () => [rule('rightsize.memory', { saved: true, version: 2, settings: { enabled: false, percent: 20, days: 14, percentile: 99 } }), rule('rightsize.cpu'), rule('rightsize.storage')] })
    expect(field(w, 'rightsize.memory', 'percent').element.value).toBe('20')
    await form(w, 'rightsize.memory').get('[data-testid=reset]').trigger('click')
    expect(field(w, 'rightsize.memory', 'percent').element.value).toBe('50')
    expect(field(w, 'rightsize.memory', 'days').element.value).toBe('7')
    expect(field(w, 'rightsize.memory', 'percentile').element.value).toBe('95')
    expect(field(w, 'rightsize.memory', 'enabled').element.checked).toBe(true)
    expect(saveRule).not.toHaveBeenCalled()
    expect(save(w, 'rightsize.memory').attributes('disabled')).toBeUndefined() // it differs from what is saved, so it can be saved
  })

  it('remembers who is making the change, passes it on, and refuses a name the hub would', async () => {
    const storage = memory()
    const { w, saveRule } = await ready({ storage })
    await key(w).setValue('k')
    await w.get('[data-testid=rules-actor]').setValue('sam@example.com')
    await field(w, 'rightsize.memory', 'percent').setValue('40')
    await save(w, 'rightsize.memory').trigger('click')
    await flushPromises()
    expect(saveRule.mock.calls[0][3].actor).toBe('sam@example.com')
    expect(storage.getItem('fleet-ui.actor')).toBe('sam@example.com')
    // the next visit starts with it
    const again = await ready({ storage })
    expect(again.w.get('[data-testid=rules-actor]').element.value).toBe('sam@example.com')

    const bad = await ready()
    await key(bad.w).setValue('k')
    await bad.w.get('[data-testid=rules-actor]').setValue('a;b')
    await field(bad.w, 'rightsize.memory', 'percent').setValue('40')
    expect(bad.w.get('[data-testid=err-actor]').text()).not.toBe('')
    await save(bad.w, 'rightsize.memory').trigger('click')
    expect(bad.saveRule).not.toHaveBeenCalled()
  })

  it('does not keep the manage key in storage', async () => {
    const storage = memory()
    const spy = vi.spyOn(storage, 'setItem')
    const { w } = await ready({ storage })
    await key(w).setValue('the-manage-key')
    await field(w, 'rightsize.memory', 'percent').setValue('40')
    await save(w, 'rightsize.memory').trigger('click')
    await flushPromises()
    for (const [, v] of spy.mock.calls) expect(String(v)).not.toContain('the-manage-key')
  })

  it('saves one rule without touching what is being edited in another', async () => {
    const { w } = await ready()
    await key(w).setValue('k')
    await field(w, 'rightsize.cpu', 'days').setValue('14')
    await field(w, 'rightsize.memory', 'percent').setValue('40')
    await save(w, 'rightsize.memory').trigger('click')
    await flushPromises()
    expect(field(w, 'rightsize.cpu', 'days').element.value).toBe('14')
    expect(save(w, 'rightsize.cpu').attributes('disabled')).toBeUndefined()
  })

  it('shows what the hub sent as text, never as markup', async () => {
    const { w } = await ready({ rules: () => [rule('rightsize.memory', { saved: true, version: 1, updatedBy: '<img src=x onerror=alert(1)>' }), rule('rightsize.cpu'), rule('rightsize.storage')] })
    expect(w.find('img').exists()).toBe(false)
    expect(form(w, 'rightsize.memory').get('[data-testid=status]').text()).toContain('<img src=x onerror=alert(1)>')
  })
})
