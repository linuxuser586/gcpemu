import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Copy, Plus, Tag as TagIcon, Trash2, X } from 'lucide-react'
import { Fragment, useId, useState } from 'react'

import { errorMessage } from '@/api/errors'
import { Link, useCarriedNavigate } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { ConfirmDialog } from '@/components/resource/ConfirmDialog'
import { DetailList, JsonView, Mono } from '@/components/resource/DetailList'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardTitle } from '@/components/ui/card'
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogTitle,
  DialogTrigger,
} from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { toast } from '@/lib/toast'

import { formatBytes, time } from '../gcs/format'
import { CopyText } from '../sql/InstancePage'
import {
  createTag,
  deleteTag,
  deleteVersion,
  digestOf,
  dockerImagesQuery,
  imageRef,
  moveTag,
  platform,
  shortDigest,
  TAG,
  tagMessage,
  type DockerImage,
  type RepoRef,
} from './api'
import {
  ChooseProject,
  imagePath,
  repoPath,
  useDigest,
  useImage,
  useRegistryHost,
  useRepoRef,
} from './ArLayout'
import { imageOfDocker } from './RepositoryPage'

/** isIndex reports whether a manifest is a multi-arch index (manifest list). */
export const isIndex = (d: DockerImage) =>
  d.mediaType === 'application/vnd.oci.image.index.v1+json' ||
  d.mediaType === 'application/vnd.docker.distribution.manifest.list.v2+json'

/** kindLabel names a manifest's media type for people. */
export function kindLabel(d: DockerImage): string {
  if (isIndex(d)) return 'Index'
  if (d.artifactType) return 'Artifact'
  return 'Image'
}

/**
 * imageDigests are an image's digests, newest first, with the manifests
 * of an index nested under it rather than listed on their own.
 */
export function imageDigests(all: DockerImage[], image: string) {
  const mine = all.filter((d) => imageOfDocker(d) === image)
  const byDigest = new Map(mine.map((d) => [digestOf(d.name), d]))
  const children = new Set<string>()
  for (const d of mine) for (const m of d.imageManifests ?? []) children.add(m.digest ?? '')
  return {
    byDigest,
    top: mine.filter((d) => !children.has(digestOf(d.name)) || (d.tags?.length ?? 0) > 0),
  }
}

/** usePull returns the pull command of image at a tag or digest. */
function usePull(ref: RepoRef, image: string) {
  const host = useRegistryHost(ref.location)
  return (tagOrDigest: string) => `docker pull ${imageRef(host, ref, image, tagOrDigest)}`
}

/** CopyButton copies a command to the clipboard. */
function CopyButton({ label, value }: { label: string; value: string }) {
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(value)
      toast.success(`Copied: ${value}`)
    } catch {
      toast.error('The clipboard is not available here; select the text to copy it.')
    }
  }
  return (
    <Button
      size="sm"
      variant="outline"
      aria-label={label}
      title={value}
      onClick={() => void copy()}
    >
      <Copy aria-hidden />
      Pull
    </Button>
  )
}

/** TagChip is a tag with a button that deletes it. */
function TagChip({ repo, image, tag }: { repo: RepoRef; image: string; tag: string }) {
  const qc = useQueryClient()
  const del = useMutation({
    mutationFn: () => deleteTag(repo, image, tag),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['ar'] })
      toast.success(`Deleted tag ${tag}.`)
    },
  })
  return (
    <Badge className="gap-1 font-mono" data-testid="tag">
      {tag}
      <ConfirmDialog
        trigger={
          <button
            type="button"
            aria-label={`Delete tag ${tag}`}
            className="rounded-sm outline-none hover:text-destructive focus-visible:ring-[3px] focus-visible:ring-ring/50"
          >
            <X className="size-3" aria-hidden />
          </button>
        }
        title={`Delete tag ${tag}?`}
        description="The digest it points at is kept; only the tag is removed."
        confirmLabel="Delete tag"
        onConfirm={() => del.mutateAsync()}
      />
    </Badge>
  )
}

/**
 * AddTag tags a digest: a new tag is created, and a tag already on
 * another digest of the image is moved to this one.
 */
