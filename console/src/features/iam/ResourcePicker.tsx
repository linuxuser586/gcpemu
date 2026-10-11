import { useQuery } from '@tanstack/react-query'
import { useId } from 'react'

import { infoQuery } from '@/api/queries'
import { Field, fieldAria } from '@/components/resource/Field'
import { Input } from '@/components/ui/input'
import { NativeSelect } from '@/components/ui/native-select'

import { repositoriesQuery } from '../ar/api'
import { bucketsQuery } from '../gcs/api'
import { subscriptionsQuery, topicsQuery } from '../pubsub/api'
import { secretsQuery } from '../secrets/api'
import { KINDS, lastSegment, serviceAccountsQuery, type KindId } from './api'

/**
 * useNameSuggestions lists the names of a Project's resources of a kind,
 * for the picker's suggestions, from the API of the Service that serves
 * them. Kinds without a list in the console suggest nothing.
 */
function useNameSuggestions(project: string, kind: KindId): string[] {
  const on = (k: KindId) => !!project && kind === k
  const accounts = useQuery({ ...serviceAccountsQuery(project), enabled: on('serviceAccount') })
  const buckets = useQuery({ ...bucketsQuery(project), enabled: on('bucket') })
  const topics = useQuery({ ...topicsQuery(project), enabled: on('topic') })
  const subs = useQuery({ ...subscriptionsQuery(project), enabled: on('subscription') })
  const repos = useQuery({ ...repositoriesQuery(project, ''), enabled: on('repository') })
  const secrets = useQuery({ ...secretsQuery(project, ''), enabled: on('secret') })
  switch (kind) {
    case 'serviceAccount':
      return accounts.data?.map((a) => a.email) ?? []
    case 'bucket':
      return buckets.data?.map((b) => b.name) ?? []
    case 'topic':
      return topics.data?.map((t) => lastSegment(t.name)) ?? []
    case 'subscription':
      return subs.data?.map((s) => lastSegment(s.name)) ?? []
    case 'repository':
      return (
        repos.data?.map((r) => {
          const m = /locations\/([^/]+)\/repositories\/([^/]+)$/.exec(r.name)
          return m ? `${m[1]}/${m[2]}` : r.name
        }) ?? []
      )
    case 'secret':
      return secrets.data?.map((s) => lastSegment(s.name)) ?? []
    default:
      return []
  }
}

/**
 * ResourcePicker chooses a resource whose IAM policy to read or test: its
 * kind, among those of the Services enabled, and its name in the Project.
 */
export function ResourcePicker({
  project,
  kind,
  name,
  error,
  onKind,
  onName,
}: {
  project: string
  kind: KindId
  name: string
  error?: string
  onKind: (kind: KindId) => void
  onName: (name: string) => void
}) {
  const id = useId()
  const { data: info } = useQuery(infoQuery())
  const enabled = new Set(info?.services ?? [])
  const current = KINDS.find((k) => k.id === kind) ?? KINDS[0]!
  const suggestions = useNameSuggestions(project, kind)
  return (
    <div className="grid gap-4 sm:grid-cols-2">
      <Field id={`${id}-kind`} label="Resource type">
        <NativeSelect
          {...fieldAria(`${id}-kind`)}
          value={kind}
          onChange={(e) => onKind(e.target.value as KindId)}
        >
          {KINDS.filter((k) => enabled.has(k.service) || k.id === kind).map((k) => (
            <option key={k.id} value={k.id}>
              {k.label}
            </option>
          ))}
        </NativeSelect>
      </Field>
      {current.name && (
        <Field id={`${id}-name`} label={current.name.label} error={error}>
          <Input
            {...fieldAria(`${id}-name`, error)}
            list={`${id}-names`}
            autoComplete="off"
            spellCheck={false}
            className="font-mono"
            placeholder={current.name.placeholder}
            value={name}
            onChange={(e) => onName(e.target.value)}
          />
          <datalist id={`${id}-names`}>
            {suggestions.map((s) => (
              <option key={s} value={s} />
            ))}
          </datalist>
        </Field>
      )}
    </div>
  )
}
