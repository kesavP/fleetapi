import { reactive } from 'vue'

// Which category each application is in, for the "Move the app to a category" menu and the category filters.
//
// Every application starts in a category its namespace gives it (application, control-plane or monitoring: see api/applications.js), and
// can be moved to another or to one the user makes. The choices are kept in THIS BROWSER (localStorage, per project): they are the user's own
// way of organising what they look at, not data of the project, so they are not shared with other people or other browsers. The interface is
// small (categoryFor, move, add) so they can be kept somewhere shared later without touching the pages.
//
// A category name is lowercase letters, digits, spaces and dashes (it travels in the address as a comma-separated list, so no commas).

export const BUILT_IN = ['application', 'control-plane', 'monitoring']
export const MAX_CUSTOM = 12
export const MAX_NAME = 32
const KEY = 'fleet-ui.categories.' // + the project

/** The name as it will be kept (trimmed, lower case, one space between words), or '' when it is not a usable name. */
export function normalizeCategory(name) {
  const n = String(name ?? '').trim().toLowerCase().replace(/\s+/g, ' ')
  return /^[a-z0-9]([a-z0-9 -]{0,30}[a-z0-9])?$/.test(n) && n.length <= MAX_NAME ? n : ''
}

function browserStorage() {
  try {
    return window.localStorage
  } catch {
    return null
  }
}

export function createCategoryStore({ session, storage = browserStorage() } = {}) {
  const state = reactive({ project: '', assignments: {}, custom: [] })

  function load(project) {
    state.project = project
    state.assignments = {}
    state.custom = []
    if (!storage || !project) return
    try {
      const saved = JSON.parse(storage.getItem(KEY + project) || 'null')
      if (!saved) return
      const custom = (Array.isArray(saved.custom) ? saved.custom : []).filter((c) => typeof c === 'string').map(normalizeCategory).filter((n) => n && !BUILT_IN.includes(n))
      state.custom = [...new Set(custom)].slice(0, MAX_CUSTOM)
      const known = new Set([...BUILT_IN, ...state.custom])
      for (const [id, cat] of Object.entries(saved.assignments && typeof saved.assignments === 'object' ? saved.assignments : {})) {
        if (typeof cat === 'string' && known.has(cat)) state.assignments[id] = cat // one that names a category that is gone is dropped
      }
    } catch {
      // unreadable: start clean
    }
  }
  function save() {
    if (!storage || !state.project) return
    try {
      storage.setItem(KEY + state.project, JSON.stringify({ assignments: state.assignments, custom: state.custom }))
    } catch {
      // storage refused (private mode, full): the choices last for this visit only
    }
  }

  const sync = () => {
    const project = (session && session.get() ? session.get().project : '') || ''
    if (project !== state.project) load(project)
  }
  sync()
  if (session && session.subscribe) session.subscribe(sync)

  const all = () => [...BUILT_IN, ...state.custom]

  return {
    state,
    /** Every category there is: the built-in ones, then the user's. */
    all,
    /** The category of an application: the one it was moved to, else `fallback` (what its namespace gives it). */
    categoryFor: (id, fallback) => (state.assignments[id] && all().includes(state.assignments[id]) ? state.assignments[id] : fallback),
    /** Whether the user has moved it (as opposed to it being where its namespace put it). */
    moved: (id) => id in state.assignments,
    /**
     * Moves an application to a category. Moving it to the category its namespace would give it (`fallback`) is not a choice any more, so it
     * is forgotten. Returns false for a category that does not exist.
     */
    move(id, category, fallback) {
      if (!all().includes(category)) return false
      if (category === fallback) delete state.assignments[id]
      else state.assignments[id] = category
      save()
      return true
    },
    /** Makes a category. Returns its name (the existing one when it was there already) or '' when the name cannot be used or there are too many. */
    add(name) {
      const n = normalizeCategory(name)
      if (!n || BUILT_IN.includes(n)) return BUILT_IN.includes(n) ? n : ''
      if (!state.custom.includes(n)) {
        if (state.custom.length >= MAX_CUSTOM) return ''
        state.custom.push(n)
        save()
      }
      return n
    },
    /** Why a name cannot be used, or ''. */
    problem(name) {
      const n = normalizeCategory(name)
      if (!String(name ?? '').trim()) return 'Give the category a name.'
      if (!n) return `Use lowercase letters, digits, spaces and dashes, up to ${MAX_NAME} characters.`
      if (!BUILT_IN.includes(n) && !state.custom.includes(n) && state.custom.length >= MAX_CUSTOM) return `At most ${MAX_CUSTOM} categories of your own.`
      return ''
    },
  }
}
