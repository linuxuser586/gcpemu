import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { ArrowRight, Pencil, Trash2 } from 'lucide-react'
import { useState, type ReactNode } from 'react'

import { Link, useCarriedNavigate } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { ConfirmDialog } from '@/components/resource/ConfirmDialog'
import { DetailList, JsonView, Mono, type DetailRow } from '@/components/resource/DetailList'
import { Button } from '@/components/ui/button'
import { Card, CardTitle } from '@/components/ui/card'
import { toast } from '@/lib/toast'

import { CopyText } from '../sql/InstancePage'
import {
  deleteResource,
  kindInfo,
  listenersQuery,
  parseRef,
  relPath,
  resourceQuery,
  type LbRoute,
  type Ref,
  type Resource,
} from './api'
import { CertificateStatus } from './Certificates'
import { BackendHealth } from './Health'
import { lbPath, listPath, RefLink, useLbRef } from './LbLayout'
import { readValues, SPECS, type FieldSpec, type Values } from './spec'
import { RouteTree, TestUrl } from './UrlMap'

/**
 * ResourcePage is one load-balancing resource: edit and delete, what the
 * kind shows (a forwarding rule's local listener, a URL map's route tree
 * and URL tester, a backend service's endpoint health, a certificate's
 * status), its fields and the resource as the API returns it.
 */
export function ResourcePage() {
  const ref = useLbRef()
  const k = kindInfo(ref.coll)
  const query = useQuery({ ...resourceQuery(ref), enabled: !!k })
  const r = query.data
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const del = useMutation({
    mutationFn: () => deleteResource(ref),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['lb'] })
      toast.success(`Deleted ${k?.singular ?? ''} ${ref.name}.`)
      navigate(listPath(ref.coll))
    },
  })
  if (!k) {
    return (
      <Card role="status">
        <p className="text-sm text-muted-foreground">No resource type {ref.coll}.</p>
      </Card>
    )
  }
  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="flex flex-col gap-1">
          <p className="text-sm text-muted-foreground">
            <Link to={listPath(ref.coll)} className="underline-offset-4 hover:underline">
              {k.title}
            </Link>{' '}
            / <span className="font-mono">{ref.region || 'global'}</span>
          </p>
          <h1 className="text-2xl font-semibold">
            <span className="font-mono">{ref.name}</span>
          </h1>
        </div>
        {r && (
          <div className="flex flex-wrap gap-2">
            {k.editable && (
              <Button variant="outline" asChild>
                <Link to={lbPath(ref, 'edit')}>
                  <Pencil aria-hidden />
                  Edit
                </Link>
              </Button>
            )}
            <ConfirmDialog
              trigger={
                <Button variant="outline">
                  <Trash2 aria-hidden />
                  Delete
                </Button>
              }
              title={`Delete ${k.singular} ${ref.name}?`}
              description="The API refuses while another resource uses it. This cannot be undone."
              confirmLabel={`Delete ${k.singular}`}
              onConfirm={() => del.mutateAsync()}
            />
          </div>
        )}
      </div>
      <QueryStatus query={query} />
      {r && (
        <>
          {ref.coll === 'forwardingRules' && <Frontend fr={r} r={ref} />}
          {ref.coll === 'urlMaps' && <UrlMapPanels map={r} r={ref} />}
          {ref.coll === 'backendServices' && <BackendHealth bs={r} r={ref} />}
          {ref.coll === 'sslCertificates' && <CertificateStatus cert={r} />}
          <Card>
            <CardTitle>Details</CardTitle>
            <DetailList label={`${k.singular} details`} rows={details(r, ref)} />
          </Card>
          <Card>
            <CardTitle>Resource</CardTitle>
            <JsonView value={r} label={`${k.singular} JSON`} />
          </Card>
        </>
      )}
    </div>
  )
}

/** display renders a form value of a field as a detail. */
function display(f: FieldSpec, v: Values[string]): ReactNode {
  switch (f.type) {
    case 'checkbox':
      return v ? 'On' : 'Off'
    case 'ref':
      return v ? <RefLink link={v as string} /> : undefined
    case 'refs': {
      const l = v as string[]
      return l.length ? (
        <span className="flex flex-wrap gap-x-3">
          {l.map((x) => (
            <RefLink key={x} link={x} />
          ))}
        </span>
      ) : undefined
    }
    case 'lines':
    case 'labels':
      return v ? <Mono>{(v as string).split('\n').join(', ')}</Mono> : undefined
    default:
      return v ? f.mono ? <Mono>{v as string}</Mono> : (v as string) : undefined
  }
}

