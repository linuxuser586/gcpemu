import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Pencil, Trash2 } from 'lucide-react'
import { useParams } from 'react-router'

import { Link, useCarriedNavigate } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { ConfirmDialog } from '@/components/resource/ConfirmDialog'
import { CopyAs } from '@/components/resource/CopyAs'
import { DetailList, JsonView, Mono } from '@/components/resource/DetailList'
import { Button } from '@/components/ui/button'
import { Card, CardTitle } from '@/components/ui/card'
import { toast } from '@/lib/toast'
import { useViewState } from '@/lib/viewState'

import { deleteFirewall, firewallQuery, priorityOf } from './api'
import { firewallRecipe } from './copy'
import { action, rules, targets } from './Firewalls'
import { firewallPage, NetworkLink } from './links'
import { RecordedNotice } from './RecordedNotice'

const list = (l?: string[]) => (l?.length ? <Mono>{l.join(', ')}</Mono> : undefined)

/** FirewallPage is one firewall rule: its match, its targets, and the rule as the API returns it. */
export function FirewallPage() {
  const { firewall: name = '' } = useParams()
  const [view] = useViewState()
  const project = view.project ?? ''
  const query = useQuery(firewallQuery(project, name))
  const f = query.data
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const remove = useMutation({
    mutationFn: () => deleteFirewall(project, name),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['compute'] })
      toast.success(`Deleted firewall rule ${name}.`)
      navigate('/compute/firewalls')
    },
  })
  const egress = f?.direction === 'EGRESS'
  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="flex flex-col gap-1">
          <p className="text-sm text-muted-foreground">
            <Link to="/compute/firewalls" className="underline-offset-4 hover:underline">
              VPC network
            </Link>{' '}
            / firewall rule
          </p>
          <h1 className="text-2xl font-semibold">
            <span className="font-mono">{name}</span>
          </h1>
        </div>
        {f && (
          <div className="flex flex-wrap gap-2">
            <CopyAs what="firewall rule" recipe={() => firewallRecipe(f, project)} />
            <Button variant="outline" asChild>
              <Link to={firewallPage(name, 'edit')}>
                <Pencil aria-hidden />
                Edit firewall rule
              </Link>
            </Button>
            <ConfirmDialog
              trigger={
                <Button variant="outline">
                  <Trash2 aria-hidden />
                  Delete firewall rule
                </Button>
              }
              title={`Delete firewall rule ${name}?`}
              description="The rule is gone for good; the emulator never enforced it."
              confirmLabel="Delete firewall rule"
              onConfirm={() => remove.mutateAsync()}
            />
          </div>
        )}
      </div>
      <RecordedNotice />
      <QueryStatus query={query} />
      {f && (
        <>
          <Card aria-labelledby="firewall-title">
            <CardTitle id="firewall-title">Firewall rule</CardTitle>
            <DetailList
              label="Firewall rule details"
              rows={[
                ['Network', <NetworkLink key="n" link={f.network} />],
                ['Direction', <Mono key="d">{f.direction ?? 'INGRESS'}</Mono>],
                ['Priority', <Mono key="p">{priorityOf(f)}</Mono>],
                ['Action', action(f)],
                ['Protocols and ports', <Mono key="r">{rules(f)}</Mono>],
                ['Targets', targets(f)],
                egress
                  ? ['Destination ranges', list(f.destinationRanges)]
                  : ['Source ranges', list(f.sourceRanges)],
                ['Source tags', egress ? undefined : list(f.sourceTags)],
                ['Source service accounts', egress ? undefined : list(f.sourceServiceAccounts)],
                ['Enforcement', f.disabled ? 'Disabled' : 'Enabled'],
                ['Logs', f.logConfig?.enable ? 'On' : 'Off'],
                ['Description', f.description],
                ['Created', f.creationTimestamp && <Mono key="c">{f.creationTimestamp}</Mono>],
              ]}
            />
          </Card>
          <Card>
            <CardTitle>Resource</CardTitle>
            <JsonView value={f} label="Firewall rule JSON" />
          </Card>
        </>
      )}
    </div>
  )
}
