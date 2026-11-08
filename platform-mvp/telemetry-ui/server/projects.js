import { readFileSync } from 'node:fs'
import { createHash, timingSafeEqual } from 'node:crypto'

// The projects and their READ keys, from the file the hub chart already writes for telemetry-ingest (`projects.json`, which also
// holds the ingest keys). Only names and read keys are taken from it, and a key is never logged or returned.

const NAME = /^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$/

/** [{name, readKeys: [..]}], skipping entries that are malformed (a project with no read key cannot be evaluated or asked about). */
export function loadProjects(path, read = (p) => readFileSync(p, 'utf8')) {
  const data = JSON.parse(read(path))
  const out = []
  for (const p of Array.isArray(data && data.projects) ? data.projects : []) {
    if (!p || typeof p.name !== 'string' || !NAME.test(p.name)) continue
    const keys = (Array.isArray(p.readKeys) ? p.readKeys : []).filter((k) => typeof k === 'string' && k.length > 0)
    if (keys.length) out.push({ name: p.name, readKeys: keys })
  }
  return out
}

const digest = (s) => createHash('sha256').update(String(s)).digest()

/** True when `key` is one of the project's read keys. Compared in constant time, so the answer's timing says nothing about the key. */
export function keyMatches(project, key) {
  if (!key) return false
  const given = digest(key)
  let ok = false
  for (const k of project.readKeys) ok = timingSafeEqual(given, digest(k)) || ok
  return ok
}
