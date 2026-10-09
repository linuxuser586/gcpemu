import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Pencil, Trash2 } from 'lucide-react'
import { Outlet, useLocation, useOutletContext } from 'react-router'

import { Link, useCarriedNavigate } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { ConfirmDialog } from '@/components/resource/ConfirmDialog'
import { DetailList, JsonView, Mono } from '@/components/resource/DetailList'
import { Button } from '@/components/ui/button'
import { Card, CardTitle } from '@/components/ui/card'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { toast } from '@/lib/toast'
import { cn } from '@/lib/utils'

import { formatBytes, time } from '../gcs/format'
import { CopyText } from '../sql/InstancePage'
import {
  deletePackage,
  deleteRepository,
  dockerImagesQuery,
  imageOf,
  lastSegment,
  packagesQuery,
  repositoryQuery,
  type DockerImage,
  type Repository,
} from './api'
import { ChooseProject, imagePath, repoPath, useRegistryHost, useRepoRef } from './ArLayout'
import { MODE_LABEL } from './Repositories'

const TABS = [
  ['', 'Images'],
  ['overview', 'Overview'],
] as const

/**
 * RepositoryPage is one repository: edit and delete above tabs for its
 * images and its configuration.
 */
export function RepositoryPage() {
  const ref = useRepoRef()
  const query = useQuery({ ...repositoryQuery(ref), enabled: !!ref.project })
  const r = query.data
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const { pathname } = useLocation()
  const del = useMutation({
    mutationFn: () => deleteRepository(ref),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['ar'] })
      toast.success(`Deleted repository ${ref.repo}.`)
      navigate('/ar')
    },
  })
  if (!ref.project) return <ChooseProject what="see its repositories" />
  const base = repoPath(ref)
  const rest = pathname.slice(pathname.indexOf(base) + base.length).split('/')[1] ?? ''
  const active = TABS.some(([p]) => p === rest) ? rest : ''

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="flex flex-col gap-1">
          <p className="text-sm text-muted-foreground">
            <Link to="/ar" className="underline-offset-4 hover:underline">
              Repositories
            </Link>{' '}
            / <span className="font-mono">{ref.location}</span>
          </p>
          <h1 className="text-2xl font-semibold">
            <span className="font-mono">{ref.repo}</span>
          </h1>
        </div>
        {r && (
          <div className="flex flex-wrap gap-2">
            <Button variant="outline" asChild>
              <Link to={repoPath(ref, 'edit')}>
                <Pencil aria-hidden />
                Edit repository
              </Link>
            </Button>
            <ConfirmDialog
              trigger={
                <Button variant="outline">
                  <Trash2 aria-hidden />
                  Delete repository
                </Button>
              }
              title={`Delete repository ${ref.repo}?`}
              description="Every image, digest and tag in it is deleted. This cannot be undone."
              confirmLabel="Delete repository"
              onConfirm={() => del.mutateAsync()}
            />
          </div>
        )}
      </div>
      <QueryStatus query={query} />
      {r && (
        <>
          <nav aria-label="Repository" className="flex flex-wrap gap-1 border-b">
            {TABS.map(([path, label]) => (
              <Link
                key={path}
                to={path ? `${base}/${path}` : base}
                aria-current={active === path ? 'page' : undefined}
                className={cn(
                  '-mb-px border-b-2 px-3 py-2 text-sm font-medium outline-none focus-visible:ring-[3px] focus-visible:ring-ring/50',
                  active === path
                    ? 'border-primary text-foreground'
                    : 'border-transparent text-muted-foreground hover:text-foreground',
                )}
              >
                {label}
              </Link>
            ))}
          </nav>
          <Outlet context={r} />
        </>
      )}
    </div>
  )
}

/** useRepository is the repository a RepositoryPage tab belongs to. */
export const useRepository = () => useOutletContext<Repository>()

/** imageOfDocker is the image path a docker image belongs to. */
export const imageOfDocker = (d: DockerImage) => {
  const id = lastSegment(d.name)
  return decodeURIComponent(id.slice(0, id.lastIndexOf('@')))
}

/** ImageSummary is what the Images tab shows of one image. */
interface ImageSummary {
  digests: number
  tags: string[]
  size: number
}

/** summarize groups a repository's docker images by image path. */
export function summarize(images: DockerImage[]): Map<string, ImageSummary> {
  const out = new Map<string, ImageSummary>()
  for (const d of images) {
    const img = imageOfDocker(d)
    const s = out.get(img) ?? { digests: 0, tags: [], size: 0 }
    s.digests++
    s.tags.push(...(d.tags ?? []))
    s.size += Number(d.imageSizeBytes ?? 0)
    out.set(img, s)
  }
  return out
}

/**
 * Images lists a repository's images (its packages) with their digests,
 * tags and sizes, and deletes an image with everything in it.
 */
