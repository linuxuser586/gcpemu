import { zodResolver } from '@hookform/resolvers/zod'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Download, History, Pencil, Trash2 } from 'lucide-react'
import { useForm } from 'react-hook-form'
import { z } from 'zod'

import { ApiError } from '@/api/errors'
import { Link, useCarriedNavigate } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { ConfirmDialog } from '@/components/resource/ConfirmDialog'
import { DetailList, JsonView, Mono } from '@/components/resource/DetailList'
import { Field, fieldAria } from '@/components/resource/Field'
import { ResourceEditor, type Body } from '@/components/resource/ResourceEditor'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { Textarea } from '@/components/ui/textarea'
import { toast } from '@/lib/toast'

import {
  deleteObject,
  downloadUrl,
  generationsQuery,
  objectQuery,
  patchObject,
  restoreGeneration,
  type StorageObject,
} from './api'
import { baseName, formatBytes, parentPrefix, prefixCrumbs, time } from './format'
import { browsePath, objectPath, useBucket, useObjectName } from './GcsLayout'
import { SignedUrlDialog } from './SignedUrlDialog'

const notFound = (e: unknown) => e instanceof ApiError && e.httpStatus === 404

/**
 * ObjectPage is one object: its metadata, its generations (FR-GCS-003)
 * with download, restore and delete of each, and the live object's
 * actions. An object whose live version was deleted is still shown when
 * noncurrent generations of it remain, so that one can be restored.
 */
export function ObjectPage() {
  const bucket = useBucket()
  const name = useObjectName()
  const query = useQuery(objectQuery(bucket, name))
  const gens = useQuery(generationsQuery(bucket, name))
  const o = query.data
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const del = useMutation({
    mutationFn: () => deleteObject(bucket, name),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['gcs'] })
      toast.success(`Deleted ${name}.`)
      navigate(browsePath(bucket, parentPrefix(name)))
    },
  })
  const deleted = notFound(query.error)

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="flex min-w-0 flex-col gap-1">
          <p className="flex flex-wrap gap-1 font-mono text-sm text-muted-foreground">
            <Link to={browsePath(bucket)} className="underline-offset-4 hover:underline">
              {bucket}
            </Link>
            {prefixCrumbs(parentPrefix(name)).map(([label, p]) => (
              <span key={p}>
                /{' '}
                <Link to={browsePath(bucket, p)} className="underline-offset-4 hover:underline">
                  {label}
                </Link>
              </span>
            ))}
          </p>
          <h2 className="text-xl font-semibold break-all">
            <span className="font-mono">{baseName(name)}</span>
          </h2>
        </div>
        {o && (
          <div className="flex flex-wrap gap-2">
            <Button variant="outline" asChild>
              <a href={downloadUrl(bucket, name)} download={baseName(name)}>
                <Download aria-hidden />
                Download
              </a>
            </Button>
            <SignedUrlDialog bucket={bucket} object={name} />
            <Button variant="outline" asChild>
              <Link to={objectPath(bucket, name, 'edit')}>
                <Pencil aria-hidden />
                Edit metadata
              </Link>
            </Button>
            <ConfirmDialog
              trigger={
                <Button variant="outline">
                  <Trash2 aria-hidden />
                  Delete
                </Button>
              }
              title={`Delete ${name}?`}
              description="With object versioning on, the live version is kept as a noncurrent generation that can be restored."
              confirmLabel="Delete object"
              onConfirm={() => del.mutateAsync()}
            />
          </div>
        )}
      </div>
      {deleted ? (
        <Card role="status">
          <p className="text-sm text-muted-foreground">
            This object has no live version.
            {gens.data?.length ? ' Restore one of its noncurrent generations below.' : ''}
          </p>
        </Card>
      ) : (
        <QueryStatus query={query} />
      )}
      {o && <ObjectDetails o={o} />}
      <Generations bucket={bucket} name={name} />
      {o && (
        <Card>
          <CardTitle>Object resource</CardTitle>
          <JsonView value={o} label="Object JSON" />
        </Card>
      )}
    </div>
  )
}

