// Starts an Ephemeral Instance from bin/gcpemu (built with the real console
// by `task build`): the shared one Playwright's webServer runs
// (emulator.ts), and private ones for tests that change all of an
// Instance's state (state.spec.ts).
import { spawn } from 'node:child_process'
import { existsSync, mkdtempSync, readFileSync, rmSync } from 'node:fs'
import os from 'node:os'
import path from 'node:path'

const exe = process.platform === 'win32' ? '.exe' : ''
const bin = process.env.GCPEMU_BIN ?? path.resolve(import.meta.dirname, `../../bin/gcpemu${exe}`)

export interface Instance {
  /** url is the gateway's, e.g. http://127.0.0.1:4510. */
  url: string
  /** dir is the instance dir. */
  dir: string
  stop: () => Promise<void>
}

/**
 * startInstance runs `gcpemu start` with services and extra flags, on
 * dynamic ports in a new temporary instance dir, and resolves once it is
 * ready. onExit is called if it exits before stop.
 */
export async function startInstance(
  services: string[],
  extra: string[] = [],
  opts: { env?: NodeJS.ProcessEnv; onExit?: (code: number | null) => void } = {},
): Promise<Instance> {
  const dir = mkdtempSync(path.join(os.tmpdir(), 'gcpemu-console-e2e-'))
  // --console: CI=true turns the console off by default (FR-CI-003).
  const child = spawn(
    bin,
    [
      'start',
      '--ephemeral',
      '--services',
      services.join(','),
      ...extra,
      '--port',
      '0',
      '--console',
      '--log-level',
      'warn',
      '--data-dir',
      dir,
    ],
    { stdio: ['ignore', 'inherit', 'inherit'], env: { ...process.env, ...opts.env } },
  )
  let stopping = false
  const exited = new Promise<void>((resolve) =>
    child.once('exit', (code) => {
      rmSync(dir, { recursive: true, force: true })
      if (!stopping) opts.onExit?.(code)
      resolve()
    }),
  )
  const stop = async () => {
    stopping = true
    child.kill('SIGTERM')
    await exited
  }

  const endpoints = path.join(dir, 'endpoints.json')
  const deadline = Date.now() + 60_000
  while (Date.now() < deadline) {
    await new Promise((r) => setTimeout(r, 100))
    if (child.exitCode !== null) break
    if (!existsSync(endpoints)) continue
    const { gateway } = JSON.parse(readFileSync(endpoints, 'utf8')) as { gateway?: string }
    if (!gateway) continue
    const ready = await fetch(`http://${gateway}/_emu/v1/ready`).catch(() => undefined)
    if (ready?.ok) return { url: `http://${gateway}`, dir, stop }
  }
  await stop()
  throw new Error('gcpemu did not become ready within 60 s')
}
