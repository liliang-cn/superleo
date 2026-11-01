import { type CSSProperties, useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import {
  ArrowUpIcon, HexagonIcon, 
  PlusIcon, SquareIcon, SquarePenIcon, CalendarIcon, AlarmClockIcon, TerminalIcon, TriangleAlertIcon, XIcon, ChevronRightIcon,
  LayoutDashboardIcon, ActivityIcon, GaugeIcon, ScrollTextIcon, PaperclipIcon, PanelLeftCloseIcon, PanelLeftOpenIcon, HistoryIcon, MessagesSquareIcon,
} from "lucide-react";
import RunTracePanel from "../components/RunTracePanel";
import TraceChip from "../components/TraceChip";
import { useI18n } from "../lib/i18n";
import { getQueenName, queenNamed, queenRef, queenTitle, useQueenName } from "../lib/queenName";
import { useChat } from "../lib/useChat";
import { useAttachments } from "../lib/useAttachments";
import AttachmentChips from "../components/AttachmentChips";
import { usePulse } from "../components/Reactor";
import LiveTurn from "./LiveTurn";
import { ChatTurnBrowser, newestBrowserTurn } from "../components/BrowserCard";
import EventSheet from "../components/EventSheet";
import { AgentInfo, useAgentMentions } from "../lib/useAgentMentions";
import { AgentMenu } from "../components/AgentMenu";
import { attentionTitle, AttentionItem } from "../canvas/attention";
import { attentionPath } from "../lib/routes";
import type { ToolApproval } from "../lib/useToolApprovals";
import { PATHS } from "../lib/routes";
import { withoutCallNotes } from "../lib/format";
import { Response } from "@/components/ai-elements/response";
import { Bee, elapsed, short, useBees, useLinkedAgents, useCodingRuns, useHive, useUpcoming, useWorkers } from "../canvas/data";
import type { CodingRun, UpcomingItem } from "../canvas/tiles";
import NameDashboardModal from "../components/NameDashboardModal";
import { dashboards, hasRenderableBlock, suggestName, Dashboard } from "../lib/dashboards";
import {
  CancelAllChats, CancelCLIRun, ChatSessions, HiveLinkStatus, TakeOver,
} from "../../wailsjs/go/app/App";
import { EventsOn } from "../../wailsjs/runtime";
import HiveBanner, { useHiveCutOff } from "../components/HiveBanner";
import EarlierChatsPane, { useEarlierCount } from "./EarlierChats";
import { useMembership } from "../lib/membership";
import LiveHive from "./LiveHive";
import LiveRun, { useRun } from "./LiveRun";
import LiveStatus from "./LiveStatus";
import StatsBlock from "../components/StatsBlock";
import { takeCompose } from "../lib/compose";
import { useNotedUpcoming } from "../lib/useNotedUpcoming";
import { notifyOf, ringSummary, upcomingWhen, type ScheduleItem } from "../lib/schedule";
import { ModeChip } from "../components/ModeChip";
import { isPersonalConversation, loadQueenSession, queenSession, useQueenSession, withConsoleTitles } from "../lib/queenConsole";
import { useDeskPane, useDeskTheme, useDeskWorld } from "./DeskShell";
import { machine } from "../lib/machine";
import { SentMessage } from "../components/SentFiles";

type TabKey = string; // "hive" | "run:<id>" | "dash:<id>"

const served = Boolean((window as unknown as Record<string, unknown>).superleoServed);

function useNow(ms = 1000) {
  const [now, setNow] = useState(() => new Date());
  useEffect(() => {
    const t = window.setInterval(() => setNow(new Date()), ms);
    return () => window.clearInterval(t);
  }, [ms]);
  return now;
}

interface Session { id: string; title: string; updated_at: string }

/** The conversations, newest first, without the ones the screens keep for
 *  themselves: the queen's own, the tiles', and the bees' wake-ups (whose
 *  first message is the standing-agent preamble, not something you said).
 *  The old per-device consoles (hive-console-<device>) are kept, titled
 *  after their device; the shared one has its own row. */
/** "Tue 15:00", "Today 09:00": when a coming-up item is, in one short line. */
function shortWhen(at: string | undefined, now: Date | number, t: (k: string) => string): string {
  if (!at) return "";
  const d = new Date(at);
  const start = new Date(now); start.setHours(0, 0, 0, 0);
  const days = Math.floor((d.getTime() - start.getTime()) / 86400000);
  const day = days === 0 ? t("Today") : days === 1 ? t("Tomorrow") : d.toLocaleDateString(undefined, { weekday: "short" });
  return `${day} ${d.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit", hour12: false })}`;
}

/** The conversations on the core, newest first. Read again after this window
 *  sends, whenever any conversation finishes or starts elsewhere (the phone,
 *  the web, a job), when the window comes back, and every fifteen seconds. */
function useRecents(sending: boolean): Session[] {
  const [list, setList] = useState<Session[]>([]);
  const load = useCallback(() => {
    ChatSessions()
      .then((l) => withConsoleTitles(((l ?? []) as unknown as Session[])
        .filter((s) => isPersonalConversation(s, queenSession()))
        .map((s) => ({ ...s, title: withoutCallNotes(s.title) }))
        .sort((a, b) => b.updated_at.localeCompare(a.updated_at))
        .slice(0, 9)))
      .then(setList)
      .catch(() => {});
  }, []);
  useEffect(() => {
    if (!sending) load();
  }, [sending, load]);
  useEffect(() => {
    const offs = ["chat:done", "chat:error", "chat:cancelled"].map((n) => EventsOn(n, () => load()));
    const t = window.setInterval(load, 15000);
    window.addEventListener("focus", load);
    return () => {
      offs.forEach((off) => { if (typeof off === "function") off(); });
      window.clearInterval(t);
      window.removeEventListener("focus", load);
    };
  }, [load]);
  return list;
}

function ago(iso: string, now: Date): string {
  const s = Math.max(0, (now.getTime() - new Date(iso).getTime()) / 1000);
  if (s < 90) return "now";
  if (s < 3600) return `${Math.round(s / 60)}m`;
  if (s < 86400) return `${Math.round(s / 3600)}h`;
  return new Date(iso).toLocaleDateString([], { weekday: "short" });
}

function nextLabel(items: UpcomingItem[]): string {
  const n = items.find((i) => i.enabled && i.next);
  if (!n?.next) return "";
  const d = new Date(n.next);
  const t = d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", hour12: false });
  return d.toDateString() === new Date().toDateString() ? t : `${d.toLocaleDateString([], { weekday: "short" })} ${t}`;
}

/** The tabs the right pane always has. */
const FIXED: TabKey[] = ["hive", "status", "stats"];
/** Which of the fixed tabs was last in front, so a home left on its figures
 *  opens on them. */
const TAB_KEY = "superleo-desk-live-tab";

const PIN_COLORS = ["dk-a-later", "dk-a-hive", "dk-a-code", "dk-a-bees", "dk-a-know", "dk-a-stats", "dk-a-rec"];

/**
 * The desktop home. Left, where things are; middle, the conversation with the
 * queen, with whatever needs you in it; right, what the hive is doing, live —
 * the comb, or a coding agent's session as it types — with a way to take
 * over; along the bottom, what you pinned.
 */
export default function DeskView({ approvals, openSession, onSessionOpened, attentionItems, attentionError }: {
  attentionItems: AttentionItem[];
  attentionError?: string;
  approvals: { pending: ToolApproval[]; resolve: (id: string, allow: boolean) => void };
  /** A conversation to open on arrival — a notification's, a scheduled run's. */
  openSession?: string;
  onSessionOpened?: () => void;
}) {
  const { t } = useI18n();
  const queen = useQueenName();
  const navigate = useNavigate();
  // Kept the way it was left, by the shell, whose toolbar holds its toggle.
  const { folded, fold, setBusy } = useDeskPane();
  const layoutRef = useRef<HTMLDivElement>(null);
  const [sideWidth, setSideWidth] = useState(() => {
    try { const n = Number(localStorage.getItem("superleo-desk-side-width")); return Number.isFinite(n) && n >= 200 ? Math.min(480, n) : 252; } catch { return 252; }
  });
  const [resizing, setResizing] = useState(false);
  const clampWidth = (n: number) => Math.round(Math.max(200, Math.min(480, (layoutRef.current?.clientWidth ?? 1200) - (folded || window.matchMedia("(max-width: 1240px)").matches ? 418 : 760), n)));
  const resizeSide = (n: number) => setSideWidth(clampWidth(n));
  useEffect(() => { try { localStorage.setItem("superleo-desk-side-width", String(sideWidth)); } catch { /* private window */ } }, [sideWidth]);
  // The conversation list folds away to give the conversation the room; kept per device.
  const [sideFolded, setSideFolded] = useState(() => { try { return localStorage.getItem("superleo-desk-side-folded") === "1"; } catch { return false; } });
  const foldSide = (on: boolean) => { setSideFolded(on); try { localStorage.setItem("superleo-desk-side-folded", on ? "1" : "0"); } catch { /* private window */ } };
  useEffect(() => {
    const fit = () => setSideWidth((n) => clampWidth(n));
    window.addEventListener("resize", fit);
    fit();
    return () => window.removeEventListener("resize", fit);
  }, [folded]);
  const now = useNow();
  const hive = useHive();
  const bees = useBees();
  const upcoming = useUpcoming();
  const next = useNotedUpcoming(5);
  const runs = useCodingRuns();
  const linked = useLinkedAgents();
  const chat = useChat();
  const workerList = useWorkers();
  const [traceOpen, setTraceOpen] = useState(false);
  const queenId = useQueenSession();
  const pulse = usePulse();
  const recents = useRecents(chat.sending);
  const cutOff = useHiveCutOff();
  const { m: membership } = useMembership();
  const earlierCount = useEarlierCount(!!membership?.joined);
  const [earlier, setEarlier] = useState(false);
  const attention = attentionItems;
  const [draft, setDraft] = useState("");
  const composerRef = useRef<HTMLTextAreaElement>(null);
  // @ offers the agents the core knows (hermes, pi, claude.mac, …) and the
  // hive's workers, found by their short names too; a worker's message goes
  // to the queen, who hands it on.
  const workerNames = useMemo<AgentInfo[]>(() => hive.members
    .filter((m) => !/queen/.test(m.name))
    .map((m) => {
      const t = hive.tasks.find((x) => x.worker === m.name && x.state === "running");
      return { name: m.name, alias: short(m.name), about: `Hive worker ${short(m.name)}${m.state === "lost" ? ", not answering" : t ? ", busy" : ", idle"}` };
    }), [hive]);
  const mentions = useAgentMentions(draft, setDraft, workerNames);
  const { theme } = useDeskTheme();
  const world = useDeskWorld();
  const [naming, setNaming] = useState<{content:string; prompt:string} | null>(null);
  const [saveError, setSaveError] = useState("");
  const [pins, setPins] = useState<Dashboard[]>([]);
  const [onQueen, setOnQueen] = useState(true);
  const [opened, setOpened] = useState<TabKey[]>([]);
  const [closed, setClosed] = useState<Set<TabKey>>(new Set());
  const [active, setActiveState] = useState<TabKey>(() => {
    try { const v = localStorage.getItem(TAB_KEY); if (v && FIXED.includes(v)) return v; } catch { /* fine */ }
    return "hive";
  });
  const setActive = (k: TabKey) => {
    setActiveState(k);
    if (FIXED.includes(k)) try { localStorage.setItem(TAB_KEY, k); } catch { /* fine */ }
  };
  const needsRef = useRef<HTMLDivElement>(null);
  const endRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!openSession) loadQueenSession().then((id) => chat.loadSession(id)).catch(() => {});
    dashboards.list().then((l) => setPins(l ?? [])).catch(() => {});
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  useEffect(() => {
    if (!openSession) return;
    setOnQueen(openSession === queenSession());
    chat.loadSession(openSession).catch(() => {});
    onSessionOpened?.();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [openSession]);

  useEffect(() => {
    const load = () => dashboards.list().then(list => setPins(list ?? [])).catch(() => {});
    const offs = ["dashboard:updated", "dashboard:refreshing"].map(name => EventsOn(name, load));
    return () => offs.forEach(off => typeof off === "function" && off());
  }, []);

  const msgs = chat.messages.filter((m) => m.kind !== "context");
  const newestBrowser = newestBrowserTurn(msgs, chat.trace);
  const lastText = msgs.length ? msgs[msgs.length - 1].content : "";
  // Follow the conversation while the person is at the bottom of it: a tool
  // call, a thought or a line of answer landing keeps the newest in view. If
  // they have scrolled up to read, it leaves them there; a new message brings
  // them back down.
  const msgsRef = useRef<HTMLDivElement>(null);
  const atBottom = useRef(true);
  useEffect(() => {
    const el = msgsRef.current;
    if (!el) return;
    const onScroll = () => { atBottom.current = el.scrollHeight - el.scrollTop - el.clientHeight < 80; };
    const follow = new MutationObserver(() => { if (atBottom.current) el.scrollTop = el.scrollHeight; });
    el.addEventListener("scroll", onScroll, { passive: true });
    follow.observe(el, { childList: true, subtree: true, characterData: true });
    return () => { el.removeEventListener("scroll", onScroll); follow.disconnect(); };
  }, []);
  useEffect(() => { atBottom.current = true; endRef.current?.scrollIntoView({ block: "end" }); }, [msgs.length]);
  useEffect(() => { if (atBottom.current) endRef.current?.scrollIntoView({ block: "end" }); }, [lastText.length]);

  const waiting: Bee[] = bees.filter((b) => b.waitingFor && !b.paused);
  // Approvals and waiting bees are answered in the conversation (the cards);
  // the rest of what needs you, and what is coming up, sits in the sidebar.
  const needs = attention.filter((i) => i.level === "needs").length;
  const feed = attention.filter((i) => i.level === "soon");
  // 接下来的安排: noted's next items and SuperLeo's own meetings and
  // reminders, one list by time.
  const nextRows = useMemo(() => {
    type R = { key: string; at: number; noted?: ScheduleItem; life?: AttentionItem };
    const rows: R[] = [];
    next.items.forEach((it) => {
      const end = +new Date(it.endTime || it.startTime);
      if (!it.allDay && end < now.getTime()) return;
      rows.push({ key: `n-${it.id}-${it.startTime}`, at: +new Date(it.startTime), noted: it });
    });
    feed.forEach((it, i) => rows.push({ key: `l-${it.kind}-${it.ref}-${i}`, at: it.at ? +new Date(it.at) : Infinity, life: it }));
    return rows.sort((a, b) => a.at - b.at).slice(0, 6);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [next.items, feed.map((f) => `${f.ref}@${f.at}`).join(), Math.floor(now.getTime() / 60000)]);
  const [sheet, setSheet] = useState<AttentionItem | null>(null);
  const openItem = (it: AttentionItem) => {
    if (it.kind === "event" || it.kind === "reminder") setSheet(it);
    else navigate(attentionPath(it));
  };

  // The tabs: the hive, its status and the figures always; every coding run
  // that is going; and whatever was opened.
  const liveRuns = runs.filter((r) => r.state === "running");
  // The hive tab is a window onto the 3D world; Menu mode has none, so its
  // first tab is the status.
  const fixed = world ? FIXED : FIXED.filter((k) => k !== "hive");
  const tabs: TabKey[] = useMemo(() => {
    const out: TabKey[] = [...fixed];
    liveRuns.forEach((r) => { const k = "run:" + r.id; if (!closed.has(k)) out.push(k); });
    opened.forEach((k) => { if (!out.includes(k)) out.push(k); });
    return out;
  }, [liveRuns.map((r) => r.id).join(), opened, closed, world]);
  const current = tabs.includes(active) ? active : fixed[0];
  const open = (k: TabKey) => {
    setClosed((c) => { const n = new Set(c); n.delete(k); return n; });
    setOpened((o) => (o.includes(k) ? o : [...o, k]));
    setActive(k);
    fold(false);
  };
  const close = (k: TabKey) => {
    setOpened((o) => o.filter((x) => x !== k));
    setClosed((c) => new Set(c).add(k));
    if (current === k) setActive(fixed[0]);
  };

  const runOf = (k: TabKey) => runs.find((r) => "run:" + r.id === k);
  const dashOf = (k: TabKey) => pins.find((d) => "dash:" + d.id === k);
  const tabLabel = (k: TabKey) => {
    if (k === "hive") return "The hive";
    if (k === "status") return "Status";
    if (k === "stats") return "Stats";
    const r = runOf(k);
    if (r) return runName(r);
    return dashOf(k)?.name ?? "…";
  };

  const attach = useAttachments();
  const send = () => {
    const t = draft.trim();
    if (!t && attach.paths.length === 0) return;
    const { payload, images } = attach.build(t);
    setDraft("");
    attach.clear();
    chat.send(payload, images);
  };

  const pickSession = (id: string) => {
    setEarlier(false);
    setOnQueen(id === queenSession());
    chat.loadSession(id).catch(() => {});
  };

  // The round chat button, from any page: the queen's conversation, ready to type in.
  const onQueenRef = useRef(onQueen);
  onQueenRef.current = onQueen;
  const chatRef = useRef(chat);
  chatRef.current = chat;
  useEffect(() => {
    const compose = () => {
      if (!takeCompose()) return;
      if (!onQueenRef.current) { setOnQueen(true); chatRef.current.loadSession(queenSession()).catch(() => {}); }
      // After the shell has shown this page (a window opening, a page
      // changing): a hidden box takes no focus.
      window.setTimeout(() => composerRef.current?.focus(), 80);
    };
    compose();
    window.addEventListener("superleo:compose", compose);
    return () => window.removeEventListener("superleo:compose", compose);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // Who is working: workers with an order, and coding agents running somewhere.
  // Esc hides the pane when it floats over the conversation.
  useEffect(() => {
    if (folded) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape" && window.matchMedia("(max-width: 1240px)").matches) fold(true);
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [folded]);
  const busyWorkers = new Set([
    ...hive.tasks.filter((t) => t.state === "running").map((t) => t.worker),
    ...runs.filter((r) => r.state === "running").map((r) => r.agent),
  ]).size;
  useEffect(() => { setBusy(busyWorkers > 0); }, [busyWorkers, setBusy]);
  useEffect(() => () => setBusy(false), [setBusy]);
  const status = !hive.loaded ? t("Finding the hive…") : !hive.role ? t("This {machine}", { machine: t(machine) }) : `${workerList.up} of ${workerList.list.length} workers up, ${busyWorkers} busy`;
  const title = onQueen ? queenTitle(queen) : recents.find((s) => s.id === chat.sessionId)?.title || t("Conversation");

  return (
    <div ref={layoutRef} className={`${folded ? "dk folded" : "dk"}${sideFolded ? " side-folded" : ""}${resizing ? " dk-resizing" : ""}`} style={{ "--dk-side-width": sideFolded ? "0px" : `${sideWidth}px` } as CSSProperties}>
      <aside id="desk-sidebar" className="cv-glass dk-side">
        <div className="dk-ws">
          <b>SuperLeo</b>
          <button className="dk-icon" title={t("Hide sidebar")} aria-label={t("Hide sidebar")} aria-controls="desk-sidebar" aria-expanded="true" onClick={() => foldSide(true)} style={{ marginLeft: "auto" }}><PanelLeftCloseIcon size={17} /></button>
          <button className="dk-icon" title={t("New chat")} aria-label={t("New chat")} onClick={() => { setEarlier(false); chat.newSession(); setOnQueen(false); }}><SquarePenIcon size={17} /></button>
        </div>
        <button className={onQueen ? "dk-row on" : "dk-row"} onClick={() => pickSession(queenId)}><span className="dk-qhex">Q</span>{queenTitle(queen)}</button>
        {(needs > 0 || attentionError) && (
          <button className="dk-row need" onClick={() => navigate(PATHS.tasks)} title={attention.filter((i) => i.level === "needs").slice(0, 3).map(attentionTitle).join("\n")}>
            <TriangleAlertIcon size={16} />
            <span className="dk-row-t">{attentionError ? t("Could not check pending items") : t("{count} items waiting for you", { count: needs })}</span>
            {needs > 0 && <span className="dk-n">{needs}</span>}
          </button>
        )}

        {(nextRows.length > 0 || next.ok) && (
          <div className="dk-sec dk-sec-link">
            <span>{t("Up next")}</span>
            <button onClick={() => navigate(PATHS.schedule)} title={t("Schedule")}>{t("All")}<ChevronRightIcon size={12} /></button>
          </div>
        )}
        {nextRows.length === 0 && next.ok && (
          <button className="dk-rec dk-next-none" onClick={() => navigate(PATHS.schedule)}><CalendarIcon size={14} /><span>{t("Nothing planned this week")}</span></button>
        )}
        <div className="dk-next">
          {nextRows.map(({ key, noted, life }) => noted ? (
            <button key={key} className="dk-next-row" onClick={() => navigate(`${PATHS.schedule}?id=${encodeURIComponent(noted.id)}`)}
              title={[noted.title, ringSummary(noted.ringTimes), noted.location].filter(Boolean).join(" · ")}>
              <b>{noted.title || t("Untitled")}</b>
              <em>{upcomingWhen(noted, now)}</em>
              <ModeChip mode={notifyOf(noted)} />
            </button>
          ) : life ? (
            <button key={key} className="dk-next-row" onClick={() => openItem(life)} title={[life.title, life.detail, life.place].filter(Boolean).join(" · ")}>
              <b>{life.title}</b>
              <em>{shortWhen(life.at, now, t)}</em>
              <span className="mode-chip compact" title={t(life.kind === "reminder" ? "Reminder" : "Event")}>{life.kind === "reminder" ? <AlarmClockIcon size={12} /> : <CalendarIcon size={12} />}</span>
            </button>
          ) : null)}
        </div>

        {pins.length > 0 && <div className="dk-sec">{t("Pinned")}</div>}
        {pins.slice(0, 6).map((d) => (
          <button key={d.id} className={current === "dash:" + d.id ? "dk-rec on" : "dk-rec"} onClick={() => open("dash:" + d.id)}>
            <LayoutDashboardIcon size={14} /><span>{d.name}</span>
          </button>
        ))}

        <button className="dk-row" onClick={() => navigate(PATHS.conversations)} data-testid="all-conversations">
          <MessagesSquareIcon size={16} /><span className="dk-row-t">{t("Conversations")}</span>
        </button>
        {earlierCount > 0 && (
          <button className={earlier ? "dk-row on dk-earlier-entry" : "dk-row dk-earlier-entry"} onClick={() => setEarlier(true)} data-testid="earlier-entry">
            <HistoryIcon size={16} /><span className="dk-row-t">{t("Earlier chats on this Mac")}</span>
          </button>
        )}
      </aside>

      {!sideFolded && <div className="dk-side-resizer" role="separator" tabIndex={0}
        aria-label={t("Resize sidebar")} aria-orientation="vertical" aria-controls="desk-sidebar"
        aria-valuemin={200} aria-valuemax={480} aria-valuenow={sideWidth}
        title={t("Drag to resize · double-click to reset")}
        onDoubleClick={() => resizeSide(252)}
        onKeyDown={(e) => {
          if (e.key === "ArrowLeft" || e.key === "ArrowRight") { e.preventDefault(); resizeSide(sideWidth + (e.key === "ArrowRight" ? 16 : -16)); }
          if (e.key === "Home") { e.preventDefault(); resizeSide(252); }
        }}
        onPointerDown={(e) => {
          if (e.button !== 0) return;
          e.preventDefault(); e.currentTarget.focus(); e.currentTarget.setPointerCapture(e.pointerId); setResizing(true);
        }}
        onPointerMove={(e) => {
          if (e.currentTarget.hasPointerCapture(e.pointerId)) resizeSide(e.clientX - (layoutRef.current?.getBoundingClientRect().left ?? 0));
        }}
        onPointerUp={(e) => { if (e.currentTarget.hasPointerCapture(e.pointerId)) e.currentTarget.releasePointerCapture(e.pointerId); setResizing(false); }}
        onLostPointerCapture={() => setResizing(false)}
      />}
      {earlier && <EarlierChatsPane onClose={() => setEarlier(false)} />}
      <section className="cv-glass dk-chat" hidden={earlier}>
        <header className="dk-chead">
          {sideFolded && <>
            <button className="dk-icon" title={t("Show sidebar")} aria-label={t("Show sidebar")} aria-controls="desk-sidebar" aria-expanded="false" onClick={() => foldSide(false)}><PanelLeftOpenIcon size={17} /></button>
            <button className="dk-icon" title={t("New chat")} aria-label={t("New chat")} onClick={() => { setEarlier(false); chat.newSession(); setOnQueen(false); }}><SquarePenIcon size={17} /></button>
          </>}
          <span className="dk-qhex big">Q</span>
          <div><b title={title}>{title}</b><small><i className={hive.role ? "dk-ok" : "dk-off"} />{status}</small></div>
          {/* What this conversation's runs sent the model, and what each tool
              did: kept by SuperLeo itself, on the machine the conversation is on. */}
          <button className="dk-icon" title={t("Trace")} aria-label={t("Trace")} onClick={() => setTraceOpen(true)}><ScrollTextIcon size={17} /></button>
        </header>
        {traceOpen && <RunTracePanel taskId={chat.sessionId} live={chat.sending} onClose={() => setTraceOpen(false)} />}
        <div className="dk-msgs" ref={msgsRef}>
          <HiveBanner className="dk-hive" />
          {msgs.length === 0 && (
            <div className="dk-empty">{t("Ask {name} anything, or give the hive an order: “check disk on every worker and tell me the fullest”.", { name: queenRef(queen) })}</div>
          )}
          {msgs.map((m, index) =>
            m.role === "user" ? (
              <div key={m.id} className="dk-u"><SentMessage content={withoutCallNotes(m.content)} /></div>
            ) : (
              <div key={m.id} className={m.error ? "dk-q bad" : "dk-q"}>
                <LiveTurn m={m} trace={chat.trace} pulse={pulse} session={chat.sessionId} />
                <ChatTurnBrowser m={m} trace={chat.trace} newest={newestBrowser} />
                {m.content ? <Response>{withoutCallNotes(m.content)}</Response>
                  : m.cancelled ? <span className="dk-thinking">{t("Stopped.")}</span> : null}
                {m.cancelled && m.content && <div className="dk-thinking" role="status">{t("Stopped.")}</div>}
                {m.error && <div className="dk-err">{m.error}</div>}
                {m.traceId && <TraceChip traceId={m.traceId} session={chat.sessionId} />}
                {!m.streaming && !m.error && hasRenderableBlock(m.content) && <button className="dk-save-dashboard" onClick={() => {
                  const prompt = [...msgs.slice(0, index)].reverse().find(message => message.role === "user")?.content ?? "";
                  setSaveError(""); setNaming({content:m.content,prompt});
                }}><LayoutDashboardIcon size={14}/>{t("Save as dashboard")}</button>}

              </div>
            ),
          )}
          <div ref={needsRef} className="dk-needs">
            {approvals.pending.map((a) => (
              <div key={a.id} className="dk-card">
                <div className="dk-card-t"><TriangleAlertIcon size={15} />{t("Needs you")}</div>
                <p>{a.by ? `${a.by} wants` : "The hive wants"} to run <code>{a.command || a.tool}</code></p>
                <div className="dk-card-btns">
                  <button className="dk-btn pri" onClick={() => approvals.resolve(a.id, true)}>{t("Allow once")}</button>
                  <button className="dk-btn" onClick={() => approvals.resolve(a.id, false)}>{t("Deny")}</button>
                  {a.expiresAt && <small>denied on its own in {elapsed(new Date().toISOString(), new Date(a.expiresAt).getTime())}</small>}
                </div>
              </div>
            ))}
            {waiting.map((b) => (
              <div key={b.id} className="dk-card">
                <div className="dk-card-t"><TriangleAlertIcon size={15} />{t("Needs you")}</div>
                <p><b>{b.name}</b> is waiting: {b.waitingFor}</p>
                <div className="dk-card-btns"><button className="dk-btn pri" onClick={() => navigate(PATHS.agents)}>{t("Open Bees")}</button></div>
              </div>
            ))}
          </div>
          <div ref={endRef} />
        </div>
        <form className="dk-comp" onSubmit={(e) => { e.preventDefault(); if (!cutOff) send(); }}>
          <AgentMenu
            matches={mentions.matches}
            active={mentions.active}
            onPick={(a) => {
              const next = mentions.accept(a);
              if (next) requestAnimationFrame(() => { composerRef.current?.focus(); composerRef.current?.setSelectionRange(next.caret, next.caret); });
            }}
          />
          <textarea
            ref={composerRef}
            name="message" rows={1} value={draft} disabled={cutOff} placeholder={cutOff ? t("Waiting for the hive…") : t("Ask {name}, or give the hive an order. @ for an agent or a worker", { name: queenRef(queen) })}
            onChange={(e) => { setDraft(e.target.value); mentions.update(e.target.value, e.target.selectionStart ?? e.target.value.length); }}
            onKeyDown={(e) => {
              if (mentions.onKeyDown(e)) return;
              if (e.key === "Enter" && !e.shiftKey && !e.nativeEvent.isComposing) { e.preventDefault(); send(); }
            }}
            onBlur={mentions.close}
            onPaste={attach.paste}
          />
          {attach.paths.length > 0 && <div className="dk-files"><AttachmentChips paths={attach.paths} onRemove={attach.remove} /></div>}
          <button type="button" className="dk-clip" onClick={attach.pick} disabled={attach.importing || cutOff} title={t("Attach")} data-testid="dk-attach"><PaperclipIcon size={17} /></button>
          {chat.sending && !draft.trim() && attach.paths.length === 0 ? (
            <button type="button" className="dk-send stop" title={t("Stop")} onClick={() => chat.cancel()}><SquareIcon size={14} fill="currentColor" /></button>
          ) : (
            <button type="submit" className="dk-send" title={t("Send")} disabled={cutOff || !draft.trim() && attach.paths.length === 0}><ArrowUpIcon size={18} /></button>
          )}
        </form>
      </section>
      {sheet && <EventSheet item={sheet} onClose={() => setSheet(null)} />}
      {naming && <NameDashboardModal suggested={suggestName(naming.prompt)} prompt={naming.prompt} onCancel={() => setNaming(null)} onSave={async name => {
        const saved = naming; setNaming(null);
        try { await dashboards.save(name, saved.content, saved.prompt); setPins(await dashboards.list()); }
        catch (error) { setSaveError(String(error instanceof Error ? error.message : error)); }
      }} />}
      {saveError && <div className="dk-dashboard-error" role="alert">{saveError}<button onClick={() => setSaveError("")} aria-label={t("Close")}><XIcon size={16}/></button></div>}

      {/* Folded, the pane is simply gone; its toggle stays in the shell's
          toolbar, top right. */}
      {!folded && (
      <section className={tabs.length > fixed.length ? "dk-live" : "dk-live bare"}>
        {/* Tabs only when there is something to switch to: a run or a
            dashboard opened beside the fixed panes. */}
        {tabs.length > fixed.length && <div className="dk-tabs">
          {tabs.map((k) => {
            const r = runOf(k);
            const on = k === current;
            const liveDot = k === "hive" ? busyWorkers > 0 : r?.state === "running";
            return (
              <button key={k} className={on ? "dk-tab on" : "dk-tab"} onClick={() => setActive(k)}>
                {liveDot ? <i className="dk-livedot" /> : k.startsWith("run:") ? <TerminalIcon size={14} /> : k.startsWith("dash:") ? <LayoutDashboardIcon size={14} /> : k === "status" ? <ActivityIcon size={14} /> : k === "stats" ? <GaugeIcon size={14} /> : <HexagonIcon size={14} />}
                <span>{tabLabel(k)}</span>
                {!fixed.includes(k) && <XIcon size={13} className="dk-tab-x" onClick={(e) => { e.stopPropagation(); close(k); }} />}
              </button>
            );
          })}
          <button className="dk-icon" title="Open a coding run" onClick={() => navigate(PATHS.coding)}><PlusIcon size={17} /></button>
        </div>}
        <div className={current === "hive" ? "dk-screen see-world" : "cv-glass dk-screen"}>
          <div className="dk-in">
            {/* The hive tab is a window onto the world behind the app. */}
            {current === "status" && <LiveStatus hive={hive} now={now} agents={linked} runs={runs} />}
            {current === "stats" && <div className="dk-stats"><StatsBlock hive={hive} runs={runs} /></div>}
            {current.startsWith("run:") && <RunTab id={current.slice(4)} now={now} />}
            {current.startsWith("dash:") && (
              <div className="dk-dash">{dashOf(current) ? <Response>{dashOf(current)!.source}</Response> : "Gone."}</div>
            )}
          </div>
        </div>
        <Controls current={current} run={runOf(current)} dash={dashOf(current)} busy={busyWorkers} onOpenHive={() => navigate(PATHS.hive)} onOpenStats={() => navigate(PATHS.stats)} />
      </section>
      )}

    </div>
  );
}

function RunTab({ id, now }: { id: string; now: Date }) {
  const run = useRun(id);
  return <LiveRun run={run} now={now} />;
}

/** Under the screen: who is driving, and how to step in. */
function Controls({ current, run, dash, busy, onOpenHive, onOpenStats }: { current: TabKey; run?: CodingRun; dash?: Dashboard; busy: number; onOpenHive: () => void; onOpenStats: () => void }) {
  const [err, setErr] = useState("");
  useEffect(() => { setErr(""); }, [current]);
  const detail = useRunSession(run?.id);
  if (current === "stats") {
    return (
      <div className="dk-ctl">
        <button className="dk-btn light" onClick={onOpenStats}>Open Stats</button>
      </div>
    );
  }
  if (current === "hive" || current === "status") {
    return (
      <div className="dk-ctl">
        <span>{busy > 0 ? `${queenNamed() ? getQueenName().name : "The queen"} has the hive` : "The hive is resting"}</span>
        {busy > 0 && <button className="dk-btn ghost" onClick={() => CancelAllChats().catch((e) => setErr(String(e)))}>Stop all</button>}
        <button className="dk-btn light" onClick={onOpenHive}>Open the hive</button>
        {err && <em className="dk-ctl-err">{err}</em>}
      </div>
    );
  }
  if (run) {
    const going = run.state === "running";
    const where = !run.remote ? `this ${machine}` : /mac/i.test(run.remote) ? "the Mac" : run.remote;
    return (
      <div className="dk-ctl">
        <span>{going ? `${agentName(run.agent)} is working on ${where}` : `${agentName(run.agent)} ${run.state === "done" ? "finished" : run.state}`}</span>
        {going && <button className="dk-btn ghost" onClick={() => CancelCLIRun(run.id).catch((e) => setErr(String(e)))}>Stop</button>}
        {/* Take over opens a terminal on the machine this window runs on; a
            browser tab has none to open. */}
        {detail?.session && !served && (
          <button className="dk-btn light" onClick={() => TakeOver(run.agent, detail.session!, detail.cwd).catch((e) => setErr(String(e?.message ?? e)))}>Take over</button>
        )}
        {err && <em className="dk-ctl-err">{err}</em>}
      </div>
    );
  }
  if (dash) {
    return (
      <div className="dk-ctl">
        <span>{dash.name}</span>
        <button className="dk-btn light" onClick={() => dashboards.refresh(dash.id).catch((e) => setErr(String(e)))}>Refresh</button>
        {err && <em className="dk-ctl-err">{err}</em>}
      </div>
    );
  }
  return <div className="dk-ctl" />;
}

/** "claude.mac": the agent at the machine, once — the agent's own name may
 *  already carry the machine. */
export const runName = (r: { agent: string; remote?: string }) => (r.agent.includes(".") || !r.remote ? r.agent : `${r.agent}.${r.remote}`);

const agentName = (a: string) => (a.startsWith("claude") ? "Claude Code" : a.startsWith("codex") ? "Codex" : a);

function useRunSession(id?: string) {
  const run = useRun(id ?? "");
  return id ? run : null;
}
