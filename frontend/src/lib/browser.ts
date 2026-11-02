import { useEffect, useState, useSyncExternalStore } from "react";
import { BrowserConnect, BrowserStatus as fetchBrowserStatus, SetBrowserMode } from "../../wailsjs/go/app/App";

/**
 * The built-in browser, as every screen sees it: whether there is one, the
 * live stream of its page, and which of a turn's tool calls drove it.
 *
 * The model drives a real browser through agent-browser, mounted as an MCP
 * server; this file is the one place that knows its tool names and its stream
 * protocol, so the card in a conversation and the large panel read the same
 * page through the same socket code.
 */

export interface BrowserStatus {
  enabled: boolean;
  ready: boolean;
  version: string;
  mode: "chrome" | "own" | "off" | string;
  streamUrl: string;
  /** Attached to its browser. False in the person's Chrome until a connect. */
  connected?: boolean;
  error?: string;
  /** My Chrome: the extension that drives it, and what to paste into it. */
  bridge?: { connected: boolean; browser?: string; version?: string; code: string; dir: string; cdpUrl: string };
  /** Which run has the browser and which wait in line for it. */
  queue?: { holder?: string; holderSince?: string; waiting?: string[] };
}

const OFF: BrowserStatus = { enabled: false, ready: false, version: "", mode: "off", streamUrl: "" };

// ---- status ---------------------------------------------------------------

// One poll for the whole window, however many cards and panels ask. Quick
// while the browser is still coming up, slow once it is there.
let status: BrowserStatus | null = null;
const statusSubs = new Set<() => void>();
let statusTimer = 0;
// The mode just asked for, while the service rebuilds around it: the status
// is read every second until it gets there (ready, or an error), so turning
// the browser on shows it installing and then the page, with no reload.
// An error left over from before the switch is not its outcome, so the
// first few seconds of errors are not taken as one.
let target: { mode: string; from: number; until: number } | null = null;

async function pollStatus() {
  window.clearTimeout(statusTimer);
  let next: BrowserStatus;
  try {
    const raw = (await fetchBrowserStatus()) as unknown as Partial<BrowserStatus> | null;
    next = { ...OFF, ...(raw ?? {}) } as BrowserStatus;
  } catch {
    // A backend from before the browser has no such method: the same as one
    // with the browser turned off.
    next = OFF;
  }
  const landed = !!target && (Date.now() > target.until || (!!next.error && Date.now() > target.from + 3000) || (target.mode === "off" ? !next.enabled : next.mode === target.mode && next.ready));
  if (landed) target = null;
  if (landed || JSON.stringify(next) !== JSON.stringify(status)) {
    status = next;
    statusSubs.forEach((f) => f());
    // A stream held back while the person's Chrome was unattached goes up
    // the moment it is attached.
    hubs.forEach((h) => h.resume());
  }
  if (statusSubs.size > 0 || target) {
    const settling = next.enabled && (!next.ready || !!next.error);
    statusTimer = window.setTimeout(pollStatus, target ? 1000 : settling ? 3000 : 15000);
  }
}

function subscribeStatus(f: () => void) {
  statusSubs.add(f);
  if (statusSubs.size === 1) void pollStatus();
  return () => {
    statusSubs.delete(f);
    if (statusSubs.size === 0) window.clearTimeout(statusTimer);
  };
}

/** The browser's status, shared and kept fresh; null until first heard. */
export function useBrowserStatus(): BrowserStatus | null {
  return useSyncExternalStore(subscribeStatus, () => status);
}

/** The mode the browser is being switched to, until the switch has landed; "" otherwise. */
export function useBrowserSwitching(): string {
  return useSyncExternalStore(subscribeStatus, () => target?.mode ?? "");
}

/**
 * Turn the browser on in the person's own Chrome ("chrome"), in a separate
 * one agent-browser launches ("own"), or off. The backend saves it and
 * rebuilds; the status is then followed closely until it has settled.
 */
export async function setBrowserMode(mode: "chrome" | "own" | "off") {
  target = { mode, from: Date.now(), until: Date.now() + 180000 };
  statusSubs.forEach((f) => f());
  try {
    await SetBrowserMode(mode);
  } catch (e) {
    target = null;
    statusSubs.forEach((f) => f());
    throw e;
  }
  void pollStatus();
}

/**
 * Attach the browser now. In the person's own Chrome agent-browser attaches
 * only when a command needs it, so a panel opened before the model's first
 * call would otherwise sit at "not connected"; once attached, the stream
 * says so itself.
 */
