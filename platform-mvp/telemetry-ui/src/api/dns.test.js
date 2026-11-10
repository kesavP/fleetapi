import { describe, expect, it } from 'vitest'
import { MAX_DOMAINS, summarizeDns } from './dns'

const FE = '/k8s/shop/frontend-5d6f8c7b9-xk2lp/server'
const FE2 = '/k8s/shop/frontend-5d6f8c7b9-bk7q2/server'
const CART = '/k8s/shop/cartservice-7c9d8f6b5d-bk7q2/server'
const s = (container_id, domain, status, rate, request_type = 'TypeA') => ({ labels: { container_id, domain, status, request_type }, rate })
const ID = '/k8s/shop/frontend'

describe('summarizeDns', () => {
  it('is null for an application that made no lookups', () => {
    expect(summarizeDns([s(CART, 'a.example', 'ok', 5)], ID)).toBeNull()
    expect(summarizeDns([], ID)).toBeNull()
    expect(summarizeDns(null, ID)).toBeNull()
  })

  it('adds up the lookups of every pod of the application and no other', () => {
    const r = summarizeDns([s(FE, 'a.example', 'ok', 3), s(FE2, 'a.example', 'ok', 2), s(CART, 'a.example', 'ok', 100)], ID)
    expect(r.rps).toBe(5)
    expect(r.domains).toEqual([{ domain: 'a.example', rps: 5, failedRps: 0, nxdomainRps: 0 }])
  })

  it('counts a server that could not answer as a failure, and a name that does not exist as not one', () => {
    const r = summarizeDns([
      s(FE, 'x.example', 'ok', 6),
      s(FE, 'x.example', 'servfail', 1),
      s(FE, 'x.example', 'refused', 1),
      s(FE, 'x.example', 'format_error', 0.5),
      s(FE, 'x.example', 'not_implemented', 0.5),
      s(FE, 'x.example', 'nxdomain', 8),
    ], ID)
    expect(r.rps).toBe(17)
    expect(r.failedRps).toBe(3)
    expect(r.nxdomainRps).toBe(8)
    expect(r.failedShare).toBeCloseTo(3 / 17, 9)
  })

  it('lists the names with failures first, then the busiest', () => {
    const r = summarizeDns([s(FE, 'busy', 'ok', 50), s(FE, 'flaky', 'servfail', 1), s(FE, 'flaky', 'ok', 1), s(FE, 'quiet', 'ok', 2)], ID)
    expect(r.domains.map((d) => d.domain)).toEqual(['flaky', 'busy', 'quiet'])
  })

  it('keeps the list short and says how many there were', () => {
    const many = Array.from({ length: 40 }, (_, i) => s(FE, `d${String(i).padStart(2, '0')}.example`, 'ok', 1 + i))
    const r = summarizeDns(many, ID)
    expect(r.domains).toHaveLength(MAX_DOMAINS)
    expect(r.domainCount).toBe(40)
    expect(r.domains[0].domain).toBe('d39.example') // the busiest
  })

  it('treats a lookup with no name as "(unknown)", and no lookups at all as a share of zero', () => {
    expect(summarizeDns([s(FE, '', 'ok', 1)], ID).domains[0].domain).toBe('(unknown)')
    expect(summarizeDns([s(FE, 'a', 'ok', 0)], ID).failedShare).toBe(0)
  })
})
