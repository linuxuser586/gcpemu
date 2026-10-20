import { useCallback, useLayoutEffect } from 'react'
// eslint-disable-next-line no-restricted-imports -- the one module that may use it
import { createSearchParams, useLocation, useSearchParams } from 'react-router'
import type { NavigateOptions, URLSearchParamsInit } from 'react-router'

type SearchParamsUpdate = URLSearchParamsInit | ((prev: URLSearchParams) => URLSearchParamsInit)

// The console has one router, so every use of the hook shares what has been
// sent: a view that uses it twice, such as through useViewState, builds each
// update on the other's too. Pending are the params sent since the location
// last rendered, oldest first. It starts afresh once nothing uses it.
let latest = { key: '', search: '', pending: [] as string[] }
let users = 0

/**
 * useLatestSearchParams is react-router's useSearchParams, except that a
 * function passed to the setter gets the latest params: those of the newest
 * update sent, even if it has not rendered yet. React renders a navigation
 * in a transition, so two quick updates would otherwise both start from the
 * params of the last render, and the second would undo the first. A
 * navigation made elsewhere, such as a Link or the back button, replaces
 * any update still pending.
 */
export function useLatestSearchParams(): [
  URLSearchParams,
  (update: SearchParamsUpdate, options?: NavigateOptions) => void,
] {
  const [search, setSearch] = useSearchParams()
  const { key } = useLocation()
  const rendered = search.toString()
  useLayoutEffect(() => {
    users++
    return () => {
      if (--users === 0) latest = { key: '', search: '', pending: [] }
    }
  }, [])
  useLayoutEffect(() => {
    if (latest.key === key) return
    // A navigation landed: one sent here, which settles it and every older
    // one, or one made elsewhere, which wins over all of them.
    const i = latest.pending.lastIndexOf(rendered)
    latest.pending = i < 0 ? [] : latest.pending.slice(i + 1)
    latest.key = key
    latest.search = rendered
  }, [key, rendered])

  const set = useCallback(
    (update: SearchParamsUpdate, options?: NavigateOptions) => {
      const prev = new URLSearchParams(latest.pending.at(-1) ?? latest.search)
      const params = createSearchParams(typeof update === 'function' ? update(prev) : update)
      latest.pending.push(params.toString())
      setSearch(params, options)
    },
    [setSearch],
  )
  return [search, set]
}
