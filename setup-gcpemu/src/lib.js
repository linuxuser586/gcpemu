// Pure helpers of setup-gcpemu (unit-tested in lib.test.js).

import { createHash } from 'node:crypto'

// platform maps Node's platform and arch to gcpemu's release asset suffix.
export function platform(nodePlatform, nodeArch) {
  const os = { linux: 'linux', darwin: 'darwin' }[nodePlatform]
  const arch = { x64: 'amd64', arm64: 'arm64' }[nodeArch]
  if (!os || !arch) {
    throw new Error(`gcpemu has no release for ${nodePlatform}/${nodeArch} (linux and darwin, amd64 and arm64)`)
  }
  return { os, arch, asset: `gcpemu-${os}-${arch}` }
}

// normalizeTag turns "1.2.0" into "v1.2.0"; other forms pass through.
export function normalizeTag(v) {
  v = v.trim()
  return /^\d+\.\d+/.test(v) ? `v${v}` : v
}

// parseSums parses a sha256sum file into {file: digest}.
export function parseSums(text) {
  const out = {}
  for (const line of text.split('\n')) {
    const m = line.trim().match(/^([0-9a-f]{64})\s+\*?(\S+)$/)
    if (m) out[m[2].replace(/^.*\//, '')] = m[1]
  }
  return out
}

// splitArgs splits whitespace-separated arguments, honouring single and
// double quotes.
export function splitArgs(s) {
  const out = []
  const re = /"([^"]*)"|'([^']*)'|(\S+)/g
  let m
  while ((m = re.exec(s)) !== null) out.push(m[1] ?? m[2] ?? m[3])
  return out
}

// startArgs builds the `gcpemu start` command line. Every listener picks a
// free port unless the extra args choose ports, so parallel jobs on one
// runner do not collide (FR-CI-005).
export function startArgs({ instance, dataDir, services, seed, config, waitTimeout, extra }) {
  const args = ['start', '--detach', '--instance', instance, '--data-dir', dataDir, '--wait-timeout', waitTimeout]
  if (services) args.push('--services', services.replace(/\s+/g, ''))
  if (seed) args.push('--seed', seed)
  if (config) args.push('--config', config)
  const more = splitArgs(extra || '')
  if (!more.some((a) => a === '--port' || a.startsWith('--port='))) args.push('--port', '0')
  return args.concat(more)
}

// parseEnv parses `gcpemu env --shell github` output (KEY=value lines).
export function parseEnv(text) {
  const out = {}
  for (const line of text.split('\n')) {
    const i = line.indexOf('=')
    if (i > 0) out[line.slice(0, i)] = line.slice(i + 1)
  }
  return out
}

// imagesKey is the container image cache key: per release digest,
// platform and service set.
export function imagesKey({ os, arch, digest, services }) {
  const set = (services || 'all').split(',').map((s) => s.trim()).filter(Boolean).sort().join(',')
  const h = createHash('sha256').update(set).digest('hex').slice(0, 12)
  return { key: `gcpemu-images-${os}-${arch}-${digest.slice(0, 16)}-${h}`, prefix: `gcpemu-images-${os}-${arch}-${digest.slice(0, 16)}-` }
}

// jobFailed reports whether the current job has a failed step, from the
// "list jobs for a workflow run attempt" response: true, false, or null
// when the job cannot be identified.
export function jobFailed(jobs, runnerName) {
  const mine = (jobs || []).filter((j) => j.status === 'in_progress' && j.runner_name === runnerName)
  if (mine.length !== 1) return null
  return (mine[0].steps || []).some((s) => s.conclusion === 'failure')
}

// jobMismatch describes why jobFailed found no single job for runnerName:
// the jobs on that runner, and the jobs in progress on any runner.
export function jobMismatch(jobs, runnerName) {
  const list = (js, fmt) => (js.length ? js.map(fmt).join(', ') : 'none')
  const on = (jobs || []).filter((j) => j.runner_name === runnerName)
  const running = (jobs || []).filter((j) => j.status === 'in_progress')
  return `RUNNER_NAME "${runnerName}"; jobs on it: ${list(on, (j) => `${j.name} (${j.status})`)}; in progress: ${list(running, (j) => `${j.name} on "${j.runner_name}"`)}`
}

// listJobsAttempts is how many times the post step tries each request
// listing the run's jobs.
export const listJobsAttempts = 3

// maxRetryAfter bounds a Retry-After wait; a longer one ends the retries,
// so the post step stays near listJobsAttempts request timeouts.
const maxRetryAfter = 10000

// retryDelay returns how many milliseconds to wait before retrying a jobs
// listing after its attempt-th try failed with an HTTP status or a thrown
// error (network error, timeout), or null to give up. 5xx, 429 and errors
// are retried with 1 s then 2 s backoff, or after Retry-After when given;
// other statuses (401/403/404) are not.
export function retryDelay(attempt, { status, error, retryAfter } = {}, now = Date.now()) {
  if (attempt >= listJobsAttempts) return null
  if (!error && status !== 429 && !(status >= 500 && status <= 599)) return null
  const after = retryAfterMs(retryAfter, now)
  if (after === null) return 1000 * 2 ** (attempt - 1)
  return after <= maxRetryAfter ? after : null
}

