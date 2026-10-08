import { useSyncExternalStore } from 'react'

// A minimal toast queue. Toasts are transient UI, not preferences, so they
// live here rather than in the Zustand store.

export interface Toast {
  id: number
  kind: 'error' | 'success'
  message: string
}

let toasts: Toast[] = []
let nextId = 1
const listeners = new Set<() => void>()

function emit() {
  for (const l of listeners) l()
}

export function dismiss(id: number) {
  toasts = toasts.filter((t) => t.id !== id)
  emit()
}

function push(kind: Toast['kind'], message: string, ms: number) {
  const id = nextId++
  toasts = [...toasts, { id, kind, message }]
  emit()
  setTimeout(() => dismiss(id), ms)
}

export const toast = {
  error: (message: string) => push('error', message, 8000),
  success: (message: string) => push('success', message, 4000),
}

function subscribe(l: () => void) {
  listeners.add(l)
  return () => listeners.delete(l)
}

export function useToasts(): Toast[] {
  return useSyncExternalStore(
    subscribe,
    () => toasts,
    () => toasts,
  )
}
