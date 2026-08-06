/**
 * Light/dark theme.
 *
 * Binary on purpose. A tri-state system/light/dark cycle means the first click
 * on a light OS goes system -> light and paints nothing new, so the control
 * reads as broken. The OS preference still decides the starting theme; the
 * toggle then flips it and the choice is remembered.
 */

export const STORAGE_KEY = 'blast-radius:theme';

export function systemTheme() {
  return window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
}

export function storedTheme() {
  try {
    const value = localStorage.getItem(STORAGE_KEY);
    return value === 'light' || value === 'dark' ? value : null;
  } catch {
    return null; // private browsing, or storage disabled
  }
}

export function currentTheme() {
  return storedTheme() || systemTheme();
}

export function applyTheme(theme) {
  document.documentElement.dataset.theme = theme;
  return theme;
}

export function setTheme(theme) {
  try {
    localStorage.setItem(STORAGE_KEY, theme);
  } catch {
    // Storage is optional; the theme still applies for this page.
  }
  return applyTheme(theme);
}

export function toggleTheme() {
  return setTheme(currentTheme() === 'dark' ? 'light' : 'dark');
}

/** Follows the OS until the user picks a side, then stops. */
export function watchSystemTheme(onChange) {
  if (!window.matchMedia) return;
  window.matchMedia('(prefers-color-scheme: dark)').addEventListener('change', (event) => {
    if (storedTheme()) return;
    onChange(applyTheme(event.matches ? 'dark' : 'light'));
  });
}
