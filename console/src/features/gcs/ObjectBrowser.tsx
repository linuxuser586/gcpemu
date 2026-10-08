import { useInfiniteQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { Download, File, Folder, Trash2, Upload } from 'lucide-react'
import { useCallback, useRef, useState, type DragEvent } from 'react'
import { useSearchParams } from 'react-router'

import { errorMessage } from '@/api/errors'
import { Link } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { ConfirmDialog } from '@/components/resource/ConfirmDialog'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { toast } from '@/lib/toast'
import { cn } from '@/lib/utils'
import { useViewState } from '@/lib/viewState'

import {
  deleteObject,
  downloadUrl,
  objectsQuery,
  uploadObject,
  type ObjectPage,
  type StorageObject,
} from './api'
import { baseName, formatBytes, prefixCrumbs, time } from './format'
import { browsePath, objectPath, useBucket } from './GcsLayout'
import { VirtualRows } from './VirtualRows'

export type Row = { kind: 'prefix'; prefix: string } | { kind: 'object'; object: StorageObject }

/**
 * rowsOf flattens loaded pages into one level of the browser: folders
 * first, then objects, without the zero-byte object that marks a folder.
 */
export function rowsOf(pages: ObjectPage[] | undefined, prefix: string): Row[] {
  const prefixes: Row[] = []
  const objects: Row[] = []
  for (const p of pages ?? []) {
    for (const pre of p.prefixes ?? []) prefixes.push({ kind: 'prefix', prefix: pre })
    for (const o of p.items ?? []) {
      if (o.name !== prefix) objects.push({ kind: 'object', object: o })
    }
  }
  return [...prefixes, ...objects]
}

export interface PendingFile {
  path: string
  file: File
}

type Entry = {
  isFile: boolean
  isDirectory: boolean
  name: string
  file?: (cb: (f: File) => void, err: (e: unknown) => void) => void
  createReader?: () => {
    readEntries: (cb: (es: Entry[]) => void, err: (e: unknown) => void) => void
  }
}

async function walk(entry: Entry, dir: string, out: PendingFile[]) {
  if (entry.isFile && entry.file) {
    const file = await new Promise<File>((res, rej) => entry.file!(res, rej))
    out.push({ path: dir + entry.name, file })
    return
  }
  if (!entry.isDirectory || !entry.createReader) return
  const reader = entry.createReader()
  for (;;) {
    const batch = await new Promise<Entry[]>((res, rej) => reader.readEntries(res, rej))
    if (batch.length === 0) break
    for (const e of batch) await walk(e, `${dir}${entry.name}/`, out)
  }
}

/**
 * droppedFiles returns what was dropped, with the paths of files inside
 * dropped folders. Entries and files must be taken while the drop event
 * runs. A file not backed by one on disk (WebKit: "Path does not exist")
 * has an entry that cannot be read, so then the plain files are used.
 */
export async function droppedFiles(dt: DataTransfer): Promise<PendingFile[]> {
  const items = Array.from(dt.items ?? [])
  const entries = items.map((i) => (i.webkitGetAsEntry?.() ?? null) as Entry | null)
  const files = Array.from(dt.files).map((file) => ({ path: file.name, file }))
  if (entries.length === 0 || entries.some((e) => e === null)) return files
  const out: PendingFile[] = []
  try {
    for (const e of entries) await walk(e!, '', out)
  } catch {
    return files
  }
  return out
}

interface UploadState {
  path: string
  status: 'uploading' | 'done' | 'failed'
  error?: string
}

/** useUploads uploads files under a prefix one at a time, tracking each. */
function useUploads(bucket: string, prefix: string) {
  const qc = useQueryClient()
  const [uploads, setUploads] = useState<UploadState[]>([])
  const upload = useCallback(
    async (files: PendingFile[]) => {
      if (files.length === 0) return
      setUploads(files.map((f) => ({ path: prefix + f.path, status: 'uploading' })))
      let failed = 0
      for (const [i, f] of files.entries()) {
        const set = (s: Partial<UploadState>) =>
          setUploads((u) => u.map((x, j) => (j === i ? { ...x, ...s } : x)))
        try {
          await uploadObject(bucket, prefix + f.path, f.file)
          set({ status: 'done' })
        } catch (e) {
          failed++
          set({ status: 'failed', error: errorMessage(e) })
        }
      }
      void qc.invalidateQueries({ queryKey: ['gcs', 'objects', bucket] })
      const ok = files.length - failed
      if (ok) toast.success(`Uploaded ${ok} ${ok === 1 ? 'object' : 'objects'}.`)
      if (failed) toast.error(`${failed} ${failed === 1 ? 'upload' : 'uploads'} failed.`)
    },
    [bucket, prefix, qc],
  )
  return { uploads, upload, clear: () => setUploads([]) }
}

const ROW_HEIGHT = 40
const GRID = 'grid grid-cols-[minmax(0,1fr)_6rem_10rem_11rem_5.5rem] items-center gap-3 px-3'

/**
 * ObjectBrowser browses a bucket one folder (prefix) at a time, loading
 * pages of objects.list as the list scrolls and rendering only the rows
 * in view. Files dropped on it, or chosen with Upload, are uploaded into
 * the folder. ?prefix= is the folder and ?q= narrows it by name prefix.
 */
export function ObjectBrowser() {
  const bucket = useBucket()
  const [search, setSearch] = useSearchParams()
  const prefix = search.get('prefix') ?? ''
  const [view, setView] = useViewState()
  const [text, setText] = useState(view.q ?? '')
  const query = useInfiniteQuery(objectsQuery(bucket, prefix + text))
  const rows = rowsOf(query.data?.pages, prefix)
  const { uploads, upload, clear } = useUploads(bucket, prefix)
  const [dragging, setDragging] = useState(false)
  const input = useRef<HTMLInputElement>(null)
  const qc = useQueryClient()
  const del = useMutation({
    mutationFn: (name: string) => deleteObject(bucket, name),
    onSuccess: (_r, name) => {
      void qc.invalidateQueries({ queryKey: ['gcs'] })
      toast.success(`Deleted ${name}.`)
    },
  })
  const { hasNextPage, isFetchingNextPage, fetchNextPage } = query
  const onEnd = useCallback(() => {
    if (hasNextPage && !isFetchingNextPage) void fetchNextPage()
  }, [hasNextPage, isFetchingNextPage, fetchNextPage])

  const drop = (e: DragEvent) => {
    e.preventDefault()
    setDragging(false)
    void droppedFiles(e.dataTransfer).then(upload)
  }
  const goTo = (p: string) => {
    setText('')
    setSearch((prev) => {
      const next = new URLSearchParams(prev)
      if (p) next.set('prefix', p)
      else next.delete('prefix')
      next.delete('q')
      return next
    })
  }

  return (
    <Card
      onDragOver={(e) => {
        if (!e.dataTransfer.types.includes('Files')) return
        e.preventDefault()
        setDragging(true)
      }}
      onDragLeave={(e) => {
        if (!e.currentTarget.contains(e.relatedTarget as Node | null)) setDragging(false)
      }}
      onDrop={drop}
      data-testid="object-browser"
      className={cn(dragging && 'outline-2 outline-offset-2 outline-primary outline-dashed')}
    >
      <div className="flex flex-wrap items-end justify-between gap-3">
        <nav aria-label="Folder" className="flex flex-wrap items-center gap-1 font-mono text-sm">
          <button
            type="button"
            className="text-primary underline-offset-4 hover:underline"
            onClick={() => goTo('')}
          >
            {bucket}
          </button>
          {prefixCrumbs(prefix).map(([label, p]) => (
            <span key={p} className="flex items-center gap-1">
              <span className="text-muted-foreground">/</span>
              <button
                type="button"
                className="text-primary underline-offset-4 hover:underline"
                aria-current={p === prefix ? 'location' : undefined}
                onClick={() => goTo(p)}
              >
                {label}
              </button>
            </span>
          ))}
        </nav>
        <div className="flex flex-wrap items-end gap-2">
          <label htmlFor="gcs-object-filter" className="flex flex-col gap-1 text-sm">
            Filter
            <Input
              id="gcs-object-filter"
              type="search"
              className="w-56"
              placeholder="Name prefix"
              value={text}
              onChange={(e) => {
                setText(e.target.value)
                setView({ q: e.target.value })
              }}
            />
          </label>
          <Button onClick={() => input.current?.click()}>
            <Upload aria-hidden />
            Upload files
          </Button>
          <input
            ref={input}
            type="file"
            multiple
            hidden
            aria-label="Files to upload"
            onChange={(e) => {
              const files = Array.from(e.target.files ?? []).map((file) => ({
                path: file.name,
                file,
              }))
              e.target.value = ''
              void upload(files)
            }}
          />
        </div>
      </div>
      <p className="text-xs text-muted-foreground">
        Drop files or folders here to upload them to{' '}
        <span className="font-mono">
          {bucket}/{prefix}
        </span>
        .
      </p>
      {uploads.length > 0 && (
        <div className="flex flex-col gap-1 rounded-md border p-3 text-sm" aria-label="Uploads">
          <div className="flex items-center justify-between">
            <span className="font-medium">Uploads</span>
            {uploads.every((u) => u.status !== 'uploading') && (
              <Button size="sm" variant="ghost" onClick={clear}>
                Dismiss
              </Button>
            )}
          </div>
          <ul className="flex max-h-40 flex-col gap-0.5 overflow-auto">
            {uploads.map((u) => (
              <li key={u.path} data-testid="upload" data-status={u.status} className="flex gap-2">
                <span className="min-w-0 flex-1 truncate font-mono">{u.path}</span>
                <span
                  className={cn(
                    u.status === 'failed' && 'text-destructive',
                    u.status === 'done' && 'text-success',
                  )}
                >
                  {u.status === 'uploading' ? 'Uploading…' : u.status === 'done' ? 'Done' : u.error}
                </span>
              </li>
            ))}
          </ul>
        </div>
      )}
      <QueryStatus query={query} />
      {query.data && rows.length === 0 && (
        <p className="text-sm text-muted-foreground">
          {text ? 'No objects match the filter.' : 'No objects here yet.'}
        </p>
      )}
      {rows.length > 0 && (
        <VirtualRows
          label="Objects"
          rows={rows}
          rowHeight={ROW_HEIGHT}
          height={560}
          onEnd={onEnd}
          header={
            <div role="row" className={cn(GRID, 'h-10 text-sm font-medium')}>
              <span role="columnheader">Name</span>
              <span role="columnheader" className="text-right">
                Size
              </span>
              <span role="columnheader">Type</span>
              <span role="columnheader">Updated</span>
              <span role="columnheader">
                <span className="sr-only">Actions</span>
              </span>
            </div>
          }
          render={(row, i) =>
            row.kind === 'prefix' ? (
              <div
                role="row"
                aria-rowindex={i + 2}
                data-testid="folder"
                className={cn(GRID, 'h-full border-b text-sm hover:bg-muted/50')}
              >
                <span role="cell" className="flex min-w-0 items-center gap-2">
                  <Folder className="size-4 shrink-0 text-muted-foreground" aria-hidden />
                  <Link
                    to={browsePath(bucket, row.prefix)}
                    className="truncate font-mono text-primary underline-offset-4 hover:underline"
                  >
                    {baseName(row.prefix)}
                  </Link>
                </span>
                <span role="cell" className="text-right text-muted-foreground">
                  —
                </span>
                <span role="cell" className="text-muted-foreground">
                  Folder
                </span>
                <span role="cell" />
                <span role="cell" />
              </div>
            ) : (
              <ObjectRow object={row.object} index={i} onDelete={(name) => del.mutateAsync(name)} />
            )
          }
        />
      )}
      {isFetchingNextPage && <p className="text-sm text-muted-foreground">Loading more…</p>}
    </Card>
  )
}

function ObjectRow({
  object: o,
  index,
  onDelete,
}: {
  object: StorageObject
  index: number
  onDelete: (name: string) => Promise<unknown>
}) {
  const name = baseName(o.name)
  return (
    <div
      role="row"
      aria-rowindex={index + 2}
      data-testid="object"
      className={cn(GRID, 'h-full border-b text-sm hover:bg-muted/50')}
    >
      <span role="cell" className="flex min-w-0 items-center gap-2">
        <File className="size-4 shrink-0 text-muted-foreground" aria-hidden />
        <Link
          to={objectPath(o.bucket, o.name)}
          className="truncate font-mono text-primary underline-offset-4 hover:underline"
        >
          {name}
        </Link>
      </span>
      <span role="cell" className="text-right tabular-nums">
        {formatBytes(o.size)}
      </span>
      <span role="cell" className="truncate font-mono text-xs">
        {o.contentType}
      </span>
      <span role="cell" className="truncate">
        {time(o.updated)}
      </span>
      <span role="cell" className="flex justify-end gap-0.5">
        <Button variant="ghost" size="icon" asChild>
          <a href={downloadUrl(o.bucket, o.name)} download={name} aria-label={`Download ${name}`}>
            <Download aria-hidden />
          </a>
        </Button>
        <ConfirmDialog
          trigger={
            <Button variant="ghost" size="icon" aria-label={`Delete ${name}`}>
              <Trash2 aria-hidden />
            </Button>
          }
          title={`Delete ${o.name}?`}
          description="With object versioning on, it is kept as a noncurrent generation."
          confirmLabel="Delete object"
          onConfirm={() => onDelete(o.name)}
        />
      </span>
    </div>
  )
}