/** HIDDEN fields are shown elsewhere (the route tree) or are secrets. */
const HIDDEN = new Set(['routing', 'certificate', 'privateKey', 'region', 'name'])

function details(r: Resource, ref: Ref): DetailRow[] {
  const spec = SPECS[ref.coll]
  const v = readValues(spec, r)
  return [
    ['Scope', <Mono key="s">{ref.region || 'global'}</Mono>],
    ...spec.fields
      .filter((f) => !HIDDEN.has(f.key) && (!f.show || f.show(v)))
      .map((f): DetailRow => [f.label, display(f, v[f.key]!)]),
    [
      'Created',
      typeof r.creationTimestamp === 'string' && new Date(r.creationTimestamp).toLocaleString(),
    ],
  ]
}

/**
 * Frontend shows where a forwarding rule listens on this machine
 * (FR-LB-002) and the chain it serves: target proxy → URL map.
 */
function Frontend({ fr, r }: { fr: Resource; r: Ref }) {
  const listeners = useQuery(listenersQuery(r.project))
  const l = listeners.data?.get(relPath(fr.selfLink))
  const proxyRef = parseRef(fr.target as string | undefined)
  const proxy = useQuery({ ...resourceQuery(proxyRef!), enabled: !!proxyRef })
  const scheme = l?.https ? 'https' : 'http'
  return (
    <Card aria-labelledby="listener-title">
      <CardTitle id="listener-title">Local listener</CardTitle>
      <QueryStatus query={listeners} />
      {listeners.data && !l && (
        <p className="text-sm text-muted-foreground">
          Not listening: the forwarding rule’s target proxy or URL map cannot serve yet.
        </p>
      )}
      {l && (
        <div className="flex flex-col gap-3 text-sm">
          <DetailList
            label="Listener"
            rows={[
              ['Forwarding rule', <Mono key="f">{`${l.ipAddress}:${l.port}`}</Mono>],
              ['Listens on', <Mono key="l">{l.listener}</Mono>],
              [
                'How',
                {
                  loopback: 'A loopback address on the rule’s port',
                  edge: 'The lb-edge container (a privileged port)',
                  fallback: 'A loopback address on a high port',
                }[l.mode],
              ],
              [
                'Emulated DNS',
                `Names that resolve to ${l.ipAddress} answer ${l.listener.replace(/:\d+$/, '')}`,
              ],
            ]}
          />
          <div className="flex flex-col gap-1">
            <span className="text-muted-foreground">Send a request</span>
            <CopyText
              label="curl command"
              value={`curl ${l.https ? '-k ' : ''}${scheme}://${l.listener}/`}
            />
          </div>
        </div>
      )}
      <p className="flex flex-wrap items-center gap-2 text-sm">
        <span className="text-muted-foreground">Serves</span>
        <RefLink link={fr.target as string | undefined} />
        {typeof proxy.data?.urlMap === 'string' && (
          <>
            <ArrowRight className="size-3.5 text-muted-foreground" aria-hidden />
            <RefLink link={proxy.data.urlMap} />
          </>
        )}
      </p>
    </Card>
  )
}

/** UrlMapPanels are a URL map's route tree and its URL tester. */
function UrlMapPanels({ map, r }: { map: Resource; r: Ref }) {
  const [hit, setHit] = useState<LbRoute>()
  return (
    <>
      <Card aria-labelledby="tree-title">
        <div className="flex flex-wrap items-center justify-between gap-2">
          <CardTitle id="tree-title">Routes</CardTitle>
          <Button variant="outline" size="sm" asChild>
            <Link to={lbPath(r, 'edit')}>
              <Pencil aria-hidden />
              Edit URL map
            </Link>
          </Button>
        </div>
        <RouteTree map={map} hit={hit} />
      </Card>
      <TestUrl map={map} mapRef={r} onResult={setHit} />
    </>
  )
}
