import { describe, expect, it, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import ClickHouseTab from './ClickHouseTab.vue'

const session = (v = { project: 'gke', key: 'k' }) => ({ get: () => v })
const GB = 1024 ** 3
const INFO = {
  connection: { reachable: true, protocol: 'native', database: 'telemetry', tls: true, version: '24.8.1', ping_ms: 2.4 },
  storage: [
    { type: 'metrics', rows: 1200, estimated_bytes: 5 * 1024 ** 2, compression: 11.04, ttl_seconds: 336 * 3600, oldest: new Date(Date.now() - 3 * 86400_000).toISOString() },
    { type: 'traces', rows: 0, estimated_bytes: 0, compression: 0, ttl_seconds: 168 * 3600, oldest: null },
    { type: 'logs', rows: 50, estimated_bytes: 2048, compression: 5.95, ttl_seconds: 168 * 3600, oldest: new Date(Date.now() - 25 * 60_000).toISOString() },
    { type: 'profiles', rows: 3, estimated_bytes: 100, compression: 2, ttl_seconds: 168 * 3600, oldest: null },
  ],
  cluster: null,
  warnings: [],
}
const SHARED = {
  ...INFO,
  storage: INFO.storage.map((s) => ({ ...s, table_bytes: s.estimated_bytes * 4, table_rows: s.rows * 4 })),
  cluster: { servers: [{ host: 'shard1-replica1', disks: [{ name: 'default', path: '/var/lib/clickhouse/', type: 'Local', free: 61 * GB, total: 100 * GB }, { name: 'cold', path: '/mnt/cold/', type: 'Local', free: 5 * GB, total: 100 * GB }, { name: 'odd', path: '/x/', type: '', free: 0, total: 0 }, { name: 'gcs', path: '', type: 'ObjectStorage', free: 0, total: 0, unlimited: true, used: 3 * GB }] }] },
}
async function mountTab(over = {}, props = {}) {
  const client = { get: vi.fn(async () => over.info ?? INFO) }
  if (over.fail) client.get = vi.fn(async () => { throw new Error(over.fail) })
  const w = mount(ClickHouseTab, { props: { client, session: session(), ...props }, global: { stubs: { RouterLink: { template: '<a><slot/></a>' } } } })
  await flushPromises()
  return { w, client }
}
const rowsOf = (w, label) => w.findAll(`table[aria-label="${label}"] tbody tr`).map((r) => r.findAll('td').map((c) => c.text()))

describe('ClickHouseTab', () => {
  it('asks the service for the project\'s ClickHouse information once', async () => {
    const { client } = await mountTab()
    expect(client.get).toHaveBeenCalledTimes(1)
    expect(client.get).toHaveBeenCalledWith('clickhouse')
  })

  it('shows the protocol, database and TLS as read-only, and "<hidden>" for the address and credentials', async () => {
    const { w } = await mountTab()
    const radios = w.findAll('input[type=radio]')
    expect(radios.map((r) => r.element.checked)).toEqual([true, false, false])
    expect(radios.every((r) => r.element.disabled || r.element.closest('fieldset').disabled)).toBe(true)
    expect(w.get('[data-testid=address]').element.value).toBe('<hidden>')
    expect(w.get('[data-testid=user]').element.value).toBe('<hidden>')
    expect(w.get('[data-testid=password]').element.value).toBe('')
    expect(w.get('[data-testid=database]').element.value).toBe('telemetry')
    const [tls, skip] = w.findAll('.checks input')
    expect([tls.element.checked, skip.element.checked, tls.element.disabled, skip.element.disabled]).toEqual([true, false, true, true])
    for (const id of ['address', 'user', 'password', 'database']) expect(w.get(`[data-testid=${id}]`).attributes('readonly')).toBeDefined()
  })

  it('says it is connected, with the version and the answer time, and has no way to save anything', async () => {
    const { w } = await mountTab()
    expect(w.get('.status').text()).toContain('Connected · ClickHouse 24.8.1 · 2 ms')
    expect(w.text()).not.toMatch(/Test & Save|Save/)
  })

  it('"Test connection" asks again and shows the new answer', async () => {
    const { w, client } = await mountTab()
    client.get.mockResolvedValueOnce({ ...INFO, connection: { ...INFO.connection, reachable: false, error: 'ClickHouse did not answer.' } })
    await w.get('.status button').trigger('click')
    await flushPromises()
    expect(client.get).toHaveBeenCalledTimes(2)
    expect(w.get('.status').text()).toContain('ClickHouse did not answer.')
    expect(w.get('.status .bad').exists()).toBe(true)
  })

  it('lists the storage of each kind of data: size, rows, compression, retention and how long ago it starts', async () => {
    const { w } = await mountTab()
    const r = rowsOf(w, 'Storage usage')
    expect(r.map((c) => c[0])).toEqual(['metrics', 'traces', 'logs', 'profiling'])
    expect(r[0]).toEqual(['metrics', '5 MiB', '1,200', '11.0x', '14d', '3d ago'])
    expect(r[1]).toEqual(['traces', '0 B', '0', '–', '7d', '–']) // nothing stored: no compression, no start
    expect(r[2][4]).toBe('7d')
    expect(r[2][5]).toBe('25m ago')
    expect(w.findAll('table[aria-label="Storage usage"] th').map((h) => h.text())).not.toContain('Platform size')
  })

  it('without shared cluster details, says so and shows no disks', async () => {
    const { w } = await mountTab()
    expect(w.get('[data-testid=not-shared]').text()).toContain('has not shared')
    expect(w.find('table[aria-label="Cluster topology"]').exists()).toBe(false)
  })

  it('with shared details, adds the platform-wide columns and the disks with a usage bar each', async () => {
    const { w } = await mountTab({ info: SHARED })
    expect(w.findAll('table[aria-label="Storage usage"] th').map((h) => h.text())).toContain('Platform size')
    expect(rowsOf(w, 'Storage usage')[0].slice(-2)).toEqual(['20 MiB', '4,800'])
    expect(w.find('[data-testid=not-shared]').exists()).toBe(false)
    const disks = rowsOf(w, 'Cluster topology')
    expect(disks).toHaveLength(4)
    expect(disks[0][0]).toBe('shard1-replica1')
    expect(disks[0][1]).toContain('/var/lib/clickhouse/')
    expect(disks[0][1]).toContain('type: Local, name: default')
    expect(disks[0][4]).toContain('39%')
    const meters = w.findAll('[role=meter]')
    expect(meters.map((m) => m.attributes('aria-valuenow'))).toEqual(['39', '95']) // the disk with no size has no bar
    expect(meters[0].classes()).toContain('ok')
    expect(meters[1].classes()).toContain('bad')
    expect(disks[2][4]).toBe('–')
    expect(disks[2][1]).toContain('type: Local') // an empty type is shown as the default
    // a bucket has no size: no "-1 B", no bar, but what is stored on it
    expect(disks[3].slice(1)).toEqual(['gcstype: Object storage, name: gcs', 'unlimited', 'unlimited', '3 GiB stored'])
  })

  it('shows what the service warned about', async () => {
    const { w } = await mountTab({ info: { ...INFO, warnings: ['The sizes of the tables could not be read.'] } })
    expect(w.get('[role=note]').text()).toContain('could not be read')
  })

  it('shows a failure instead of a table, with the service\'s sentence', async () => {
    const { w } = await mountTab({ fail: 'The read key was not accepted.' })
    expect(w.get('[role=alert]').text()).toContain('not accepted')
    expect(w.find('table').exists()).toBe(false)
    expect(w.find('.status').exists()).toBe(false)
  })

  it('asks nothing and points to the connection page when there is no connection', async () => {
    const { w, client } = await mountTab({}, { session: session(null) })
    expect(client.get).not.toHaveBeenCalled()
    expect(w.text()).toContain('Not connected')
  })

  it('never renders anything that looks like a secret even if the service sent one by mistake', async () => {
    const { w } = await mountTab({ info: { ...INFO, connection: { ...INFO.connection, addr: 'ch.internal:9000', user: 'admin', password: 'hunter2' } } })
    expect(w.html()).not.toMatch(/ch\.internal|admin|hunter2/)
  })
})
