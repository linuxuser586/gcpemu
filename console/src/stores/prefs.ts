import { create } from 'zustand'
import { createJSONStorage, persist } from 'zustand/middleware'

// Per-browser UI preferences that must not travel in a link. Anything a
// link should reproduce belongs in the URL (lib/viewState), and server
// state belongs in TanStack Query.

export type ThemePref = 'system' | 'light' | 'dark'

/** CopyForm is a form Copy as writes a resource in (FR-UI-015). */
export type CopyForm = 'gcloud' | 'curl' | 'tofu'

interface Prefs {
  theme: ThemePref
  sidebarCollapsed: boolean
  /**
   * principal is who the console acts as, "user:EMAIL" or
   * "serviceAccount:EMAIL", or '' for the Default principal (FR-UI-004).
   */
  principal: string
  /** copyForm is the form Copy as last showed. */
  copyForm: CopyForm
  setTheme: (theme: ThemePref) => void
  toggleSidebar: () => void
  setPrincipal: (principal: string) => void
  setCopyForm: (copyForm: CopyForm) => void
}

export const usePrefs = create<Prefs>()(
  persist(
    (set) => ({
      theme: 'system',
      sidebarCollapsed: false,
      principal: '',
      copyForm: 'gcloud',
      setTheme: (theme) => set({ theme }),
      toggleSidebar: () => set((s) => ({ sidebarCollapsed: !s.sidebarCollapsed })),
      setPrincipal: (principal) => set({ principal }),
      setCopyForm: (copyForm) => set({ copyForm }),
    }),
    {
      name: 'gcpemu-console-prefs',
      storage: createJSONStorage(() => localStorage),
      partialize: ({ theme, sidebarCollapsed, principal, copyForm }) => ({
        theme,
        sidebarCollapsed,
        principal,
        copyForm,
      }),
    },
  ),
)
