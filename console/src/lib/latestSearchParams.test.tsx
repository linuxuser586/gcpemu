import { act, render } from '@testing-library/react'
import { Router, type Location, type Navigator } from 'react-router'
import { expect, it, vi } from 'vitest'

import { useLatestSearchParams } from './latestSearchParams'

// The tests render the hook in a Router whose location they control, so
// that they decide when a navigation the hook sent lands, and see each one.

type Setter = ReturnType<typeof useLatestSearchParams>[1]

function setup(search: string) {
  const navigator = {
    createHref: () => '',
    go: vi.fn(),
    push: vi.fn(),
    replace: vi.fn(),
  } satisfies Navigator
  let set!: Setter
  let seen = ''
  function Probe() {
    const [params, setParams] = useLatestSearchParams()
    set = setParams
    seen = params.toString()
    return null
  }
  let n = 0
  const at = (search: string): Location => ({
    pathname: '/view',
    search: search && `?${search}`,
    hash: '',
    state: null,
    key: `k${n++}`,
  })
  let loc = at(search)
  const tree = () => (
    <Router location={loc} navigator={navigator}>
      <Probe />
    </Router>
  )
  const utils = render(tree())
  return {
    navigator,
    set: (...args: Parameters<Setter>) => act(() => set(...args)),
    seen: () => seen,
    /** land renders a navigation to search, as the router would. */
    land: (search: string) => {
      loc = at(search)
      utils.rerender(tree())
    },
    /** rerender renders the same location again, as a change of local state does. */
    rerender: () => utils.rerender(tree()),
    /** sent is the search of the latest navigation the hook made. */
    sent: () => {
      const calls = [...navigator.push.mock.calls, ...navigator.replace.mock.calls]
      const order = [
        ...navigator.push.mock.invocationCallOrder,
        ...navigator.replace.mock.invocationCallOrder,
      ]
      const last = calls[order.indexOf(Math.max(...order))]![0] as { search: string }
      return new URLSearchParams(last.search).toString()
    },
  }
}

const patch = (k: string, v: string) => (prev: URLSearchParams) => {
  const next = new URLSearchParams(prev)
  if (v) next.set(k, v)
  else next.delete(k)
  return next
}

it('keeps both of two updates sent before a re-render', () => {
  const h = setup('status=4xx')
  h.set(patch('status', ''))
  h.set(patch('method', 'patch'))
  expect(h.sent()).toBe('method=patch')
  expect(h.seen()).toBe('status=4xx')
})

it('builds on the newest update when an older one lands', () => {
  const h = setup('')
  h.set(patch('a', '1'))
  h.set(patch('b', '2'))
  h.land('a=1')
  h.set(patch('c', '3'))
  expect(h.sent()).toBe('a=1&b=2&c=3')
  h.land('a=1&b=2')
  h.land('a=1&b=2&c=3')
  h.set(patch('a', ''))
  expect(h.sent()).toBe('b=2&c=3')
})

it('adopts a navigation made elsewhere', () => {
  const h = setup('')
  h.set(patch('a', '1'))
  h.land('project=p')
  h.set(patch('b', '2'))
  expect(h.sent()).toBe('project=p&b=2')
})

it('keeps a pending update across a re-render of the same location', () => {
  const h = setup('status=4xx')
  h.set(patch('status', ''))
  // Local state re-renders the view before the navigation lands.
  h.rerender()
  h.set(patch('method', 'patch'))
  expect(h.sent()).toBe('method=patch')
})

it('takes a value as well as a function, and passes replace through', () => {
  const h = setup('a=1')
  h.set({ b: '2' }, { replace: true })
  expect(h.navigator.replace).toHaveBeenCalledTimes(1)
  expect(h.navigator.push).not.toHaveBeenCalled()
  expect(h.sent()).toBe('b=2')
  h.set(patch('c', '3'))
  expect(h.navigator.push).toHaveBeenCalledTimes(1)
  expect(h.sent()).toBe('b=2&c=3')
})
