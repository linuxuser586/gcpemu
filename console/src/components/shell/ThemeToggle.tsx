import { Monitor, Moon, Sun } from 'lucide-react'

import { Button } from '@/components/ui/button'
import { usePrefs, type ThemePref } from '@/stores/prefs'

const next: Record<ThemePref, ThemePref> = { system: 'light', light: 'dark', dark: 'system' }
const label: Record<ThemePref, string> = {
  system: 'Theme: follows the system',
  light: 'Theme: light',
  dark: 'Theme: dark',
}

export function ThemeToggle() {
  const theme = usePrefs((s) => s.theme)
  const setTheme = usePrefs((s) => s.setTheme)
  const Icon = theme === 'light' ? Sun : theme === 'dark' ? Moon : Monitor
  return (
    <Button
      variant="ghost"
      size="icon"
      aria-label={`${label[theme]} (change)`}
      title={label[theme]}
      onClick={() => setTheme(next[theme])}
    >
      <Icon />
    </Button>
  )
}