export const connectBrowser = async () => {
  try {
    await BrowserConnect();
  } finally {
    void pollStatus();
  }
};

/**
 * Whether the stream must stay shut: the person's own Chrome, not attached.
 * Opening the stream then makes agent-browser reach for Chrome, and pages
 * and phones redialling in a loop did that every few seconds until Chrome,
 * and the machine, hung. Only a connect the person presses attaches it.
 */
const heldBack = () => status?.mode === "chrome" && status.ready && status.connected === false;

/** Ask for the status again now, for a retry button. */
export const refreshBrowserStatus = () => void pollStatus();

/**
 * Where the page's socket goes. The desktop window runs on the machine the
 * browser does and dials its loopback stream; a page served over HTTP is on
 * some other machine as often as not, so it always goes through the relay on
 * its own origin, which carries the session cookie like every other /api call.
 */
function streamBase(st: BrowserStatus): string {
  const served = Boolean((window as unknown as Record<string, unknown>).superleoServed);
  if (st.streamUrl && !served) return st.streamUrl;
  const scheme = window.location.protocol === "https:" ? "wss:" : "ws:";
  return `${scheme}//${window.location.host}/api/browser/stream`;
}

// ---- tool calls -----------------------------------------------------------

/**
 * The operation a tool call performs on the browser, or "" when it is not a
 * browser call. Our own agent names MCP tools mcp_<server>_<tool>; a coding
 * CLI writes mcp__<server>__<tool>. The server is "browser", or
 * "agent-browser" where someone configured it by hand before it was built in.
 */
export function browserOp(tool: string | undefined): string {
  const m = /^mcp__?(?:browser|agent-browser)__?(.+)$/.exec(tool ?? "");
  if (!m) return "";
  return m[1].replace(/^agent_browser_/, "");
}

export interface BrowserCall {
  id: string;
  op: string;
  args: Record<string, unknown>;
  status: "running" | "ok" | "fail";
  /** What a coding CLI's log says about the call, where there are no arguments to read. */
  detail?: string;
}

