import { zodResolver } from '@hookform/resolvers/zod'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useForm, useWatch } from 'react-hook-form'
import { z } from 'zod'

import { infoQuery } from '@/api/queries'
import { useCarriedNavigate } from '@/components/Link'
import { Field, fieldAria } from '@/components/resource/Field'
import { ResourceEditor, type Body } from '@/components/resource/ResourceEditor'
import { Card } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { NativeSelect } from '@/components/ui/native-select'
import { serviceName, SERVICES } from '@/lib/services'
import { toast } from '@/lib/toast'

import { addFault, CODES, faultsQuery, type FaultRule } from './api'

// Adding a Fault rule (FR-UI-016, FR-UI-011): the form's fields are the
// FaultRule's, sent to POST /_emu/v1/faults. Matchers are Go regexps, so
// only the API checks them; its message is shown above the button.

/** DURATION is a Go duration as time.ParseDuration takes it, e.g. 250ms or 1m30s. */
const DURATION = /^(\d+(\.\d*)?|\.\d+)(ns|us|µs|ms|s|m|h)((\d+(\.\d*)?|\.\d+)(ns|us|µs|ms|s|m|h))*$/

const isZero = (d: string) => !d || /^[0.]+(ns|us|µs|ms|s|m|h)$/.test(d) || /^0+$/.test(d)

const schema = z
  .object({
    id: z.string().trim(),
    service: z.string(),
    method: z.string(),
    resource: z.string(),
    code: z.string(),
    httpStatus: z
      .string()
      .trim()
      .refine(
        (v) => !v || (/^\d+$/.test(v) && +v >= 100 && +v <= 599),
        'HTTP status must be between 100 and 599.',
      ),
    latency: z
      .string()
      .trim()
      .refine(
        (v) => !v || v === '0' || DURATION.test(v),
        'Latency must be a duration, e.g. 250ms or 2s.',
      ),
    drop: z.boolean(),
    probability: z
      .string()
      .trim()
      .refine(
        (v) => !v || (!Number.isNaN(+v) && +v >= 0 && +v <= 1),
        'Probability must be in [0,1]; 0 means always.',
      ),
    count: z.string().trim().regex(/^\d*$/, 'Count must be a whole number; 0 means unlimited.'),
    disabled: z.boolean(),
  })
  .superRefine((v, ctx) => {
    if (!v.code && isZero(v.latency) && !v.drop) {
      ctx.addIssue({
        code: 'custom',
        path: ['code'],
        message: 'A rule needs an error code, a latency or a dropped connection.',
      })
    }
  })
export type Values = z.infer<typeof schema>

const defaults: Values = {
  id: '',
  service: '',
  method: '',
  resource: '',
  code: 'UNAVAILABLE',
  httpStatus: '',
  latency: '',
  drop: false,
  probability: '',
  count: '',
  disabled: false,
}

const str = (v: unknown) => (typeof v === 'string' ? v : '')
const num = (v: unknown) => (typeof v === 'number' && v !== 0 ? String(v) : '')

/** ruleBody writes the form's values onto a FaultRule body; empty fields are left out. */
export function ruleBody(v: Values, base: Body): Body {
  const out: Body = { ...base }
  const set = (k: keyof FaultRule, value: unknown, keep: boolean) => {
    if (keep) out[k] = value
    else delete out[k]
  }
  set('id', v.id.trim(), !!v.id.trim())
  set('service', v.service, !!v.service)
  set('method', v.method, !!v.method)
  set('resource', v.resource, !!v.resource)
  set('code', v.code, !!v.code)
  set('httpStatus', +v.httpStatus, !!+v.httpStatus)
  set('latency', v.latency.trim(), !isZero(v.latency.trim()))
  set('drop', true, v.drop)
  set('probability', +v.probability, !!+v.probability)
  set('count', +v.count, !!+v.count)
  set('disabled', true, v.disabled)
  return out
}

/** ruleValues reads the form's values out of a FaultRule body. */
export function ruleValues(body: Body): Values {
  return {
    id: str(body.id),
    service: str(body.service),
    method: str(body.method),
    resource: str(body.resource),
    code: str(body.code),
    httpStatus: num(body.httpStatus),
    latency: str(body.latency),
    drop: body.drop === true,
    probability: num(body.probability),
    count: num(body.count),
    disabled: body.disabled === true,
  }
}

