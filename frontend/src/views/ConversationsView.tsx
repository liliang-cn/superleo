import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { ArrowLeftIcon, BotIcon, CpuIcon, HexagonIcon, PlusIcon, SearchIcon, SendIcon, TerminalIcon, Trash2Icon, XIcon, type LucideIcon } from "lucide-react";
import {
  AskIn, ConversationThread, Conversations, DeleteChatSession, FollowUpCLIRun, ReplyToCodingSession, SendChat, StartCLIRun,
} from "../../wailsjs/go/app/App";
import { app, backend } from "../../wailsjs/go/models";
import { EventsOn } from "../../wailsjs/runtime";
import { Response } from "@/components/ai-elements/response";
import { SentMessage } from "../components/SentFiles";
import { useI18n } from "../lib/i18n";
import { useImeGuard } from "../lib/ime";
import { visibleAnswer, withoutCallNotes } from "../lib/format";
import { queenTitle, useQueenName } from "../lib/queenName";
import { isLegacyConsole, legacyConsoleTitle, pairedDeviceNames, queenSession, useQueenSession } from "../lib/queenConsole";
import { useStickToBottom } from "../lib/useStickToBottom";
import "./conversations.css";

type Conv = app.Conversation;

const KIND: Record<string, { Icon: LucideIcon; label: string }> = {
  queen: { Icon: HexagonIcon, label: "Queen" },
  worker: { Icon: CpuIcon, label: "Worker" },
  agent: { Icon: BotIcon, label: "Agent" },
  coding: { Icon: TerminalIcon, label: "Coding" },
};

/** The time of the last word: the clock today, the day before that. */
function when(iso: string): string {
  const d = new Date(iso);
  if (isNaN(d.getTime())) return "";
  const now = new Date();
  if (d.toDateString() === now.toDateString()) return d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", hour12: false });
  if (d.getFullYear() === now.getFullYear()) return d.toLocaleDateString([], { month: "numeric", day: "numeric" });
  return d.toLocaleDateString();
}

