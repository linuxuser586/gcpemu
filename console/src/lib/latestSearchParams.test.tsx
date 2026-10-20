import { act, render } from '@testing-library/react'
import { Router, type Location, type Navigator } from 'react-router'
import { expect, it, vi } from 'vitest'

import { useLatestSearchParams } from './latestSearchParams'

// The tests render the hook in a Router whose location they control, so
// that they decide when a navigation the hook sent lands, and see each one.

type Setter = ReturnType<typeof useLatestSearchParams>[1]

// Location keys are unique across tests, as a router's are across navigations.
let n = 0

function setup(search: string) {
  const navigations: { search: string }[] = []
  const navigator = {
    createHref: () => '',
    go: vi.fn(),
    push: vi.fn((to: { search: string }) => void navigations.push(to)),
    replace: vi.fn((to: { search: string }) => void navigations.push(to)),
  } satisfies Navigator
  let set!: Setter
  let other!: Setter
  let seen = ''
  // A view may use the hook twice, as one using useViewState does.
  function Probe() {
    const [params, setParams] = useLatestSearchParams()
    set = setParams
    other = useLatestSearchParams()[1]
    seen = params.toString()
    return null
  }
  const at = (search: string, key = `k${n++}`): Location => ({
    pathname: '/view',
    search: search && `?${search}`,
    hash: '',
    state: null,
    key,
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
    /** setOther updates through the view's other use of the hook. */
    setOther: (...args: Parameters<Setter>) => act(() => other(...args)),
    seen: () => seen,
    /** land renders a navigation to search, as the router would, and returns its key. */
    land: (search: string, key?: string) => {
      loc = at(search, key)
      utils.rerender(tree())
      return loc.key
    },
    /** rerender renders the same location again, as a change of local state does. */
    rerender: () => utils.rerender(tree()),
    /** sent is the search of the latest navigation the hook made. */
    sent: () => new URLSearchParams(navigations.at(-1)?.search).toString(),
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

it('builds on an update sent by another use of the hook in the view', () => {
  const h = setup('status=4xx')
  h.set(patch('status', ''))
  h.setOther(patch('q', 'x'))
  expect(h.sent()).toBe('q=x')
  h.land('')
  h.set(patch('method', 'patch'))
  expect(h.sent()).toBe('q=x&method=patch')
})

it('drops a landed update when the back button returns to an earlier location', () => {
  const h = setup('')
  const first = h.land('a=1')
  h.set(patch('b', '2'))
  h.land('a=1&b=2')
  h.land('a=1', first)
  h.set(patch('c', '3'))
  expect(h.sent()).toBe('a=1&c=3')
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
