import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useForm } from 'react-hook-form'
import { useSearchParams } from 'react-router'

import { useCarriedNavigate } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { Field, fieldAria } from '@/components/resource/Field'
import { ResourceEditor, type Body } from '@/components/resource/ResourceEditor'
import { Card } from '@/components/ui/card'
import { NativeSelect } from '@/components/ui/native-select'
import { toast } from '@/lib/toast'
import { useViewState } from '@/lib/viewState'

import { patchResource, refOf, relPath, resourceQuery, scopeOf, type Resource } from '../lb/api'
import { keysOf, resolver, SpecFields } from '../lb/ResourceForm'
import { patchBody, patchValues, readValues, type Values } from '../lb/spec'
import { backendsQuery, isOrigin, isOriginColl, kindName } from './api'
import { originPath, useOriginRef, type OriginRef } from './CdnLayout'
import { SPECS } from './spec'

// Adding an origin and editing its cache settings (FR-UI-011) are both
// patches of the backend service or bucket: a JSON merge patch of its
// cdnPolicy with the fingerprint, and enableCdn when adding.

const method = (r: OriginRef) =>
  r.coll === 'backendServices'
    ? `${r.region ? 'regionBackendServices' : 'backendServices'}.patch`
    : 'backendBuckets.patch'

/**
 * AddOrigin turns Cloud CDN on for a backend service or bucket of the
 * Project, with its cache settings. ?backend= picks the backend.
 */
export function AddOrigin() {
  const [view] = useViewState()
  const project = view.project ?? ''
  const [params, setParams] = useSearchParams()
  const backends = useQuery(backendsQuery(project))
  const candidates = (backends.data ?? [])
    .filter((b) => !isOrigin(b.r))
    .sort((a, b) => a.r.name.localeCompare(b.r.name) || a.coll.localeCompare(b.coll))
  const chosen = params.get('backend') ?? ''
  const pick = candidates.find((b) => relPath(b.r.selfLink) === chosen)
  return (
    <div className="flex flex-col gap-6">
      <div>
        <h1 className="text-2xl font-semibold">Add origin</h1>
        <p className="text-sm text-muted-foreground">
          Turn Cloud CDN on for a backend service or bucket of Project{' '}
          <span className="font-mono">{project}</span>.
        </p>
      </div>
      <Card>
        <QueryStatus query={backends} />
        {backends.data && candidates.length === 0 && (
          <p className="text-sm text-muted-foreground">
            Every backend service and bucket of this Project is an origin already, or there are
            none: create one in the Load Balancer view.
          </p>
        )}
        {candidates.length > 0 && (
          <Field
            id="origin-backend"
            label="Backend"
            hint="A backend service or bucket without Cloud CDN."
          >
            <NativeSelect
              {...fieldAria('origin-backend', undefined, 'hint')}
              value={pick ? chosen : ''}
              onChange={(e) =>
                setParams(
                  (p) => {
                    p.set('backend', e.target.value)
                    return p
                  },
                  { replace: true },
                )
              }
            >
              <option value="">Choose…</option>
              {candidates.map(({ coll, r }) => (
                <option key={r.selfLink ?? r.name} value={relPath(r.selfLink)}>
                  {r.name} ({kindName(coll)}
                  {scopeOf(r) === 'global' ? '' : `, ${scopeOf(r)}`})
                </option>
              ))}
            </NativeSelect>
          </Field>
        )}
        {pick && (
          <CdnForm
            key={pick.r.selfLink}
            r={refOf(pick.r, pick.coll, project) as OriginRef}
            cur={pick.r}
            adding
          />
        )}
      </Card>
    </div>
  )
}

/** EditOrigin edits an origin's cache settings. */
export function EditOrigin() {
  const ref = useOriginRef()
  const ok = isOriginColl(ref.coll)
  const query = useQuery({ ...resourceQuery(ref), enabled: ok })
  return (
    <div className="flex flex-col gap-6">
      <h1 className="text-2xl font-semibold">
        Cache settings of <span className="font-mono">{ref.name}</span>
      </h1>
      <QueryStatus query={query} />
      {!ok && (
        <Card role="status">
          <p className="text-sm text-muted-foreground">
            {ref.coll} cannot be a Cloud CDN origin: backend services and buckets can.
          </p>
        </Card>
      )}
      {ok && query.data && (
        <Card>
          <CdnForm r={ref} cur={query.data} adding={!isOrigin(query.data)} />
        </Card>
      )}
    </div>
  )
}

function CdnForm({ r, cur, adding }: { r: OriginRef; cur: Resource; adding: boolean }) {
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const spec = SPECS[r.coll]
  const form = useForm<Values>({
    resolver: resolver(spec, true),
    defaultValues: readValues(spec, cur, { region: r.region }),
  })
  const save = useMutation({
    meta: { toast: false },
    mutationFn: async (body: Body) => {
      if (Object.keys(body).filter((k) => k !== 'fingerprint').length === 0) return false
      await patchResource(r, body)
      return true
    },
    onSuccess: (changed) => {
      void qc.invalidateQueries({ queryKey: ['cdn'] })
      void qc.invalidateQueries({ queryKey: ['lb'] })
      toast.success(
        !changed
          ? 'Nothing to change.'
          : adding
            ? `Added origin ${r.name}.`
            : `Saved the cache settings of ${r.name}.`,
      )
      navigate(originPath(r))
    },
  })
  return (
    <ResourceEditor
      form={form}
      initialBody={adding ? { enableCdn: true } : {}}
      toBody={patchBody(spec, cur)}
      fromBody={patchValues(spec, cur)}
      fields={keysOf(spec)}
      onSubmit={(body) => save.mutateAsync(body)}
      submitLabel={adding ? 'Add origin' : 'Save'}
      onCancel={() => navigate(adding ? '/cdn' : originPath(r))}
      jsonHint={
        <>
          Sent to <span className="font-mono">{method(r)}</span>: objects merge, lists replace and
          null removes a field.
        </>
      }
    >
      <p className="text-sm text-muted-foreground">
        {r.coll === 'backendServices' ? 'Backend service' : 'Backend bucket'}{' '}
        <span className="font-mono">{r.name}</span>
        {r.region ? (
          <>
            {' '}
            in region <span className="font-mono">{r.region}</span>
          </>
        ) : null}
        .
      </p>
      <SpecFields spec={spec} form={form} edit project={r.project} region={r.region} />
    </ResourceEditor>
  )
}
