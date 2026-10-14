import { zodResolver } from '@hookform/resolvers/zod'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useForm, useWatch } from 'react-hook-form'
import { z } from 'zod'

import { useCarriedNavigate } from '@/components/Link'
import { Field, fieldAria } from '@/components/resource/Field'
import { ResourceEditor, type Body } from '@/components/resource/ResourceEditor'
import { Card } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { toast } from '@/lib/toast'

import { saveSnapshot, SNAPSHOT_NAME, snapshotsQuery } from './api'

// Saving a State snapshot (FR-UI-017, FR-UI-011): the form's one field is
// the body of POST /_emu/v1/snapshots. Saving under a taken name replaces
// that snapshot, so the form says so before it is sent.

const schema = z.object({
  name: z
    .string()
    .trim()
    .regex(
      SNAPSHOT_NAME,
      'Use up to 63 letters, digits, ".", "_" or "-", starting with a letter or digit.',
    ),
})
export type Values = z.infer<typeof schema>

/** snapshotBody writes the form's values onto the request body. */
export const snapshotBody = (v: Values, base: Body): Body => ({ ...base, name: v.name.trim() })

/** snapshotValues reads the form's values out of the request body. */
export const snapshotValues = (body: Body): Values => ({
  name: typeof body.name === 'string' ? body.name : '',
})

/** SaveSnapshot saves all of the Instance's state as a State snapshot. */
export function SaveSnapshot() {
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const { data: existing } = useQuery(snapshotsQuery())
  const form = useForm<Values>({ resolver: zodResolver(schema), defaultValues: { name: '' } })
  const name = useWatch({ control: form.control, name: 'name' }).trim()
  const taken = existing?.some((s) => s.name === name)
  const save = useMutation({
    meta: { toast: false },
    mutationFn: (body: Body) => saveSnapshot(body as { name: string }),
    onSuccess: (s) => {
      void qc.invalidateQueries({ queryKey: snapshotsQuery().queryKey })
      toast.success(`Saved State snapshot ${s.name}.`)
      navigate('/state')
    },
  })
  const hint = taken
    ? `Replaces the State snapshot ${name}.`
    : 'Saved in the instance dir, so it outlives an Ephemeral Instance.'
  return (
    <Card>
      <h1 className="text-lg font-semibold">Save State snapshot</h1>
      <ResourceEditor
        form={form}
        initialBody={{ name: '' }}
        toBody={snapshotBody}
        fromBody={snapshotValues}
        fields={['name']}
        onSubmit={(body) => save.mutateAsync(body)}
        submitLabel={taken ? 'Replace snapshot' : 'Save snapshot'}
        onCancel={() => navigate('/state')}
        jsonHint={
          <>
            Sent to <span className="font-mono">POST /_emu/v1/snapshots</span>.
          </>
        }
      >
        <p className="text-sm text-muted-foreground">
          The Instance must be ready with no Operation running, and no Cloud SQL instance stopped.
          GKE clusters are saved without the Kubernetes objects inside them.
        </p>
        <Field
          id="snapshot-name"
          label="Name"
          error={form.formState.errors.name?.message}
          hint={hint}
        >
          <Input
            autoComplete="off"
            spellCheck={false}
            className="font-mono"
            {...fieldAria('snapshot-name', form.formState.errors.name?.message, hint)}
            {...form.register('name')}
          />
        </Field>
      </ResourceEditor>
    </Card>
  )
}
