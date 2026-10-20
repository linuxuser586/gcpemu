import { useState } from 'react'

import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { useViewState } from '@/lib/viewState'

import { GLOBAL } from './api'

/** useBuildLocation is the location in ?location=, global by default. */
export function useBuildLocation(): string {
  const [view] = useViewState()
  return view.location || GLOBAL
}

/**
 * LocationFilter picks the location whose Builds or Build triggers are
 * shown. Cloud Build lists no locations, so it is typed.
 */
export function LocationFilter({ what }: { what: string }) {
  const [, setView] = useViewState()
  const location = useBuildLocation()
  const [text, setText] = useState(location)
  return (
    <form
      role="search"
      aria-label={`Choose the location of ${what}`}
      className="flex flex-wrap items-end gap-3"
      onSubmit={(e) => {
        e.preventDefault()
        const l = text.trim()
        setView({ location: l === GLOBAL ? '' : l })
      }}
    >
      <label htmlFor="build-location" className="flex flex-col gap-1 text-sm">
        Location
        <Input
          id="build-location"
          className="w-56 font-mono"
          placeholder={GLOBAL}
          value={text}
          onChange={(e) => setText(e.target.value)}
        />
      </label>
      <Button type="submit" variant="outline">
        Show
      </Button>
    </form>
  )
}
