import { create } from 'zustand'
import { createJSONStorage, persist } from 'zustand/middleware'

// Per-browser UI preferences that must not travel in a link. Anything a
// link should reproduce belongs in the URL (lib/viewState), and server
// state belongs in TanStack Query.

export type ThemePref = 'system' | 'light' | 'dark'

interface Prefs {
  theme: ThemePref
  sidebarCollapsed: boolean
  setTheme: (theme: ThemePref) => void
  toggleSidebar: () => void
}

export const usePrefs = create<Prefs>()(
  persist(
    (set) => ({
      theme: 'system',
      sidebarCollapsed: false,
      setTheme: (theme) => set({ theme }),
      toggleSidebar: () => set((s) => ({ sidebarCollapsed: !s.sidebarCollapsed })),
    }),
    {
      name: 'gcpemu-console-prefs',
      storage: createJSONStorage(() => localStorage),
      partialize: ({ theme, sidebarCollapsed }) => ({ theme, sidebarCollapsed }),
    },
  ),
)