function ObjectDetails({ o }: { o: StorageObject }) {
  const metadata = Object.entries(o.metadata ?? {})
  return (
    <Card>
      <CardTitle>Metadata</CardTitle>
      <DetailList
        label="Object details"
        rows={[
          [
            'Size',
            `${formatBytes(o.size)}${o.size && Number(o.size) >= 1024 ? ` (${o.size} bytes)` : ''}`,
          ],
          ['Content type', o.contentType && <Mono>{o.contentType}</Mono>],
          ['Content encoding', o.contentEncoding && <Mono>{o.contentEncoding}</Mono>],
          ['Content disposition', o.contentDisposition && <Mono>{o.contentDisposition}</Mono>],
          ['Content language', o.contentLanguage && <Mono>{o.contentLanguage}</Mono>],
          ['Cache control', o.cacheControl && <Mono>{o.cacheControl}</Mono>],
          ['Storage class', o.storageClass],
          ['Generation', o.generation && <Mono>{o.generation}</Mono>],
          ['Metageneration', o.metageneration],
          ['MD5', o.md5Hash && <Mono>{o.md5Hash}</Mono>],
          ['CRC32C', o.crc32c && <Mono>{o.crc32c}</Mono>],
          ['Temporary hold', o.temporaryHold ? 'On' : undefined],
          ['Event-based hold', o.eventBasedHold ? 'On' : undefined],
          ['Retained until', time(o.retentionExpirationTime)],
          ['Components', o.componentCount ? String(o.componentCount) : undefined],
          [
            'Custom metadata',
            metadata.length > 0 && <Mono>{metadata.map(([k, v]) => `${k}: ${v}`).join(', ')}</Mono>,
          ],
          ['Created', time(o.timeCreated)],
          ['Updated', time(o.updated)],
        ]}
      />
    </Card>
  )
}

/** Generations lists every generation of an object, live first. */
function Generations({ bucket, name }: { bucket: string; name: string }) {
  const query = useQuery(generationsQuery(bucket, name))
  const qc = useQueryClient()
  const done = (msg: string) => {
    void qc.invalidateQueries({ queryKey: ['gcs'] })
    toast.success(msg)
  }
  const restore = useMutation({
    mutationFn: (g: string) => restoreGeneration(bucket, name, g),
    onSuccess: (o, g) => done(`Restored generation ${g} as generation ${o.generation}.`),
  })
  const remove = useMutation({
    mutationFn: (g: string) => deleteObject(bucket, name, g),
    onSuccess: (_r, g) => done(`Deleted generation ${g}.`),
  })
  const gens = query.data ?? []
  return (
    <Card>
      <CardTitle>Generations</CardTitle>
      <QueryStatus query={query} />
      {query.data && gens.length === 0 && (
        <p className="text-sm text-muted-foreground">This object has no generations.</p>
      )}
      {gens.length > 0 && (
        <Table aria-label="Generations">
          <TableHeader>
            <TableRow>
              <TableHead>Generation</TableHead>
              <TableHead>Status</TableHead>
              <TableHead className="text-right">Size</TableHead>
              <TableHead>Created</TableHead>
              <TableHead>
                <span className="sr-only">Actions</span>
              </TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {gens.map((g) => {
              const gen = g.generation ?? ''
              const live = !g.timeDeleted
              return (
                <TableRow key={gen} data-testid="generation">
                  <TableCell className="font-mono">{gen}</TableCell>
                  <TableCell>
                    {live ? (
                      <Badge variant="success">Live</Badge>
                    ) : (
                      <span className="flex flex-col">
                        <Badge>Noncurrent</Badge>
                        <span className="text-xs text-muted-foreground">
                          since {time(g.timeDeleted)}
                        </span>
                      </span>
                    )}
                  </TableCell>
                  <TableCell className="text-right tabular-nums">{formatBytes(g.size)}</TableCell>
                  <TableCell>{time(g.timeCreated)}</TableCell>
                  <TableCell>
                    <div className="flex justify-end gap-1">
                      <Button variant="ghost" size="icon" asChild>
                        <a
                          href={downloadUrl(bucket, name, gen)}
                          download={baseName(name)}
                          aria-label={`Download generation ${gen}`}
                        >
                          <Download aria-hidden />
                        </a>
                      </Button>
                      {!live && (
                        <ConfirmDialog
                          trigger={
                            <Button
                              variant="ghost"
                              size="icon"
                              aria-label={`Restore generation ${gen}`}
                            >
                              <History aria-hidden />
                            </Button>
                          }
                          title={`Restore generation ${gen}?`}
                          description="It is copied over the object as a new live generation; with versioning on, the current live version becomes noncurrent."
                          confirmLabel="Restore"
                          onConfirm={() => restore.mutateAsync(gen)}
                        />
                      )}
                      <ConfirmDialog
                        trigger={
                          <Button
                            variant="ghost"
                            size="icon"
                            aria-label={`Delete generation ${gen}`}
                          >
                            <Trash2 aria-hidden />
                          </Button>
                        }
                        title={`Delete generation ${gen}?`}
                        description="This generation is removed for good."
                        confirmLabel="Delete generation"
                        onConfirm={() => remove.mutateAsync(gen)}
                      />
                    </div>
                  </TableCell>
                </TableRow>
              )
            })}
          </TableBody>
        </Table>
      )}
    </Card>
  )
}

