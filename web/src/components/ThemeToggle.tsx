/**
 * Auto / light / dark, in that order, because auto is the right default and a
 * three-way control should start where most people should stay.
 *
 * "Auto" is the ABSENCE of a data-theme attribute, not a third value written
 * into one: the CSS follows prefers-color-scheme on its own, so auto means
 * "stop overriding" rather than "override with whatever the system said a
 * moment ago". That distinction matters at sunset, when a phone flips itself
 * to dark and a stored snapshot of "light" would fight it.
 *
 * The choice is remembered per browser in localStorage, which is per-viewer
 * and per-device and cannot be read back by anything else -- right for a
 * preference, and the reason every access is wrapped: a private window or
 * blocked site data makes these throw rather than return null.
 */

import { useEffect, useState } from 'react'

type Theme = 'auto' | 'light' | 'dark'

const KEY = 'herdr-expose.theme'
const ORDER: Theme[] = ['auto', 'light', 'dark']

const LABEL: Record<Theme, string> = {
  auto: 'Auto',
  light: 'Light',
  dark: 'Dark',
}

function read(): Theme {
  try {
    const v = localStorage.getItem(KEY)
    if (v === 'light' || v === 'dark' || v === 'auto') return v
  } catch {
    // Private window, or site data blocked. Not an error: auto is a fine
    // answer, and a theme toggle must never be a reason the app fails to load.
  }
  return 'auto'
}

export function applyStoredTheme() {
  apply(read())
}

function apply(t: Theme) {
  const root = document.documentElement
  if (t === 'auto') root.removeAttribute('data-theme')
  else root.setAttribute('data-theme', t)
}

export function ThemeToggle() {
  const [theme, setTheme] = useState<Theme>(read)

  useEffect(() => {
    apply(theme)
    try {
      if (theme === 'auto') localStorage.removeItem(KEY)
      else localStorage.setItem(KEY, theme)
    } catch {
      // Same as above: the theme still applies for this session.
    }
  }, [theme])

  const next = ORDER[(ORDER.indexOf(theme) + 1) % ORDER.length]

  return (
    <button
      type="button"
      className="theme-toggle"
      onClick={() => setTheme(next)}
      // The label says the CURRENT state; the hint says what tapping does.
      // A button that only names its next state leaves you unable to tell
      // what is on without pressing it.
      title={`Theme: ${LABEL[theme]} — tap for ${LABEL[next]}`}
      aria-label={`Theme: ${LABEL[theme]}. Tap for ${LABEL[next]}.`}
    >
      <Icon theme={theme} />
    </button>
  )
}

function Icon({ theme }: { theme: Theme }) {
  if (theme === 'light') {
    return (
      <svg viewBox="0 0 16 16" aria-hidden="true">
        <circle cx="8" cy="8" r="3.2" fill="currentColor" />
        {[0, 45, 90, 135, 180, 225, 270, 315].map((d) => (
          <line
            key={d}
            x1="8"
            y1="1.4"
            x2="8"
            y2="3.1"
            stroke="currentColor"
            strokeWidth="1.5"
            strokeLinecap="round"
            transform={`rotate(${d} 8 8)`}
          />
        ))}
      </svg>
    )
  }
  if (theme === 'dark') {
    return (
      <svg viewBox="0 0 16 16" aria-hidden="true">
        <path
          d="M13 9.6A5.6 5.6 0 0 1 6.4 3a5.6 5.6 0 1 0 6.6 6.6z"
          fill="currentColor"
        />
      </svg>
    )
  }
  // Auto: half and half, which is what it does.
  return (
    <svg viewBox="0 0 16 16" aria-hidden="true">
      <circle cx="8" cy="8" r="5.4" fill="none" stroke="currentColor" strokeWidth="1.5" />
      <path d="M8 2.6a5.4 5.4 0 0 1 0 10.8z" fill="currentColor" />
    </svg>
  )
}
