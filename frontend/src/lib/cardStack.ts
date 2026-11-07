/**
 * The 3D view's notification stack, as plain logic.
 *
 * Nothing here knows about React, Wails or the clock: time and timers come in
 * through the options, so the rules (what times out, what a hover or an open
 * card pauses, how the overflow folds into "+N") can be tested with a fake
 * clock. components/CardStack.tsx draws it, lib/cardFeed.ts fills it.
 */

export type CardKind = "notice" | "run" | "approval";
export type CardLevel = "info" | "success" | "warn" | "error";

export interface Card {
  /** Stable identity. A push under an id already showing replaces that card. */
  id: string;
  kind: CardKind;
  level: CardLevel;
  title: string;
  /** What the collapsed card says: two lines at most. */
  preview: string;
  /** The whole content, Markdown. Shown when the card is expanded. */
  body: string;
  /** A small line of where it came from: a schedule's prompt, who asks. */
  source?: string;
  /** Epoch ms. */
  at: number;
  /** Conversation the primary action opens, if any. */
  session?: string;
  /** What the feed needs to act on it: a run key, an approval id. */
  ref?: string;
  /** Ms until it leaves by itself; 0 stays until handled. */
  ttl: number;
  /** Whether × and dismiss-all may remove it. An approval is answered instead. */
  dismissable: boolean;
}

export type CardInput = Omit<Card, "at" | "ttl" | "dismissable"> & Partial<Pick<Card, "at" | "ttl" | "dismissable">>;

export type RemoveReason = "dismiss" | "timeout" | "resolved";

export interface StackState {
  /** Newest first. */
  cards: Card[];
  /** What is drawn in full: the newest few, or all of them once opened up. */
  visible: Card[];
  /** How many are folded into the "+N" row. */
  hidden: number;
  showAll: boolean;
  expanded: string[];
}

/** How long an info or success card stays. Warn, error and approvals stay. */
export const AUTO_MS = 5000;
/** The most drawn in full before the rest fold into "+N". */
export const MAX_VISIBLE = 4;
/** After a hover ends a card is given at least this long, not the sliver left. */
export const MIN_AFTER_HOVER = 1500;

export function defaultTtl(kind: CardKind, level: CardLevel): number {
  if (kind === "approval") return 0;
  return level === "info" || level === "success" ? AUTO_MS : 0;
}

export interface StackOptions {
  now?: () => number;
  setTimer?: (fn: () => void, ms: number) => unknown;
  clearTimer?: (handle: unknown) => void;
  maxVisible?: number;
  onRemove?: (card: Card, reason: RemoveReason) => void;
}

interface Clock { handle: unknown; startedAt: number; left: number }

export function createStack(opts: StackOptions = {}) {
  const now = opts.now ?? Date.now;
  const setTimer = opts.setTimer ?? ((fn, ms) => setTimeout(fn, ms));
  const clearTimer = opts.clearTimer ?? ((h) => clearTimeout(h as ReturnType<typeof setTimeout>));
  const maxVisible = opts.maxVisible ?? MAX_VISIBLE;

  let cards: Card[] = [];
  let showAll = false;
  const expanded = new Set<string>();
  const hovered = new Set<string>();
  const clocks = new Map<string, Clock>();
  const listeners = new Set<() => void>();
  let snapshot: StackState | null = null;

  const emit = () => { snapshot = null; listeners.forEach((l) => l()); };

  const paused = (id: string) => hovered.has(id) || expanded.has(id);

  function stop(id: string) {
    const c = clocks.get(id);
    if (!c) return;
    clearTimer(c.handle);
    clocks.delete(id);
  }

  function start(card: Card, left: number) {
    stop(card.id);
    if (card.ttl <= 0 || paused(card.id)) return;
    const handle = setTimer(() => { clocks.delete(card.id); remove(card.id, "timeout"); }, left);
    clocks.set(card.id, { handle, startedAt: now(), left });
  }

  /** Freezes a running clock, keeping what is left of it. */
  function freeze(id: string): number | undefined {
    const c = clocks.get(id);
    if (!c) return undefined;
    const left = Math.max(0, c.left - (now() - c.startedAt));
    clearTimer(c.handle);
    clocks.delete(id);
    return left;
  }

  // What a paused card still has to live, so a hover does not hand out a fresh
  // full lifetime and does not take the card away the instant it ends.
  const remaining = new Map<string, number>();

  function pause(id: string) {
    const left = freeze(id);
    if (left !== undefined) remaining.set(id, left);
  }

  function resume(id: string) {
    const card = cards.find((c) => c.id === id);
    if (!card || paused(id) || card.ttl <= 0) return;
    const left = remaining.get(id) ?? card.ttl;
    start(card, Math.max(left, Math.min(MIN_AFTER_HOVER, card.ttl)));
  }

  function remove(id: string, reason: RemoveReason) {
    const card = cards.find((c) => c.id === id);
    if (!card) return;
    stop(id);
    remaining.delete(id);
    expanded.delete(id);
    hovered.delete(id);
    cards = cards.filter((c) => c.id !== id);
    if (cards.length <= maxVisible) showAll = false;
    emit();
    opts.onRemove?.(card, reason);
  }

  return {
    push(input: CardInput): Card {
      const card: Card = {
        ...input,
        at: input.at ?? now(),
        ttl: input.ttl ?? defaultTtl(input.kind, input.level),
        dismissable: input.dismissable ?? input.kind !== "approval",
      };
      // The same id again takes the old card's place at the top, with a fresh
      // life. A card being read keeps its open state and its pause.
      const had = cards.some((c) => c.id === card.id);
      stop(card.id);
      remaining.delete(card.id);
      cards = [card, ...cards.filter((c) => c.id !== card.id)];
      if (!had) { expanded.delete(card.id); hovered.delete(card.id); }
      start(card, card.ttl);
      emit();
      return card;
    },
    /** The × on one card. An approval ignores it: it is answered, not closed. */
    dismiss(id: string) {
      const card = cards.find((c) => c.id === id);
      if (card && card.dismissable) remove(id, "dismiss");
    },
    /** Takes a card away because what it asked about is over. */
    resolve(id: string) { remove(id, "resolved"); },
    dismissAll() {
      for (const c of [...cards]) if (c.dismissable) remove(c.id, "dismiss");
    },
    /** Empties the stack without telling anyone: the view is going away. */
    reset() {
      for (const id of [...clocks.keys()]) stop(id);
      remaining.clear(); expanded.clear(); hovered.clear();
      cards = []; showAll = false;
      emit();
    },
    setHover(id: string, on: boolean) {
      if (!cards.some((c) => c.id === id)) return;
      if (on === hovered.has(id)) return;
      if (on) { hovered.add(id); pause(id); } else { hovered.delete(id); resume(id); }
    },
    toggleExpanded(id: string) {
      if (!cards.some((c) => c.id === id)) return;
      if (expanded.has(id)) { expanded.delete(id); resume(id); }
      else { expanded.add(id); pause(id); }
      emit();
    },
    setShowAll(on: boolean) {
      const next = on && cards.length > maxVisible;
      if (next === showAll) return;
      showAll = next;
      emit();
    },
    state(): StackState {
      if (!snapshot) {
        const visible = showAll ? cards : cards.slice(0, maxVisible);
        snapshot = { cards, visible, hidden: cards.length - visible.length, showAll, expanded: [...expanded] };
      }
      return snapshot;
    },
    subscribe(l: () => void) { listeners.add(l); return () => { listeners.delete(l); }; },
  };
}

export type Stack = ReturnType<typeof createStack>;