export function Images() {
  const ref = useRepoRef()
  const packages = useQuery(packagesQuery(ref))
  const docker = useQuery(dockerImagesQuery(ref))
  const qc = useQueryClient()
  const del = useMutation({
    mutationFn: (image: string) => deletePackage(ref, image),
    onSuccess: (_, image) => {
      void qc.invalidateQueries({ queryKey: ['ar'] })
      toast.success(`Deleted image ${image}.`)
    },
  })
  const summary = summarize(docker.data ?? [])
  const list = packages.data ?? []
  return (
    <Card>
      <CardTitle>Images</CardTitle>
      <QueryStatus query={packages} />
      {packages.data && list.length === 0 && (
        <p className="text-sm text-muted-foreground">
          No images yet: push one to the repository (see Overview for the commands).
        </p>
      )}
      {list.length > 0 && (
        <Table aria-label="Images">
          <TableHeader>
            <TableRow>
              <TableHead>Image</TableHead>
              <TableHead className="text-right">Digests</TableHead>
              <TableHead>Tags</TableHead>
              <TableHead className="text-right">Size</TableHead>
              <TableHead>Created</TableHead>
              <TableHead>Updated</TableHead>
              <TableHead>
                <span className="sr-only">Actions</span>
              </TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {list.map((p) => {
              const img = imageOf(p.name)
              const s = summary.get(img)
              return (
                <TableRow key={p.name} data-testid="image">
                  <TableCell>
                    <Link
                      className="font-mono text-primary underline-offset-4 hover:underline"
                      to={imagePath(ref, img)}
                    >
                      {img}
                    </Link>
                  </TableCell>
                  <TableCell className="text-right">{s?.digests}</TableCell>
                  <TableCell className="font-mono text-xs">{s?.tags.sort().join(', ')}</TableCell>
                  <TableCell className="text-right">{s && formatBytes(s.size)}</TableCell>
                  <TableCell>{time(p.createTime)}</TableCell>
                  <TableCell>{time(p.updateTime)}</TableCell>
                  <TableCell>
                    <div className="flex justify-end">
                      <ConfirmDialog
                        trigger={
                          <Button size="sm" variant="outline" aria-label={`Delete image ${img}`}>
                            <Trash2 aria-hidden />
                            Delete
                          </Button>
                        }
                        title={`Delete image ${img}?`}
                        description="Every digest and tag of the image is deleted. This cannot be undone."
                        confirmLabel="Delete image"
                        onConfirm={() => del.mutateAsync(img)}
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

const pairs = (m?: Record<string, string>) => {
  const e = Object.entries(m ?? {})
  return e.length > 0 ? <Mono>{e.map(([k, v]) => `${k}=${v}`).join(', ')}</Mono> : undefined
}

/** remoteSource names a remote repository's upstream. */
function remoteSource(r: Repository): string | undefined {
  const rc = r.remoteRepositoryConfig
  if (!rc) return undefined
  const dr = rc.dockerRepository
  if (dr?.publicRepository)
    return dr.publicRepository === 'DOCKER_HUB' ? 'Docker Hub' : dr.publicRepository
  return dr?.customRepository?.uri ?? rc.commonRepository?.uri
}

/**
 * RepositoryOverview shows the repository's configuration, how to push
 * to and pull from it, and the resource as JSON.
 */
export function RepositoryOverview() {
  const r = useRepository()
  const ref = useRepoRef()
  const host = useRegistryHost(ref.location)
  const prefix = `${host}/${ref.project}/${ref.repo}`
  const upstreams = r.virtualRepositoryConfig?.upstreamPolicies ?? []
  return (
    <div className="flex flex-col gap-6">
      <Card>
        <CardTitle>Configuration</CardTitle>
        <DetailList
          label="Repository details"
          rows={[
            ['Name', <Mono key="n">{r.name}</Mono>],
            ['Registry URI', <Mono key="u">{r.registryUri}</Mono>],
            ['Format', r.format === 'DOCKER' ? 'Docker' : r.format],
            ['Mode', MODE_LABEL[r.mode ?? ''] ?? r.mode],
            ['Remote source', remoteSource(r)],
            [
              'Upstream repositories',
              upstreams.length > 0 ? (
                <Mono>
                  {upstreams
                    .map((u) => `${lastSegment(u.repository ?? '')} (priority ${u.priority ?? 0})`)
                    .join(', ')}
                </Mono>
              ) : undefined,
            ],
            ['Description', r.description],
            ['Labels', pairs(r.labels)],
            ['Immutable tags', r.dockerConfig?.immutableTags ? 'Yes' : 'No'],
            ['Size', formatBytes(r.sizeBytes ?? 0)],
            ['Created', time(r.createTime)],
            ['Updated', time(r.updateTime)],
          ]}
        />
      </Card>
      <Card>
        <CardTitle>Push and pull</CardTitle>
        <p className="text-sm text-muted-foreground">
          Images live under <span className="font-mono">{prefix}</span>.
        </p>
        <CopyText label="tag command" value={`docker tag IMAGE ${prefix}/IMAGE:TAG`} />
        <CopyText label="push command" value={`docker push ${prefix}/IMAGE:TAG`} />
        <CopyText label="pull command" value={`docker pull ${prefix}/IMAGE:TAG`} />
      </Card>
      <Card>
        <CardTitle>Repository resource</CardTitle>
        <JsonView value={r} label="Repository JSON" />
      </Card>
    </div>
  )
}
