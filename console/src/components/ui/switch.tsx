import type { ComponentProps } from 'react'

import { cn } from '@/lib/utils'

/** Switch is an on/off button (role="switch"); label it with aria-label or aria-labelledby. */
export function Switch({
  checked,
  className,
  ...props
}: Omit<ComponentProps<'button'>, 'role' | 'type'> & { checked: boolean }) {
  return (
    <button
      type="button"
      role="switch"
      aria-checked={checked}
      className={cn(
        'relative inline-flex h-6 w-11 shrink-0 items-center rounded-full border-2 border-transparent transition-colors outline-none focus-visible:ring-[3px] focus-visible:ring-ring/50 disabled:opacity-50',
        checked ? 'bg-primary' : 'bg-input',
        className,
      )}
      {...props}
    >
      <span
        aria-hidden
        className={cn(
          'inline-block size-5 rounded-full bg-background shadow-sm transition-transform',
          checked ? 'translate-x-5' : 'translate-x-0',
        )}
      />
    </button>
  )
}
