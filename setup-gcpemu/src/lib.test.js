import assert from 'node:assert/strict'
import { test } from 'node:test'

import { cacheableImages, imagesKey, jobFailed, normalizeTag, parseEnv, parseSums, platform, shouldUpload, splitArgs, startArgs } from './lib.js'

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
})