const host = (u: unknown) => {
  if (typeof u !== "string" || !u) return "";
  try { return new URL(/^[a-z]+:\/\//i.test(u) ? u : `https://${u}`).host.replace(/^www\./, ""); } catch { return u.slice(0, 40); }
};
const quoted = (v: unknown) => {
  const s = typeof v === "string" ? v.replace(/\s+/g, " ").trim() : "";
  return s ? `“${s.length > 28 ? s.slice(0, 27) + "…" : s}”` : "";
};
const pointee = (a: Record<string, unknown>) => {
  const v = a.selector ?? a.ref ?? a.element ?? a.target ?? "";
  const s = typeof v === "string" ? v.trim() : "";
  return s.length > 26 ? s.slice(0, 25) + "…" : s;
};

/** A browser call in a few words: "open github.com", "type “rust”", "scroll down". */
export function browserLabel(c: BrowserCall): string {
  const a = c.args ?? {};
  if (c.detail && Object.keys(a).length === 0) {
    const d = c.detail.replace(/\s+/g, " ").trim();
    return `${c.op.replace(/_/g, " ")} ${d.length > 40 ? d.slice(0, 39) + "…" : d}`.trim();
  }
  switch (c.op) {
    case "open": return ["open", host(a.url)].filter(Boolean).join(" ");
    case "tab_new": return ["new tab", host(a.url)].filter(Boolean).join(" ");
    case "click": return ["click", pointee(a)].filter(Boolean).join(" ");
    case "fill": case "type": return [c.op, quoted(a.text ?? a.value)].filter(Boolean).join(" ");
    case "press": return ["press", typeof a.key === "string" ? a.key : ""].filter(Boolean).join(" ");
    case "scroll": return ["scroll", typeof a.direction === "string" ? a.direction : typeof a.y === "number" ? (a.y < 0 ? "up" : "down") : ""].filter(Boolean).join(" ");
    case "select": return ["select", quoted(a.value ?? a.values)].filter(Boolean).join(" ");
    case "wait_for_text": return ["wait for", quoted(a.text)].filter(Boolean).join(" ");
    case "wait_for_selector": return ["wait for", pointee(a)].filter(Boolean).join(" ");
    case "wait_ms": return `wait ${typeof a.ms === "number" ? `${a.ms}ms` : ""}`.trim();
    case "wait_for_load": return "wait for load";
    case "get_text": return "read text";
    case "get_url": return "read url";
    case "get_title": return "read title";
    case "tools_profiles": return "profiles";
    case "tab_list": return "list tabs";
    case "tab_switch": return "switch tab";
    case "tab_close": return "close tab";
    default: return c.op.replace(/_/g, " ");
  }
}

/** Parse what a stored step says ("Called mcp_browser_agent_browser_open"). */
export function browserOpOfStep(text: string): string {
  const m = /^Call(?:ed|ing)\s+(\S+)/.exec(text.trim());
  return m ? browserOp(m[1]) : "";
}

// ---- the stream ----------------------------------------------------------

export interface FrameMeta {
  deviceWidth: number;
  deviceHeight: number;
  pageScaleFactor?: number;
  offsetTop?: number;
  scrollOffsetX?: number;
  scrollOffsetY?: number;
  timestamp?: number;
}

export interface BrowserTab {
  tabId: string;
  title: string;
  url: string;
  active: boolean;
}

export interface StreamState {
  /** The socket itself: not asked for, dialling, up, or down and retrying. */
  socket: "idle" | "connecting" | "open" | "retrying";
  /** The last picture of the page, as a data URL. Frames only come when the page changes. */
  frame: string;
  meta: FrameMeta | null;
  url: string;
  title: string;
  tabs: BrowserTab[];
  /** What agent-browser says about its own browser: attached, and how big. */
  connected: boolean | null;
  viewport: { width: number; height: number } | null;
}

const EMPTY: StreamState = { socket: "idle", frame: "", meta: null, url: "", title: "", tabs: [], connected: null, viewport: null };

/**
 * The most recent picture and address of the page, whichever socket saw it.
 * A card that is not live keeps showing this rather than an empty box.
 */
let lastSeen: Pick<StreamState, "frame" | "meta" | "url" | "title"> = { frame: "", meta: null, url: "", title: "" };
export const lastBrowserFrame = () => lastSeen;

/**
 * One socket per frame rate, shared by everyone who asks for that rate and
 * closed when the last of them lets go: every card in a conversation reads
 * the same small stream, and the panel has its own faster one.
 */
class Hub {
  state: StreamState = { ...EMPTY, ...lastSeen };
  private subs = new Set<() => void>();
  private ws: WebSocket | null = null;
  private base = "";
  private retry = 0;
  private timer = 0;

  constructor(private fps: number) {}

  subscribe(f: () => void) {
    this.subs.add(f);
    return () => { this.subs.delete(f); };
  }

  // Every view that wants the stream right now, by the address it asked for.
  // Views share the socket, so it stays up until the last of them lets go.
  private wants: string[] = [];

  /** Want the stream at this address until the returned release is called. */
  want(base: string): () => void {
    this.wants.push(base);
    this.aim();
    return () => {
      const i = this.wants.indexOf(base);
      if (i >= 0) this.wants.splice(i, 1);
      this.aim();
    };
  }

  private aim() {
    const base = this.wants[this.wants.length - 1] ?? "";
    if (base === this.base && (!base || this.ws || this.timer)) return;
    this.stop();
    this.base = base;
    if (base) this.dial();
  }

  send(msg: Record<string, unknown>) {
    if (this.ws?.readyState === WebSocket.OPEN) this.ws.send(JSON.stringify(msg));
  }

  private set(p: Partial<StreamState>) {
    this.state = { ...this.state, ...p };
    this.subs.forEach((f) => f());
  }

  /** Dial now if wanted and not up: the status changed under a held stream. */
  resume() {
    if (this.base && !this.ws && !this.timer) this.dial();
  }

  private dial() {
    window.clearTimeout(this.timer);
    this.timer = 0;
    if (heldBack()) {
      this.retry = 0;
      this.set({ socket: "idle", connected: false });
      return;
    }
    const sep = this.base.includes("?") ? "&" : "?";
    let ws: WebSocket;
    try {
      ws = new WebSocket(`${this.base}${sep}maxFps=${this.fps}`);
    } catch {
      this.again();
      return;
    }
    this.ws = ws;
    this.set({ socket: this.retry ? "retrying" : "connecting" });
    ws.onopen = () => {
      this.retry = 0;
      this.set({ socket: "open" });
      // The rate on the URL is what agent-browser starts at; saying it again
      // once open covers a relay or a version that only honours the message.
      ws.send(JSON.stringify({ type: "config", maxFps: this.fps }));
    };
    ws.onmessage = (e) => this.onMessage(e.data);
    ws.onclose = () => {
      if (this.ws !== ws) return;
      this.ws = null;
      this.again();
    };
  }

  /** Dial again later, waiting longer each time, up to fifteen seconds. */
  private again() {
    if (!this.base) return;
    if (heldBack()) {
      this.retry = 0;
      this.set({ socket: "idle", connected: false });
      return;
    }
    const wait = Math.min(15000, 1000 * 2 ** this.retry);
    this.retry++;
    this.set({ socket: "retrying", connected: this.state.connected === true ? null : this.state.connected });
    this.timer = window.setTimeout(() => this.dial(), wait);
  }

  private stop() {
    window.clearTimeout(this.timer);
    this.timer = 0;
    this.retry = 0;
    const ws = this.ws;
    this.ws = null;
    if (ws) {
      ws.onclose = ws.onmessage = ws.onopen = null;
      ws.close();
    }
    if (this.state.socket !== "idle") this.set({ socket: "idle" });
  }

  private onMessage(raw: unknown) {
    if (typeof raw !== "string") return;
    let m: any;
    try { m = JSON.parse(raw); } catch { return; }
    switch (m?.type) {
      case "frame": {
        if (typeof m.data !== "string") return;
        const frame = `data:image/jpeg;base64,${m.data}`;
        const meta = m.metadata && m.metadata.deviceWidth > 0 ? (m.metadata as FrameMeta) : this.state.meta;
        lastSeen = { ...lastSeen, frame, meta };
        this.set({ frame, meta });
        break;
      }
      case "url":
        if (typeof m.url === "string") { lastSeen = { ...lastSeen, url: m.url }; this.set({ url: m.url }); }
        break;
      case "tabs": {
        const tabs: BrowserTab[] = Array.isArray(m.tabs)
          ? m.tabs.map((t: any) => ({ tabId: String(t.tabId ?? t.targetId ?? ""), title: String(t.title ?? ""), url: String(t.url ?? ""), active: !!t.active }))
          : [];
        const on = tabs.find((t) => t.active);
        // The tab list is also where the address and title of the page come
        // from before any url message has: agent-browser sends tabs on connect.
        const p: Partial<StreamState> = { tabs };
        if (on) { p.url = on.url; p.title = on.title; lastSeen = { ...lastSeen, url: on.url, title: on.title }; }
        this.set(p);
        break;
      }
      case "status":
        this.set({
          connected: typeof m.connected === "boolean" ? m.connected : this.state.connected,
          viewport: m.viewportWidth > 0 && m.viewportHeight > 0 ? { width: m.viewportWidth, height: m.viewportHeight } : this.state.viewport,
        });
        break;
    }
  }
}

const hubs = new Map<number, Hub>();
const hubFor = (fps: number) => {
  let h = hubs.get(fps);
  if (!h) { h = new Hub(fps); hubs.set(fps, h); }
  return h;
};

export interface BrowserStream extends StreamState {
  status: BrowserStatus | null;
  /** The mode being switched to, while the backend rebuilds; "" otherwise. */
  switching: string;
  send: (msg: Record<string, unknown>) => void;
}

/**
 * The live page, for one view. `fps` picks the shared socket (4 for a card,
 * 15 for the panel); `live` false reads the last frame without dialling.
 * The socket waits for the browser to be ready, retries with backoff when it
 * drops, and closes when the last view using it unmounts.
 */
export function useBrowserStream(fps: number, live = true): BrowserStream {
  const st = useBrowserStatus();
  const switching = useBrowserSwitching();
  const hub = hubFor(fps);
  const [, bump] = useState(0);
  const base = live && st?.enabled && st.ready ? streamBase(st) : "";
  useEffect(() => hub.subscribe(() => bump((n) => n + 1)), [hub]);
  useEffect(() => (base ? hub.want(base) : undefined), [hub, base]);
  const state = live ? hub.state : { ...EMPTY, ...lastSeen };
  return { ...state, status: st, switching, send: (msg) => hub.send(msg) };
}

// ---- the panel -------------------------------------------------------------

// The large view is one per window, opened from a card or from the rail.
let panelOpen = false;
const panelSubs = new Set<() => void>();
const setPanel = (v: boolean) => { panelOpen = v; panelSubs.forEach((f) => f()); };
export const openBrowserPanel = () => setPanel(true);
export const closeBrowserPanel = () => setPanel(false);
export function useBrowserPanelOpen(): boolean {
  return useSyncExternalStore((f) => { panelSubs.add(f); return () => { panelSubs.delete(f); }; }, () => panelOpen);
}

// ---- turns that are not in a useChat -----------------------------------------

/**
 * The browser calls of each request, for the screens that follow a turn from
 * the raw chat events instead of through useChat (the world's queen card and
 * its command line). Kept for the life of the window, newest few hundred.
 */
const byRequest = new Map<string, BrowserCall[]>();
const turnSubs = new Set<() => void>();
let turnsVersion = 0;
let seq = 0;

export function noteBrowserEvent(p: { requestId?: string; type?: string; tool?: string; args?: Record<string, unknown>; result?: any }) {
  const op = browserOp(p?.tool);
  if (!op || !p.requestId) return;
  const list = byRequest.get(p.requestId) ?? [];
  if (p.type === "tool_call") {
    list.push({ id: `b${++seq}`, op, args: p.args ?? {}, status: "running" });
  } else if (p.type === "tool_result") {
    const i = [...list].reverse().findIndex((c) => c.status === "running" && c.op === op);
    if (i < 0) return;
    const r = p.result;
    const failed = r && typeof r === "object" && (r.ok === false || r.error || r.success === false || r.isError === true);
    list[list.length - 1 - i] = { ...list[list.length - 1 - i], status: failed ? "fail" : "ok" };
  } else return;
  byRequest.set(p.requestId, [...list]);
  if (byRequest.size > 300) byRequest.delete(byRequest.keys().next().value as string);
  turnsVersion++;
  turnSubs.forEach((f) => f());
}

/** A turn is over: anything it left running did not report back. */
const finished = new Set<string>();
/** Whether a request's turn has ended (done, failed or stopped). */
export const browserTurnOver = (requestId: string) => finished.has(requestId);

export function settleBrowserTurn(requestId: string, failed = false) {
  if (!finished.has(requestId)) {
    finished.add(requestId);
    if (finished.size > 300) finished.delete(finished.values().next().value as string);
    turnsVersion++;
    turnSubs.forEach((f) => f());
  }
  const list = byRequest.get(requestId);
  if (!list?.some((c) => c.status === "running")) return;
  byRequest.set(requestId, list.map((c) => (c.status === "running" ? { ...c, status: failed ? "fail" : "ok" } : c)));
  turnsVersion++;
  turnSubs.forEach((f) => f());
}

const subscribeTurns = (f: () => void) => { turnSubs.add(f); return () => { turnSubs.delete(f); }; };

/** The browser calls of any request, by its id; the caller re-renders as more come. */
export function useBrowserTurns(): (requestId: string | undefined) => BrowserCall[] {
  useSyncExternalStore(subscribeTurns, () => turnsVersion);
  return (requestId) => (requestId ? byRequest.get(requestId) ?? [] : []);
}

// ---- a turn's calls, from what each screen keeps ----------------------------

/** The browser calls among a turn's traced tool calls. */
export function browserCallsOfTrace(trace: { id: string; askId: string; tool: string; args: Record<string, any>; inner: boolean; status: "running" | "ok" | "fail" }[], askId: string): BrowserCall[] {
  const out: BrowserCall[] = [];
  for (const t of trace) {
    if (t.askId !== askId) continue;
    const op = browserOp(t.tool);
    if (op) out.push({ id: t.id, op, args: t.args ?? {}, status: t.status });
  }
  return out;
}

/**
 * The browser calls of a turn reopened from history, which kept only one line
 * per step ("Called mcp_browser_agent_browser_open"): the operation without
 * its arguments, and no outcome, so each reads as done.
 */
export function browserCallsOfSteps(steps: { id: string; kind: string; text: string; tool?: string }[] | undefined): BrowserCall[] {
  const out: BrowserCall[] = [];
  for (const s of steps ?? []) {
    if (s.kind !== "tool") continue;
    const op = browserOp(s.tool) || browserOpOfStep(s.text);
    if (op) out.push({ id: s.id, op, args: {}, status: "ok" });
  }
  return out;
}

/** True for a progress line that is a browser call, which the card shows instead. */
export const isBrowserStep = (s: { kind: string; text: string; tool?: string }) =>
  s.kind === "tool" && !!(browserOp(s.tool) || browserOpOfStep(s.text));

// Each turn's last picture of the page, kept when the turn stops being live,
// so an older card shows where that turn left the page and not where the
// browser is now.
const turnFrames = new Map<string, Pick<StreamState, "frame" | "meta" | "url" | "title">>();
export const turnFrame = (key: string) => turnFrames.get(key);
export function keepTurnFrame(key: string, s: Pick<StreamState, "frame" | "meta" | "url" | "title">) {
  if (!s.frame) return;
  turnFrames.set(key, { frame: s.frame, meta: s.meta, url: s.url, title: s.title });
  if (turnFrames.size > 60) turnFrames.delete(turnFrames.keys().next().value as string);
}