function AddTag({
  repo,
  image,
  digest,
  existing,
}: {
  repo: RepoRef
  image: string
  digest: string
  existing: Set<string>
}) {
  const [open, setOpen] = useState(false)
  const [tag, setTag] = useState('')
  const [error, setError] = useState<string>()
  const id = useId()
  const qc = useQueryClient()
  const add = useMutation({
    meta: { toast: false },
    mutationFn: () =>
      existing.has(tag) ? moveTag(repo, image, tag, digest) : createTag(repo, image, tag, digest),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['ar'] })
      toast.success(`${existing.has(tag) ? 'Moved' : 'Added'} tag ${tag}.`)
      setOpen(false)
      setTag('')
    },
    onError: (e) => setError(errorMessage(e)),
  })
  return (
    <Dialog
      open={open}
      onOpenChange={(o) => {
        setOpen(o)
        setError(undefined)
      }}
    >
      <DialogTrigger asChild>
        <Button size="sm" variant="outline" aria-label={`Add tag to ${shortDigest(digest)}`}>
          <TagIcon aria-hidden />
          Tag
        </Button>
      </DialogTrigger>
      <DialogContent>
        <DialogTitle>Add tag</DialogTitle>
        <DialogDescription>
          Points a tag at <span className="font-mono">{shortDigest(digest)}</span>. A tag already on
          another digest of the image moves here.
        </DialogDescription>
        <form
          className="flex flex-col gap-3"
          onSubmit={(e) => {
            e.preventDefault()
            if (!TAG.test(tag)) {
              setError(tagMessage(tag))
              return
            }
            add.mutate()
          }}
        >
          <label htmlFor={id} className="text-sm font-medium">
            Tag
          </label>
          <Input
            id={id}
            autoComplete="off"
            spellCheck={false}
            className="font-mono"
            value={tag}
            onChange={(e) => setTag(e.target.value)}
          />
          {error && (
            <p role="alert" className="text-sm text-destructive">
              {error}
            </p>
          )}
          <DialogFooter>
            <DialogClose asChild>
              <Button type="button" variant="outline">
                Cancel
              </Button>
            </DialogClose>
            <Button type="submit" disabled={add.isPending}>
              <Plus aria-hidden />
              {existing.has(tag) ? 'Move tag' : 'Add tag'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

/** DeleteDigest deletes a digest and the tags on it. */
function DeleteDigest({
  repo,
  image,
  d,
  onDone,
}: {
  repo: RepoRef
  image: string
  d: DockerImage
  onDone?: () => void
}) {
  const qc = useQueryClient()
  const digest = digestOf(d.name)
  const del = useMutation({
    mutationFn: () => deleteVersion(repo, image, digest),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['ar'] })
      toast.success(`Deleted digest ${shortDigest(digest)}.`)
      onDone?.()
    },
  })
  const tags = d.tags ?? []
  return (
    <ConfirmDialog
      trigger={
        <Button size="sm" variant="outline" aria-label={`Delete digest ${shortDigest(digest)}`}>
          <Trash2 aria-hidden />
          Delete
        </Button>
      }
      title={`Delete digest ${shortDigest(digest)}?`}
      description={
        tags.length > 0
          ? `Its tags (${tags.join(', ')}) are deleted with it. This cannot be undone.`
          : 'This cannot be undone.'
      }
      confirmLabel="Delete digest"
      onConfirm={() => del.mutateAsync()}
    />
  )
}

/** Breadcrumb leads back to the repository and image. */
function Breadcrumb({ repo, image }: { repo: RepoRef; image?: string }) {
  return (
    <p className="text-sm text-muted-foreground">
      <Link to="/ar" className="underline-offset-4 hover:underline">
        Repositories
      </Link>{' '}
      /{' '}
      <Link to={repoPath(repo)} className="font-mono underline-offset-4 hover:underline">
        {repo.repo}
      </Link>
      {image && (
        <>
          {' '}
          /{' '}
          <Link
            to={imagePath(repo, image)}
            className="font-mono underline-offset-4 hover:underline"
          >
            {image}
          </Link>
        </>
      )}
    </p>
  )
}

/**
 * ImagePage is one image: its digests, newest first, with their tags,
 * sizes and, for a multi-arch index, the manifest of each platform.
 * Tags are added, moved and deleted, digests deleted, and each has a
 * pull command to copy.
 */
