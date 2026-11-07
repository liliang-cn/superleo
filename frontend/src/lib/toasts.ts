import { useEffect, useState } from "react";
import { EventsOff, EventsOn } from "../../wailsjs/runtime/runtime";
import { noticeArrived } from "./notifications";

/**
 * Toasts, fed by the backend.
 *
 * Every message the user has to see is published once in Go and drawn by
 * whoever is listening — a webhook for the person who walked away, this for the
 * one who is here. So this file subscribes rather than decides: it does not
 * know why a message exists, only how it looks.
 *
 * A store rather than context because half the callers are not components. A
 * failed RPC in lib/, an error caught in an event handler, a Wails binding that
 * rejected — those had nowhere to put a message and so each grew its own inline
 * `note` state that only the one screen showing it could see.
 */

export type ToastLevel = "info" | "success" | "warn" | "error";

export interface Toast {
  id: string;
  level: ToastLevel;
  title: string;
  message: string;
  /** What produced it — a schedule's prompt, a tool. Drawn as a second line. */
  source?: string;
  /** Conversation to open when it is clicked, if any. */
  session?: string;
  /** Milliseconds before it fades; 0 keeps it until dismissed. */
  ttl: number;
  at: number;
}

/** How long each level stays, in ms. */
const TTL: Record<ToastLevel, number> = {
  info: 4000,
  success: 4000,
  warn: 8000,
  // An error stays until it is dismissed. The whole reason to show one is that
  // something needs doing about it, and a message that removes itself while the
  // user is reading it is worse than no message: they know something appeared
  // and cannot find out what.
  error: 0,
};

/** The most on screen at once. Older ones drop off the top. */
const MAX_VISIBLE = 4;

type Listener = (toasts: Toast[]) => void;

let toasts: Toast[] = [];
const listeners = new Set<Listener>();
const timers = new Map<string, ReturnType<typeof setTimeout>>();

function publish() {
  const snapshot = toasts;
  for (const listener of listeners) listener(snapshot);
}

function schedule(toast: Toast) {
  if (toast.ttl <= 0) return;
  timers.set(
    toast.id,
    setTimeout(() => dismiss(toast.id), toast.ttl),
  );
}

function clearTimer(id: string) {
  const timer = timers.get(id);
  if (timer !== undefined) {
    clearTimeout(timer);
    timers.delete(id);
  }
}

export function dismiss(id: string) {
  clearTimer(id);
  const next = toasts.filter((t) => t.id !== id);
  if (next.length === toasts.length) return;
  toasts = next;
  publish();
}

export function dismissAll() {
  for (const id of timers.keys()) clearTimeout(timers.get(id)!);
  timers.clear();
  toasts = [];
  publish();
}

export interface ToastInput {
  level?: ToastLevel;
  title?: string;
  message: string;
  source?: string;
  session?: string;
  /**
   * Replaces any toast already showing under the same key rather than stacking
   * beside it. A scheduled run that reports twice, or a poll that fails every
   * four seconds, should leave one toast behind and not a column of them.
   */
  key?: string;
  ttl?: number;
}

/**
 * A raw tap on every toast as it is raised, for a surface that draws them
 * itself (the 3D view's stack). Not a snapshot: a burst of toasts in one tick
 * reaches it one by one, where a component reading the store would see only
 * the last render's worth.
 */
const shownListeners = new Set<(toast: Toast) => void>();
export function onToastShown(listener: (toast: Toast) => void): () => void {
  shownListeners.add(listener);
  return () => { shownListeners.delete(listener); };
}

export function show(input: ToastInput): string {
  const level = input.level ?? "info";
  const id = input.key ?? `t${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;

  // A repeat under the same key takes the old one's place, so its timer has to
  // go with it — otherwise the first toast's expiry removes the second.
  clearTimer(id);

  const toast: Toast = {
    id,
    level,
    title: input.title ?? "",
    message: input.message,
    source: input.source,
    session: input.session,
    ttl: input.ttl ?? TTL[level],
    at: Date.now(),
  };

  toasts = [...toasts.filter((t) => t.id !== id), toast].slice(-MAX_VISIBLE);
  // Anything pushed off the end keeps a timer that would fire against nothing.
  for (const id of [...timers.keys()]) {
    if (!toasts.some((t) => t.id === id)) clearTimer(id);
  }
  schedule(toast);
  publish();
  for (const listener of shownListeners) listener(toast);
  return id;
}

export const toast = {
  info: (message: string, extra?: Omit<ToastInput, "message" | "level">) =>
    show({ ...extra, message, level: "info" }),
  success: (message: string, extra?: Omit<ToastInput, "message" | "level">) =>
    show({ ...extra, message, level: "success" }),
  warn: (message: string, extra?: Omit<ToastInput, "message" | "level">) =>
    show({ ...extra, message, level: "warn" }),
  error: (message: string, extra?: Omit<ToastInput, "message" | "level">) =>
    show({ ...extra, message, level: "error" }),
  show,
  dismiss,
  dismissAll,
};

/** Subscribe a component to the store. */
export function useToasts(): Toast[] {
  const [current, setCurrent] = useState<Toast[]>(toasts);
  useEffect(() => {
    listeners.add(setCurrent);
    // The store may have moved between the first render and this effect.
    setCurrent(toasts);
    return () => {
      listeners.delete(setCurrent);
    };
  }, []);
  return current;
}

/** The payload the backend publishes. Mirrors backend.Notice. */
interface NoticePayload {
  level?: ToastLevel;
  title?: string;
  message?: string;
  source?: string;
  session?: string;
  key?: string;
}

/**
 * Draw every notice the backend publishes.
 *
 * Mounted once, near the root. Notices arrive over the same event bridge
 * everything else uses, so this works in the desktop window and in serve mode
 * without either knowing about the other.
 */
export function useBackendToasts(): void {
  useEffect(() => {
    EventsOn("notice", (payload: NoticePayload) => {
      const message = (payload?.message ?? "").trim();
      // A notice with nothing to say is a bug somewhere upstream, and an empty
      // toast in the corner is the least useful way to report it.
      if (!message) return;
      // The centre keeps what this is about to let fade. It is told from here
      // rather than subscribing itself because EventsOff removes every handler
      // for a name, so two subscribers to "notice" is one unmount away from no
      // subscribers at all.
      noticeArrived();
      show({
        level: payload.level ?? "info",
        title: payload.title,
        message,
        source: payload.source,
        session: payload.session,
        key: payload.key,
      });
    });
    return () => EventsOff("notice");
  }, []);
}