/** CreateFault adds a Fault rule. */
export function CreateFault() {
  const navigate = useCarriedNavigate()
  const qc = useQueryClient()
  const { data: info } = useQuery(infoQuery())
  const form = useForm<Values>({ resolver: zodResolver(schema), defaultValues: defaults })
  const errors = form.formState.errors
  const service = useWatch({ control: form.control, name: 'service' })
  const enabled = new Set(info?.services ?? [])
  const services = SERVICES.filter((s) => enabled.has(s.id) || s.id === service)
  const create = useMutation({
    meta: { toast: false },
    mutationFn: (body: Body) => addFault(body as FaultRule),
    onSuccess: (rule) => {
      void qc.invalidateQueries({ queryKey: faultsQuery().queryKey })
      toast.success(`Added rule ${rule.id ?? ''}${rule.disabled ? ', disabled' : ''}.`)
      navigate('/faults')
    },
  })
  const text = (
    name: 'id' | 'method' | 'resource' | 'httpStatus' | 'latency' | 'probability' | 'count',
    label: string,
    hint: string,
    mono = true,
  ) => (
    <Field id={`fault-${name}`} label={label} error={errors[name]?.message} hint={hint}>
      <Input
        autoComplete="off"
        spellCheck={false}
        className={mono ? 'font-mono' : undefined}
        {...fieldAria(`fault-${name}`, errors[name]?.message, hint)}
        {...form.register(name)}
      />
    </Field>
  )
  const check = (name: 'drop' | 'disabled', label: string, hint: string) => (
    <div className="flex flex-col gap-1">
      <label className="flex items-center gap-2 text-sm font-medium">
        <input
          type="checkbox"
          className="size-4"
          aria-describedby={`fault-${name}-hint`}
          {...form.register(name)}
        />
        {label}
      </label>
      <p id={`fault-${name}-hint`} className="text-xs text-muted-foreground">
        {hint}
      </p>
    </div>
  )
  return (
    <Card>
      <h1 className="text-lg font-semibold">Add fault rule</h1>
      <ResourceEditor
        form={form}
        initialBody={ruleBody(defaults, {})}
        toBody={ruleBody}
        fromBody={ruleValues}
        fields={['id', 'code', 'httpStatus', 'latency', 'probability', 'count']}
        onSubmit={(body) => create.mutateAsync(body)}
        submitLabel="Add rule"
        onCancel={() => navigate('/faults')}
        jsonHint={
          <>
            Sent to <span className="font-mono">POST /_emu/v1/faults</span>. The rule is added after
            the existing ones.
          </>
        }
      >
        <p className="text-sm text-muted-foreground">
          A request matching every matcher given fires the rule. Empty matchers match everything.
        </p>
        <div className="grid gap-4 sm:grid-cols-2">
          <Field id="fault-service" label="Service">
            <NativeSelect {...fieldAria('fault-service')} {...form.register('service')}>
              <option value="">Any Service</option>
              {services.map((s) => (
                <option key={s.id} value={s.id}>
                  {serviceName(s.id)}
                </option>
              ))}
            </NativeSelect>
          </Field>
          {text('id', 'ID', 'Optional: generated as fault-N when empty.')}
        </div>
        {text(
          'method',
          'Method',
          'Regexp on "METHOD /path" for REST, e.g. ^POST /storage/v1/b, or on the gRPC method, e.g. /google.pubsub.v1.Publisher/Publish.',
        )}
        {text('resource', 'Resource', 'Regexp on the request path, or the gRPC method.')}
        <div className="grid gap-4 sm:grid-cols-2">
          <Field
            id="fault-code"
            label="Error code"
            error={errors.code?.message}
            hint="Returned with the API's error envelope."
          >
            <NativeSelect
              {...fieldAria('fault-code', errors.code?.message, 'hint')}
              {...form.register('code')}
            >
              <option value="">None</option>
              {CODES.map((c) => (
                <option key={c} value={c}>
                  {c}
                </option>
              ))}
            </NativeSelect>
          </Field>
          {text('httpStatus', 'HTTP status', 'Optional: overrides the error code’s status.')}
        </div>
        <div className="grid gap-4 sm:grid-cols-2">
          {text('latency', 'Latency', 'Added before the request is handled, e.g. 250ms or 2s.')}
          {check(
            'drop',
            'Drop the connection',
            'Close it without a response; gRPC calls fail with UNAVAILABLE.',
          )}
        </div>
        <div className="grid gap-4 sm:grid-cols-2">
          {text(
            'probability',
            'Probability',
            'Share of matching requests that fire it, in (0,1]; empty for every one.',
          )}
          {text('count', 'Count', 'Fire at most this many times; empty for no limit.')}
        </div>
        {check('disabled', 'Add disabled', 'Kept in the list without firing until enabled.')}
      </ResourceEditor>
    </Card>
  )
}