export function ImagePage() {
  const ref = useRepoRef()
  const image = useImage()
  const query = useQuery({ ...dockerImagesQuery(ref), enabled: !!ref.project })
  const pull = usePull(ref, image)
  if (!ref.project) return <ChooseProject what="see its images" />
  const { byDigest, top } = imageDigests(query.data ?? [], image)
  const existing = new Set(top.flatMap((d) => d.tags ?? []))

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-col gap-1">
        <Breadcrumb repo={ref} />
        <h1 className="text-2xl font-semibold">
          <span className="font-mono">{image}</span>
        </h1>
      </div>
      <Card>
        <CardTitle>Digests</CardTitle>
        <QueryStatus query={query} />
        {query.data && top.length === 0 && (
          <p className="text-sm text-muted-foreground">
            The image has no digests: it was deleted, or never pushed.
          </p>
        )}
        {top.length > 0 && (
          <Table aria-label="Digests">
            <TableHeader>
              <TableRow>
                <TableHead>Digest</TableHead>
                <TableHead>Tags</TableHead>
                <TableHead>Kind</TableHead>
                <TableHead className="text-right">Size</TableHead>
                <TableHead>Uploaded</TableHead>
                <TableHead>
                  <span className="sr-only">Actions</span>
                </TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {top.map((d) => {
                const digest = digestOf(d.name)
                const short = shortDigest(digest)
                const pullRef = d.tags?.[0] ?? digest
                return (
                  <Fragment key={d.name}>
                    <TableRow data-testid="digest">
                      <TableCell>
                        <Link
                          className="font-mono text-primary underline-offset-4 hover:underline"
                          to={imagePath(ref, image, digest)}
                          title={digest}
                        >
                          {short}
                        </Link>
                      </TableCell>
                      <TableCell>
                        <div className="flex flex-wrap gap-1">
                          {(d.tags ?? []).map((t) => (
                            <TagChip key={t} repo={ref} image={image} tag={t} />
                          ))}
                        </div>
                      </TableCell>
                      <TableCell>{kindLabel(d)}</TableCell>
                      <TableCell className="text-right">
                        {formatBytes(d.imageSizeBytes ?? 0)}
                      </TableCell>
                      <TableCell>{time(d.uploadTime)}</TableCell>
                      <TableCell>
                        <div className="flex flex-wrap justify-end gap-2">
                          <CopyButton
                            label={`Copy pull command of ${short}`}
                            value={pull(pullRef)}
                          />
                          <AddTag repo={ref} image={image} digest={digest} existing={existing} />
                          <DeleteDigest repo={ref} image={image} d={d} />
                        </div>
                      </TableCell>
                    </TableRow>
                    {(d.imageManifests ?? []).map((m) => {
                      const child = byDigest.get(m.digest ?? '')
                      return (
                        <TableRow
                          key={`${d.name}/${m.digest}`}
                          data-testid="platform"
                          className="bg-muted/40"
                        >
                          <TableCell className="pl-8 font-mono text-xs" title={m.digest}>
                            ↳ {shortDigest(m.digest ?? '')}
                          </TableCell>
                          <TableCell className="font-mono text-xs">{platform(m)}</TableCell>
                          <TableCell>Platform</TableCell>
                          <TableCell className="text-right">
                            {child ? formatBytes(child.imageSizeBytes ?? 0) : '—'}
                          </TableCell>
                          <TableCell>{time(child?.uploadTime)}</TableCell>
                          <TableCell>
                            <div className="flex justify-end">
                              <CopyButton
                                label={`Copy pull command of ${platform(m)}`}
                                value={pull(m.digest ?? '')}
                              />
                            </div>
                          </TableCell>
                        </TableRow>
                      )
                    })}
                  </Fragment>
                )
              })}
            </TableBody>
          </Table>
        )}
      </Card>
    </div>
  )
}

/**
 * DigestPage is one digest of an image: its manifest's details, tags,
 * per-platform manifests and pull commands, and the resource as JSON.
 */
