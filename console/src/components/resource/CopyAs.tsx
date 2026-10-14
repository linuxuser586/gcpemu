import { Check, Copy, Terminal } from 'lucide-react'
import { useEffect, useId, useRef, useState, type ReactNode } from 'react'

import { origin } from '@/api/fetch'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogTitle,
  DialogTrigger,
} from '@/components/ui/dialog'
import { curl, type Recipe } from '@/lib/copyAs'
import { cn } from '@/lib/utils'
import { usePrefs, type CopyForm } from '@/stores/prefs'

const FORMS: readonly { form: CopyForm; label: string; hint: ReactNode }[] = [
  {
    form: 'gcloud',
    label: 'gcloud',
    hint: (
      <>
        Run it in a shell that has the output of <code className="font-mono">gcpemu env</code>.
      </>
    ),
  },
  {
    form: 'curl',
    label: 'curl',
    hint: 'It goes straight to this Instance with no credentials, so it runs as the Default principal.',
  },
  {
    form: 'tofu',
    label: 'OpenTofu',
    hint: (
      <>
        Add it to a configuration with the provider block{' '}
        <code className="font-mono">gcpemu tofu-provider</code> prints.
      </>
    ),
  },
]

const list = <T,>(v?: T | T[]): T[] => (v === undefined ? [] : Array.isArray(v) ? v : [v])

/** text is recipe in one form, or '' when no client of that kind creates it. */
export function recipeText(recipe: Recipe, form: CopyForm, at = origin()): string {
  switch (form) {
    case 'gcloud':
      return list(recipe.gcloud).join('\n\n')
    case 'curl':
      return list(recipe.rest)
        .map((c) => curl(at, c))
        .join('\n\n')
    case 'tofu':
      return recipe.tofu ?? ''
  }
}

export interface CopyAsProps {
  /** what kind of resource it is, e.g. "topic". */
  what: string
  recipe: () => Recipe
  /** name tells the resource apart where several share a page, as table rows do. */
  name?: string
  /** size matches the buttons beside it; icon is the icon-only button of a table row. */
  size?: 'default' | 'sm' | 'icon'
}

/**
 * CopyAs offers a resource as the gcloud command, the curl request and the
 * OpenTofu resource block that would create it as it is (FR-UI-015). The
 * form last chosen is remembered for the next resource.
 */
export function CopyAs({ what, recipe, name, size = 'default' }: CopyAsProps) {
  const [open, setOpen] = useState(false)
  const form = usePrefs((s) => s.copyForm)
  const setForm = usePrefs((s) => s.setCopyForm)
  const [copied, setCopied] = useState<'yes' | 'select' | null>(null)
  const pre = useRef<HTMLPreElement>(null)
  const id = useId()
  useEffect(() => {
    if (copied !== 'yes') return
    const t = setTimeout(() => setCopied(null), 3000)
    return () => clearTimeout(t)
  }, [copied])

  const text = open ? recipeText(recipe(), form) : ''
  const current = FORMS.find((f) => f.form === form) ?? FORMS[0]!

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(text)
      setCopied('yes')
    } catch {
      // No clipboard access: select the text for the user to copy.
      const sel = globalThis.getSelection()
      if (pre.current && sel) sel.selectAllChildren(pre.current)
      setCopied('select')
    }
  }

  return (
    <Dialog
      open={open}
      onOpenChange={(o) => {
        setOpen(o)
        setCopied(null)
      }}
    >
      <DialogTrigger asChild>
        <Button
          variant={size === 'icon' ? 'ghost' : 'outline'}
          size={size}
          aria-label={name ? `Copy ${what} ${name} as` : `Copy ${what} as`}
          title={size === 'icon' ? 'Copy as' : undefined}
        >
          <Terminal aria-hidden />
          {size !== 'icon' && 'Copy as'}
        </Button>
      </DialogTrigger>
      <DialogContent className="max-w-3xl">
        <DialogTitle>Copy {what} as</DialogTitle>
        <DialogDescription>
          The command, request or resource block that creates the {what} as it is now.
        </DialogDescription>
        <div
          role="tablist"
          aria-label="Copy as"
          className="inline-flex w-fit gap-1 rounded-md bg-muted p-1"
        >
          {FORMS.map((f) => (
            <button
              key={f.form}
              type="button"
              role="tab"
              id={`${id}-${f.form}`}
              aria-selected={form === f.form}
              aria-controls={`${id}-panel`}
              onClick={() => {
                setForm(f.form)
                setCopied(null)
              }}
              className={cn(
                'rounded-sm px-3 py-1 text-sm font-medium outline-none focus-visible:ring-[3px] focus-visible:ring-ring/50',
                form === f.form
                  ? 'bg-background shadow-xs'
                  : 'text-muted-foreground hover:text-foreground',
              )}
            >
              {f.label}
            </button>
          ))}
        </div>
        <div
          role="tabpanel"
          id={`${id}-panel`}
          aria-labelledby={`${id}-${form}`}
          className="flex min-w-0 flex-col gap-2"
        >
          {text ? (
            <>
              <pre
                ref={pre}
                aria-label={`${current.label} for the ${what}`}
                className="max-h-[60vh] overflow-auto rounded-md bg-muted p-3 font-mono text-xs"
              >
                {text}
              </pre>
              <p className="text-xs text-muted-foreground">{current.hint}</p>
              <Button variant="outline" className="w-fit" onClick={() => void copy()}>
                {copied === 'yes' ? <Check aria-hidden /> : <Copy aria-hidden />}
                {copied === 'yes' ? 'Copied' : 'Copy'}
              </Button>
              <p role="status" className="sr-only">
                {copied === 'yes' && 'Copied to the clipboard'}
              </p>
              {copied === 'select' && (
                <p className="text-sm text-muted-foreground">
                  The clipboard is not available here; the text is selected for you to copy.
                </p>
              )}
            </>
          ) : (
            <p className="text-sm text-muted-foreground">
              {current.label} cannot create this {what}.
            </p>
          )}
        </div>
      </DialogContent>
    </Dialog>
  )
}
