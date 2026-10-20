import { useCallback, useLayoutEffect, useRef } from 'react'
// eslint-disable-next-line no-restricted-imports -- the one module that may use it
import { createSearchParams, useLocation, useSearchParams } from 'react-router'
import type { NavigateOptions, URLSearchParamsInit } from 'react-router'

type Next = URLSearchParamsInit | ((prev: URLSearchParams) => URLSearchParamsInit)

/**
 * useLatestSearchParams is react-router's useSearchParams, except that a
 * function passed to the setter gets the latest params: those of the newest
 * update this hook has sent, even if it has not rendered yet. React renders
 * a navigation in a transition, so two quick updates would otherwise both
 * start from the params of the last render, and the second would undo the
 * first. A navigation made elsewhere, such as a Link or the back button,
 * replaces any update still pending.
 */
export function useLatestSearchParams(): [
  URLSearchParams,
  (next: Next, options?: NavigateOptions) => void,
] {
  const [search, setSearch] = useSearchParams()
  const { key } = useLocation()
  const rendered = useRef({ key, search: search.toString() })
  // Sent are the params this hook has sent since the location it last saw.
  const sent = useRef({ key, pending: [] as string[] })
  useLayoutEffect(() => {
    rendered.current = { key, search: search.toString() }
  }, [key, search])

  const set = useCallback(
    (next: Next, options?: NavigateOptions) => {
      const now = rendered.current
      const s = sent.current
      if (s.key !== now.key) {
        // A navigation landed: ours, which settles it and every older
        // one, or someone else's, which wins over all of them.
        const i = s.pending.lastIndexOf(now.search)
        s.pending = i < 0 ? [] : s.pending.slice(i + 1)
        s.key = now.key
      }
      const prev = new URLSearchParams(s.pending.at(-1) ?? now.search)
      const params = createSearchParams(typeof next === 'function' ? next(prev) : next)
      s.pending.push(params.toString())
      setSearch(params, options)
    },
    [setSearch],
  )
  return [search, set]
}
