import { zodResolver } from '@hookform/resolvers/zod'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useForm, type UseFormReturn } from 'react-hook-form'
import { useParams } from 'react-router'
import { z } from 'zod'

import { useCarriedNavigate } from '@/components/Link'
import { QueryStatus } from '@/components/QueryStatus'
import { Field, fieldAria } from '@/components/resource/Field'
import { ResourceEditor, type Body } from '@/components/resource/ResourceEditor'
import { Card } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Textarea } from '@/components/ui/textarea'
import { formatLabels, labelsError, parseLabels } from '@/lib/labels'
import { toast } from '@/lib/toast'

import {
  createTopic,
  duration,
  goDuration,
  invalidName,
  patchTopic,
  seconds,
  topicName,
  topicQuery,
  validId,
  type Topic,
} from './api'
import { ChooseProject, topicPath, useProject } from './PubSubLayout'

// Create and edit forms for topics (FR-PS-001, FR-UI-011): create sends
// topics.create's Topic; edit sends topics.patch with the fields that
// changed and an updateMask naming them. Schemas, CMEK and the rest are in
// the JSON editor.

type Obj = Record<string, unknown>
const obj = (v: unknown): Obj =>
  typeof v === 'object' && v !== null && !Array.isArray(v) ? (v as Obj) : {}
const str = (v: unknown) => (typeof v === 'string' ? v : '')

/** retentionError is the API's message for a retention outside 10 minutes to 31 days. */
export function retentionError(v: string): string | undefined {
  if (!v) return undefined
  if (!/^\d+$/.test(v)) return 'The retention must be a whole number of seconds.'
  const s = Number(v)
  if (s < 600 || s > 31 * 86400) {
    return `Invalid message_retention_duration: must be between 10 minutes and 31 days, got ${goDuration(s)}.`
  }
  return undefined
}

export const labelsField = z.string().superRefine((v, ctx) => {
  const err = labelsError(v)
  if (err) ctx.addIssue({ code: 'custom', message: err })
})

export const retentionField = z
  .string()
  .trim()
  .superRefine((v, ctx) => {
    const err = retentionError(v)
    if (err) ctx.addIssue({ code: 'custom', message: err })
  })

const editSchema = z.object({ labels: labelsField, retentionSeconds: retentionField })
export type EditValues = z.infer<typeof editSchema>

const createSchema = (project: string) =>
  editSchema.extend({
    id: z.string().superRefine((v, ctx) => {
      if (!validId(v)) {
        ctx.addIssue({ code: 'custom', message: invalidName('topics', topicName(project, v)) })
      }
    }),
  })
export type CreateValues = EditValues & { id: string }

/** topicValues reads the form's fields out of a Topic body. */
export function topicValues<V extends EditValues>(body: Body, prev: V): V {
  return {
    ...prev,
    labels: formatLabels(obj(body.labels) as Record<string, string>),
    retentionSeconds: seconds(str(body.messageRetentionDuration)),
  }
}

/** topicBody writes the form's fields onto a Topic body, dropping unset ones. */
export function topicBody(v: EditValues, base: Body): Body {
  const out: Body = { ...base }
  const labels = parseLabels(v.labels)
  if (Object.keys(labels).length > 0) out.labels = labels
  else delete out.labels
  if (v.retentionSeconds) out.messageRetentionDuration = duration(v.retentionSeconds)
  else delete out.messageRetentionDuration
  return out
}

/**
 * topicPatch writes what the edit form changed onto a topics.patch body;
 * a field cleared is null (or empty labels), which with the field in the
 * updateMask removes it.
 */
export function topicPatch(t: Topic, v: EditValues, base: Body): Body {
  const was = topicValues(t as unknown as Body, v)
  const now = topicBody(v, {})
  const out: Body = { ...base }
  delete out.labels
  delete out.messageRetentionDuration
  if (v.labels !== was.labels) out.labels = now.labels ?? {}
  if (v.retentionSeconds !== was.retentionSeconds) {
    out.messageRetentionDuration = now.messageRetentionDuration ?? null
  }
  return out
}

/** topicPatchValues reads an edit body back into the form, defaulting to t. */
export const topicPatchValues =
  (t: Topic) =>
  (body: Body, prev: EditValues): EditValues => {
    const was = topicValues(t as unknown as Body, prev)
    const now = topicValues(body, prev)
    return {
      labels: 'labels' in body ? now.labels : was.labels,
      retentionSeconds:
        'messageRetentionDuration' in body ? now.retentionSeconds : was.retentionSeconds,
    }
  }

