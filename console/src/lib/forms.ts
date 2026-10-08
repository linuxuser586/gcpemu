import type { FieldValues, Path, UseFormSetError } from 'react-hook-form'

import { fieldViolations } from '@/api/errors'

/**
 * applyFieldErrors shows an API error's google.rpc.BadRequest field
 * violations on the form fields they name, and reports whether it showed
 * any. A form whose mutation sets meta {toast: false} toasts the error
 * itself when this returns false.
 */
export function applyFieldErrors<T extends FieldValues>(
  setError: UseFormSetError<T>,
  error: unknown,
  fields: readonly Path<T>[],
): boolean {
  let applied = false
  for (const v of fieldViolations(error)) {
    const field = fields.find((f) => f === v.field)
    if (field) {
      setError(field, { type: 'server', message: v.description })
      applied = true
    }
  }
  return applied
}
