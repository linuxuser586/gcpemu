import { useMutation, useQueryClient } from '@tanstack/react-query'
import { Pencil, Plus, Trash2 } from 'lucide-react'
import { useId } from 'react'

import { Link } from '@/components/Link'
import { ConfirmDialog } from '@/components/resource/ConfirmDialog'
import { CopyAs } from '@/components/resource/CopyAs'
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

import { relPath } from '../lb/api'
import {
  PSA_PEERING,
  removePeering,
  type Network,
  type NetworkPeering,
  type NetworkRef,
} from './api'
import { peeringRecipe } from './copy'
import { addPeeringPage, NetworkLink, peeringEditPage } from './links'

// A network's VPC Network Peerings (networks.addPeering, updatePeering,
// removePeering). A peering turns ACTIVE when the peer network peers
// back; its route exchange settings are Recorded: no routes are
// exchanged.

/** customRoutes says which custom routes a peering imports and exports. */
export function customRoutes(p: NetworkPeering) {
  const ways = [p.importCustomRoutes && 'Import', p.exportCustomRoutes && 'Export'].filter(Boolean)
  return ways.length ? ways.join(' and ') : 'None'
}

/** PeerLink links a peer network in project to its page, and shows any other one's path. */
function PeerLink({ link, project }: { link: string; project: string }) {
  const path = relPath(link)
  if (path.startsWith(`projects/${project}/global/networks/`)) return <NetworkLink link={link} />
  return <span className="font-mono break-all">{path}</span>
}

/**
 * Peerings is a network's peerings, with the peering a private services
 * access connection adds, which goes with the connection.
 */
export function Peerings({ r, network }: { r: NetworkRef; network: Network }) {
  const id = useId()
  const qc = useQueryClient()
  const remove = useMutation({
    mutationFn: (name: string) => removePeering(r, name),
    onSuccess: (_, name) => {
      void qc.invalidateQueries({ queryKey: ['compute'] })
      toast.success(`Removed peering ${name}.`)
    },
  })
  const peerings = network.peerings ?? []
  return (
    <Card aria-labelledby={`${id}-title`}>
      <div className="flex flex-wrap items-center justify-between gap-2">
        <CardTitle id={`${id}-title`}>Peerings</CardTitle>
        <Button variant="outline" size="sm" asChild>
          <Link to={addPeeringPage(r.network)}>
            <Plus aria-hidden />
            Add peering
          </Link>
        </Button>
      </div>
      <p className="text-sm text-muted-foreground">
        A peering is active once the peer network peers back. Its route exchange is Recorded: the
        emulator exchanges no routes between the networks.
      </p>
      {peerings.length === 0 ? (
        <p className="text-sm text-muted-foreground">No peerings in this network.</p>
      ) : (
        <Table aria-label="Peerings">
          <TableHeader>
            <TableRow>
              <TableHead>Name</TableHead>
              <TableHead>Peer network</TableHead>
              <TableHead>State</TableHead>
              <TableHead>Custom routes</TableHead>
              <TableHead>
                <span className="sr-only">Actions</span>
              </TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {peerings.map((p) => {
              const psa = p.name === PSA_PEERING
              return (
                <TableRow key={p.name} data-testid="peering">
                  <TableCell>
                    <span className="font-mono">{p.name}</span>
                    {psa && (
                      <span className="block text-xs text-muted-foreground">
                        Private services access: goes with its connection
                      </span>
                    )}
                  </TableCell>
                  <TableCell>
                    <PeerLink link={p.network} project={r.project} />
                  </TableCell>
                  <TableCell className="font-mono" title={p.stateDetails}>
                    {p.state}
                  </TableCell>
                  <TableCell>{customRoutes(p)}</TableCell>
                  <TableCell>
                    {!psa && (
                      <div className="flex flex-wrap justify-end gap-1">
                        <CopyAs
                          what="peering"
                          name={p.name}
                          size="icon"
                          recipe={() => peeringRecipe(p, r.network, r.project)}
                        />
                        <Button variant="ghost" size="icon" asChild>
                          <Link
                            to={peeringEditPage(r.network, p.name)}
                            aria-label={`Edit peering ${p.name}`}
                          >
                            <Pencil aria-hidden />
                          </Link>
                        </Button>
                        <ConfirmDialog
                          trigger={
                            <Button
                              variant="ghost"
                              size="icon"
                              aria-label={`Remove peering ${p.name}`}
                            >
                              <Trash2 aria-hidden />
                            </Button>
                          }
                          title={`Remove peering ${p.name}?`}
                          description="The peer network's peering back turns inactive; remove it there too."
                          confirmLabel="Remove peering"
                          onConfirm={() => remove.mutateAsync(p.name)}
                        />
                      </div>
                    )}
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
