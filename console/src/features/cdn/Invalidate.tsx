import { useMutation, useQueryClient } from '@tanstack/react-query'
import { Eraser } from 'lucide-react'
import { useState, type FormEvent } from 'react'

import { Field, fieldAria } from '@/components/resource/Field'
import { Button } from '@/components/ui/button'
import { Card, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { NativeSelect } from '@/components/ui/native-select'
import { toast } from '@/lib/toast'

import { parseRef, relPath, scopeOf, type Resource } from '../lb/api'
import { invalidate, type CacheInvalidationRule } from './api'

/** pathError is the API's message for an invalid invalidation path. */
export function pathError(path: string): string | undefined {
  const bad = (why: string) => `Invalid value for field 'path': '${path}'. ${why}`
  if (path && !path.startsWith('/')) return bad('Path must start with /.')
  const star = path.indexOf('*')
  if (star >= 0 && star !== path.length - 1)
    return bad('A wildcard * is only allowed at the end of the path.')
  return undefined
}

/**
 * Invalidate removes cached entries by host and path (with a trailing /*
 * wildcard) or cache tag through urlMaps.invalidateCache (FR-CDN-005).
 * Invalidation is per URL map: it covers every origin the map routes to.
 * On an origin's page, maps are those that route to origin.
 */
export function Invalidate({ maps, origin }: { maps: Resource[]; origin?: string }) {
  const qc = useQueryClient()
  const [map, setMap] = useState('')
  const [host, setHost] = useState('')
  const [path, setPath] = useState('/*')
  const [tags, setTags] = useState('')
  const [errors, setErrors] = useState<{ map?: string; path?: string }>({})
  const chosen = map || relPath(maps[0]?.selfLink)
  const run = useMutation({
    mutationFn: (rule: CacheInvalidationRule) => invalidate(parseRef(chosen)!, rule),
    onSuccess: (_op, rule) => {
      void qc.invalidateQueries({ queryKey: ['cdn'] })
      const what = rule.path ?? `tags ${rule.cacheTags?.join(', ')}`
      toast.success(
        `Invalidated ${rule.host ?? ''}${what} through URL map ${parseRef(chosen)?.name}.`,
      )
    },
  })
  const submit = (e: FormEvent) => {
    e.preventDefault()
    const p = path.trim()
    const cacheTags = tags
      .split(',')
      .map((t) => t.trim())
      .filter(Boolean)
    const errs = {
      map: parseRef(chosen) ? undefined : 'Choose the URL map that routes to the origin.',
      path: p || cacheTags.length ? pathError(p) : 'Enter a path such as /images/* or cache tags.',
    }
    setErrors(errs)
    if (errs.map || errs.path) return
    const rule: CacheInvalidationRule = {}
    if (host.trim()) rule.host = host.trim()
    if (p) rule.path = p
    if (cacheTags.length) rule.cacheTags = cacheTags
    run.mutate(rule)
  }
  return (
    <Card aria-labelledby="invalidate-title">
      <CardTitle id="invalidate-title">Invalidate cached content</CardTitle>
      <p className="text-sm text-muted-foreground">
        Removes matching entries of every origin the URL map routes to
        {origin ? `, ${origin} among them,` : ''} before the operation completes.
      </p>
      {maps.length === 0 ? (
        <p className="text-sm text-muted-foreground">
          No URL map routes to {origin ? 'this origin' : 'an origin'} yet: invalidation goes through
          one.
        </p>
      ) : (
        <form className="flex flex-col gap-3" aria-label="Invalidate" onSubmit={submit} noValidate>
          <div className="grid gap-3 sm:grid-cols-2">
            <Field id="inv-map" label="URL map" error={errors.map}>
              <NativeSelect
                {...fieldAria('inv-map', errors.map)}
                value={chosen}
                onChange={(e) => setMap(e.target.value)}
              >
                {maps.map((m) => (
                  <option key={m.selfLink ?? m.name} value={relPath(m.selfLink)}>
                    {scopeOf(m) === 'global' ? m.name : `${m.name} (${scopeOf(m)})`}
                  </option>
                ))}
              </NativeSelect>
            </Field>
            <Field id="inv-host" label="Host" hint="Empty for every host.">
              <Input
                {...fieldAria('inv-host', undefined, 'hint')}
                className="font-mono"
                spellCheck={false}
                placeholder="www.example.com"
                value={host}
                onChange={(e) => setHost(e.target.value)}
              />
            </Field>
            <Field
              id="inv-path"
              label="Path"
              error={errors.path}
              hint="One path, or a prefix ending in /*; /* is everything."
            >
              <Input
                {...fieldAria('inv-path', errors.path, 'hint')}
                className="font-mono"
                spellCheck={false}
                value={path}
                onChange={(e) => setPath(e.target.value)}
              />
            </Field>
            <Field
              id="inv-tags"
              label="Cache tags"
              hint="Comma-separated: only entries whose Cache-Tag header has one."
            >
              <Input
                {...fieldAria('inv-tags', undefined, 'hint')}
                className="font-mono"
                spellCheck={false}
                value={tags}
                onChange={(e) => setTags(e.target.value)}
              />
            </Field>
          </div>
          <Button type="submit" variant="outline" className="w-fit" disabled={run.isPending}>
            <Eraser aria-hidden />
            {run.isPending ? 'Invalidating…' : 'Invalidate'}
          </Button>
        </form>
      )}
    </Card>
  )
}