function TopicFields({
  form,
  prefix,
}: {
  form: UseFormReturn<EditValues> | UseFormReturn<CreateValues>
  prefix: string
}) {
  const f = form as UseFormReturn<EditValues>
  const errors = f.formState.errors
  return (
    <>
      <Field
        id={`${prefix}-labels`}
        label="Labels"
        error={errors.labels?.message}
        hint="key=value, one per line"
      >
        <Textarea
          rows={2}
          spellCheck={false}
          className="font-mono"
          {...fieldAria(`${prefix}-labels`, errors.labels?.message, 'key')}
          {...f.register('labels')}
        />
      </Field>
      <Field
        id={`${prefix}-retention`}
        label="Message retention (seconds)"
        error={errors.retentionSeconds?.message}
        hint="600 to 2678400. Keeps published messages for replay by seek, even once acknowledged. Empty for none."
      >
        <Input
          inputMode="numeric"
          spellCheck={false}
          {...fieldAria(`${prefix}-retention`, errors.retentionSeconds?.message, '600')}
          {...f.register('retentionSeconds')}
        />
      </Field>
    </>
  )
}

// ---- create ----

export function CreateTopic() {
  const project = useProject()
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const form = useForm<CreateValues>({
    resolver: zodResolver(createSchema(project)),
    defaultValues: { id: '', labels: '', retentionSeconds: '' },
  })
  const errors = form.formState.errors
  const create = useMutation({
    meta: { toast: false },
    mutationFn: ({ body, id }: { body: Body; id: string }) => createTopic(project, id, body),
    onSuccess: (_, { id }) => {
      void qc.invalidateQueries({ queryKey: ['pubsub'] })
      toast.success(`Created topic ${id}.`)
      navigate(topicPath(id))
    },
  })
  if (!project) return <ChooseProject what="create a topic in it" />
  return (
    <Card>
      <h1 className="text-lg font-semibold">Create topic</h1>
      <ResourceEditor
        form={form}
        initialBody={{}}
        toBody={topicBody}
        fromBody={topicValues}
        fields={['id', 'labels']}
        onSubmit={(body, values) => create.mutateAsync({ body, id: values.id })}
        submitLabel="Create"
        onCancel={() => navigate('/pubsub')}
        jsonHint={
          <>
            Sent to <span className="font-mono">topics.create</span> as{' '}
            <span className="font-mono">projects/{project}/topics/</span> with the topic ID of the
            form.
          </>
        }
      >
        <Field
          id="topic-id"
          label="Topic ID"
          error={errors.id?.message}
          hint="Starts with a letter; 3 to 255 letters, digits and - _ . ~ + %. Cannot be changed."
        >
          <Input
            autoComplete="off"
            spellCheck={false}
            {...fieldAria('topic-id', errors.id?.message, 'Starts')}
            {...form.register('id')}
          />
        </Field>
        <TopicFields form={form} prefix="topic" />
      </ResourceEditor>
    </Card>
  )
}

// ---- edit ----

export function EditTopic() {
  const project = useProject()
  const { topic = '' } = useParams()
  const query = useQuery({ ...topicQuery(project, topic), enabled: !!project })
  if (!project) return <ChooseProject what="edit its topics" />
  if (!query.data) return <QueryStatus query={query} />
  return <EditTopicForm t={query.data} project={project} id={topic} />
}

function EditTopicForm({ t, project, id }: { t: Topic; project: string; id: string }) {
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const form = useForm<EditValues>({
    resolver: zodResolver(editSchema),
    defaultValues: topicValues(t as unknown as Body, { labels: '', retentionSeconds: '' }),
  })
  const back = () => navigate(topicPath(id, 'overview'))
  const save = useMutation({
    meta: { toast: false },
    mutationFn: async (body: Body) => {
      const mask = Object.keys(body)
      if (mask.length === 0) return false
      await patchTopic(project, id, body, mask)
      return true
    },
    onSuccess: (changed) => {
      void qc.invalidateQueries({ queryKey: ['pubsub'] })
      toast.success(changed ? `Updated topic ${id}.` : 'Nothing to change.')
      back()
    },
  })
  return (
    <Card>
      <h2 className="text-lg font-semibold">
        Edit topic <span className="font-mono">{id}</span>
      </h2>
      <ResourceEditor
        form={form}
        initialBody={{}}
        toBody={(v, base) => topicPatch(t, v, base)}
        fromBody={topicPatchValues(t)}
        fields={['labels']}
        onSubmit={(body) => save.mutateAsync(body)}
        submitLabel="Save"
        onCancel={back}
        jsonHint={
          <>
            Sent to <span className="font-mono">topics.patch</span> as the topic, with an updateMask
            of its fields: null clears a field.
          </>
        }
      >
        <TopicFields form={form} prefix="edit-topic" />
      </ResourceEditor>
    </Card>
  )
}
