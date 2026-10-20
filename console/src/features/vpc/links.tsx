import { Fragment } from 'react'

import { Link } from '@/components/Link'

import { lastSeg, parseRef, relPath } from '../lb/api'
import { lbPath } from '../lb/LbLayout'
import { routerRefOf } from '../nat/api'
import { routerPage } from '../nat/NatLayout'
import type { AddressRef, SubnetRef, Subnetwork } from './api'

// The VPC network view's paths, links to networks and subnetworks that
// other views (Cloud NAT) show too, and links from an address to the
// resources that use it in their own views (Load Balancer, Cloud NAT).

const enc = encodeURIComponent

/** networkPage is the console path of a network, or of a page under it. */
export const networkPage = (network: string, sub = '') =>
  `/compute/global/networks/${enc(network)}${sub ? `/${sub}` : ''}`

/** subnetPage is the console path of a subnetwork, or of a page under it. */
export const subnetPage = (r: Pick<SubnetRef, 'region' | 'subnetwork'>, sub = '') =>
  `/compute/regions/${r.region}/subnetworks/${enc(r.subnetwork)}${sub ? `/${sub}` : ''}`

/** firewallPage is the console path of a firewall rule, or of a page under it. */
export const firewallPage = (name: string, sub = '') =>
  `/compute/global/firewalls/${enc(name)}${sub ? `/${sub}` : ''}`

/** routePage is the console path of a route. */
export const routePage = (name: string) => `/compute/global/routes/${enc(name)}`

/** addressPage is the console path of a regional or global address. */
export const addressPage = (r: Pick<AddressRef, 'region' | 'address'>) =>
  `/compute/${r.region ? `regions/${r.region}` : 'global'}/addresses/${enc(r.address)}`

const linkClass = 'font-mono text-primary underline-offset-4 hover:underline'

/** NetworkLink links a network's selfLink or name to its page. */
export function NetworkLink({ link }: { link?: string }) {
  const name = lastSeg(link)
  if (!name) return <>—</>
  return (
    <Link className={linkClass} to={networkPage(name)}>
      {name}
    </Link>
  )
}

/** FirewallLink links a firewall rule to its page. */
export function FirewallLink({ name }: { name: string }) {
  return (
    <Link className={linkClass} to={firewallPage(name)}>
      {name}
    </Link>
  )
}

/** RouteLink links a route to its page. */
export function RouteLink({ name }: { name: string }) {
  return (
    <Link className={linkClass} to={routePage(name)}>
      {name}
    </Link>
  )
}

/** SubnetLink links a subnetwork to its page. */
export function SubnetLink({ region, name }: { region: string; name: string }) {
  return (
    <Link className={linkClass} to={subnetPage({ region, subnetwork: name })}>
      {name}
    </Link>
  )
}

/** SubnetList links subnetworks to their pages, comma-separated, or "—" for none. */
export function SubnetList({ subnets }: { subnets: Pick<Subnetwork, 'name' | 'region'>[] }) {
  if (!subnets.length) return <span className="font-mono">—</span>
  return (
    <span>
      {subnets.map((s, i) => (
        <Fragment key={`${s.region}/${s.name}`}>
          {i > 0 && ', '}
          <SubnetLink region={lastSeg(s.region)} name={s.name} />
        </Fragment>
      ))}
    </span>
  )
}

/** AddressLink links an address to its page. */
export function AddressLink({ region, name }: { region: string; name: string }) {
  return (
    <Link className={linkClass} to={addressPage({ region, address: name })}>
      {name}
    </Link>
  )
}

/**
 * userPage is the console path of a resource that uses an address, where
 * the console has a view of it: a forwarding rule in Load Balancer, a
 * router (whose NATs use it) in Cloud NAT.
 */
function userPage(link: string): string | undefined {
  const lb = parseRef(link)
  if (lb?.coll === 'forwardingRules') return lbPath(lb)
  const router = routerRefOf(link)
  return router ? routerPage(router) : undefined
}

/** AddressUsers lists the resources that use an address, linked where the console has a view. */
export function AddressUsers({ users }: { users?: string[] }) {
  if (!users?.length) return <span className="font-mono">—</span>
  return (
    <span className="flex flex-col">
      {users.map((u) => {
        const to = userPage(u)
        return to ? (
          <Link key={u} className={linkClass} to={to}>
            {lastSeg(u)}
          </Link>
        ) : (
          <span key={u} className="font-mono break-all">
            {relPath(u)}
          </span>
        )
      })}
    </span>
  )
}
