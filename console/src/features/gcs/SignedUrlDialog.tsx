import { zodResolver } from '@hookform/resolvers/zod'
import { useMutation, useQuery } from '@tanstack/react-query'
import { Copy, Link2 } from 'lucide-react'
import { useId, useState } from 'react'
import { useForm } from 'react-hook-form'
import { z } from 'zod'

import { errorMessage } from '@/api/errors'
import { gcpFetch } from '@/api/fetch'
import { infoQuery } from '@/api/queries'
import { Field, fieldAria } from '@/components/resource/Field'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogTitle,
  DialogTrigger,
} from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { NativeSelect } from '@/components/ui/native-select'
import { Textarea } from '@/components/ui/textarea'
import { toast } from '@/lib/toast'
import { useViewState } from '@/lib/viewState'

import { MAX_EXPIRY_SECONDS, SIGNED_METHODS, signedUrl } from './signedUrl'

const schema = z.object({
  serviceAccount: z
    .string()
    .trim()
    .min(1, 'A service account signs the URL.')
    .includes('@', { message: 'Enter the service account’s email.' }),
  method: z.enum(SIGNED_METHODS),
  expires: z
    .number({ error: 'Invalid X-Goog-Expires: must be between 1 and 604800 seconds.' })
    .int('Invalid X-Goog-Expires: must be between 1 and 604800 seconds.')
    .min(1, 'Invalid X-Goog-Expires: must be between 1 and 604800 seconds.')
    .max(MAX_EXPIRY_SECONDS, 'Invalid X-Goog-Expires: must be between 1 and 604800 seconds.'),
})
type Values = z.infer<typeof schema>

/** serviceAccountsQuery lists a Project's service accounts to sign with. */
const serviceAccountsQuery = (project: string, enabled: boolean) => ({
  queryKey: ['iam', 'serviceAccounts', project],
  queryFn: async () =>
    (
      await gcpFetch<{ accounts?: { email: string }[] }>(
        `/iam/v1/projects/${encodeURIComponent(project)}/serviceAccounts`,
      )
    ).accounts ?? [],
  enabled,
})

/**
 * SignedUrlDialog generates a V4 signed URL for one object (FR-GCS-006),
 * signed through IAM Credentials signBlob by a service account. Signing
 * needs the iam Service.
 */
export function SignedUrlDialog({ bucket, object }: { bucket: string; object: string }) {
  const [open, setOpen] = useState(false)
  const [url, setUrl] = useState('')
  const [view] = useViewState()
  const { data: info } = useQuery(infoQuery())
  const iam = info?.services.includes('iam') ?? false
  const accounts = useQuery(serviceAccountsQuery(view.project ?? '', open && iam && !!view.project))
  const id = useId()
  const form = useForm<Values>({
    resolver: zodResolver(schema),
    defaultValues: { serviceAccount: '', method: 'GET', expires: 3600 },
  })
  const errors = form.formState.errors
  const sign = useMutation({
    meta: { toast: false },
    mutationFn: (v: Values) =>
      signedUrl(
        {
          bucket,
          object,
          method: v.method,
          expiresSeconds: v.expires,
          serviceAccount: v.serviceAccount,
          host: globalThis.location.host,
          now: new Date(),
        },
        globalThis.location.protocol,
      ),
    onSuccess: setUrl,
  })

  return (
    <Dialog
      open={open}
      onOpenChange={(o) => {
        setOpen(o)
        if (!o) {
          setUrl('')
          sign.reset()
        }
      }}
    >
      <DialogTrigger asChild>
        <Button variant="outline">
          <Link2 aria-hidden />
          Signed URL
        </Button>
      </DialogTrigger>
      <DialogContent>
        <DialogTitle>Generate a signed URL</DialogTitle>
        <DialogDescription>
          Anyone with the URL can send its method to{' '}
          <span className="font-mono break-all">{object}</span> until it expires.
        </DialogDescription>
        {!iam ? (
          <p role="status" className="text-sm text-muted-foreground">
            Signing uses IAM Credentials <span className="font-mono">signBlob</span>; enable the{' '}
            <span className="font-mono">iam</span> Service to generate signed URLs.
          </p>
        ) : (
          <form
            noValidate
            className="flex flex-col gap-4"
            onSubmit={(e) => void form.handleSubmit((v) => sign.mutate(v))(e)}
          >
            <Field
              id={`${id}-sa`}
              label="Service account"
              error={errors.serviceAccount?.message}
              hint="Signs with its system-managed key; you need iam.serviceAccounts.signBlob on it."
            >
              <Input
                list={`${id}-accounts`}
                autoComplete="off"
                spellCheck={false}
                {...fieldAria(`${id}-sa`, errors.serviceAccount?.message, 'Signs')}
                {...form.register('serviceAccount')}
              />
              <datalist id={`${id}-accounts`}>
                {accounts.data?.map((a) => (
                  <option key={a.email} value={a.email} />
                ))}
              </datalist>
            </Field>
            <div className="grid gap-4 sm:grid-cols-2">
              <Field id={`${id}-method`} label="Method">
                <NativeSelect {...fieldAria(`${id}-method`)} {...form.register('method')}>
                  {SIGNED_METHODS.map((m) => (
                    <option key={m} value={m}>
                      {m}
                    </option>
                  ))}
                </NativeSelect>
              </Field>
              <Field
                id={`${id}-expires`}
                label="Expires after (seconds)"
                error={errors.expires?.message}
                hint="At most 604800 (7 days)."
              >
                <Input
                  type="number"
                  min={1}
                  max={MAX_EXPIRY_SECONDS}
                  {...fieldAria(`${id}-expires`, errors.expires?.message, 'At')}
                  {...form.register('expires', { valueAsNumber: true })}
                />
              </Field>
            </div>
            {sign.error && (
              <p
                role="alert"
                className="rounded-md border border-destructive/50 p-3 text-sm text-destructive"
              >
                {errorMessage(sign.error)}
              </p>
            )}
            <Button type="submit" className="w-fit" disabled={sign.isPending}>
              Generate
            </Button>
          </form>
        )}
        {url && (
          <div className="flex flex-col gap-2">
            <label htmlFor={`${id}-url`} className="text-sm font-medium">
              Signed URL
            </label>
            <Textarea
              id={`${id}-url`}
              readOnly
              rows={4}
              className="font-mono text-xs break-all"
              value={url}
              onFocus={(e) => e.currentTarget.select()}
            />
            <Button
              variant="outline"
              className="w-fit"
              onClick={() =>
                void navigator.clipboard
                  ?.writeText(url)
                  .then(() => toast.success('Copied the signed URL.'))
                  .catch(() => toast.error('Could not copy; select the URL and copy it.'))
              }
            >
              <Copy aria-hidden />
              Copy
            </Button>
          </div>
        )}
      </DialogContent>
    </Dialog>
  )
}
