import { Fragment } from 'react'

import { Link } from '@/components/Link'

import { lastSeg } from '../lb/api'
import type { SubnetRef, Subnetwork } from './api'

// The VPC network view's paths, and links to networks and subnetworks
// that other views (Cloud NAT) show too.

const enc = encodeURIComponent

/** networkPage is the console path of a network, or of a page under it. */
export const networkPage = (network: string, sub = '') =>
  `/compute/global/networks/${enc(network)}${sub ? `/${sub}` : ''}`

/** subnetPage is the console path of a subnetwork, or of a page under it. */
export const subnetPage = (r: Pick<SubnetRef, 'region' | 'subnetwork'>, sub = '') =>
  `/compute/regions/${r.region}/subnetworks/${enc(r.subnetwork)}${sub ? `/${sub}` : ''}`

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
