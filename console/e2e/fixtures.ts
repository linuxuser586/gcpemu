import { test as base, expect, type Request } from '@playwright/test'

// Every test records the requests its page makes, so that it can check
// what the console talks to (FR-UI-001, FR-UI-003, FR-UI-004).
export const test = base.extend<{ requests: Request[] }>({
  requests: async ({ page }, use) => {
    const requests: Request[] = []
    page.on('request', (r) => requests.push(r))
    await use(requests)
  },
})

export { expect }

/** projectId returns a Project ID unique to the test's browser. */
export function projectId(prefix: string, browserName: string): string {
  return `${prefix}-${browserName}`.slice(0, 30)
}
