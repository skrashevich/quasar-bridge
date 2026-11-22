"use strict";

(() => {
  const key = "quasarBridge.theme";
  const system = window.matchMedia("(prefers-color-scheme: dark)");
  const preference = value => value === "light" || value === "dark" ? value : "system";
  let theme = "system";
  let picker;

  function apply() {
    document.documentElement.dataset.theme = theme === "system" ? (system.matches ? "dark" : "light") : theme;
    if (picker) picker.value = theme;
  }

  try { theme = preference(localStorage.getItem(key)); } catch { /* Storage is optional. */ }
  apply();
  system.addEventListener("change", apply);
  window.addEventListener("storage", event => {
    if (event.key !== key && event.key !== null) return;
    theme = preference(event.newValue);
    apply();
  });
  document.addEventListener("DOMContentLoaded", () => {
    picker = document.getElementById("panel-theme");
    if (!picker) return;
    picker.value = theme;
    picker.addEventListener("change", () => {
      theme = preference(picker.value);
      apply();
      try {
        if (theme === "system") localStorage.removeItem(key);
        else localStorage.setItem(key, theme);
      } catch { /* The choice still applies to this page. */ }
    });
  });
})();
