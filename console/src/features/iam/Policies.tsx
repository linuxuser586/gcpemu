import { useState } from 'react'
import { useSearchParams } from 'react-router'

import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'

import { kindOf, nameError, type KindId } from './api'
import { ChooseProject, useProject } from './IamLayout'
import { PolicyEditor } from './PolicyEditor'
import { ResourcePicker } from './ResourcePicker'

/**
 * Policies shows and edits the IAM policy of any resource that has one
 * (FR-IAM-003): the chosen Project's by default, another resource's with
 * ?kind= and ?name= (its name relative to the Project).
 */
export function Policies() {
  const project = useProject()
  const [search, setSearch] = useSearchParams()
  const kind = kindOf(search.get('kind'))
  const name = search.get('name') ?? ''
  const [text, setText] = useState(name)
  const [error, setError] = useState<string>()
  if (!project) return <ChooseProject title="Policies" what="see its IAM policies" />
  const valid = !nameError(kind, name)
  const go = (k: KindId, n: string) =>
    setSearch((prev) => {
      const next = new URLSearchParams(prev)
      if (k === 'project') next.delete('kind')
      else next.set('kind', k)
      if (n) next.set('name', n)
      else next.delete('name')
      return next
    })

  return (
    <div className="flex flex-col gap-6">
      <div>
        <h1 className="text-2xl font-semibold">Policies</h1>
        <p className="text-sm text-muted-foreground">
          Who has which role on a resource of Project <span className="font-mono">{project}</span>.
          A resource&apos;s bindings add to its Project&apos;s.
        </p>
      </div>
      <Card>
        <form
          role="search"
          aria-label="Resource"
          className="flex flex-col gap-3"
          noValidate
          onSubmit={(e) => {
            e.preventDefault()
            const err = nameError(kind, text.trim())
            setError(err)
            if (!err) go(kind.id, text.trim())
          }}
        >
          <ResourcePicker
            project={project}
            kind={kind.id}
            name={text}
            error={error}
            onKind={(k) => {
              setText('')
              setError(undefined)
              go(k, '')
            }}
            onName={(n) => {
              setText(n)
              setError(undefined)
            }}
          />
          {kind.name && (
            <Button type="submit" className="w-fit">
              Show policy
            </Button>
          )}
        </form>
      </Card>
      {valid && <PolicyEditor key={`${kind.id}/${name}`} r={{ project, kind: kind.id, name }} />}
    </div>
  )
}