// retryAfterMs parses a Retry-After header (seconds or an HTTP date).
function retryAfterMs(v, now) {
  v = (v ?? '').trim()
  if (v === '') return null
  if (/^\d+$/.test(v)) return Number(v) * 1000
  const t = Date.parse(v)
  return Number.isNaN(t) ? null : Math.max(0, t - now)
}

// listJobsReason classifies why listing the run's jobs failed for good:
// "access" for 401/403/404 (a private repository without access gives
// 404), else "api" with the status or error and the attempts made.
export function listJobsReason({ status, error, attempts }) {
  if (!error && [401, 403, 404].includes(status)) return { cause: 'access', detail: `HTTP ${status}` }
  const what = error || `HTTP ${status}`
  return { cause: 'api', detail: `${what} after ${attempts} attempt${attempts === 1 ? '' : 's'}` }
}

// unknownNotice is the notice for a job result that could not be read,
// by the reason's cause: "access", "api" or "job" (this job is not among
// the run's jobs, or the runner's variables are missing).
export function unknownNotice({ cause, detail }) {
  switch (cause) {
    case 'access':
      return `setup-gcpemu could not read this job's result (${detail}: grant "actions: read" to the token); uploading diagnostics`
    case 'api':
      return `setup-gcpemu could not read this job's result (${detail}); uploading diagnostics`
    default:
      return `setup-gcpemu could not identify this job among the run's jobs${detail ? ` (${detail})` : ''}; uploading diagnostics`
  }
}

// shouldUpload decides on the diagnostics artifact; an unknown job result
// uploads, since diagnostics of a failure matter more than a stray artifact.
export function shouldUpload(mode, failed) {
  switch (mode) {
    case 'always':
      return true
    case 'never':
      return false
    case 'failure':
      return failed !== false
    default:
      throw new Error(`diagnostics: "${mode}" is not failure, always or never`)
  }
}

// cacheableImages returns the images to cache: those of the instance's
// containers plus local images from the registries gcpemu pulls from that
// were not on the runner before the action ran (before), so a second
// instance does not cache the first one's images.
export function cacheableImages(containerImages, localImages, before = []) {
  const prefixes = ['rancher/', 'docker.io/rancher/', 'postgres:', 'docker.io/library/postgres:']
  const had = new Set(before)
  const set = new Set(containerImages.filter(Boolean))
  for (const ref of localImages) {
    if (!ref.includes('<none>') && !had.has(ref) && prefixes.some((p) => ref.startsWith(p))) set.add(ref)
  }
  return [...set].sort()
}

// signerIdentity is the Sigstore certificate identity a Signed release's
// SHA256SUMS bundle must carry (NFR-SEC-005): this repository's release
// workflow at that same tag, issued by GitHub Actions. sigstore-js tests
// the identity as an unanchored regexp, so pattern is the URI escaped and
// anchored.
export function signerIdentity(tag) {
  const uri = `https://github.com/linuxuser586/gcpemu/.github/workflows/release.yml@refs/tags/${tag}`
  return {
    uri,
    pattern: `^${uri.replace(/[.*+?^${}()|[\]\\/]/g, '\\$&')}$`,
    issuer: 'https://token.actions.githubusercontent.com',
  }
}

// compareTags orders two vX.Y.Z[-pre][+build] tags by semver precedence:
// negative, zero or positive. A prerelease sorts before its release.
export function compareTags(a, b) {
  const pa = semver(a)
  const pb = semver(b)
  for (let i = 0; i < 3; i++) if (pa.core[i] !== pb.core[i]) return pa.core[i] - pb.core[i]
  if (!pa.pre.length || !pb.pre.length) return pb.pre.length - pa.pre.length
  for (let i = 0; i < Math.min(pa.pre.length, pb.pre.length); i++) {
    const x = pa.pre[i]
    const y = pb.pre[i]
    if (x === y) continue
    const nx = /^\d+$/.test(x)
    const ny = /^\d+$/.test(y)
    if (nx && ny) return Number(x) - Number(y)
    if (nx !== ny) return nx ? -1 : 1
    return x < y ? -1 : 1
  }
  return pa.pre.length - pb.pre.length
}

function semver(tag) {
  const m = /^v?(\d+)\.(\d+)\.(\d+)(?:-([0-9A-Za-z.-]+))?(?:\+[0-9A-Za-z.-]+)?$/.exec(tag)
  if (!m) throw new Error(`"${tag}" is not a semver tag (vX.Y.Z)`)
  return { core: [Number(m[1]), Number(m[2]), Number(m[3])], pre: m[4] ? m[4].split('.') : [] }
}

// firstSignedRelease is the first Signed release: every release from it on
// publishes SHA256SUMS.sigstore.json.
export const firstSignedRelease = 'v0.10.0'

// missingBundle decides what a release without a SHA256SUMS bundle gets:
// "warn" before the first Signed release, "fail" from it on, and "fail"
// for a tag that is not semver, which cannot be placed before it.
export function missingBundle(tag) {
  try {
    return compareTags(tag, firstSignedRelease) < 0 ? 'warn' : 'fail'
  } catch {
    return 'fail'
  }
}
