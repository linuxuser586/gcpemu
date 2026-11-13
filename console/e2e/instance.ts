// Starts an Ephemeral Instance from bin/gcpemu (built with the real console
// by `just build`): the shared one Playwright's webServer runs
// (emulator.ts), and private ones for tests that change all of an
// Instance's state (state.spec.ts) or need their Request log entries kept
// (requests.spec.ts), or link with each other (links.spec.ts).
//
// Each Instance runs in a Test data dir under testDirRoot whose owner lock
// bin/gcpemu-testdir holds for this process (ADR 0076). Nothing here
// relies on running when the run ends: on Windows, Playwright ends the
// webServer's process tree with taskkill /T /F. The lock then drops with
// the helper, and the next run's reclaimEnded, `gcpemu prune` or an
// emutest sweep reclaims the dir and its Instance (#1035).
import { spawn, spawnSync, type ChildProcess } from 'node:child_process'
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync } from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { createInterface } from 'node:readline'

const exe = process.platform === 'win32' ? '.exe' : ''
export const bin =
  process.env.GCPEMU_BIN ?? path.resolve(import.meta.dirname, `../../bin/gcpemu${exe}`)
const testdirBin =
  process.env.GCPEMU_TESTDIR_BIN ??
  path.resolve(import.meta.dirname, `../../bin/gcpemu-testdir${exe}`)

/** testDirRoot holds the Test data dirs of the console e2e runs. */
export const testDirRoot = path.join(os.tmpdir(), 'gcpemu-console-e2e')

// How long stop waits for `gcpemu stop`, then for the Instance to exit
// once killed, and how long the run-start reclaim may take: gcpemu-testdir
// gives up after 45 s, inside the time playwright.config.ts gives the
// webServer to start.
const stopTimeout = 60_000
const killTimeout = 10_000
const reclaimTimeout = 60_000

// heldLine is the line `gcpemu-testdir hold` prints once it holds the lock.
const heldLine = 'held'

export interface Instance {
  /** url is the gateway's, e.g. http://127.0.0.1:4562. */
  url: string
  /** dir is the instance dir. */
  dir: string
  /** link is its --link-bind listener's address, when it has one. */
  link?: string
  stop: () => Promise<void>
}

/** TestDir is a Test data dir whose owner lock this process holds. */
export interface TestDir {
  dir: string
  /** release lets go of the lock. */
  release: () => Promise<void>
}

/**
 * newTestDir makes a Test data dir under testDirRoot and takes its owner
 * lock through `gcpemu-testdir hold`, which keeps it until its stdin, a
 * pipe from this process, closes.
 */
export async function newTestDir(): Promise<TestDir> {
  mkdirSync(testDirRoot, { recursive: true })
  const dir = mkdtempSync(path.join(testDirRoot, 'data-'))
  const helper = spawn(testdirBin, ['hold', dir], { stdio: ['pipe', 'pipe', 'inherit'] })
  const exited = new Promise<void>((resolve) => helper.once('exit', () => resolve()))
  const held = await new Promise<boolean>((resolve) => {
    createInterface({ input: helper.stdout }).once('line', (line) =>
      resolve(line.trim() === heldLine),
    )
    helper.once('exit', () => resolve(false))
    helper.once('error', () => resolve(false))
  })
  if (!held) {
    rmSync(dir, { recursive: true, force: true })
    throw new Error(
      `${testdirBin} could not hold the owner lock of ${dir} (just console-e2e builds it)`,
    )
  }
  let released = false
  const release = () => {
    if (!released) helper.stdin.end()
    released = true
    return exited
  }
  return { dir, release }
}

/**
 * reclaimEnded reclaims the Test data dirs of ended runs, with their
 * Instances, that a killed run left behind. It never fails the run: a
 * failure is logged.
 */
export function reclaimEnded() {
  const r = spawnSync(testdirBin, ['reclaim', testDirRoot], {
    encoding: 'utf8',
    timeout: reclaimTimeout,
  })
  if (r.error || r.status !== 0) {
    console.warn(
      `warning: reclaiming ended console e2e runs: ${r.error?.message ?? r.stderr.trim()}`,
    )
  }
}

/** runGcpemu runs gcpemu with args and resolves once it exits, or at timeout ms. */
function runGcpemu(args: string[], env: NodeJS.ProcessEnv, timeout: number) {
  return new Promise<void>((resolve) => {
    const p = spawn(bin, args, { stdio: ['ignore', 'ignore', 'inherit'], env, timeout })
    p.once('exit', () => resolve())
    p.once('error', () => resolve())
  })
}

/** within resolves true once p settles, or false after ms. */
function within(p: Promise<unknown>, ms: number) {
  return new Promise<boolean>((resolve) => {
    const timer = setTimeout(() => resolve(false), ms)
    void p.then(() => {
      clearTimeout(timer)
      resolve(true)
    })
  })
}

/**
 * stopInstance ends child, the gcpemu Instance in testDir: through
 * `gcpemu stop`, so that it removes what it made, then with a kill. It
 * then lets go of testDir's lock and removes testDir, unless the Instance
 * did not exit cleanly: whatever it left, its dir records, so the next
 * run's reclaim removes both (ADR 0076).
 */
export async function stopInstance(
  child: ChildProcess,
  testDir: TestDir,
  env: NodeJS.ProcessEnv = process.env,
) {
  // A child that never started (child.pid unset) emits no exit.
  const running = child.pid !== undefined && child.exitCode === null && child.signalCode === null
  if (running) {
    const exited = new Promise<void>((resolve) => child.once('exit', () => resolve()))
    await runGcpemu(['stop', '--data-dir', testDir.dir], env, stopTimeout)
    if (!(await within(exited, killTimeout))) {
      child.kill('SIGKILL')
      await within(exited, killTimeout)
    }
  }
  await testDir.release()
  if (child.exitCode !== 0) return
  try {
    // Windows may hold the dir's files open for a moment after the exit.
    rmSync(testDir.dir, { recursive: true, force: true, maxRetries: 5 })
  } catch (e) {
    console.warn(`warning: removing ${testDir.dir}: ${(e as Error).message}`)
  }
}

/**
 * startInstance runs `gcpemu start` with services and extra flags, on
 * dynamic ports in a new Test data dir, and resolves once it is ready.
 * onExit is called if it exits before stop.
 */
export async function startInstance(
  services: string[],
  extra: string[] = [],
  opts: { env?: NodeJS.ProcessEnv; onExit?: (code: number | null) => void } = {},
): Promise<Instance> {
  const testDir = await newTestDir()
  const dir = testDir.dir
  const env = { ...process.env, ...opts.env }
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
    { stdio: ['ignore', 'inherit', 'inherit'], env },
  )
  let stopped: Promise<void> | undefined
  child.once('exit', (code) => {
    if (!stopped) opts.onExit?.(code)
  })
  const stop = () => {
    stopped ??= stopInstance(child, testDir, env)
    return stopped
  }

  const endpoints = path.join(dir, 'endpoints.json')
  const deadline = Date.now() + 60_000
  while (Date.now() < deadline) {
    await new Promise((r) => setTimeout(r, 100))
    if (child.exitCode !== null) break
    if (!existsSync(endpoints)) continue
    const { gateway, link } = JSON.parse(readFileSync(endpoints, 'utf8')) as {
      gateway?: string
      link?: string
    }
    if (!gateway) continue
    const ready = await fetch(`http://${gateway}/_emu/v1/ready`).catch(() => undefined)
    if (ready?.ok) return { url: `http://${gateway}`, dir, link, stop }
  }
  await stop()
  throw new Error('gcpemu did not become ready within 60 s')
}
