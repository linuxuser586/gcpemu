import { spawn, type ChildProcess } from 'node:child_process'

import { expect, goto, test } from './fixtures'
import { bin, newTestDir, stopInstance, type TestDir } from './instance'

// FR-UI-009: beyond loopback the console needs the admin token. This file
// runs its own Instance bound to 0.0.0.0 and opens the console URL it
// prints, as a user would.

// GCPEMU_ADMIN_TOKEN unset: the Instance makes one and prints it.
const env = { ...process.env, GCPEMU_ADMIN_TOKEN: '' }

let child: ChildProcess | undefined
let testDir: TestDir | undefined
let consoleURL = ''

test.beforeAll(async () => {
  testDir = await newTestDir()
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
      testDir.dir,
    ],
    { stdio: ['ignore', 'ignore', 'pipe'], env },
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

test.afterAll(async () => {
  if (child && testDir) await stopInstance(child, testDir, env)
  else await testDir?.release()
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
  await goto(page, consoleURL)
  // The token is swapped for a cookie and dropped from the address bar.
  await expect(page).toHaveURL(`${origin}/console/`)
  await expect(page.getByTestId('ready-gcs')).toContainText('Ready')
  expect((await events).status()).toBe(200)
})
