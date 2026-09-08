"use client";

import { useEffect, useState } from "react";

type ThemePref = "system" | "light" | "dark";

const STORAGE_KEY = "adminui.theme";
const ORDER: ThemePref[] = ["system", "light", "dark"];
const LABEL: Record<ThemePref, string> = { system: "System", light: "Light", dark: "Dark" };

// ponytail: localStorage/matchMedia can throw (private browsing, quota, no
// `window` during SSR) — best-effort persistence only, same guarded pattern
// as lib/tenant.tsx's stored-tenant helpers.
function readStoredTheme(): ThemePref {
  try {
    const v = localStorage.getItem(STORAGE_KEY);
    if (v === "light" || v === "dark") return v;
  } catch {
    // see comment above
  }
  return "system";
}

function writeStoredTheme(pref: ThemePref): void {
  try {
    if (pref === "system") localStorage.removeItem(STORAGE_KEY);
    else localStorage.setItem(STORAGE_KEY, pref);
  } catch {
    // see comment above
  }
}

function prefersDark(): boolean {
  try {
    return window.matchMedia("(prefers-color-scheme: dark)").matches;
  } catch {
    return false;
  }
}

function applyTheme(pref: ThemePref): void {
  const dark = pref === "dark" || (pref === "system" && prefersDark());
  document.documentElement.classList.toggle("dark", dark);
}

function ThemeIcon({ pref }: { pref: ThemePref }) {
  if (pref === "light") {
    return (
      <svg aria-hidden="true" viewBox="0 0 24 24" fill="none" className="h-4 w-4">
        <circle cx="12" cy="12" r="4" stroke="currentColor" strokeWidth="1.5" />
        <path
          d="M12 2v2.5M12 19.5V22M4.2 4.2l1.8 1.8M18 18l1.8 1.8M2 12h2.5M19.5 12H22M4.2 19.8 6 18M18 6l1.8-1.8"
          stroke="currentColor"
          strokeWidth="1.5"
          strokeLinecap="round"
        />
      </svg>
    );
  }
  if (pref === "dark") {
    return (
      <svg aria-hidden="true" viewBox="0 0 24 24" fill="none" className="h-4 w-4">
        <path d="M20 14.5A8 8 0 1 1 9.5 4a6.5 6.5 0 0 0 10.5 10.5Z" stroke="currentColor" strokeWidth="1.5" />
      </svg>
    );
  }
  return (
    <svg aria-hidden="true" viewBox="0 0 24 24" fill="none" className="h-4 w-4">
      <rect x="3" y="4.5" width="18" height="12" rx="2" stroke="currentColor" strokeWidth="1.5" />
      <path d="M8 20h8M12 16.5V20" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" />
    </svg>
  );
}

// A labeled tri-state control (system/light/dark), not a bare sun/moon icon
// switch: each click cycles to the next mode and the current mode is always
// spelled out as visible text, which also doubles as its accessible name.
export function ThemeToggle() {
  // Lazy-init from storage (same pattern as lib/tenant.tsx's stored-tenant
  // read) rather than reading it in a mount effect: this component only ever
  // renders once RequireAuth has resolved (client-side, well past the
  // server-rendered/hydration pass), so there's no SSR markup to mismatch.
  const [pref, setPref] = useState<ThemePref>(() => readStoredTheme());

  useEffect(() => {
    if (pref !== "system") return;
    let mql: MediaQueryList;
    try {
      mql = window.matchMedia("(prefers-color-scheme: dark)");
    } catch {
      return;
    }
    const onChange = () => applyTheme("system");
    mql.addEventListener("change", onChange);
    return () => mql.removeEventListener("change", onChange);
  }, [pref]);

  function cycle(): void {
    const next = ORDER[(ORDER.indexOf(pref) + 1) % ORDER.length];
    setPref(next);
    writeStoredTheme(next);
    applyTheme(next);
  }

  return (
    <button
      type="button"
      onClick={cycle}
      title="Change theme"
      className="flex h-9 items-center gap-1.5 rounded-md border border-border bg-bg-elevated px-2.5 text-sm text-fg-muted transition-colors hover:border-border-strong hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-text focus-visible:ring-offset-2 focus-visible:ring-offset-bg"
    >
      <ThemeIcon pref={pref} />
      Theme: {LABEL[pref]}
    </button>
  );
}
