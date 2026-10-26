import assert from 'node:assert/strict'
import { test } from 'node:test'

import { cacheableImages, compareTags, firstSignedRelease, imagesKey, jobFailed, jobMismatch, listJobsReason, missingBundle, normalizeTag, parseEnv, parseSums, platform, retryDelay, shouldUpload, signerIdentity, splitArgs, startArgs, jobStatusFailed, unknownNotice } from './lib.js'

test('platform', () => {
  assert.deepEqual(platform('linux', 'arm64'), { os: 'linux', arch: 'arm64', asset: 'gcpemu-linux-arm64' })
  assert.equal(platform('darwin', 'x64').asset, 'gcpemu-darwin-amd64')
  assert.throws(() => platform('win32', 'x64'), /no release for win32/)
})

test('normalizeTag', () => {
  assert.equal(normalizeTag('1.2.0'), 'v1.2.0')
  assert.equal(normalizeTag('v1.2.0'), 'v1.2.0')
  assert.equal(normalizeTag(' v0.0.0-selftest '), 'v0.0.0-selftest')
})

test('parseSums', () => {
  const a = 'a'.repeat(64)
  const b = 'b'.repeat(64)
  assert.deepEqual(parseSums(`${a}  gcpemu-linux-amd64\n${b} *dist/gcpemu-linux-arm64\njunk\n`), {
    'gcpemu-linux-amd64': a,
    'gcpemu-linux-arm64': b,
  })
})

test('startArgs', () => {
  const base = { instance: 'default', dataDir: '/tmp/d', waitTimeout: '120s' }
  assert.deepEqual(startArgs({ ...base, services: 'gcs, pubsub', seed: 's.yaml', config: 'c.yaml', extra: '--iam-mode enforce' }), [
    'start', '--detach', '--instance', 'default', '--data-dir', '/tmp/d', '--wait-timeout', '120s',
    '--services', 'gcs,pubsub', '--seed', 's.yaml', '--config', 'c.yaml', '--port', '0', '--iam-mode', 'enforce',
  ])
  assert.ok(!startArgs({ ...base, extra: '--port gateway=4510' }).includes('0'))
  assert.ok(!startArgs({ ...base, extra: '--port=gateway=4510' }).includes('0'))
})

test('splitArgs', () => {
  assert.deepEqual(splitArgs(` --a "b c" 'd e'  f `), ['--a', 'b c', 'd e', 'f'])
  assert.deepEqual(splitArgs(''), [])
})

test('parseEnv', () => {
  assert.deepEqual(parseEnv('A=1\nB=x=y\n\n'), { A: '1', B: 'x=y' })
})

test('imagesKey', () => {
  const d = 'f'.repeat(64)
  const k1 = imagesKey({ os: 'linux', arch: 'amd64', digest: d, services: 'sql,gke' })
  const k2 = imagesKey({ os: 'linux', arch: 'amd64', digest: d, services: 'gke, sql' })
  assert.equal(k1.key, k2.key)
  assert.ok(k1.key.startsWith(k1.prefix))
  assert.notEqual(k1.key, imagesKey({ os: 'linux', arch: 'amd64', digest: d, services: '' }).key)
})

test('jobFailed', () => {
  const jobs = [
    { status: 'completed', runner_name: 'r1', steps: [{ conclusion: 'failure' }] },
    { status: 'in_progress', runner_name: 'r2', steps: [{ conclusion: 'success' }, { conclusion: null }] },
    { status: 'in_progress', runner_name: 'r3', steps: [{ conclusion: 'success' }, { conclusion: 'failure' }] },
  ]
  assert.equal(jobFailed(jobs, 'r2'), false)
  assert.equal(jobFailed(jobs, 'r3'), true)
  assert.equal(jobFailed(jobs, 'r1'), null)
  assert.equal(jobFailed([], 'r2'), null)
})

