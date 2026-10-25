import { queryOptions } from '@tanstack/react-query'

import { admin, unwrap } from '@/api/admin'
import type { components, paths } from '@/api/admin.gen'
import { POLL_MS } from '@/api/queries'

// The Links view's reads and writes (ADR 0027), all on the admin API:
// Links and their state, Shared and Delegated Projects, Discovered
// Instances and Pairing requests. None of it is in the event stream, so
// the queries poll; only the status query dials the links.

export type LinkState = components['schemas']['LinkState']
export type Share = components['schemas']['Share']
export type Delegation = components['schemas']['Delegation']
export type Discovered = components['schemas']['DiscoveredInstance']
export type LinkStatus = NonNullable<
  paths['/_emu/v1/links/status']['get']['responses']['200']['content']['application/json']['links']
>[number]
export type PairingRequest = NonNullable<
  paths['/_emu/v1/links/requests']['get']['responses']['200']['content']['application/json']['requests']
>[number]

/** linksQuery is this Instance's links, its shares and delegations, without dialling. */
export const linksQuery = () =>
  queryOptions({
    queryKey: ['admin', 'links'],
    queryFn: async () => {
      const r = await unwrap(admin.GET('/_emu/v1/links'))
      return {
        links: r.links ?? [],
        fingerprint: r.fingerprint ?? '',
        shared: r.shared ?? [],
        delegated: r.delegated ?? [],
      }
    },
    refetchInterval: POLL_MS,
  })

/**
 * linkStatusQuery dials each link: whether it is reachable, at which
 * protocol version, and which Projects it shares with this Instance.
 */
export const linkStatusQuery = () =>
  queryOptions({
    queryKey: ['admin', 'links', 'status'],
    queryFn: async (): Promise<LinkStatus[]> =>
      (await unwrap(admin.GET('/_emu/v1/links/status'))).links ?? [],
    refetchInterval: POLL_MS,
  })

export const pairingRequestsQuery = () =>
  queryOptions({
    queryKey: ['admin', 'links', 'requests'],
    queryFn: async (): Promise<PairingRequest[]> =>
      (await unwrap(admin.GET('/_emu/v1/links/requests'))).requests ?? [],
    refetchInterval: POLL_MS,
  })

/** discoveredQuery browses the LAN, which takes about a second; it does not poll. */
export const discoveredQuery = () =>
  queryOptions({
    queryKey: ['admin', 'links', 'discovered'],
    queryFn: async (): Promise<Discovered[]> =>
      (await unwrap(admin.GET('/_emu/v1/links/discovered'))).instances ?? [],
  })

export const identify = (address: string) =>
  unwrap(admin.POST('/_emu/v1/links/identify', { body: { address } }))

/** pair asks the Instance at address to pair, and waits for it to approve. */
export const pair = (body: { address: string; fingerprint: string; name?: string }) =>
  unwrap(admin.POST('/_emu/v1/links', { body }))

export const unpair = (name: string) =>
  unwrap(admin.DELETE('/_emu/v1/links/{name}', { params: { path: { name } } }))

export const approve = (id: string, name?: string) =>
  unwrap(
    admin.POST('/_emu/v1/links/requests/{id}/approve', {
      params: { path: { id } },
      body: name ? { name } : {},
    }),
  )

export const reject = (id: string) =>
  unwrap(admin.POST('/_emu/v1/links/requests/{id}/reject', { params: { path: { id } } }))

export const share = (body: Share) => unwrap(admin.POST('/_emu/v1/links/shares', { body }))

export const unshare = ({ project, with: w }: Share) =>
  unwrap(
    admin.DELETE('/_emu/v1/links/shares/{project}', {
      params: { path: { project }, query: w ? { with: w } : {} },
    }),
  )

export const delegate = (body: Delegation) =>
  unwrap(admin.POST('/_emu/v1/links/delegations', { body }))

export const undelegate = (project: string) =>
  unwrap(admin.DELETE('/_emu/v1/links/delegations/{project}', { params: { path: { project } } }))

/** sharedWith returns the Projects this Instance shares with the link name. */
export function sharedWith(shares: Share[], name: string): string[] {
  return [...new Set(shares.filter((s) => !s.with || s.with === name).map((s) => s.project))]
}

/**
 * unavailable reports why the link a Project is delegated to cannot serve
 * it now, or '' while it can or before it is dialled.
 */
export function unavailable(status: LinkStatus[] | undefined, link: string): string {
  const st = status?.find((s) => s.name === link)
  if (!st) return ''
  if (st.state === 'broken') return st.error || `Link ${link} is broken.`
  if (!st.reachable) return st.error || `Link ${link} is unreachable.`
  return ''
}
