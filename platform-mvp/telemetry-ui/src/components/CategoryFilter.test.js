import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import CategoryFilter from './CategoryFilter.vue'

const CATS = ['application', 'control-plane', 'monitoring', 'payments']
const mountIt = (props = {}) => mount(CategoryFilter, { props: { categories: CATS, modelValue: ['application'], ...props }, attachTo: document.body })
const last = (w) => w.emitted('update:modelValue').at(-1)[0]

describe('CategoryFilter', () => {
  it('has a checkbox for every category, with the chosen ones ticked', () => {
    const w = mountIt({ modelValue: ['application', 'payments'] })
    expect(w.findAll('.cat').map((l) => l.text())).toEqual(CATS)
    expect(w.findAll('.cat input').map((b) => b.element.checked)).toEqual([true, false, false, true])
    w.unmount()
  })

  it('adds a category to the chosen ones and takes one away', async () => {
    const w = mountIt({ modelValue: ['application', 'monitoring'] })
    await w.findAll('.cat input')[3].setValue(true)
    expect(last(w)).toEqual(['application', 'monitoring', 'payments'])
    await w.findAll('.cat input')[2].setValue(false)
    expect(last(w)).toEqual(['application'])
    w.unmount()
  })

  it('does not let the last one be switched off, and puts the box back', async () => {
    const w = mountIt()
    await w.findAll('.cat input')[0].setValue(false)
    expect(w.emitted('update:modelValue')).toBeUndefined()
    expect(w.findAll('.cat input')[0].element.checked).toBe(true) // the browser unticked it; the component restores it
    w.unmount()
  })

  it('opens a small form from the "+", and closes it again', async () => {
    const w = mountIt()
    const plus = w.get('.cat-plus')
    expect(plus.attributes('aria-expanded')).toBe('false')
    expect(w.find('.cat-form').exists()).toBe(false)
    await plus.trigger('click')
    expect(plus.attributes('aria-expanded')).toBe('true')
    expect(w.find('.cat-form').exists()).toBe(true)
    await plus.trigger('click')
    expect(w.find('.cat-form').exists()).toBe(false)
    w.unmount()
  })

  it('asks for the new category once it is valid, and shows why it is not valid before that', async () => {
    const w = mountIt({ validate: (n) => (n.includes(',') ? 'No commas.' : '') })
    await w.get('.cat-plus').trigger('click')
    await w.get('.cat-form input').setValue('a,b')
    await w.get('.cat-form').trigger('submit')
    expect(w.get('.cat-form [role=alert]').text()).toBe('No commas.')
    expect(w.emitted('add')).toBeUndefined()
    await w.get('.cat-form input').setValue('team a')
    await w.get('.cat-form').trigger('submit')
    expect(w.emitted('add')[0]).toEqual(['team a'])
    expect(w.find('.cat-form').exists()).toBe(false)
    w.unmount()
  })
})
