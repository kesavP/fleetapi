import { describe, expect, it } from 'vitest'
import { hostOf, histogramQuantile, isLoopback, isServerError, parseLe, podOf, workloadOf } from './workload'

describe('workloadOf', () => {
  it.each([
    ['/k8s/shop/frontend-5d6f8c7b9-xk2lp/server', '/k8s/shop/frontend'], // Deployment
    ['/k8s/shop/cart-api-7c9d8f6b5d-bk7q2/app', '/k8s/shop/cart-api'], // a dash in the name
    ['/k8s/kube-system/kube-proxy-x7k2m/kube-proxy', '/k8s/kube-system/kube-proxy'], // DaemonSet
    ['/k8s/data/redis-0/redis', '/k8s/data/redis'], // StatefulSet
    ['/k8s-cronjob/ops/nightly/job', '/k8s-cronjob/ops/nightly'],
    ['/k8s/shop/standalone/app', '/k8s/shop/standalone'], // a bare pod
  ])('%s -> %s', (container, id) => {
    expect(workloadOf(container).id).toBe(id)
  })

  it('splits the namespace and the name', () => {
    expect(workloadOf('/k8s/shop/frontend-5d6f8c7b9-xk2lp/server')).toEqual({ id: '/k8s/shop/frontend', ns: 'shop', name: 'frontend', label: 'shop/frontend' })
  })

  it('is null for what is not a pod', () => {
    for (const c of ['/system.slice/sshd.service', '/docker/abc', '', undefined, '/k8s/shop']) expect(workloadOf(c)).toBeNull()
  })
})

describe('addresses', () => {
  it('finds the host', () => {
    expect(hostOf('10.0.0.1:80')).toBe('10.0.0.1')
    expect(hostOf('[fe80::1]:80')).toBe('fe80::1')
    expect(hostOf('db.example.com:5432')).toBe('db.example.com')
    expect(hostOf('::1')).toBe('::1')
  })
  it('recognises only real loopback', () => {
    for (const a of ['127.0.0.1:80', '127.9.9.9', '[::1]:80', '::ffff:127.0.0.1', 'localhost:5000']) expect(isLoopback(a)).toBe(true)
    for (const a of ['128.0.0.1:80', '10.0.0.127:80', '127.example.com:80', '1270.0.0.1']) expect(isLoopback(a)).toBe(false)
  })
})

describe('isServerError', () => {
  it('counts 5xx and the gRPC failures the server causes, not the ones the caller causes', () => {
    for (const s of ['500', '503', '5xx', 'grpc:INTERNAL', 'grpc:UNAVAILABLE', 'grpc:DEADLINE_EXCEEDED']) expect(isServerError(s)).toBe(true)
    for (const s of ['200', '2xx', '404', '4xx', 'grpc:NOT_FOUND', 'grpc:INVALID_ARGUMENT', 'grpc:CANCELLED', 'grpc:OK', '', undefined]) expect(isServerError(s)).toBe(false)
  })
})

describe('histogramQuantile', () => {
  const b = (o) => new Map(Object.entries(o).map(([k, v]) => [k === 'inf' ? Infinity : Number(k), v]))
  it('interpolates inside the bucket that holds the rank', () => {
    // 100 calls/s: 50 under 5 ms, 90 under 10 ms, all under 100 ms. p95 = rank 95, in (10 ms, 100 ms].
    expect(histogramQuantile(0.95, b({ 0.005: 50, 0.01: 90, 0.1: 100, inf: 100 }))).toBeCloseTo(0.055, 6)
    expect(histogramQuantile(0.5, b({ 0.005: 50, 0.01: 90, 0.1: 100, inf: 100 }))).toBeCloseTo(0.005, 6)
  })
  it('answers "at least the largest bound" when the rank is in the overflow bucket', () => {
    expect(histogramQuantile(0.95, b({ 0.1: 50, 1: 90, inf: 100 }))).toBe(1)
  })
  it('repairs buckets that dip, and returns null for nothing', () => {
    expect(histogramQuantile(0.5, b({ 0.01: 80, 0.1: 70, inf: 100 }))).not.toBeNull()
    expect(histogramQuantile(0.5, new Map())).toBeNull()
    expect(histogramQuantile(0.5, b({ 0.1: 0, inf: 0 }))).toBeNull()
  })
})

describe('parseLe', () => {
  it('reads numbers and the infinite bucket', () => {
    expect(parseLe('0.5')).toBe(0.5)
    expect(parseLe('+Inf')).toBe(Infinity)
    expect(parseLe('Inf')).toBe(Infinity)
    expect(parseLe('x')).toBeNaN()
    expect(parseLe(undefined)).toBeNaN()
  })
})

describe('podOf', () => {
  it('names the pod, so containers of one pod are one', () => {
    expect(podOf('/k8s/shop/frontend-5d6f8c7b9-xk2lp/server')).toBe('/k8s/shop/frontend-5d6f8c7b9-xk2lp')
    expect(podOf('/k8s/shop/frontend-5d6f8c7b9-xk2lp/istio-proxy')).toBe('/k8s/shop/frontend-5d6f8c7b9-xk2lp')
    expect(podOf('/k8s/data/redis-0/redis')).toBe('/k8s/data/redis-0')
    expect(podOf('/k8s-cronjob/ops/nightly/job')).toBe('/k8s-cronjob/ops/nightly')
  })
  it('is null for what is not a pod', () => {
    for (const c of ['/system.slice/sshd.service', '', undefined, '/k8s/shop']) expect(podOf(c)).toBeNull()
  })
})
