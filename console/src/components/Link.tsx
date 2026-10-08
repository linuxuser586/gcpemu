import type { ComponentProps } from 'react'
import { NavLink as RouterNavLink, Link as RouterLink, useLocation, type To } from 'react-router'

import { viewParams } from '@/lib/viewState'

/** useCarried adds the current ?project=, ?location= and ?q= to to. */
function useCarried(to: To): To {
  const { search } = useLocation()
  const carried = viewParams(new URLSearchParams(search))
  if (typeof to === 'string') {
    const [path = '', query] = to.split('?')
    const params = new URLSearchParams(query)
    for (const [k, v] of carried) if (!params.has(k)) params.set(k, v)
    const s = params.toString()
    return s ? `${path}?${s}` : path
  }
  const params = new URLSearchParams(to.search)
  for (const [k, v] of carried) if (!params.has(k)) params.set(k, v)
  return { ...to, search: params.toString() }
}

/** Link is react-router's Link that carries the view state along. */
export function Link({ to, ...props }: ComponentProps<typeof RouterLink>) {
  return <RouterLink to={useCarried(to)} {...props} />
}

/** NavLink is react-router's NavLink that carries the view state along. */
export function NavLink({ to, ...props }: ComponentProps<typeof RouterNavLink>) {
  return <RouterNavLink to={useCarried(to)} {...props} />
}
