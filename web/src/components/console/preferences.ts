import { useCallback, useEffect, useState } from "react";

// localStorage-backed boolean preference. Falls back gracefully on SSR.
export function usePref(key: string, fallback: boolean): [boolean, (v: boolean) => void] {
  const [value, setValue] = useState(fallback);
  useEffect(() => {
    const raw = typeof window !== "undefined" ? localStorage.getItem(key) : null;
    if (raw != null) setValue(raw === "1");
  }, [key]);
  const set = useCallback((v: boolean) => {
    setValue(v);
    if (typeof window !== "undefined") localStorage.setItem(key, v ? "1" : "0");
  }, [key]);
  return [value, set];
}

// Read a JSON array from localStorage (returns [] on missing / invalid).
export function useHiddenBadges(): Set<string> {
  const [set] = useState(() => {
    if (typeof window === "undefined") return new Set<string>();
    try {
      const raw = localStorage.getItem("ui.hiddenBadges");
      return new Set<string>(raw ? JSON.parse(raw) : []);
    } catch {
      return new Set<string>();
    }
  });
  return set;
}
