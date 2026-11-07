import { useSyncExternalStore } from "react";

/**
 * Whether the desktop shell is showing the 3D world right now. The shell owns
 * the mode; the App, which decides which toasts to draw, only needs to know.
 */
let on = false;
const listeners = new Set<() => void>();

export function setDeskWorld(next: boolean) {
  if (next === on) return;
  on = next;
  listeners.forEach((l) => l());
}

export function useDeskWorldOn(): boolean {
  return useSyncExternalStore(
    (fn) => { listeners.add(fn); return () => { listeners.delete(fn); }; },
    () => on,
    () => on,
  );
}
