import { afterEach, describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import NotificationBell from './NotificationBell.vue'

const mounted = []
function make(items = [], props = {}) {
  const host = document.createElement('div')
  document.body.appendChild(host)
  const w = mount(NotificationBell, { props: { items, ...props }, attachTo: host })
  mounted.push(w)
  return w
}
afterEach(() => {
  while (mounted.length) mounted.pop().unmount()
  document.body.innerHTML = ''
})

const btn = (w) => w.get('[data-testid=bell]')
const list = (w) => w.find('[data-testid=bell-list]')

describe('NotificationBell', () => {
  it('is a bell button that says how many notifications there are, and shows none of them until it is pressed', () => {
    const w = make(['one', 'two', 'three'])
    expect(btn(w).attributes('aria-label')).toBe('Notifications: 3')
    expect(w.get('[data-testid=bell-count]').text()).toBe('3')
    expect(btn(w).attributes('aria-expanded')).toBe('false')
    expect(list(w).exists()).toBe(false)
    expect(w.text()).not.toContain('one')
    expect(btn(w).find('svg').exists()).toBe(true) // the bell
  })

  it('shows them in order when pressed, and hides them when pressed again', async () => {
    const w = make(['first thing', 'second thing'])
    await btn(w).trigger('click')
    expect(btn(w).attributes('aria-expanded')).toBe('true')
    expect(list(w).findAll('li').map((l) => l.text())).toEqual(['first thing', 'second thing'])
    await btn(w).trigger('click')
    expect(list(w).exists()).toBe(false)
    expect(btn(w).attributes('aria-expanded')).toBe('false')
  })

  it('has no count when there is nothing to report, and says so when opened', async () => {
    const w = make([])
    expect(btn(w).attributes('aria-label')).toBe('Notifications: none')
    expect(w.find('[data-testid=bell-count]').exists()).toBe(false)
    await btn(w).trigger('click')
    expect(list(w).text()).toContain('Nothing to report')
    expect(list(w).find('ul').exists()).toBe(false)
  })

  it('caps a long count so it fits on the bell', () => {
    const w = make(Array.from({ length: 120 }, (_, i) => `n${i}`))
    expect(w.get('[data-testid=bell-count]').text()).toBe('99+')
    expect(btn(w).attributes('aria-label')).toBe('Notifications: 120')
  })

  it('follows the items it is given', async () => {
    const w = make(['a'])
    await btn(w).trigger('click')
    await w.setProps({ items: ['a', 'b'] })
    expect(w.get('[data-testid=bell-count]').text()).toBe('2')
    expect(list(w).findAll('li')).toHaveLength(2)
    await w.setProps({ items: [] })
    expect(w.find('[data-testid=bell-count]').exists()).toBe(false)
    expect(list(w).text()).toContain('Nothing to report')
  })

  it('closes on Escape and gives the focus back to the bell', async () => {
    const w = make(['one'])
    await btn(w).trigger('click')
    document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }))
    await w.vm.$nextTick()
    expect(list(w).exists()).toBe(false)
    expect(document.activeElement).toBe(btn(w).element)
  })

  it('does not react to other keys, or to Escape while it is closed', async () => {
    const w = make(['one'])
    await btn(w).trigger('click')
    document.dispatchEvent(new KeyboardEvent('keydown', { key: 'a', bubbles: true }))
    await w.vm.$nextTick()
    expect(list(w).exists()).toBe(true)
    await btn(w).trigger('click')
    document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }))
    await w.vm.$nextTick()
    expect(btn(w).attributes('aria-expanded')).toBe('false')
  })

  it('does not take the focus when Escape is pressed while it is closed: that Escape belongs to whatever else is on the page', async () => {
    const w = make(['one'])
    const other = document.createElement('input')
    document.body.appendChild(other)
    other.focus()
    document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }))
    await w.vm.$nextTick()
    expect(document.activeElement).toBe(other)
  })

  it('closes when something else on the page is clicked, but not when its own list is', async () => {
    const w = make(['one'])
    await btn(w).trigger('click')
    await list(w).trigger('click')
    expect(list(w).exists()).toBe(true)
    document.body.click()
    await w.vm.$nextTick()
    expect(list(w).exists()).toBe(false)
  })

  it('lets an item carry a test id, and shows an item that is markup as text', async () => {
    const w = make([{ text: 'the hover menu could not load', testid: 'apps-error' }, '<img src=x onerror=alert(1)>'])
    await btn(w).trigger('click')
    expect(w.get('[data-testid=apps-error]').text()).toBe('the hover menu could not load')
    expect(w.find('img').exists()).toBe(false)
    expect(list(w).findAll('li')[1].text()).toBe('<img src=x onerror=alert(1)>')
  })

  it('ignores an item that has no text, so the count is of what is shown', () => {
    const w = make(['one', '', null, undefined, { text: '' }, { testid: 'x' }, 'two'])
    expect(w.get('[data-testid=bell-count]').text()).toBe('2')
  })

  it('takes its listeners off the page when it goes', () => {
    const add = vi.spyOn(document, 'addEventListener')
    const remove = vi.spyOn(document, 'removeEventListener')
    const w = make(['one'])
    const added = add.mock.calls.map((c) => c[0])
    w.unmount()
    mounted.pop()
    const removed = remove.mock.calls.map((c) => c[0])
    for (const type of added.filter((t) => t === 'click' || t === 'keydown')) expect(removed).toContain(type)
    add.mockRestore()
    remove.mockRestore()
  })
})
