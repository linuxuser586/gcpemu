import { queryOptions } from '@tanstack/react-query'

import { admin, unwrap } from '@/api/admin'
import type { components } from '@/api/admin.gen'

// The VPC network view's reads. The Host attachment (ADR 0014) is not
// modelled on GCP: its Offered ports are on the admin API only.

export type HostOffers = components['schemas']['HostOffers']

/** OFFERS_POLL_MS: offers change from the CLI, outside the console. */
const OFFERS_POLL_MS = 5_000

/** hostOffersQuery lists the subnetworks with Offered ports. */
export const hostOffersQuery = () =>
  queryOptions({
    queryKey: ['compute', 'hostOffers'],
    queryFn: async () => (await unwrap(admin.GET('/_emu/v1/host/offers'))).subnetworks,
    refetchInterval: OFFERS_POLL_MS,
  })
