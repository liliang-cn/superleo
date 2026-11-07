import { useEffect, useRef } from "react";
import { createStack, Card, CardLevel, RemoveReason, Stack } from "./cardStack";
import { Toast, dismiss as dismissToast, onToastShown } from "./toasts";
import { firstLine } from "./format";
import { translate } from "./i18n";
import type { ScheduleRun, ScheduleRunLog } from "./useScheduleRuns";
import type { ToolApproval } from "./useToolApprovals";

/** The one stack the 3D view draws. */
export const stack: Stack = createStack({ onRemove: (card, reason) => removed(card, reason) });

// What the feed has to tell the sources when a card leaves, so the same item
// does not come back in the other layout. Set by useCardFeed while mounted.
let sinks: { run?: (key: string) => void } = {};

function removed(card: Card, reason: RemoveReason) {
  if (reason === "resolved") return;
  if (card.kind === "notice" && card.ref) dismissToast(card.ref);
  if (card.kind === "run" && card.ref) sinks.run?.(card.ref);
}

/** A notice from lib/toasts, as a card. */
export function noticeCard(t: Toast): Parameters<Stack["push"]>[0] {
  return {
    id: `n:${t.id}`,
    kind: "notice",
    level: t.level,
    title: t.title,
    preview: t.message,
    body: t.message,
    source: t.source,
    session: t.session || undefined,
    ref: t.id,
    at: t.at,
  };
}

function runLevel(run: ScheduleRun): CardLevel {
  if (run.cancelled) return "info";
  return run.error !== "" ? "error" : "success";
}

/** A finished scheduled run, as a card. */
export function runCard(run: ScheduleRun & { key: string; receivedAt: number }): Parameters<Stack["push"]>[0] {
  const title = run.cancelled
    ? translate("Scheduled run stopped")
    : run.error !== "" ? translate("Scheduled run failed") : translate("Scheduled run finished");
  const body = run.error !== "" ? run.error : run.answer;
  return {
    id: `r:${run.key}`,
    kind: "run",
    level: runLevel(run),
    title,
    preview: firstLine(body, 160),
    body,
    source: firstLine(run.prompt, 80) || undefined,
    session: run.session || undefined,
    ref: run.key,
    at: run.receivedAt,
    // A success has been read by being seen; it retires on its own, but a
    // report is longer than a notice, so it gets longer.
    ttl: run.error !== "" || run.cancelled ? 0 : 14000,
  };
}

/** A tool call waiting on a person, as a card. */
export function approvalCard(a: ToolApproval): Parameters<Stack["push"]>[0] {
  const args = Object.keys(a.args || {}).length > 0 ? JSON.stringify(a.args, null, 2) : "";
  const text = a.command || args;
  return {
    id: `a:${a.id}`,
    kind: "approval",
    level: "warn",
    title: a.tool,
    preview: firstLine(a.command || args.replace(/\s+/g, " "), 160),
    body: text ? "```\n" + text + "\n```" : "",
    source: a.by || undefined,
    session: a.session || undefined,
    ref: a.id,
  };
}

/**
 * Feeds the stack from everything that pops up as a toast: notices, scheduled
 * runs and tool approvals. Mounted only while the 3D view is up, so items that
 * arrive in the other layouts are drawn there and never twice.
 */
export function useCardFeed(runs: ScheduleRunLog, approvals: ToolApproval[], note: string, dismissNote: () => void) {
  useEffect(() => onToastShown((t) => { stack.push(noticeCard(t)); }), []);

  // Runs: whatever was already in the log when the view came up has been
  // reported by the other layout; only newer ones are pushed.
  const seenRuns = useRef<Set<string> | null>(null);
  if (seenRuns.current === null) seenRuns.current = new Set(runs.runs.map((r) => r.key));
  sinks = { run: runs.dismiss };
  useEffect(() => {
    for (const run of [...runs.runs].reverse()) {
      if (seenRuns.current!.has(run.key)) continue;
      seenRuns.current!.add(run.key);
      if (!run.dismissed) stack.push(runCard(run));
    }
  }, [runs.runs]);
  useEffect(() => () => { sinks = {}; stack.reset(); }, []);

  // Approvals: what is pending is on the stack, and what stops being pending
  // (answered here or elsewhere, timed out) leaves it.
  const pendingIds = approvals.map((a) => a.id).join("\n");
  useEffect(() => {
    const live = new Set(approvals.map((a) => `a:${a.id}`));
    for (const a of approvals) {
      const id = `a:${a.id}`;
      if (!stack.state().cards.some((c) => c.id === id)) stack.push(approvalCard(a));
    }
    for (const c of stack.state().cards) if (c.kind === "approval" && !live.has(c.id)) stack.resolve(c.id);
  }, [pendingIds]); // eslint-disable-line react-hooks/exhaustive-deps

  // A refused answer ("no longer waiting"): the note belongs to whoever is
  // drawing approvals, which here is the stack, as an error that stays.
  useEffect(() => {
    if (note === "") return;
    stack.push({ id: "a-note", kind: "notice", level: "error", title: "", preview: note, body: note });
    dismissNote();
  }, [note]); // eslint-disable-line react-hooks/exhaustive-deps
}
