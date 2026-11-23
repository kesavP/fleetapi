// Where the project name and the read key live while the page is open.
//
// The key is a credential, so it is kept in sessionStorage: it is gone when the tab closes, it is not shared
// with other tabs, and it is never written to localStorage or put in a URL. If the browser refuses storage
// (some private modes) the session falls back to memory and simply does not survive a reload.

const PROJECT = 'fleet-ui.project'
const KEY = 'fleet-ui.key'

function browserStorage() {
  try {
    return window.sessionStorage
  } catch {
    return null
  }
}

export function createSession(storage = browserStorage()) {
  let memory = null
  const listeners = new Set()
  const changed = () => listeners.forEach((f) => f())

  return {
    /** Calls `fn` whenever the project or key is set or cleared (not with the key); returns a function that stops it. */
    subscribe(fn) {
      listeners.add(fn)
      return () => listeners.delete(fn)
    },
    /** {project, key}, or null when not connected. */
    get() {
      if (memory) return memory
      try {
        const project = storage && storage.getItem(PROJECT)
        const key = storage && storage.getItem(KEY)
        return project && key ? { project, key } : null
      } catch {
        return null
      }
    },
    set({ project, key }) {
      memory = { project, key }
      try {
        if (storage) {
          storage.setItem(PROJECT, project)
          storage.setItem(KEY, key)
        }
      } catch {
        // storage refused: memory still holds it for this page
      }
      changed()
    },
    clear() {
      memory = null
      try {
        if (storage) {
          storage.removeItem(PROJECT)
          storage.removeItem(KEY)
        }
      } catch {
        // nothing to clear
      }
      changed()
    },
  }
}
