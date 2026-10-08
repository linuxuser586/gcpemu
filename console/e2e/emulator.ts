// Runs an Ephemeral Instance from bin/gcpemu (built with the real console
// by `make build`) for the Playwright tests. Playwright's webServer starts
// this script and reads the gateway URL from the line it prints once the
// Instance is ready.
import { spawn } from 'node:child_process'
import { existsSync, mkdtempSync, readFileSync, rmSync } from 'node:fs'
import os from 'node:os'
import path from 'node:path'

const bin = process.env.GCPEMU_BIN ?? path.resolve(import.meta.dirname, '../../bin/gcpemu')
const dir = mkdtempSync(path.join(os.tmpdir(), 'gcpemu-console-e2e-'))
// --console: CI=true turns the console off by default (FR-CI-003).
// Compute Operations take 2 s, long enough to watch them
// run (FR-UI-012); compute needs no container runtime either. GKE runs
// on Linux only; its clusters need a container runtime, so the GKE view
// tests that create them skip without one (gke.spec.ts). Each browser
// runs a cluster of up to 2 nodes, beyond the default limit of 5.
const services = ['gcs', 'pubsub', 'compute', ...(process.platform === 'linux' ? ['gke'] : [])]
const child = spawn(
  bin,
  [
    'start',
    '--ephemeral',
    '--services',
    services.join(','),
    '--lro-latency',
    'compute=2s',
    '--port',
    '0',
    '--console',
    '--log-level',
    'warn',
    '--data-dir',
    dir,
  ],
  {
    stdio: ['ignore', 'inherit', 'inherit'],
    env: { ...process.env, GCPEMU_GKE_MAX_NODES: process.env.GCPEMU_GKE_MAX_NODES ?? '12' },
  },
)

let exiting = false
function stop(code: number) {
  if (exiting) return
  exiting = true
  child.kill('SIGTERM')
  child.once('exit', () => {
    rmSync(dir, { recursive: true, force: true })
    process.exit(code)
  })
}
process.on('SIGTERM', () => stop(0))
process.on('SIGINT', () => stop(0))
child.on('exit', (code) => {
  if (exiting) return
  console.error(`gcpemu exited (${code}) before the tests finished`)
  rmSync(dir, { recursive: true, force: true })
  process.exit(1)
})

const endpoints = path.join(dir, 'endpoints.json')
const deadline = Date.now() + 60_000
while (Date.now() < deadline) {
  await new Promise((r) => setTimeout(r, 100))
  if (!existsSync(endpoints)) continue
  const { gateway } = JSON.parse(readFileSync(endpoints, 'utf8')) as { gateway?: string }
  if (!gateway) continue
  const ready = await fetch(`http://${gateway}/_emu/v1/ready`).catch(() => undefined)
  if (ready?.ok) {
    console.log(`gcpemu ready at http://${gateway}`)
    break
  }
}
if (Date.now() >= deadline) {
  console.error('gcpemu did not become ready within 60 s')
  stop(1)
}
