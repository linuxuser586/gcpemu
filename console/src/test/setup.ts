import '@testing-library/jest-dom/vitest'

import { cleanup, configure } from '@testing-library/react'
import { setupServer } from 'msw/node'
import { afterAll, afterEach, beforeAll } from 'vitest'

import { handlers, resetFake } from './handlers'

// The default 1 s for findBy* is tight on a loaded CI runner.
configure({ asyncUtilTimeout: 3000 })

export const server = setupServer(...handlers)

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }))
afterEach(() => {
  cleanup()
  server.resetHandlers()
  resetFake()
  localStorage.clear()
})
afterAll(() => server.close())

// jsdom lacks matchMedia, which the theme follows.
Object.defineProperty(window, 'matchMedia', {
  writable: true,
  value: (query: string) => ({
    matches: false,
    media: query,
    addEventListener: () => {},
    removeEventListener: () => {},
  }),
})
