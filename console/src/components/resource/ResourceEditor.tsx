import { useId, useState, type ReactNode } from 'react'
import type { FieldValues, Path, UseFormReturn } from 'react-hook-form'

import { errorMessage } from '@/api/errors'
import { Button } from '@/components/ui/button'
import { Textarea } from '@/components/ui/textarea'
import { applyFieldErrors } from '@/lib/forms'
import { cn } from '@/lib/utils'

export type Body = Record<string, unknown>

/** parseBody parses the JSON editor's text into a request body. */
export function parseBody(text: string): Body {
  const v: unknown = JSON.parse(text)
  if (typeof v !== 'object' || v === null || Array.isArray(v)) {
    throw new Error('The request body must be a JSON object.')
  }
  return v as Body
}

export interface ResourceEditorProps<V extends FieldValues> {
  /** The form, with a Zod resolver carrying the API's validation messages. */
  form: UseFormReturn<V>
  /** The request body the editor starts from. */
  initialBody: Body
  /** toBody writes the fields the form owns onto base (a copy of it). */
  toBody: (values: V, base: Body) => Body
  /** fromBody reads the fields the form owns out of body. */
  fromBody: (body: Body, prev: V) => V
  /** fields lists the form's fields that API field violations may name. */
  fields: readonly Path<V>[]
  /** onSubmit sends body; values are the form's, in either mode. */
  onSubmit: (body: Body, values: V) => Promise<unknown>
  submitLabel: string
  onCancel: () => void
  /** The form's fields. */
  children: ReactNode
  /** What the JSON is, e.g. which method it is sent to. */
  jsonHint?: ReactNode
}

/**
 * ResourceEditor is the create and edit pattern of every Service view
 * (FR-UI-011): a form whose fields mirror the API's, with the API's
 * validation messages, and a raw JSON editor of the whole request body
 * for every field the form omits. Both edit one body: switching to JSON
 * shows the form's values merged into it, and switching back reads the
 * form's fields out of it while keeping the rest. Errors the API returns
 * are shown on the fields they name and above the submit button.
 */
export function ResourceEditor<V extends FieldValues>({
  form,
  initialBody,
  toBody,
  fromBody,
  fields,
  onSubmit,
  submitLabel,
  onCancel,
  children,
  jsonHint,
}: ResourceEditorProps<V>) {
  const [mode, setMode] = useState<'form' | 'json'>('form')
  const [base, setBase] = useState<Body>(initialBody)
  const [text, setText] = useState('')
  const [jsonError, setJsonError] = useState<string>()
  const [serverError, setServerError] = useState<string>()
  const [pending, setPending] = useState(false)
  const id = useId()

  const toJson = () => {
    const body = toBody(form.getValues(), structuredClone(base))
    setBase(body)
    setText(JSON.stringify(body, null, 2))
    setJsonError(undefined)
    setMode('json')
  }
  const toForm = () => {
    let body: Body
    try {
      body = parseBody(text)
    } catch (e) {
      setJsonError(errorMessage(e))
      return
    }
    setBase(body)
    form.reset(fromBody(body, form.getValues()))
    setMode('form')
  }

  const send = async (body: Body) => {
    setServerError(undefined)
    setPending(true)
    try {
      await onSubmit(body, form.getValues())
    } catch (e) {
      if (mode === 'form') applyFieldErrors(form.setError, e, fields)
      setServerError(errorMessage(e))
    } finally {
      setPending(false)
    }
  }

  const submit = (e: React.FormEvent) => {
    if (mode === 'form') {
      void form.handleSubmit((values) => send(toBody(values, structuredClone(base))))(e)
      return
    }
    e.preventDefault()
    let body: Body
    try {
      body = parseBody(text)
    } catch (err) {
      setJsonError(errorMessage(err))
      return
    }
    void send(body)
  }

  const tab = (m: 'form' | 'json', label: string, onClick: () => void) => (
    <button
      type="button"
      role="tab"
      aria-selected={mode === m}
      onClick={() => mode !== m && onClick()}
      className={cn(
        'rounded-sm px-3 py-1 text-sm font-medium outline-none focus-visible:ring-[3px] focus-visible:ring-ring/50',
        mode === m ? 'bg-background shadow-xs' : 'text-muted-foreground hover:text-foreground',
      )}
    >
      {label}
    </button>
  )

  return (
    <form onSubmit={submit} noValidate className="flex flex-col gap-5">
      <div
        role="tablist"
        aria-label="Editor"
        className="inline-flex w-fit gap-1 rounded-md bg-muted p-1"
      >
        {tab('form', 'Form', toForm)}
        {tab('json', 'JSON', toJson)}
      </div>
      {mode === 'form' ? (
        <div className="flex max-w-2xl flex-col gap-4">{children}</div>
      ) : (
        <div className="flex flex-col gap-1">
          <label htmlFor={`${id}-json`} className="text-sm font-medium">
            Request body
          </label>
          <Textarea
            id={`${id}-json`}
            spellCheck={false}
            className="min-h-96 font-mono text-xs"
            value={text}
            aria-invalid={!!jsonError}
            aria-describedby={jsonError ? `${id}-json-error` : undefined}
            onChange={(e) => {
              setText(e.target.value)
              setJsonError(undefined)
            }}
          />
          {jsonHint && <p className="text-xs text-muted-foreground">{jsonHint}</p>}
          {jsonError && (
            <p id={`${id}-json-error`} role="alert" className="text-sm text-destructive">
              {jsonError}
            </p>
          )}
        </div>
      )}
      {serverError && (
        <p
          role="alert"
          className="rounded-md border border-destructive/50 p-3 text-sm text-destructive"
        >
          {serverError}
        </p>
      )}
      <div className="flex gap-2">
        <Button type="submit" disabled={pending}>
          {submitLabel}
        </Button>
        <Button type="button" variant="outline" onClick={onCancel}>
          Cancel
        </Button>
      </div>
    </form>
  )
}
