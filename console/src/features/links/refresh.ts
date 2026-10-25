import { useQueryClient } from '@tanstack/react-query'

import { projectsQuery } from '@/api/queries'

import { discoveredQuery, linkStatusQuery, linksQuery, pairingRequestsQuery } from './api'

/**
 * useRefreshLinks returns a function that refetches what a change to
 * links, shares or delegations moves: the links, their status, the
 * Pairing requests, and the Merged list of Projects. Only a change of
 * Pairing, rediscover, browses the LAN again, which takes about a second,
 * to mark the Discovered Instances paired or not.
 */
export function useRefreshLinks() {
  const qc = useQueryClient()
  return ({ rediscover = false } = {}) => {
    void qc.invalidateQueries({ queryKey: linksQuery().queryKey, exact: true })
    void qc.invalidateQueries({ queryKey: linkStatusQuery().queryKey })
    void qc.invalidateQueries({ queryKey: pairingRequestsQuery().queryKey })
    void qc.invalidateQueries({ queryKey: projectsQuery().queryKey })
    if (rediscover) void qc.invalidateQueries({ queryKey: discoveredQuery().queryKey })
  }
}
