import { test as base, expect, type Page, type Request } from '@playwright/test'

// The requests a page had answered. In Chromium, headerValue and
// response wait forever on a request the browser cancelled, as a
// navigation does to the fetches of the page it unloads (#234).
const answered = new WeakSet<Request>()

// Every test records the requests its page makes, so that it can check
// what the console talks to (FR-UI-001, FR-UI-003, FR-UI-004).
export const test = base.extend<{ requests: Request[] }>({
  requests: async ({ page }, use) => {
    const requests: Request[] = []
    page.on('request', (r) => requests.push(r))
    page.on('response', (r) => answered.add(r.request()))
    await use(requests)
  },
})

export { expect }

/**
 * expectConsoleRequests checks that every request goes to a path the
 * test allows and carries no credentials, except one to a path that
 * credentialed matches, which carries a bearer token.
 */
export async function expectConsoleRequests(
  requests: Request[],
  opts: { allowed: RegExp; credentialed?: RegExp },
) {
  for (const r of requests) {
    const url = new URL(r.url())
    // A blob: URL (a saved download) never leaves the browser.
    if (url.protocol === 'blob:') continue
    expect(url.pathname, r.url()).toMatch(opts.allowed)
    // An answered request has the headers it went out with; one that never
    // was has the headers the page set, where credentials would come from.
    const auth = answered.has(r)
      ? await r.headerValue('authorization')
      : (r.headers()['authorization'] ?? null)
    if (opts.credentialed?.test(url.pathname)) {
      expect(auth, r.url()).toMatch(/^Bearer /)
    } else {
      expect(auth, r.url()).toBeNull()
    }
  }
}

/**
 * clipboardText reads the clipboard with LF line endings. Chromium on
 * Windows writes CRLF whatever the page gave it (#961), and CI runs on
 * Linux, so comparing multi-line text read directly fails only on Windows.
 */
export async function clipboardText(page: Page): Promise<string> {
  const text = await page.evaluate(() => navigator.clipboard.readText())
  return text.replaceAll('\r\n', '\n')
}

/** projectId returns a Project ID unique to the test's browser. */
export function projectId(prefix: string, browserName: string): string {
  return `${prefix}-${browserName}`.slice(0, 30)
}

/**
 * goto navigates page as page.goto does, trying again once when Chromium
 * cancels the navigation because the host's network changed (#939).
 */
export async function goto(page: Page, ...args: Parameters<Page['goto']>) {
  return tryAgainOnNetworkChange(page, () => page.goto(...args))
}

/** reload reloads page as page.reload does, trying again once as goto does. */
export async function reload(page: Page, ...args: Parameters<Page['reload']>) {
  return tryAgainOnNetworkChange(page, () => page.reload(...args))
}

async function tryAgainOnNetworkChange<T>(page: Page, navigate: () => Promise<T>): Promise<T> {
  try {
    return await navigate()
  } catch (err) {
    if (!(err instanceof Error) || !/\bnet::ERR_NETWORK_CHANGED\b/.test(err.message)) throw err
    // Chromium commits its error page for the cancelled navigation after
    // the navigation has failed, and that interrupts one started first.
    // A browser that shows no error page lets the wait time out.
    await page
      .waitForURL(/^chrome-error:/, { waitUntil: 'commit', timeout: 5_000 })
      .catch(() => undefined)
    return navigate()
  }
}
