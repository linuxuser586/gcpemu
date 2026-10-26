// setup-gcpemu post step (FR-CI-002): on failure, upload the instance
// log, request log, resource dump, container logs and kubeconfig as an
// artifact; after a successful job, cache the container images; stop the
// instance.

import { DefaultArtifactClient } from '@actions/artifact'
import * as cache from '@actions/cache'
import * as core from '@actions/core'
import * as exec from '@actions/exec'
import { randomBytes } from 'node:crypto'
import fs from 'node:fs'
import path from 'node:path'

import { setTimeout as sleep } from 'node:timers/promises'

import { cacheableImages, jobFailed, jobMismatch, listJobsReason, retryDelay, shouldUpload, jobStatusFailed, unknownNotice } from './lib.js'

async function run() {
  const bin = core.getState('bin')
  if (!bin || core.getState('started') !== 'true') return
  const instance = core.getState('instance')
  const dataDir = core.getState('dataDir')
  const gateway = core.getState('gateway')
  const docker = core.getState('docker') === 'true'

  const { failed, reason } = core.getState('startFailed') === 'true' ? { failed: true } : await currentJobFailed()
  const containers = gateway ? ((await adminJSON(gateway, 'containers'))?.containers ?? []) : []

  if (shouldUpload(core.getInput('diagnostics') || 'failure', failed)) {
    if (failed === null) core.notice(unknownNotice(reason))
    await step('upload diagnostics', () => uploadDiagnostics({ bin, instance, dataDir, gateway, containers, docker }))
  }
  if (failed === false && docker && core.getBooleanInput('cache') && core.getState('imagesKey') && core.getState('imagesHit') !== 'true') {
    await step('cache container images', () => saveImages(containers))
  }
  await exec.exec(bin, ['stop', '--instance', instance, '--data-dir', dataDir], { ignoreReturnCode: true })
}

// currentJobFailed reports whether this job failed: { failed } with true
// or false, or { failed: null, reason } when the result is unknown (see
// unknownNotice). The runner's job.status comes first; the API is the
// fallback, since its job listing can lag the run.
async function currentJobFailed() {
  const failed = jobStatusFailed(core.getInput('job-status'))
  if (failed !== null) return { failed }
  return apiJobFailed()
}

// apiJobFailed asks the API whether a step of this job failed.
async function apiJobFailed() {
  const { GITHUB_API_URL, GITHUB_REPOSITORY, GITHUB_RUN_ID, GITHUB_RUN_ATTEMPT, RUNNER_NAME } = process.env
  if (!GITHUB_REPOSITORY || !GITHUB_RUN_ID || !RUNNER_NAME) {
    return { failed: null, reason: { cause: 'job', detail: 'GITHUB_REPOSITORY, GITHUB_RUN_ID or RUNNER_NAME is not set' } }
  }
  const jobs = []
  for (let page = 1; page <= 10; page++) {
    const url = `${GITHUB_API_URL || 'https://api.github.com'}/repos/${GITHUB_REPOSITORY}/actions/runs/${GITHUB_RUN_ID}/attempts/${GITHUB_RUN_ATTEMPT || 1}/jobs?per_page=100&page=${page}`
    const { body, reason } = await listJobs(url)
    if (reason) return { failed: null, reason }
    const got = body.jobs ?? []
    jobs.push(...got)
    if (jobs.length >= body.total_count || got.length === 0) break
  }
  const failed = jobFailed(jobs, RUNNER_NAME)
  return failed === null ? { failed, reason: { cause: 'job', detail: jobMismatch(jobs, RUNNER_NAME) } } : { failed }
}

// listJobs fetches one page of the run's jobs, retrying transient
// failures (retryDelay): { body } or { reason }.
async function listJobs(url) {
  for (let attempt = 1; ; attempt++) {
    let status, error, retryAfter
    try {
      const res = await fetch(url, {
        headers: { Accept: 'application/vnd.github+json', Authorization: `Bearer ${core.getInput('token')}` },
        signal: AbortSignal.timeout(10000),
      })
      if (res.ok) return { body: await res.json() }
      status = res.status
      retryAfter = res.headers.get('retry-after')
    } catch (e) {
      error = e.message
    }
    core.debug(`list jobs (attempt ${attempt}): ${error || `HTTP ${status}`}`)
    const delay = retryDelay(attempt, { status, error, retryAfter })
    if (delay === null) return { reason: listJobsReason({ status, error, attempts: attempt }) }
    await sleep(delay)
  }
}

