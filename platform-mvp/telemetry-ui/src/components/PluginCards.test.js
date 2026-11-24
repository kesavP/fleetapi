import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import PluginCards from './PluginCards.vue'

const cards = [
  { id: 'connections', title: 'Connections', rows: [{ label: 'Connects', value: '40/s' }, { label: 'Failed', value: '2/s (2%)', level: 'caution' }, { label: 'Open', value: '3', level: 'bad' }], note: null },
  { id: 'network', title: 'Network', rows: [{ label: 'Sent', value: '3 KiB/s' }], note: 'Over TCP only.' },
]

describe('PluginCards', () => {
  it('draws a card for each plugin, with its rows, under an id the page can link to', () => {
    const w = mount(PluginCards, { props: { cards } })
    expect(w.findAll('.card')).toHaveLength(2)
    expect(w.get('#connections h3').text()).toBe('Connections')
    expect(w.get('#connections').text()).toContain('Connects40/s')
    expect(w.get('[data-card=network]').text()).toContain('Sent3 KiB/s')
  })

  it('marks a row by its level, and a card with a note shows it', () => {
    const w = mount(PluginCards, { props: { cards } })
    const dd = w.findAll('#connections dd')
    expect(dd[0].classes()).not.toContain('bad')
    expect(dd[1].classes()).toContain('caution')
    expect(dd[2].classes()).toContain('bad')
    expect(w.get('#network .help').text()).toBe('Over TCP only.')
    expect(w.find('#connections .help').exists()).toBe(false)
  })

  it('shows plugin text as text, never as markup', () => {
    const evil = [{ id: 'x', title: '<img src=x onerror=alert(1)>', rows: [{ label: '<b>l</b>', value: '<script>1</script>' }], note: '<i>n</i>' }]
    const w = mount(PluginCards, { props: { cards: evil } })
    expect(w.find('img').exists()).toBe(false)
    expect(w.find('b').exists()).toBe(false)
    expect(w.find('script').exists()).toBe(false)
    expect(w.find('i').exists()).toBe(false)
    expect(w.text()).toContain('<b>l</b>')
  })

  it('draws nothing at all without cards', () => {
    expect(mount(PluginCards, { props: { cards: [] } }).find('[data-section=plugins]').exists()).toBe(false)
  })
})
