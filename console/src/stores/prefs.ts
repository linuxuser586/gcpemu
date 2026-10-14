import { create } from 'zustand'
import { createJSONStorage, persist } from 'zustand/middleware'

// Per-browser UI preferences that must not travel in a link. Anything a
// link should reproduce belongs in the URL (lib/viewState), and server
// state belongs in TanStack Query.

export type ThemePref = 'system' | 'light' | 'dark'

interface Prefs {
  theme: ThemePref
  sidebarCollapsed: boolean
  /**
   * principal is who the console acts as, "user:EMAIL" or
   * "serviceAccount:EMAIL", or '' for the Default principal (FR-UI-004).
   */
  principal: string
  setTheme: (theme: ThemePref) => void
  toggleSidebar: () => void
  setPrincipal: (principal: string) => void
}

export const usePrefs = create<Prefs>()(
  persist(
    (set) => ({
      theme: 'system',
      sidebarCollapsed: false,
      principal: '',
      setTheme: (theme) => set({ theme }),
      toggleSidebar: () => set((s) => ({ sidebarCollapsed: !s.sidebarCollapsed })),
      setPrincipal: (principal) => set({ principal }),
    }),
    {
      name: 'gcpemu-console-prefs',
      storage: createJSONStorage(() => localStorage),
      partialize: ({ theme, sidebarCollapsed, principal }) => ({
        theme,
        sidebarCollapsed,
        principal,
      }),
    },
  ),
)
