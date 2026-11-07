import { useEffect, useRef, useState, useSyncExternalStore } from "react";
import {
  BellIcon, CheckIcon, ChevronDownIcon, ChevronUpIcon, ClockIcon, InfoIcon, MessageSquareIcon,
  ShieldAlertIcon, TriangleAlertIcon, XCircleIcon, XIcon,
} from "lucide-react";
import { Response } from "@/components/ai-elements/response";
import { Card } from "../lib/cardStack";
import { stack, useCardFeed } from "../lib/cardFeed";
import { useI18n } from "../lib/i18n";
import type { ScheduleRunLog } from "../lib/useScheduleRuns";
import type { ToolApproval } from "../lib/useToolApprovals";
import "./cardstack.css";

/**
 * What pops up in the 3D view: notices, scheduled-run results and tool
 * approvals in one stack, top right. The empty space around it belongs to the
 * world; only the cards take the pointer.
 */

const LEAVE_MS = 180;

function iconOf(card: Card) {
  if (card.kind === "approval") return ShieldAlertIcon;
  if (card.kind === "run") return card.level === "error" ? XCircleIcon : card.level === "success" ? CheckIcon : ClockIcon;
  switch (card.level) {
    case "success": return CheckIcon;
    case "warn": return TriangleAlertIcon;
    case "error": return XCircleIcon;
    default: return InfoIcon;
  }
}

const clock = (at: number) => new Date(at).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", hour12: false });

/** Worth a chevron: the body says more than the two lines the preview shows. */
const expandable = (c: Card) => c.body.trim() !== "" && (c.body.trim() !== c.preview.trim() || c.preview.length > 90 || c.kind === "approval");

function useStack() {
  return useSyncExternalStore(stack.subscribe, stack.state, stack.state);
}

/** Keeps a card on screen for the length of its exit animation. */
function useLeaving(visible: Card[]) {
  const [leaving, setLeaving] = useState<{ card: Card; index: number }[]>([]);
  const prev = useRef<Card[]>([]);
  useEffect(() => {
    const reduced = window.matchMedia?.("(prefers-reduced-motion: reduce)").matches;
    const gone = prev.current.map((card, index) => ({ card, index })).filter(({ card }) => !visible.some((c) => c.id === card.id));
    prev.current = visible;
    // A card folded into "+N" is still alive; only a removed one animates out.
    const dead = gone.filter(({ card }) => !stack.state().cards.some((c) => c.id === card.id));
    if (dead.length === 0 || reduced) return;
    setLeaving((l) => [...l, ...dead]);
    // No cleanup: the next change of the stack must not cancel this card's
    // exit, or it would stay behind as an invisible ghost.
    window.setTimeout(() => setLeaving((l) => l.filter((x) => !dead.includes(x))), LEAVE_MS);
  }, [visible]);
  return leaving;
}

function CardRow({ card, open, onOpenConversation, onResolve, leaving }: {
  card: Card;
  open: boolean;
  onOpenConversation: (session: string) => void;
  onResolve: (id: string, allow: boolean) => void;
  leaving?: boolean;
}) {
  const { t } = useI18n();
  const Icon = iconOf(card);
  const more = expandable(card);
  return (
    <div
      className={`ns-card ns-${card.kind} ns-${card.level}${open ? " open" : ""}${leaving ? " leaving" : ""}`}
      role={card.level === "error" || card.kind === "approval" ? "alert" : "status"}
      data-testid="stack-card"
      data-kind={card.kind}
      onMouseEnter={() => stack.setHover(card.id, true)}
      onMouseLeave={() => stack.setHover(card.id, false)}
    >
      <div className="ns-head">
        <Icon size={15} className="ns-icon" />
        <div className="ns-text">
          {card.title !== "" && <div className="ns-title">{card.title}</div>}
          {card.source && <div className="ns-source">{card.source}</div>}
        </div>
        <time className="ns-time">{clock(card.at)}</time>
        {more && (
          <button type="button" className="ns-btn" aria-expanded={open} aria-label={open ? t("Collapse") : t("Expand")} title={open ? t("Collapse") : t("Expand")} onClick={() => stack.toggleExpanded(card.id)}>
            {open ? <ChevronUpIcon size={14} /> : <ChevronDownIcon size={14} />}
          </button>
        )}
        {card.dismissable && (
          <button type="button" className="ns-btn" aria-label={t("Dismiss")} title={t("Dismiss")} onClick={() => stack.dismiss(card.id)}>
            <XIcon size={14} />
          </button>
        )}
      </div>
      {open ? (
        <div className="ns-body"><Response>{card.body}</Response></div>
      ) : (
        card.preview !== "" && <div className="ns-preview">{card.preview}</div>
      )}
      {(card.kind === "approval" || card.session) && (
        <div className="ns-actions">
          {card.kind === "approval" ? (
            <>
              <button type="button" className="ns-act pri" onClick={() => onResolve(card.ref!, true)}>{t("Allow once")}</button>
              <button type="button" className="ns-act" onClick={() => onResolve(card.ref!, false)}>{t("Deny")}</button>
            </>
          ) : (
            <button type="button" className="ns-act" onClick={() => { onOpenConversation(card.session!); stack.dismiss(card.id); }}>
              <MessageSquareIcon size={13} />{t("Open conversation")}
            </button>
          )}
        </div>
      )}
    </div>
  );
}

export default function CardStack({ runs, approvals, note, onDismissNote, onResolve, onOpenConversation }: {
  runs: ScheduleRunLog;
  approvals: ToolApproval[];
  note: string;
  onDismissNote: () => void;
  onResolve: (id: string, allow: boolean) => void;
  onOpenConversation: (session: string) => void;
}) {
  const { t } = useI18n();
  useCardFeed(runs, approvals, note, onDismissNote);
  const state = useStack();
  const leaving = useLeaving(state.visible);
  if (state.cards.length === 0 && leaving.length === 0) return null;

  // Cards that just left are put back at the place they had, for their exit.
  const rows: { card: Card; leaving: boolean }[] = state.visible.map((card) => ({ card, leaving: false }));
  for (const l of leaving) rows.splice(Math.min(l.index, rows.length), 0, { card: l.card, leaving: true });

  const dismissable = state.cards.filter((c) => c.dismissable).length;
  return (
    <div className={`ns-stack${state.showAll ? " all" : ""}`} aria-label={t("Notifications")} data-testid="card-stack">
      {(state.cards.length >= 2 && dismissable > 0 || state.showAll) && (
        <div className="ns-bar">
          {state.showAll && (
            <button type="button" className="ns-chip" onClick={() => stack.setShowAll(false)}><ChevronUpIcon size={12} />{t("Collapse")}</button>
          )}
          {state.cards.length >= 2 && dismissable > 0 && (
            <button type="button" className="ns-chip" onClick={() => stack.dismissAll()} data-testid="stack-dismiss-all"><XIcon size={12} />{t("Dismiss all")}</button>
          )}
        </div>
      )}
      {rows.map(({ card, leaving: gone }) => (
        <CardRow
          key={card.id}
          card={card}
          leaving={gone}
          open={state.expanded.includes(card.id)}
          onOpenConversation={onOpenConversation}
          onResolve={onResolve}
        />
      ))}
      {state.hidden > 0 && (
        <button type="button" className="ns-more" onClick={() => stack.setShowAll(true)} data-testid="stack-more">
          <BellIcon size={13} />{t("{count} more", { count: state.hidden })}
        </button>
      )}
    </div>
  );
}
