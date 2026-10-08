import { useQuery } from '@tanstack/react-query'
import { Check, Copy } from 'lucide-react'
import { useEffect, useRef, useState } from 'react'

import { envQuery } from '@/api/queries'
import { QueryStatus } from '@/components/QueryStatus'
import { Button } from '@/components/ui/button'
import { Card, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'

const shQuote = (s: string) => `'${s.replaceAll("'", `'\\''`)}'`

/** envScript formats variables as `gcpemu env` prints them for bash. */
export function envScript(vars: Record<string, string>): string {
  return Object.keys(vars)
    .sort()
    .map((k) => `export ${k}=${shQuote(vars[k] ?? '')}\n`)
    .join('')
}

export function EnvCard() {
  const query = useQuery(envQuery())
  const pre = useRef<HTMLPreElement>(null)
  const [copied, setCopied] = useState<'yes' | 'select' | null>(null)
  useEffect(() => {
    if (!copied) return
    const t = setTimeout(() => setCopied(null), 3000)
    return () => clearTimeout(t)
  }, [copied])
  const text = query.data ? envScript(query.data) : ''

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
    <Card aria-labelledby="env-title">
      <CardHeader>
        <div className="flex flex-col gap-1.5">
          <CardTitle id="env-title">Client environment</CardTitle>
          <CardDescription>
            The output of <code className="font-mono">gcpemu env</code>: paste it into a shell to
            point clients at this Instance.
          </CardDescription>
        </div>
        <Button variant="outline" size="sm" onClick={() => void copy()} disabled={!text}>
          {copied === 'yes' ? <Check aria-hidden /> : <Copy aria-hidden />}
          {copied === 'yes' ? 'Copied' : 'Copy'}
        </Button>
      </CardHeader>
      <p role="status" className="sr-only">
        {copied === 'yes' && 'Copied to the clipboard'}
      </p>
      {copied === 'select' && (
        <p className="text-sm text-muted-foreground">
          The clipboard is not available here; the text is selected for you to copy.
        </p>
      )}
      <QueryStatus query={query} />
      {query.data && (
        <pre
          ref={pre}
          data-testid="env-output"
          className="max-h-64 overflow-auto rounded-md bg-muted p-3 font-mono text-xs"
        >
          {text}
        </pre>
      )}
    </Card>
  )
}
