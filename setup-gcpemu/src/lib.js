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
