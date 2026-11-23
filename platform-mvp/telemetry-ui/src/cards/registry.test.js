import { describe, expect, it, vi } from 'vitest'
import { defineCard, loadCards } from './registry'

const NOW = 1_700_000_000_000
const flat = (v) => Array.from({ length: 40 }, (_, i) => ({ t: NOW - (40 - i) * 15000, v }))
const POD = '/k8s/shop/cart-7c9d8f6b5d-bk7q2/server'
const clientOf = (data, fail = {}) => ({
  get: vi.fn(async (path, params) => {
    if (fail[params.name]) throw new Error(fail[params.name])
    return { series: data[params.name] || [] }
  }),
})
const card = (over = {}) =>
  defineCard({
    id: 'demo',
    title: 'Demo',
    metrics: { a: { metric: 'm_a', kind: 'rate' }, g: { metric: 'm_g', kind: 'gauge' } },
    build: ({ a, g }) => ({ rows: [{ label: 'a', value: String(a.length) }, { label: 'g', value: String(g.length) }] }),
    ...over,
  })
const opts = (cards, present) => ({ seconds: 600, now: () => NOW, present: new Set(present), cards })

describe('defineCard', () => {
  it('refuses a card with no id, a bad id, no title, no build, or an unknown kind of metric', () => {
    expect(() => defineCard({ title: 't', metrics: {}, build: () => null })).toThrow(/id/)
    expect(() => defineCard({ id: 'Bad Id', title: 't', metrics: {}, build: () => null })).toThrow(/id/)
    expect(() => defineCard({ id: 'x', metrics: {}, build: () => null })).toThrow(/title/)
    expect(() => defineCard({ id: 'x', title: 't', metrics: {} })).toThrow(/build/)
    expect(() => defineCard({ id: 'x', title: 't', metrics: { a: { metric: 'm', kind: 'sum' } }, build: () => null })).toThrow(/kind/)
    expect(() => defineCard({ id: 'x', title: 't', metrics: { a: { kind: 'rate' } }, build: () => null })).toThrow(/metric/)
  })
})

describe('loadCards', () => {
  it('reads only the metrics the project has, gives build each as a list, and returns what build made', async () => {
    const client = clientOf({ m_a: [{ labels: { container_id: POD }, points: flat(3) }] })
    const out = await loadCards(client, '/k8s/shop/cart', opts([card()], ['m_a']))
    expect(client.get.mock.calls.map(([, p]) => p.name)).toEqual(['m_a'])
    expect(out.cards).toEqual([{ id: 'demo', title: 'Demo', rows: [{ label: 'a', value: '1' }, { label: 'g', value: '0' }], note: null }])
    expect(out.warnings).toEqual([])
  })

  it('leaves a card out when the project has none of its metrics, without asking for anything', async () => {
    const client = clientOf({})
    const out = await loadCards(client, '/k8s/shop/cart', opts([card()], ['something_else']))
    expect(client.get).not.toHaveBeenCalled()
    expect(out.cards).toEqual([])
  })

  it('leaves a card out when build has nothing to show for this application', async () => {
    const out = await loadCards(clientOf({}), '/k8s/shop/cart', opts([card({ build: () => null })], ['m_a']))
    expect(out.cards).toEqual([])
  })

  it('keeps the other cards, and says which one failed, when one cannot be read', async () => {
    const good = card({ id: 'good', title: 'Good' })
    const bad = card({ id: 'bad', title: 'Bad', metrics: { a: { metric: 'm_bad', kind: 'rate' } } })
    const client = clientOf({ m_a: [{ labels: { container_id: POD }, points: flat(1) }] }, { m_bad: 'boom' })
    const out = await loadCards(client, '/k8s/shop/cart', opts([bad, good], ['m_a', 'm_bad']))
    expect(out.cards.map((c) => c.id)).toEqual(['good'])
    expect(out.warnings).toEqual(['Bad: m_bad: boom'])
  })

  it('keeps a card whose optional metric fails, and warns', async () => {
    const c = card({ metrics: { a: { metric: 'm_a', kind: 'rate' }, g: { metric: 'm_g', kind: 'gauge', optional: true } } })
    const client = clientOf({ m_a: [{ labels: { container_id: POD }, points: flat(1) }] }, { m_g: 'late' })
    const out = await loadCards(client, '/k8s/shop/cart', opts([c], ['m_a', 'm_g']))
    expect(out.cards).toHaveLength(1)
    expect(out.warnings).toEqual(['Demo: m_g: late'])
  })

  it('isolates a card whose build throws', async () => {
    const boom = card({ id: 'boom', title: 'Boom', build: () => { throw new Error('bad build') } })
    const out = await loadCards(clientOf({}), '/k8s/shop/cart', opts([boom, card({ id: 'ok', title: 'Ok' })], ['m_a']))
    expect(out.cards.map((c) => c.id)).toEqual(['ok'])
    expect(out.warnings).toEqual(['Boom: bad build'])
  })

  it('warns that a truncated read may be partial', async () => {
    const client = { get: vi.fn(async () => ({ series: [], truncated: true })) }
    const out = await loadCards(client, '/k8s/shop/cart', opts([card()], ['m_a']))
    expect(out.warnings[0]).toContain('more series than one call allows')
  })

  it('refuses a card that returns rows that are not text, so nothing odd reaches the page', async () => {
    const c = card({ build: () => ({ rows: [{ label: 'x', value: { html: '<b>' } }] }) })
    const out = await loadCards(clientOf({}), '/k8s/shop/cart', opts([c], ['m_a']))
    expect(out.cards).toEqual([])
    expect(out.warnings[0]).toContain('Demo')
  })

  it('reads the cards at the same time, not one after the other', async () => {
    const order = []
    const slow = (id) => card({ id, title: id, metrics: { a: { metric: 'm_' + id, kind: 'rate' } }, build: () => ({ rows: [{ label: id, value: '1' }] }) })
    const client = { get: vi.fn(async (_p, q) => { order.push('start ' + q.name); await Promise.resolve(); order.push('end ' + q.name); return { series: [] } }) }
    await loadCards(client, '/k8s/shop/cart', opts([slow('one'), slow('two')], ['m_one', 'm_two']))
    expect(order.slice(0, 2)).toEqual(['start m_one', 'start m_two'])
  })
})