const bare = (m?: string) => (m ?? "").replace(/^https?:\/\//, "").replace(/\/+$/, "");

/** Who a row is with, and where. */
function useWho() {
  const { t } = useI18n();
  const q = useQueenName();
  return useMemo(() => ({
    name: (c: { kind: string; name?: string }) => (c.kind === "queen" ? queenTitle(q) : c.name || t(KIND[c.kind]?.label ?? "Agent")),
    kind: (c: { kind: string; source?: string }) => t(c.source === "terminal" ? "Terminal" : KIND[c.kind]?.label ?? "Agent"),
  }), [q, t]);
}

export default function ConversationsView() {
  const { t } = useI18n();
  const who = useWho();
  const [params, setParams] = useSearchParams();
  const withKey = params.get("with") ?? "";
  const openId = params.get("open") ?? "";
  const [query, setQuery] = useState("");
  const [needle, setNeedle] = useState("");
  const [data, setData] = useState<app.ConversationList | null>(null);
  const [failed, setFailed] = useState("");
  const consoleId = useQueenSession();

  useEffect(() => {
    const h = window.setTimeout(() => setNeedle(query.trim()), 250);
    return () => window.clearTimeout(h);
  }, [query]);

  const load = useCallback(() => {
    Conversations(app.ConversationFilter.createFrom({ with: withKey, query: needle, limit: 0 }))
      .then(async (l) => {
        const devices = (l?.items ?? []).some((c) => c.kind === "queen" && isLegacyConsole(c.session)) ? await pairedDeviceNames() : [];
        for (const c of l?.items ?? []) {
          if (c.kind === "queen" && isLegacyConsole(c.session)) c.title = legacyConsoleTitle(c.session, devices);
        }
        setData(l);
        setFailed("");
      })
      .catch((e) => setFailed(String(e?.message ?? e)));
  }, [withKey, needle]);
  useEffect(() => { load(); }, [load]);
  useEffect(() => {
    const offs = ["chat:done", "chat:error", "cli:run"].map((n) => EventsOn(n, () => load()));
    const tick = window.setInterval(load, 15000);
    window.addEventListener("focus", load);
    return () => {
      offs.forEach((off) => typeof off === "function" && off());
      window.clearInterval(tick);
      window.removeEventListener("focus", load);
    };
  }, [load]);

  const set = (patch: Record<string, string>) => {
    const next = new URLSearchParams(params);
    for (const [k, v] of Object.entries(patch)) v ? next.set(k, v) : next.delete(k);
    setParams(next, { replace: true });
  };

  const items = data?.items ?? [];
  const parties = data?.parties ?? [];
  const total = parties.reduce((n, p) => n + p.count, 0);

  return (
    <div className={`cv2${openId ? " has-open" : ""}`} data-testid="conversations">
      <aside className="cv2-list">
        <header className="cv2-head">
          <h1>{t("Conversations")}</h1>
          <label className="cv2-search">
            <SearchIcon size={14} />
            <input value={query} onChange={(e) => setQuery(e.target.value)} placeholder={t("Search")} aria-label={t("Search")} data-testid="conv-search" />
            {query && <button type="button" title={t("Clear")} aria-label={t("Clear")} onClick={() => setQuery("")}><XIcon size={13} /></button>}
          </label>
          <div className="cv2-who" role="tablist" aria-label={t("Who")}>
            <button role="tab" aria-selected={!withKey} className={!withKey ? "on" : ""} onClick={() => set({ with: "" })} data-testid="who-all">
              {t("Everyone")} <i>{total}</i>
            </button>
            {parties.map((p) => {
              const Icon = KIND[p.kind]?.Icon ?? BotIcon;
              return (
                <button key={p.with} role="tab" aria-selected={withKey === p.with} className={withKey === p.with ? "on" : ""}
                  onClick={() => set({ with: withKey === p.with ? "" : p.with })} title={bare(p.machine)} data-testid="who-chip">
                  <Icon size={13} /> {who.name(p)} <i>{p.count}</i>
                </button>
              );
            })}
          </div>
        </header>
        <div className="cv2-rows">
          {failed && <div className="cv2-empty" role="alert">{failed}</div>}
          {!failed && data && items.length === 0 && <div className="cv2-empty">{t("No conversations")}</div>}
          {items.map((c) => {
            const Icon = KIND[c.kind]?.Icon ?? BotIcon;
            const main = c.kind === "queen" && c.session === consoleId;
            return (
              <button key={c.id} className={`cv2-row k-${c.kind}${openId === c.id ? " on" : ""}`} onClick={() => set({ open: c.id })} data-testid="conv-row">
                <span className="cv2-ico"><Icon size={15} /></span>
                <span className="cv2-main">
                  <span className="cv2-top">
                    <b>{who.name(c)}</b>
                    {c.machine && <i>{bare(c.machine)}</i>}
                    <span className="cv2-kind">{who.kind(c)}</span>
                    <em>{c.state === "running" ? <span className="cv2-live" title={t("Working…")} /> : when(c.at)}</em>
                  </span>
                  <span className="cv2-title">{main ? t("Main conversation") : withoutCallNotes(c.title) || t("Untitled")}</span>
                  {c.last && <span className="cv2-last">{withoutCallNotes(c.last)}</span>}
                </span>
              </button>
            );
          })}
        </div>
      </aside>
      <section className="cv2-thread">
        {openId ? (
          <Thread key={openId} id={openId} onBack={() => set({ open: "" })} onOpen={(id) => set({ open: id })} onChanged={load} />
        ) : (
          <div className="cv2-none">{t("Pick a conversation")}</div>
        )}
      </section>
    </div>
  );
}

export function Thread({ id, onBack, onOpen, onChanged }: { id: string; onBack: () => void; onOpen: (id: string) => void; onChanged: () => void }) {
  const { t } = useI18n();
  const who = useWho();
  const ime = useImeGuard();
  const [th, setTh] = useState<app.ConversationThread | null>(null);
  const [error, setError] = useState("");
  const [text, setText] = useState("");
  // What was just said, until the saved thread has it.
  const [pending, setPending] = useState("");
  const [busy, setBusy] = useState(false);
  const [streamed, setStreamed] = useState("");
  const [fresh, setFresh] = useState(false);
  const [confirm, setConfirm] = useState(false);
  const live = useRef("");
  const raw = useRef("");
  const stick = useStickToBottom<HTMLDivElement>();
  const c = th?.conversation;

  const load = useCallback(() => ConversationThread(id).then((x) => { setTh(x); setError(""); return x; })
    .catch((e) => { setError(String(e?.message ?? e)); return null; }), [id]);
  useEffect(() => { void load(); }, [load]);

  const settle = useCallback(() => {
    live.current = ""; raw.current = ""; setStreamed(""); setPending(""); setBusy(false);
  }, []);

  // The queen answers on the chat events of the request; the words stream in.
  useEffect(() => {
    const offs = [
      EventsOn("chat:event", (p: any) => {
        if (!live.current || p?.requestId !== live.current) return;
        if (p.type === "partial" && p.content) { raw.current += p.content; setStreamed(visibleAnswer(raw.current)); }
        else if (p.type === "tombstone" && p.debugType === "retry") { raw.current = ""; setStreamed(""); }
      }),
      EventsOn("chat:done", (p: any) => { if (live.current && p?.requestId === live.current) load().then(settle); else if (c?.kind === "queen") void load(); }),
      EventsOn("chat:error", (p: any) => { if (live.current && p?.requestId === live.current) { setError(String(p.error ?? "")); settle(); } }),
      EventsOn("chat:cancelled", (p: any) => { if (live.current && p?.requestId === live.current) settle(); }),
    ];
    return () => offs.forEach((f) => typeof f === "function" && f());
  }, [load, settle, c?.kind]);

  // A coding run or a terminal session answers on its own time: read the
  // thread again until it stops working.
  const waiting = busy && (c?.kind === "coding");
  const running = c?.state === "running";
  useEffect(() => {
    if (!waiting && !running) return;
    let polls = 0;
    const h = window.setInterval(async () => {
      polls++;
      const x = await load();
      const done = x && x.conversation.state !== "running";
      // A terminal reply shows in its log when the agent writes; give it a minute.
      if (done && (x.conversation.source !== "terminal" || polls >= 20)) { settle(); onChanged(); }
      else if (polls >= 80) settle();
    }, 3000);
    return () => window.clearInterval(h);
  }, [waiting, running, load, settle, onChanged]);

  const send = async () => {
    const s = text.trim();
    if (!s || busy || !c) return;
    setText("");
    setPending(s);
    setBusy(true);
    setError("");
    try {
      if (fresh || (c.kind === "coding" && !c.replyable && c.source !== "terminal")) {
        const agent = (c.source === "terminal" && c.machine ? `${c.agent}.${c.machine}` : c.agent) ?? "";
        const run = await StartCLIRun(agent, s, c.cwd ?? "", "", true);
        settle();
        onChanged();
        onOpen("cli:" + run.thread);
        return;
      }
      switch (c.kind) {
        case "queen":
          raw.current = ""; setStreamed("");
          live.current = await SendChat(c.session, s, []);
          return; // the chat events finish it
        case "worker": case "agent": {
          const r = await AskIn(c.session, c.kind, c.name ?? "", s);
          await load();
          if (r.failed) setError(r.text);
          break;
        }
        case "coding":
          if (c.source === "terminal") await ReplyToCodingSession(c.machine ?? "", c.agent ?? c.name ?? "", c.session, s);
          else await FollowUpCLIRun(c.run!, s);
          await load();
          return; // polled until it is done
      }
      settle();
      onChanged();
    } catch (e: any) {
      setError(String(e?.message ?? e));
      settle();
    }
  };

  const del = async () => {
    if (!confirm) { setConfirm(true); window.setTimeout(() => setConfirm(false), 4000); return; }
    await DeleteChatSession(c!.session).catch((e) => setError(String(e?.message ?? e)));
    onChanged();
    onBack();
  };

  if (!th) return <div className="cv2-none">{error || t("Loading…")}</div>;
  const turns = (th.turns ?? []).filter((m: backend.ChatTurn) => m.kind !== "context");
  const canReply = !!c && (c.replyable || fresh) ;
  const startNew = !!c && c.kind === "coding" && !c.replyable;
  const canDelete = c?.kind === "queen" && c.session !== queenSession();
  return (
    <div className="cv2-pane" data-testid="conv-thread">
      <header className="cv2-phead">
        <button className="cv2-back" onClick={onBack} title={t("Back")} aria-label={t("Back")}><ArrowLeftIcon size={17} /></button>
        <div>
          <b>{who.name(c!)}{c!.machine ? <i> · {bare(c!.machine)}</i> : null}</b>
          <small>{who.kind(c!)}{c!.title && c!.session !== queenSession() ? ` · ${withoutCallNotes(c!.title)}` : ""}</small>
        </div>
        {canDelete && (
          <button className={`cv2-del${confirm ? " arm" : ""}`} onClick={del} title={confirm ? t("Delete this conversation?") : t("Delete")} aria-label={t("Delete")}>
            <Trash2Icon size={15} />{confirm && <span>{t("Delete")}</span>}
          </button>
        )}
      </header>
      <div className="cv2-msgs" ref={stick.ref} onScroll={stick.onScroll}>
        {turns.length === 0 && !pending && <div className="cv2-empty">{t("No messages")}</div>}
        {turns.map((m: backend.ChatTurn, i: number) => m.role === "user" ? (
          <div key={i} className="cv2-u"><SentMessage content={withoutCallNotes(m.content)} />{m.at && <small>{when(m.at)}</small>}</div>
        ) : (
          <div key={i} className={`cv2-a${m.kind === "error" ? " bad" : ""}`}>
            {m.steps && m.steps.length > 0 && (
              <details className="cv2-steps"><summary>{t("Steps")} · {m.steps.length}</summary>{m.steps.map((s, n) => <div key={n}>{s}</div>)}</details>
            )}
            {m.kind === "error" ? <span>{m.content}</span> : <Response>{withoutCallNotes(m.content)}</Response>}
            {m.at && <small>{when(m.at)}</small>}
          </div>
        ))}
        {pending && !turns.slice(-2).some((m: backend.ChatTurn) => m.role === "user" && m.content.trim() === pending) && (
          <div className="cv2-u"><SentMessage content={pending} /></div>
        )}
        {streamed && <div className="cv2-a"><Response>{streamed}</Response></div>}
        {(busy || running) && !streamed && <div className="cv2-a cv2-busy">{c!.kind === "coding" ? t("Working…") : t("Thinking…")}</div>}
        {error && <div className="cv2-a bad" role="alert">{error}</div>}
      </div>
      {startNew && !fresh && (
        <div className="cv2-readonly">
          <span>{c!.why || t("Read only")}</span>
          <button onClick={() => setFresh(true)} data-testid="conv-start-new"><PlusIcon size={14} /> {t("Start new")}</button>
        </div>
      )}
      {(canReply || (startNew && fresh)) && (
        <form className="cv2-say" onSubmit={(e) => { e.preventDefault(); void send(); }}>
          <input value={text} onChange={(e) => setText(e.target.value)} data-testid="conv-input" autoFocus
            placeholder={fresh ? t("New conversation with {name}", { name: who.name(c!) }) : t("Message {name}", { name: who.name(c!) })}
            onCompositionStart={ime.handlers.onCompositionStart} onCompositionEnd={ime.handlers.onCompositionEnd}
            onKeyDown={(e) => { if (e.key === "Enter" && ime.composing(e)) e.preventDefault(); }} />
          <button type="submit" disabled={!text.trim() || busy} title={t("Send")} aria-label={t("Send")} data-testid="conv-send"><SendIcon size={15} /></button>
        </form>
      )}
    </div>
  );
}
