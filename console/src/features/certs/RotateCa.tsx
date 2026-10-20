import { useMutation, useQueryClient } from '@tanstack/react-query'
import { RefreshCw } from 'lucide-react'

import { ConfirmDialog } from '@/components/resource/ConfirmDialog'
import { Button } from '@/components/ui/button'
import { toast } from '@/lib/toast'

import { rotateCa } from './api'

/**
 * RotateCa replaces the Instance CA (`gcpemu ca rotate`) after asking:
 * every Google-managed certificate is re-issued from the new root, and
 * clients verify TLS to the emulator again only once they trust it.
 */
export function RotateCa() {
  const qc = useQueryClient()
  const rotate = useMutation({
    mutationFn: rotateCa,
    onSuccess: (r) => {
      void qc.invalidateQueries({ queryKey: ['certs'] })
      toast.success(
        `Rotated the Instance CA and re-issued the Google-managed certificates. Trust the new CA at ${r.path}.`,
      )
    },
  })
  return (
    <ConfirmDialog
      trigger={
        <Button variant="outline">
          <RefreshCw aria-hidden />
          Rotate CA
        </Button>
      }
      title="Rotate the Instance CA?"
      description={
        <>
          A new CA replaces the Instance’s, for every Project of the Instance. The emulator
          re-issues every Google-managed certificate from it. Self-managed certificates are left as
          they are. Until clients trust the new CA, TLS to the emulator fails to verify: run{' '}
          <code className="font-mono">gcpemu ca install</code> again, and{' '}
          <code className="font-mono">gcpemu doctor</code> to see where it is trusted.
        </>
      }
      confirmLabel="Rotate CA"
      onConfirm={() => rotate.mutateAsync()}
    />
  )
}
