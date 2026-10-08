import { X } from 'lucide-react'

import { Button } from '@/components/ui/button'
import { dismiss, useToasts } from '@/lib/toast'
import { cn } from '@/lib/utils'

export function Toaster() {
  const toasts = useToasts()
  return (
    <ol
      aria-label="Notifications"
      className="fixed right-4 bottom-4 z-50 flex w-96 max-w-[calc(100vw-2rem)] flex-col gap-2"
    >
      {toasts.map((t) => (
        <li
          key={t.id}
          role={t.kind === 'error' ? 'alert' : 'status'}
          className={cn(
            'flex items-start gap-2 rounded-md border bg-popover p-3 text-sm shadow-md',
            t.kind === 'error' && 'border-destructive/50',
          )}
        >
          <p className="min-w-0 flex-1 break-words">{t.message}</p>
          <Button
            variant="ghost"
            size="icon"
            className="size-6"
            aria-label="Dismiss"
            onClick={() => dismiss(t.id)}
          >
            <X />
          </Button>
        </li>
      ))}
    </ol>
  )
}
