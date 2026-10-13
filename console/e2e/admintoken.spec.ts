import { spawn, type ChildProcess } from 'node:child_process'
import { mkdtempSync, rmSync } from 'node:fs'
import os from 'node:os'
import path from 'node:path'

import { expect, test } from './fixtures'

// FR-UI-009: beyond loopback the console needs the admin token. This file
// runs its own Instance bound to 0.0.0.0 and opens the console URL it
// prints, as a user would.

const exe = process.platform === 'win32' ? '.exe' : ''
const bin = process.env.GCPEMU_BIN ?? path.resolve(import.meta.dirname, `../../bin/gcpemu${exe}`)

let child: ChildProcess | undefined
let dir = ''
let consoleURL = ''

test.beforeAll(async () => {
  dir = mkdtempSync(path.join(os.tmpdir(), 'gcpemu-console-e2e-token-'))
  const proc = spawn(
    bin,
    [
      'start',
      '--ephemeral',
      '--services',
      'gcs',
      '--bind',
      '0.0.0.0',
      '--port',
      '0',
      '--console',
      '--log-level',
      'warn',
      '--data-dir',
      dir,
    ],
    // GCPEMU_ADMIN_TOKEN unset: the Instance makes one and prints it.
    { stdio: ['ignore', 'ignore', 'pipe'], env: { ...process.env, GCPEMU_ADMIN_TOKEN: '' } },
  )
  child = proc
  consoleURL = await new Promise<string>((resolve, reject) => {
    let err = ''
    const timer = setTimeout(() => reject(new Error(`no console URL within 30 s:\n${err}`)), 30_000)
    proc.stderr.on('data', (b: Buffer) => {
      err += b.toString()
      const m = /Web console: (http:\/\/\S+)/.exec(err)
      if (m?.[1]) {
        clearTimeout(timer)
        resolve(m[1])
      }
    })
    proc.on('exit', (code) => reject(new Error(`gcpemu exited (${code}):\n${err}`)))
  })
})

test.afterAll(() => {
  child?.kill('SIGTERM')
  if (dir) rmSync(dir, { recursive: true, force: true })
})

test('FR-UI-009: the printed console URL opens the console beyond loopback', async ({
  page,
  request,
}) => {
  const url = new URL(consoleURL)
  expect(url.searchParams.get('token')).toBeTruthy()
  const origin = url.origin

  // Without the token the console and the admin API are refused.
  expect((await request.get(`${origin}/console/`)).status()).toBe(401)
  expect((await request.get(`${origin}/_emu/v1/info`)).status()).toBe(401)
  expect((await request.get(`${origin}/_emu/v1/ready`)).status()).toBe(200)

  const events = page.waitForResponse((r) => new URL(r.url()).pathname === '/_emu/v1/events')
  await page.goto(consoleURL)
  // The token is swapped for a cookie and dropped from the address bar.
  await expect(page).toHaveURL(`${origin}/console/`)
  await expect(page.getByTestId('ready-gcs')).toContainText('Ready')
  expect((await events).status()).toBe(200)
})
