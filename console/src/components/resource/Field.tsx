import type { ReactNode } from 'react'

/**
 * Field labels one form control and shows its hint and error. The control
 * takes fieldAria(id, error, hint) so that both are announced with it.
 */
export function Field({
  id,
  label,
  error,
  hint,
  children,
}: {
  id: string
  label: ReactNode
  error?: string
  hint?: ReactNode
  children: ReactNode
}) {
  return (
    <div className="flex flex-col gap-1">
      <label htmlFor={id} className="text-sm font-medium">
        {label}
      </label>
      {children}
      {hint && (
        <p id={`${id}-hint`} className="text-xs text-muted-foreground">
          {hint}
        </p>
      )}
      {error && (
        <p id={`${id}-error`} role="alert" className="text-sm text-destructive">
          {error}
        </p>
      )}
    </div>
  )
}

export function fieldAria(id: string, error?: string, hint?: ReactNode) {
  const by = [hint ? `${id}-hint` : '', error ? `${id}-error` : ''].filter(Boolean).join(' ')
  return { id, 'aria-invalid': !!error, 'aria-describedby': by || undefined }
}