// ---- edit metadata ----

// The metadata form (FR-GCS-002 metadata patch, FR-UI-011) sends an
// objects.patch merge patch of the fields that changed.

const schema = z.object({
  contentType: z.string().trim(),
  cacheControl: z.string().trim(),
  contentDisposition: z.string().trim(),
  contentEncoding: z.string().trim(),
  contentLanguage: z.string().trim(),
  metadata: z.string().superRefine((v, ctx) => {
    for (const line of v.split('\n')) {
      if (line.trim() && !line.includes(':')) {
        ctx.addIssue({ code: 'custom', message: `Expected "key: value", got "${line.trim()}".` })
        return
      }
    }
  }),
  temporaryHold: z.boolean(),
  eventBasedHold: z.boolean(),
})
type Values = z.infer<typeof schema>

const TEXT_FIELDS = [
  'contentType',
  'cacheControl',
  'contentDisposition',
  'contentEncoding',
  'contentLanguage',
] as const

export const formatMetadata = (m?: Record<string, string>) =>
  Object.entries(m ?? {})
    .map(([k, v]) => `${k}: ${v}`)
    .join('\n')

export function parseMetadata(text: string): Record<string, string> {
  const out: Record<string, string> = {}
  for (const line of text.split('\n')) {
    const i = line.indexOf(':')
    if (i < 0) continue
    const k = line.slice(0, i).trim()
    if (k) out[k] = line.slice(i + 1).trim()
  }
  return out
}

function values(o: StorageObject): Values {
  return {
    contentType: o.contentType ?? '',
    cacheControl: o.cacheControl ?? '',
    contentDisposition: o.contentDisposition ?? '',
    contentEncoding: o.contentEncoding ?? '',
    contentLanguage: o.contentLanguage ?? '',
    metadata: formatMetadata(o.metadata),
    temporaryHold: !!o.temporaryHold,
    eventBasedHold: !!o.eventBasedHold,
  }
}

/** objectPatch writes what changed onto an objects.patch body. */
export function objectPatch(o: StorageObject, v: Values, base: Body): Body {
  const was = values(o)
  const out: Body = { ...base }
  for (const k of TEXT_FIELDS) {
    if (v[k] !== was[k]) out[k] = v[k] || null
    else delete out[k]
  }
  if (v.metadata !== was.metadata) {
    const next = parseMetadata(v.metadata)
    const m: Record<string, string | null> = { ...next }
    for (const k of Object.keys(o.metadata ?? {})) if (!(k in next)) m[k] = null
    out.metadata = m
  } else delete out.metadata
  for (const k of ['temporaryHold', 'eventBasedHold'] as const) {
    if (v[k] !== was[k]) out[k] = v[k]
    else delete out[k]
  }
  return out
}

