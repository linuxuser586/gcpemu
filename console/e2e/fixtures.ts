import { test as base, expect, type Request } from '@playwright/test'

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

/** projectId returns a Project ID unique to the test's browser. */
export function projectId(prefix: string, browserName: string): string {
  return `${prefix}-${browserName}`.slice(0, 30)
}
