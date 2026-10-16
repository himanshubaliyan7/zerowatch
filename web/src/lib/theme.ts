/**
 * Light and dark theme: the visitor's choice when they made one, otherwise what
 * their system asks for. The theme is a class on <html>; index.html applies it
 * before the first paint with the same key and rule.
 */
export type Theme = "light" | "dark";

export const THEME_STORAGE_KEY = "zerowatch:theme";

export function currentTheme(): Theme {
  return document.documentElement.classList.contains("dark") ? "dark" : "light";
}

export function setTheme(theme: Theme): void {
  document.documentElement.classList.toggle("dark", theme === "dark");
  try {
    localStorage.setItem(THEME_STORAGE_KEY, theme);
  } catch {
    // Private mode: not remembering is harmless.
  }
}
