import { useCallback } from 'react'
import { useSearchParams } from 'react-router'

// Shareable view state lives in the URL so that every view has a deep link
// (FR-UI-005). Instance-wide views ignore it but keep it, so that it
// survives a trip through them.
export const VIEW_PARAMS = ['project', 'location', 'q'] as const
export type ViewParam = (typeof VIEW_PARAMS)[number]
export type ViewState = Partial<Record<ViewParam, string>>

/** viewParams keeps only the view-state parameters of search. */
export function viewParams(search: URLSearchParams): URLSearchParams {
  const out = new URLSearchParams()
  for (const k of VIEW_PARAMS) {
    const v = search.get(k)
    if (v) out.set(k, v)
  }
  return out
}

export function useViewState(): [ViewState, (patch: ViewState) => void] {
  const [search, setSearch] = useSearchParams()
  const state: ViewState = {}
  for (const k of VIEW_PARAMS) {
    const v = search.get(k)
    if (v) state[k] = v
  }
  const set = useCallback(
    (patch: ViewState) =>
      setSearch((prev) => {
        const next = new URLSearchParams(prev)
        for (const [k, v] of Object.entries(patch)) {
          if (v) next.set(k, v)
          else next.delete(k)
        }
        return next
      }),
    [setSearch],
  )
  return [state, set]
}
