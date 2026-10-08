import { useEffect } from 'react'

import { usePrefs } from '@/stores/prefs'

const dark = () => globalThis.matchMedia?.('(prefers-color-scheme: dark)')

/** useApplyTheme follows the OS colour scheme unless the user overrode it (FR-UI-007). */
export function useApplyTheme() {
  const theme = usePrefs((s) => s.theme)
  useEffect(() => {
    const mq = dark()
    const apply = () => {
      const isDark = theme === 'dark' || (theme === 'system' && !!mq?.matches)
      document.documentElement.classList.toggle('dark', isDark)
    }
    apply()
    mq?.addEventListener('change', apply)
    return () => mq?.removeEventListener('change', apply)
  }, [theme])
}
