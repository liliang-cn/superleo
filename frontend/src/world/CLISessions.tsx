import { useCallback, useEffect, useState, useSyncExternalStore } from "react";
import { ArrowLeftIcon, PlusIcon } from "lucide-react";
import { Conversations } from "../../wailsjs/go/app/App";
import { app } from "../../wailsjs/go/models";
import { EventsOn } from "../../wailsjs/runtime";
import { useI18n } from "../lib/i18n";
import { withoutCallNotes } from "../lib/format";
import { Thread } from "../views/ConversationsView";
import Talk from "./Talk";
import { folderOf, sessionsOf, type CodingConv } from "./codingSessions";

// What each coding agent's card shows is kept for as long as the app runs, so
// the card and its maximized form (two mounts) agree, and closing the card
// and opening it again comes back to the same place. "" is the list, "new"
// the fresh-task box, anything else a conversation's id.
const shown = new Map<string, string>();
const lists = new Map<string, CodingConv[]>();
const subs = new Set<() => void>();
const show = (agent: string, to: string) => { shown.set(agent, to); subs.forEach((f) => f()); };
const useShown = (agent: string) => useSyncExternalStore(
  (f) => { subs.add(f); return () => { subs.delete(f); }; },
  () => shown.get(agent) ?? "",
);

function ago(iso: string): string {
  const d = new Date(iso);
  if (isNaN(d.getTime())) return "";
  const now = new Date();
  if (d.toDateString() === now.toDateString()) return d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", hour12: false });
  if (d.getFullYear() === now.getFullYear()) return d.toLocaleDateString([], { month: "numeric", day: "numeric" });
  return d.toLocaleDateString();
}

/** A coding agent's card: its sessions, newest first; a row opens one to
 *  read and continue, and "New" is a fresh task. */
export default function CLISessions({ agent, placeholder }: { agent: string; placeholder: string }) {
  const { t } = useI18n();
  const to = useShown(agent);
  const [items, setItems] = useState<CodingConv[] | null>(() => lists.get(agent) ?? null);

  const load = useCallback(() => {
    Conversations(app.ConversationFilter.createFrom({ with: "coding:" + agent.split(".")[0], query: "", limit: 0 }))
      .then((l) => { const s = sessionsOf((l?.items ?? []) as CodingConv[], agent); lists.set(agent, s); setItems(s); })
      .catch(() => setItems((cur) => cur ?? []));
  }, [agent]);
  useEffect(() => {
    setItems(lists.get(agent) ?? null);
    load();
    const offs = ["chat:done", "cli:run"].map((n) => EventsOn(n, () => load()));
    const tick = window.setInterval(load, 5000);
    return () => { offs.forEach((off) => typeof off === "function" && off()); window.clearInterval(tick); };
  }, [agent, load]);

  if (to && to !== "new") {
    return (
      <div className="wt cs-open" data-testid="cli-session-open">
        <Thread key={to} id={to} onBack={() => show(agent, "")} onOpen={(id) => show(agent, id)} onChanged={load} />
      </div>
    );
  }
  const empty = items !== null && items.length === 0;
  if (to === "new" || empty) {
    return (
      <div className="wt">
        {empty ? <p className="cs-none">{t("No sessions yet")}</p> : (
          <button type="button" className="cs-back" onClick={() => show(agent, "")} data-testid="cli-sessions-back"><ArrowLeftIcon size={13} />{t("Sessions")}</button>
        )}
        <Talk who={{ kind: "cli", agent }} placeholder={placeholder} />
      </div>
    );
  }
  return (
    <div className="wt cs" data-testid="cli-sessions">
      <div className="cs-head">
        <span>{t("Sessions")}</span>
        <button type="button" onClick={() => show(agent, "new")} data-testid="cli-new"><PlusIcon size={12} />{t("New")}</button>
      </div>
      <ul className="cs-list">
        {items === null
          ? [0, 1, 2].map((i) => <li key={i} className="cs-skel" aria-hidden="true"><i /><i /></li>)
          : items.map((c) => {
              const title = withoutCallNotes(c.title).replace(/\s+/g, " ").trim() || c.agent || c.name || "";
              const detail = [folderOf(c), c.source === "terminal" ? t("Terminal") : ""].filter(Boolean).join(" · ");
              return (
                <li key={c.id}>
                  <button type="button" onClick={() => show(agent, c.id)} data-testid="cli-session-row">
                    <span className="cs-t">{title}</span>
                    {c.state === "running" ? <em className="cs-live">{t("Working…")}</em> : <em>{ago(c.at)}</em>}
                    {detail && <small>{detail}</small>}
                  </button>
                </li>
              );
            })}
      </ul>
    </div>
  );
}