async function uploadDiagnostics({ bin, instance, dataDir, gateway, containers, docker }) {
  const dir = path.join(process.env.RUNNER_TEMP || dataDir, `gcpemu-diagnostics-${instance}`)
  fs.rmSync(dir, { recursive: true, force: true })
  fs.mkdirSync(dir, { recursive: true })
  const write = (name, data) => {
    if (data === undefined || data === null) return
    fs.mkdirSync(path.dirname(path.join(dir, name)), { recursive: true })
    fs.writeFileSync(path.join(dir, name), typeof data === 'string' ? data : JSON.stringify(data, null, 2))
  }
  const copy = (from, name) => {
    if (fs.existsSync(from)) fs.copyFileSync(from, path.join(dir, name))
  }
  copy(path.join(dataDir, 'gcpemu.log'), 'gcpemu.log')
  copy(path.join(dataDir, 'kubeconfig'), 'kubeconfig')
  copy(path.join(dataDir, 'endpoints.json'), 'endpoints.json')
  if (gateway) {
    for (const what of ['info', 'ready', 'requests', 'resources', 'containers']) write(`${what}.json`, await adminJSON(gateway, what))
  }
  const status = await exec.getExecOutput(bin, ['status', '--instance', instance, '--data-dir', dataDir], { ignoreReturnCode: true, silent: true })
  write('status.txt', status.stdout + status.stderr)
  if (docker) {
    for (const c of containers) {
      const logs = await exec.getExecOutput('docker', ['logs', '--tail', '5000', c.name], { ignoreReturnCode: true, silent: true })
      write(`containers/${c.name}.log`, logs.stdout + logs.stderr)
    }
  }
  const files = listFiles(dir)
  const name = core.getInput('artifact-name') || defaultArtifactName(instance)
  const retentionDays = Number(core.getInput('retention-days') || 7)
  const { id, size } = await new DefaultArtifactClient().uploadArtifact(name, files, dir, { retentionDays })
  core.info(`uploaded diagnostics artifact "${name}" (id ${id}, ${size} bytes, ${files.length} files)`)
}

function defaultArtifactName(instance) {
  const os = { linux: 'linux', darwin: 'darwin' }[process.platform] || process.platform
  const arch = { x64: 'amd64', arm64: 'arm64' }[process.arch] || process.arch
  // Matrix jobs share GITHUB_JOB; the suffix keeps names unique in a run.
  return `gcpemu-${process.env.GITHUB_JOB || 'job'}-${instance}-${os}-${arch}-${randomBytes(3).toString('hex')}`
}

async function saveImages(containers) {
  const local = await exec.getExecOutput('docker', ['images', '--format', '{{.Repository}}:{{.Tag}}'], { silent: true, ignoreReturnCode: true })
  const images = cacheableImages(
    containers.map((c) => c.image),
    local.stdout.split('\n').map((s) => s.trim()).filter(Boolean),
    JSON.parse(core.getState('imagesBefore') || '[]'),
  )
  if (images.length === 0) return
  const dir = core.getState('imagesDir')
  fs.mkdirSync(dir, { recursive: true })
  const tar = path.join(dir, 'images.tar')
  const code = await exec.exec('docker', ['save', '-o', tar, ...images], { ignoreReturnCode: true })
  if (code !== 0) return
  await cache.saveCache([dir], core.getState('imagesKey'))
  core.info(`cached ${images.length} container images: ${images.join(' ')}`)
}

async function adminJSON(gateway, what) {
  try {
    const token = core.getState('adminToken')
    const headers = token ? { 'X-Gcpemu-Admin-Token': token } : {}
    const res = await fetch(`http://${gateway}/_emu/v1/${what}`, { headers, signal: AbortSignal.timeout(20000) })
    return await res.json()
  } catch (e) {
    core.debug(`${what}: ${e.message}`)
    return null
  }
}

function listFiles(dir) {
  return fs.readdirSync(dir, { recursive: true, withFileTypes: true })
    .filter((d) => d.isFile())
    .map((d) => path.join(d.parentPath, d.name))
}

async function step(what, fn) {
  try {
    await fn()
  } catch (e) {
    core.warning(`${what}: ${e.message}`)
  }
}

run().catch((e) => core.warning(e.message))
