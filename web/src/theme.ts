/*
 * Ask Sauby · MyFlipKart.com — theme bootstrap
 * Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
 *
 * Loaded in <head> (as a classic script, before CSS paints) so there is no flash of the
 * wrong theme. Themes: "light" or "black"; with no saved choice the OS setting wins.
 */
(function () {
  const KEY = "sauby_theme";
  let saved: string | null = null;
  try { saved = localStorage.getItem(KEY); } catch { /* private mode: follow the OS */ }
  if (saved === "light" || saved === "black") document.documentElement.dataset.theme = saved;

  const current = (): "light" | "black" => {
    const t = document.documentElement.dataset.theme;
    if (t === "light" || t === "black") return t;
    return matchMedia("(prefers-color-scheme: dark)").matches ? "black" : "light";
  };
  // Exposed for the theme toggle buttons on every page.
  (window as any).saubyTheme = {
    current,
    toggle(): "light" | "black" {
      const next = current() === "black" ? "light" : "black";
      document.documentElement.dataset.theme = next;
      try { localStorage.setItem(KEY, next); } catch { /* ignore */ }
      return next;
    },
  };
})();