function patchValues(o: StorageObject) {
  return (body: Body): Values => {
    const v = values(o)
    for (const k of TEXT_FIELDS) {
      if (k in body) v[k] = typeof body[k] === 'string' ? body[k] : ''
    }
    if (typeof body.metadata === 'object' && body.metadata !== null) {
      v.metadata = formatMetadata(
        Object.fromEntries(
          Object.entries(body.metadata as Record<string, unknown>).filter(
            (e): e is [string, string] => typeof e[1] === 'string',
          ),
        ),
      )
    }
    if (typeof body.temporaryHold === 'boolean') v.temporaryHold = body.temporaryHold
    if (typeof body.eventBasedHold === 'boolean') v.eventBasedHold = body.eventBasedHold
    return v
  }
}

export function EditObject() {
  const bucket = useBucket()
  const name = useObjectName()
  const query = useQuery(objectQuery(bucket, name))
  if (!query.data) return <QueryStatus query={query} />
  return <EditObjectForm o={query.data} />
}

const TEXT_LABELS: Record<(typeof TEXT_FIELDS)[number], string> = {
  contentType: 'Content type',
  cacheControl: 'Cache control',
  contentDisposition: 'Content disposition',
  contentEncoding: 'Content encoding',
  contentLanguage: 'Content language',
}

function EditObjectForm({ o }: { o: StorageObject }) {
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const form = useForm<Values>({ resolver: zodResolver(schema), defaultValues: values(o) })
  const errors = form.formState.errors
  const back = () => navigate(objectPath(o.bucket, o.name))
  const save = useMutation({
    meta: { toast: false },
    mutationFn: async (body: Body) => {
      if (Object.keys(body).length === 0) return false
      await patchObject(o.bucket, o.name, body)
      return true
    },
    onSuccess: (changed) => {
      void qc.invalidateQueries({ queryKey: ['gcs'] })
      toast.success(changed ? `Updated ${o.name}.` : 'Nothing to change.')
      back()
    },
  })
  return (
    <Card>
      <h2 className="text-lg font-semibold break-all">
        Edit metadata of <span className="font-mono">{o.name}</span>
      </h2>
      <ResourceEditor
        form={form}
        initialBody={{}}
        toBody={(v, base) => objectPatch(o, v, base)}
        fromBody={patchValues(o)}
        fields={[...TEXT_FIELDS, 'metadata']}
        onSubmit={(body) => save.mutateAsync(body)}
        submitLabel="Save"
        onCancel={back}
        jsonHint={
          <>
            Sent to <span className="font-mono">objects.patch</span> as a JSON merge patch: null
            removes a field.
          </>
        }
      >
        <div className="grid gap-4 sm:grid-cols-2">
          {TEXT_FIELDS.map((k) => (
            <Field key={k} id={`object-${k}`} label={TEXT_LABELS[k]}>
              <Input spellCheck={false} {...fieldAria(`object-${k}`)} {...form.register(k)} />
            </Field>
          ))}
        </div>
        <Field
          id="object-metadata"
          label="Custom metadata"
          error={errors.metadata?.message}
          hint="key: value, one per line"
        >
          <Textarea
            rows={4}
            spellCheck={false}
            className="font-mono"
            {...fieldAria('object-metadata', errors.metadata?.message, 'key')}
            {...form.register('metadata')}
          />
        </Field>
        {(['temporaryHold', 'eventBasedHold'] as const).map((k) => (
          <div key={k} className="flex items-center gap-2">
            <input id={`object-${k}`} type="checkbox" className="size-4" {...form.register(k)} />
            <label htmlFor={`object-${k}`} className="text-sm font-medium">
              {k === 'temporaryHold' ? 'Temporary hold' : 'Event-based hold'}
            </label>
          </div>
        ))}
      </ResourceEditor>
    </Card>
  )
}
