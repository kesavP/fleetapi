import { afterEach, describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import NamespacePicker from './NamespacePicker.vue'

const NS = [{ name: 'default', count: 4 }, { name: 'payments', count: 2 }, { name: 'shop', count: 11 }]
const wrappers = []
function mountPicker(props = {}) {
  const w = mount(NamespacePicker, { props: { namespaces: NS, modelValue: [], ...props }, attachTo: document.body })
  wrappers.push(w)
  return w
}
afterEach(() => {
  while (wrappers.length) wrappers.pop().unmount()
})
const last = (w) => w.emitted('update:modelValue').at(-1)[0]

describe('NamespacePicker', () => {
  it('says "all" and how many applications when nothing is chosen', () => {
    expect(mountPicker().get('.ns-all').text()).toBe('all (17)')
  })

  it('shows the chosen namespaces as chips, a few of them, and how many more', () => {
    const w = mountPicker({ modelValue: ['shop', 'default', 'payments'] })
    expect(w.findAll('.ns-chip').map((c) => c.text().replace('×', ''))).toEqual(['shop', 'default'])
    expect(w.get('.ns-more').text()).toBe('+1 more')
    expect(w.find('.ns-all').exists()).toBe(false)
  })

  it('does not show a choice that is no longer on offer', () => {
    const w = mountPicker({ modelValue: ['gone', 'shop'] })
    expect(w.findAll('.ns-chip')).toHaveLength(1)
    expect(w.get('.ns-chip').text()).toContain('shop')
  })

  it('opens and closes with its button, and says so for assistive technology', async () => {
    const w = mountPicker()
    const btn = w.get('.ns-toggle')
    expect(btn.attributes('aria-expanded')).toBe('false')
    expect(w.find('.ns-pop').exists()).toBe(false)
    await btn.trigger('click')
    expect(btn.attributes('aria-expanded')).toBe('true')
    expect(w.findAll('.ns-option').map((o) => o.text())).toEqual(['default 4', 'payments 2', 'shop 11'])
    await btn.trigger('click')
    expect(w.find('.ns-pop').exists()).toBe(false)
  })

  it('chooses and unchooses a namespace', async () => {
    const w = mountPicker({ modelValue: ['shop'] })
    await w.get('.ns-toggle').trigger('click')
    await w.findAll('.ns-option input')[0].setValue(true) // default
    expect(last(w)).toEqual(['shop', 'default'])
    await w.setProps({ modelValue: ['shop', 'default'] })
    await w.findAll('.ns-option input')[2].setValue(false) // shop
    expect(last(w)).toEqual(['default'])
  })

  it('removes a namespace from its chip', async () => {
    const w = mountPicker({ modelValue: ['shop', 'default'] })
    await w.get('[aria-label="Remove namespace shop"]').trigger('click')
    expect(last(w)).toEqual(['default'])
  })

  it('filters the list, and "select matching" takes only what matches, keeping what was chosen', async () => {
    const w = mountPicker({ modelValue: ['payments'] })
    await w.get('.ns-toggle').trigger('click')
    await w.get('.ns-search').setValue('SH')
    expect(w.findAll('.ns-option').map((o) => o.text())).toEqual(['shop 11'])
    const sel = w.findAll('.ns-actions button')[0]
    expect(sel.text()).toBe('Select matching')
    await sel.trigger('click')
    expect(last(w)).toEqual(['payments', 'shop'])
  })

  it('selects all and clears', async () => {
    const w = mountPicker({ modelValue: ['shop'] })
    await w.get('.ns-toggle').trigger('click')
    await w.findAll('.ns-actions button')[0].trigger('click')
    expect(last(w)).toEqual(['shop', 'default', 'payments'])
    await w.findAll('.ns-actions button')[1].trigger('click')
    expect(last(w)).toEqual([])
  })

  it('says when nothing matches the filter, and when there are no namespaces', async () => {
    const w = mountPicker()
    await w.get('.ns-toggle').trigger('click')
    await w.get('.ns-search').setValue('zzz')
    expect(w.get('.ns-none').text()).toBe('No namespace matches.')
    const empty = mountPicker({ namespaces: [] })
    await empty.get('.ns-toggle').trigger('click')
    expect(empty.get('.ns-none').text()).toBe('No namespaces.')
  })

  it('closes on Escape and on a click elsewhere, not on a click inside', async () => {
    const w = mountPicker()
    await w.get('.ns-toggle').trigger('click')
    await w.get('.ns-search').trigger('mousedown')
    expect(w.find('.ns-pop').exists()).toBe(true)
    await w.get('.ns-pop').trigger('keydown', { key: 'Escape' })
    expect(w.find('.ns-pop').exists()).toBe(false)
    await w.get('.ns-toggle').trigger('click')
    document.body.dispatchEvent(new MouseEvent('mousedown', { bubbles: true }))
    await w.vm.$nextTick()
    expect(w.find('.ns-pop').exists()).toBe(false)
  })
})