export function DigestPage() {
  const ref = useRepoRef()
  const image = useImage()
  const digest = useDigest()
  const query = useQuery({ ...dockerImagesQuery(ref), enabled: !!ref.project })
  const pull = usePull(ref, image)
  const navigate = useCarriedNavigate()
  if (!ref.project) return <ChooseProject what="see its images" />
  const { byDigest, top } = imageDigests(query.data ?? [], image)
  const d = byDigest.get(digest)
  const existing = new Set(top.flatMap((x) => x.tags ?? []))
  const parents = top.filter((x) => x.imageManifests?.some((m) => m.digest === digest))

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="flex flex-col gap-1">
          <Breadcrumb repo={ref} image={image} />
          <h1 className="text-2xl font-semibold">
            <span className="font-mono">{shortDigest(digest)}</span>
          </h1>
        </div>
        {d && (
          <div className="flex flex-wrap gap-2">
            <AddTag repo={ref} image={image} digest={digest} existing={existing} />
            <DeleteDigest
              repo={ref}
              image={image}
              d={d}
              onDone={() => navigate(imagePath(ref, image))}
            />
          </div>
        )}
      </div>
      <QueryStatus query={query} />
      {query.data && !d && (
        <Card role="status">
          <p className="text-sm text-muted-foreground">
            Digest <span className="font-mono">{digest}</span> is not in image{' '}
            <span className="font-mono">{image}</span>.
          </p>
        </Card>
      )}
      {d && (
        <>
          <Card>
            <CardTitle>Manifest</CardTitle>
            <DetailList
              label="Digest details"
              rows={[
                ['Digest', <Mono key="d">{digest}</Mono>],
                ['URI', <Mono key="u">{d.uri}</Mono>],
                ['Kind', kindLabel(d)],
                ['Media type', <Mono key="m">{d.mediaType}</Mono>],
                ['Artifact type', d.artifactType && <Mono>{d.artifactType}</Mono>],
                [
                  'Tags',
                  (d.tags?.length ?? 0) > 0 ? (
                    <div className="flex flex-wrap gap-1">
                      {d.tags!.map((t) => (
                        <TagChip key={t} repo={ref} image={image} tag={t} />
                      ))}
                    </div>
                  ) : undefined,
                ],
                ['Size', formatBytes(d.imageSizeBytes ?? 0)],
                [
                  'In index',
                  parents.length > 0 ? (
                    <Mono>{parents.map((p) => shortDigest(digestOf(p.name))).join(', ')}</Mono>
                  ) : undefined,
                ],
                ['Uploaded', time(d.uploadTime)],
                ['Built', time(d.buildTime)],
                ['Updated', time(d.updateTime)],
              ]}
            />
          </Card>
          {(d.imageManifests?.length ?? 0) > 0 && (
            <Card>
              <CardTitle>Platforms</CardTitle>
              <Table aria-label="Platforms">
                <TableHeader>
                  <TableRow>
                    <TableHead>Platform</TableHead>
                    <TableHead>Digest</TableHead>
                    <TableHead>Media type</TableHead>
                    <TableHead className="text-right">Size</TableHead>
                    <TableHead>
                      <span className="sr-only">Actions</span>
                    </TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {d.imageManifests!.map((m) => {
                    const child = byDigest.get(m.digest ?? '')
                    return (
                      <TableRow key={m.digest} data-testid="platform">
                        <TableCell className="font-mono">
                          {platform(m)}
                          {m.osVersion ? ` (${m.osVersion})` : ''}
                        </TableCell>
                        <TableCell className="font-mono text-xs">
                          {child ? (
                            <Link
                              className="text-primary underline-offset-4 hover:underline"
                              to={imagePath(ref, image, m.digest ?? '')}
                            >
                              {m.digest}
                            </Link>
                          ) : (
                            m.digest
                          )}
                        </TableCell>
                        <TableCell className="font-mono text-xs">{m.mediaType}</TableCell>
                        <TableCell className="text-right">
                          {child ? formatBytes(child.imageSizeBytes ?? 0) : '—'}
                        </TableCell>
                        <TableCell>
                          <div className="flex justify-end">
                            <CopyButton
                              label={`Copy pull command of ${platform(m)}`}
                              value={pull(m.digest ?? '')}
                            />
                          </div>
                        </TableCell>
                      </TableRow>
                    )
                  })}
                </TableBody>
              </Table>
            </Card>
          )}
          <Card>
            <CardTitle>Pull</CardTitle>
            <CopyText label="pull command by digest" value={pull(digest)} />
            {(d.tags ?? []).map((t) => (
              <CopyText key={t} label={`pull command of tag ${t}`} value={pull(t)} />
            ))}
          </Card>
          <Card>
            <CardTitle>Docker image resource</CardTitle>
            <JsonView value={d} label="Docker image JSON" />
          </Card>
        </>
      )}
    </div>
  )
}