test('jobStatusFailed', () => {
  // The runner's job.status as the post step sees it.
  assert.equal(jobStatusFailed('success'), false)
  assert.equal(jobStatusFailed('failure'), true)
  assert.equal(jobStatusFailed('cancelled'), true)
  assert.equal(jobStatusFailed(' Failure '), true)
  // Unset, or an unevaluated expression on a runner without it: unknown.
  assert.equal(jobStatusFailed(''), null)
  assert.equal(jobStatusFailed(undefined), null)
  assert.equal(jobStatusFailed('${{ job.status }}'), null)
})

test('retryDelay', () => {
  // 5xx, 429 and errors back off 1 s then 2 s, for 3 attempts in all.
  assert.equal(retryDelay(1, { status: 502 }), 1000)
  assert.equal(retryDelay(2, { status: 504 }), 2000)
  assert.equal(retryDelay(3, { status: 503 }), null)
  assert.equal(retryDelay(1, { status: 429 }), 1000)
  assert.equal(retryDelay(1, { error: 'fetch failed' }), 1000)
  assert.equal(retryDelay(2, { error: 'The operation was aborted due to timeout' }), 2000)
  assert.equal(retryDelay(3, { error: 'fetch failed' }), null)
  // Access errors and other client errors are final.
  for (const status of [400, 401, 403, 404, 422]) assert.equal(retryDelay(1, { status }), null)
  // Retry-After, in seconds or as a date; beyond 10 s it ends the retries.
  assert.equal(retryDelay(1, { status: 429, retryAfter: '3' }), 3000)
  assert.equal(retryDelay(1, { status: 503, retryAfter: '0' }), 0)
  assert.equal(retryDelay(1, { status: 429, retryAfter: '60' }), null)
  const now = Date.parse('2026-10-13T12:00:00Z')
  assert.equal(retryDelay(1, { status: 503, retryAfter: 'Tue, 13 Oct 2026 12:00:05 GMT' }, now), 5000)
  assert.equal(retryDelay(1, { status: 503, retryAfter: 'Tue, 13 Oct 2026 11:00:00 GMT' }, now), 0)
  assert.equal(retryDelay(1, { status: 503, retryAfter: 'soon' }), 1000)
  assert.equal(retryDelay(3, { status: 429, retryAfter: '1' }), null)
})

test('jobMismatch', () => {
  const jobs = [
    { name: 'a', status: 'in_progress', runner_name: 'GitHub Actions 1' },
    { name: 'b', status: 'in_progress', runner_name: 'GitHub Actions 2' },
    { name: 'c', status: 'completed', runner_name: 'GitHub Actions 3' },
  ]
  assert.equal(jobMismatch(jobs, 'GitHub Actions 3'),
    'RUNNER_NAME "GitHub Actions 3"; jobs on it: c (completed); in progress: a on "GitHub Actions 1", b on "GitHub Actions 2"')
  assert.equal(jobMismatch([], 'r'), 'RUNNER_NAME "r"; jobs on it: none; in progress: none')
})

test('listJobsReason', () => {
  for (const status of [401, 403, 404]) assert.deepEqual(listJobsReason({ status, attempts: 1 }), { cause: 'access', detail: `HTTP ${status}` })
  assert.deepEqual(listJobsReason({ status: 502, attempts: 3 }), { cause: 'api', detail: 'HTTP 502 after 3 attempts' })
  assert.deepEqual(listJobsReason({ status: 422, attempts: 1 }), { cause: 'api', detail: 'HTTP 422 after 1 attempt' })
  assert.deepEqual(listJobsReason({ error: 'fetch failed', attempts: 3 }), { cause: 'api', detail: 'fetch failed after 3 attempts' })
})

