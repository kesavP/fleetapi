import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import MoveToCategory from './MoveToCategory.vue'

const CATS = ['application', 'control-plane', 'monitoring']
const mountIt = (props = {}) => mount(MoveToCategory, { props: { current: 'application', categories: CATS, ...props }, attachTo: document.body })

describe('MoveToCategory', () => {
  it('says what it is for, offers a new category first, and then every OTHER category', () => {
    const w = mountIt()
    expect(w.get('.move-title').text()).toBe('Move the app to a category')
    expect(w.attributes('role')).toBe('menu')
    expect(w.findAll('.move-item').map((b) => b.text())).toEqual(['+ a new category', '→ control-plane', '→ monitoring'])
    w.unmount()
  })

  it('leaves out the category it is in, whichever that is', () => {
    const w = mountIt({ current: 'monitoring' })
    expect(w.findAll('.move-item').map((b) => b.text())).toEqual(['+ a new category', '→ application', '→ control-plane'])
    w.unmount()
  })

  it('asks to move to the category chosen', async () => {
    const w = mountIt()
    await w.findAll('.move-item')[2].trigger('click')
    expect(w.emitted('move')[0]).toEqual(['monitoring'])
    w.unmount()
  })

  it('makes a new category from a name, and says why a name is refused', async () => {
    const w = mountIt({ validate: (n) => (n.trim() ? '' : 'Give the category a name.') })
    await w.findAll('.move-item')[0].trigger('click')
    expect(w.find('.cat-form').exists()).toBe(true)
    await w.get('.cat-form').trigger('submit')
    expect(w.get('[role=alert]').text()).toBe('Give the category a name.')
    expect(w.emitted('add')).toBeUndefined()
    await w.get('.cat-form input').setValue('payments')
    await w.get('.cat-form').trigger('submit')
    expect(w.emitted('add')[0]).toEqual(['payments'])
    w.unmount()
  })

  it('closes on Escape, and a click inside does not reach the page behind', async () => {
    const w = mountIt()
    await w.trigger('keydown', { key: 'Escape' })
    expect(w.emitted('close')).toHaveLength(1)
    const outer = []
    document.body.addEventListener('click', () => outer.push(1), { once: true })
    await w.trigger('click')
    expect(outer).toEqual([]) // stopped: it must not select the node that holds the menu
    w.unmount()
  })
})
