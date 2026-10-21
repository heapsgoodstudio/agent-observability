// Persist the raw preference. New workspaces start dark; existing choices win.
export const THEMES = ['system', 'light', 'dark'] as const;
export type Theme = (typeof THEMES)[number];

export function nextTheme(current: Theme): Theme {
  const i = THEMES.indexOf(current);
  return THEMES[(i + 1) % THEMES.length];
}

// localStorage key holding the raw preference.
export const THEME_STORAGE_KEY = 'claude-obs.theme';

function isTheme(value: string | null): value is Theme {
  return (THEMES as readonly string[]).includes(value ?? '');
}

// Reads the persisted preference, defaulting to "dark" (private mode or a
// blocked store must not break the page). Browser-only: guarded the same
// way frontend/src/lib/client.ts guards its own window access, so calling
// this from anywhere but an effect or event handler fails soft rather than
// throwing during Next's static-export prerender.
export function readTheme(): Theme {
  if (typeof window === 'undefined') return 'dark';
  try {
    const stored = localStorage.getItem(THEME_STORAGE_KEY);
    return isTheme(stored) ? stored : 'dark';
  } catch {
    return 'dark';
  }
}

// CSS resolves explicit system preference without replacing the saved value.
export function applyTheme(pref: Theme): void {
  if (typeof window === 'undefined') return;
  const root = document.documentElement;
  root.dataset.theme = pref;
  try {
    localStorage.setItem(THEME_STORAGE_KEY, pref);
  } catch {
    /* nothing to do; the page still renders in the resolved theme */
  }
}