test('unknownNotice', () => {
  const access = unknownNotice({ cause: 'access', detail: 'HTTP 403' })
  const api = unknownNotice({ cause: 'api', detail: 'HTTP 502 after 3 attempts' })
  const job = unknownNotice({ cause: 'job' })
  assert.equal(access, 'setup-gcpemu could not read this job\'s result (HTTP 403: grant "actions: read" to the token); uploading diagnostics')
  assert.equal(api, 'setup-gcpemu could not read this job\'s result (HTTP 502 after 3 attempts); uploading diagnostics')
  assert.equal(job, 'setup-gcpemu could not identify this job among the run\'s jobs; uploading diagnostics')
  assert.match(unknownNotice({ cause: 'job', detail: 'RUNNER_NAME is not set' }), /among the run's jobs \(RUNNER_NAME is not set\)/)
  // Only a refused request blames the token.
  assert.ok(!api.includes('actions: read') && !job.includes('actions: read'))
})

test('shouldUpload', () => {
  assert.equal(shouldUpload('failure', true), true)
  assert.equal(shouldUpload('failure', false), false)
  assert.equal(shouldUpload('failure', null), true)
  assert.equal(shouldUpload('always', false), true)
  assert.equal(shouldUpload('never', true), false)
  assert.throws(() => shouldUpload('sometimes', true))
})

test('cacheableImages', () => {
  assert.deepEqual(
    cacheableImages(['rancher/k3s:v1', ''], ['postgres:16.14-alpine', 'ubuntu:24.04', 'rancher/k3s:v1', '<none>:<none>']),
    ['postgres:16.14-alpine', 'rancher/k3s:v1'],
  )
  // Images on the runner before the action are left to their own instance.
  assert.deepEqual(cacheableImages([], ['postgres:17.10-alpine', 'rancher/k3s:v2'], ['postgres:17.10-alpine']), ['rancher/k3s:v2'])
})

test('signerIdentity', () => {
  const { pattern, ...id } = signerIdentity('v0.10.0')
  assert.deepEqual(id, {
    uri: 'https://github.com/linuxuser586/gcpemu/.github/workflows/release.yml@refs/tags/v0.10.0',
    issuer: 'https://token.actions.githubusercontent.com',
  })
  // sigstore-js tests the SAN as an unanchored regexp: the pattern matches
  // only this tag's identity, not another tag's that contains it.
  const re = new RegExp(pattern)
  assert.ok(re.test('https://github.com/linuxuser586/gcpemu/.github/workflows/release.yml@refs/tags/v0.10.0'))
  assert.ok(!re.test('https://github.com/linuxuser586/gcpemu/.github/workflows/release.yml@refs/tags/v0.10.0-rc.1'))
  assert.ok(!re.test('https://github.com/linuxuser586/gcpemu/.github/workflows/releaseXyml@refs/tags/v0.10.0'))
  assert.ok(!re.test('https://github.com/fork/x/https://github.com/linuxuser586/gcpemu/.github/workflows/release.yml@refs/tags/v0.10.0'))
})

test('compareTags', () => {
  assert.equal(compareTags('v0.10.0', 'v0.10.0'), 0)
  assert.ok(compareTags('v0.9.0', 'v0.10.0') < 0)
  assert.ok(compareTags('v0.10.1', 'v0.10.0') > 0)
  assert.ok(compareTags('v1.0.0', 'v0.10.0') > 0)
  assert.ok(compareTags('v0.10.0-rc.1', 'v0.10.0') < 0)
  assert.ok(compareTags('v0.10.0-rc.2', 'v0.10.0-rc.10') < 0)
  assert.ok(compareTags('v0.10.0-rc.1', 'v0.10.0-rc') > 0)
  assert.ok(compareTags('v0.10.0-alpha', 'v0.10.0-rc.1') < 0)
  assert.ok(compareTags('v0.10.0-1', 'v0.10.0-rc') < 0)
  assert.ok(compareTags('v0.0.0-selftest', 'v0.10.0') < 0)
  assert.ok(compareTags('v0.10.0+build.5', 'v0.10.0') === 0)
  assert.throws(() => compareTags('nightly', 'v0.10.0'), /not a semver tag/)
})

test('missingBundle', () => {
  assert.equal(firstSignedRelease, 'v0.10.0')
  assert.equal(missingBundle('v0.9.0'), 'warn')
  assert.equal(missingBundle('v0.10.0-rc.1'), 'warn')
  assert.equal(missingBundle('v0.0.0-selftest'), 'warn')
  assert.equal(missingBundle('v0.10.0'), 'fail')
  assert.equal(missingBundle('v0.10.1'), 'fail')
  assert.equal(missingBundle('v1.0.0'), 'fail')
  // A tag that isn't semver can't be placed before the cutoff.
  assert.equal(missingBundle('nightly'), 'fail')
})
