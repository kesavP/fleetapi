// What the build says about itself: which version it is, which commit it came from, when it was built. Read by vite.config.js from the
// environment at BUILD time (the Docker build passes APP_VERSION, APP_COMMIT and APP_BUILT; the Makefile fills them from the image tag, git and
// the clock), so the running page and /version.json report what was built, not what is configured where it runs.
//
// Pure, and strict about what it takes: these values end up in the page and in a JSON file, and come from a command line, so a value that is not
// the shape of a version (or a commit, or a time) is replaced by a plain marker instead of being shown.

const VERSION = /^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$/
const COMMIT = /^[0-9a-f]{7,40}(-dirty)?$/
const BUILT = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?Z$/

/**
 * -> {version, commit, built}. Not given: version "dev", no commit, built now. Given but not valid: version "unknown", no commit, no time:
 * "this was built, but the value is not usable" is different from "this is a development build", and both are different from a real version.
 */
export function buildInfo(env = {}, now = () => new Date()) {
  const given = (k) => typeof env[k] === 'string' && env[k].trim() !== ''
  const v = given('APP_VERSION') ? env.APP_VERSION.trim() : ''
  const c = given('APP_COMMIT') ? env.APP_COMMIT.trim() : ''
  const b = given('APP_BUILT') ? env.APP_BUILT.trim() : ''
  return {
    version: !v ? 'dev' : VERSION.test(v) ? v : 'unknown',
    commit: COMMIT.test(c) ? c : '',
    built: !b ? now().toISOString() : BUILT.test(b) && Number.isFinite(Date.parse(b)) ? b : '',
  }
}

/** The content of /version.json. */
export const versionFile = (info) => JSON.stringify({ name: 'telemetry-ui', ...info }, null, 2) + '\n'
