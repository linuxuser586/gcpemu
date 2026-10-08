// setup-gcpemu main step (FR-CI-001): install the verified binary, restore
// cached container images, start the instance detached and export its
// environment.

import * as cache from '@actions/cache'
import * as core from '@actions/core'
import * as exec from '@actions/exec'
import * as tc from '@actions/tool-cache'
import { createHash } from 'node:crypto'
import fs from 'node:fs'
import path from 'node:path'

import { imagesKey, normalizeTag, parseEnv, parseSums, platform, startArgs } from './lib.js'

const repo = 'linuxuser586/gcpemu'

async function run() {
  const useCache = core.getBooleanInput('cache')
  const instance = core.getInput('instance') || 'default'
  const services = core.getInput('services')
  const { os, arch, asset } = platform(process.platform, process.arch)
  const temp = process.env.RUNNER_TEMP || fs.mkdtempSync('/tmp/gcpemu-')

  let bin = core.getInput('binary')
  let digest
  if (bin) {
    bin = path.resolve(bin)
    digest = await sha256File(bin)
  } else {
    ;({ bin, digest } = await install({ os, arch, asset, temp, useCache }))
  }
  core.addPath(path.dirname(bin))
  core.saveState('bin', bin)
  core.setOutput('bin', bin)
  const version = (await output(bin, ['version'])).trim().replace(/^gcpemu\s+/, '')
  core.setOutput('version', version)
  core.info(`gcpemu ${version} (${bin})`)

  const docker = (await exec.exec('docker', ['info'], { silent: true, ignoreReturnCode: true }).catch(() => 1)) === 0
  if (useCache && docker) await restoreImages({ os, arch, digest, services, temp })

  const dataDir = path.join(temp, `gcpemu-${instance}`)
  core.saveState('dataDir', dataDir)
  core.saveState('instance', instance)
  core.saveState('docker', String(docker))
  core.exportVariable('GCPEMU_INSTANCE', instance)
  core.exportVariable('GCPEMU_DATA_DIR', dataDir)
  core.setOutput('data-dir', dataDir)

  const args = startArgs({
    instance,
    dataDir,
    services,
    seed: core.getInput('seed'),
    config: core.getInput('config'),
    waitTimeout: core.getInput('wait-timeout') || '120s',
    extra: core.getInput('args'),
  })
  core.saveState('started', 'true')
  const code = await exec.exec(bin, args, { ignoreReturnCode: true, env: { ...process.env, CI: 'true' } })
  if (code !== 0) {
    core.saveState('startFailed', 'true')
    throw new Error(`gcpemu start exited with ${code}; the diagnostics artifact has the log`)
  }

  // GITHUB_ENV unset: env prints KEY=value lines instead of appending.
  const env = parseEnv(await output(bin, ['env', '--shell', 'github', '--instance', instance, '--data-dir', dataDir], { GITHUB_ENV: '' }))
  for (const [k, v] of Object.entries(env)) core.exportVariable(k, v)
  core.setOutput('gateway', env.GCPEMU_GATEWAY || '')
  core.saveState('gateway', env.GCPEMU_GATEWAY || '')
  core.info(`exported ${Object.keys(env).sort().join(', ')}`)
}

// install downloads the release binary for the runner, verified against
// the release's SHA256SUMS, through the Actions cache.
async function install({ os, arch, asset, temp, useCache }) {
  const tag = await resolveTag(core.getInput('version') || 'latest')
  const base = `${core.getInput('download-url').replace(/\/+$/, '')}/${tag}`
  const sums = parseSums(await fetchText(`${base}/SHA256SUMS`))
  const digest = sums[asset]
  if (!digest) throw new Error(`${base}/SHA256SUMS has no ${asset}`)

  const dir = path.join(process.env.RUNNER_TOOL_CACHE || temp, 'gcpemu', digest.slice(0, 16))
  const bin = path.join(dir, 'gcpemu')
  const key = `gcpemu-bin-${os}-${arch}-${digest}`
  if (useCache && !fs.existsSync(bin)) await cache.restoreCache([dir], key).catch((e) => core.warning(`cache restore: ${e.message}`))
  if (fs.existsSync(bin) && (await sha256File(bin)) === digest) {
    core.info(`gcpemu ${tag} from cache`)
    return { bin, digest }
  }
  core.info(`downloading ${base}/${asset}`)
  const tmp = await tc.downloadTool(`${base}/${asset}`)
  const got = await sha256File(tmp)
  if (got !== digest) throw new Error(`${asset}: sha256 ${got}, SHA256SUMS says ${digest}`)
  fs.mkdirSync(dir, { recursive: true })
  fs.copyFileSync(tmp, bin)
  fs.chmodSync(bin, 0o755)
  if (useCache) await cache.saveCache([dir], key).catch((e) => core.warning(`cache save: ${e.message}`))
  return { bin, digest }
}

async function resolveTag(version) {
  if (version !== 'latest') return normalizeTag(version)
  const res = await fetch(`https://api.github.com/repos/${repo}/releases/latest`, {
    headers: { Accept: 'application/vnd.github+json', Authorization: `Bearer ${core.getInput('token')}` },
  })
  if (!res.ok) throw new Error(`latest release of ${repo}: HTTP ${res.status} (set version, or binary)`)
  return (await res.json()).tag_name
}

// restoreImages loads the images saved by an earlier successful job.
async function restoreImages({ os, arch, digest, services, temp }) {
  const { key, prefix } = imagesKey({ os, arch, digest, services })
  const dir = path.join(temp, 'gcpemu-images')
  core.saveState('imagesKey', key)
  core.saveState('imagesDir', dir)
  let hit
  try {
    hit = await cache.restoreCache([dir], key, [prefix])
  } catch (e) {
    core.warning(`image cache restore: ${e.message}`)
  }
  core.saveState('imagesHit', String(hit === key))
  const tar = path.join(dir, 'images.tar')
  if (hit && fs.existsSync(tar)) {
    core.info(`loading cached container images (${hit})`)
    await exec.exec('docker', ['load', '-q', '-i', tar], { ignoreReturnCode: true })
  }
}

async function fetchText(url) {
  let last
  for (let i = 0; i < 5; i++) {
    if (i > 0) await new Promise((r) => setTimeout(r, i * 2000))
    try {
      const res = await fetch(url, { redirect: 'follow', signal: AbortSignal.timeout(30000) })
      if (res.ok) return await res.text()
      last = new Error(`${url}: HTTP ${res.status}`)
      if (res.status === 404) break
    } catch (e) {
      last = new Error(`${url}: ${e.message}`)
    }
  }
  throw last
}

async function output(bin, args, env = {}) {
  const r = await exec.getExecOutput(bin, args, { silent: true, env: { ...process.env, ...env } })
  return r.stdout
}

function sha256File(file) {
  return new Promise((resolve, reject) => {
    const h = createHash('sha256')
    fs.createReadStream(file)
      .on('error', reject)
      .on('data', (d) => h.update(d))
      .on('end', () => resolve(h.digest('hex')))
  })
}

run().catch((e) => core.setFailed(e.message))
