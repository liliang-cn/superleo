import { ClipboardSetText } from "../../wailsjs/runtime";
import { translate } from "./i18n";

// copyText copies via the Wails clipboard runtime, falling back to the Web API.
export async function copyText(s: string): Promise<boolean> {
  try {
    if (await ClipboardSetText(s)) return true;
  } catch {
    /* fall through */
  }
  try {
    await navigator.clipboard.writeText(s);
    return true;
  } catch {
    return false;
  }
}

// parseTime accepts both shapes the backend sends a moment in: RFC3339 from a
// marshalled time.Time, and "2006-01-02 15:04:05" from the scheduled-run event.
// A space between date and time is not a format engines must parse, so it is
// normalised away first. A time.Time that was never set marshals as year 1,
// which is not a moment worth showing anywhere, so it reads as absent too.
export function parseTime(value: any): Date | null {
  const raw = typeof value === "string" ? value.trim() : "";
  if (raw === "") return null;
  for (const candidate of [raw, raw.replace(" ", "T")]) {
    const d = new Date(candidate);
    if (!Number.isNaN(d.getTime()) && d.getFullYear() > 1970) return d;
  }
  return null;
}

// formatStamp renders a moment in the platform's locale, to the minute.
export function formatStamp(d: Date): string {
  return d.toLocaleString(undefined, {
    weekday: "short",
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });
}

// fromNow is the other half of a timestamp: the absolute time answers "when",
// this answers "is that what I meant".
export function fromNow(d: Date, now: number = Date.now()): string {
  const delta = d.getTime() - now;
  const mins = Math.round(Math.abs(delta) / 60000);
  let text: string;
  if (mins < 1) text = "less than a minute";
  else if (mins < 60) text = `${mins} min`;
  else if (mins < 60 * 24) {
    const h = Math.round(mins / 60);
    text = `${h} hour${h === 1 ? "" : "s"}`;
  } else {
    const days = Math.round(mins / (60 * 24));
    text = `${days} day${days === 1 ? "" : "s"}`;
  }
  return delta < 0 ? `${text} ago` : `in ${text}`;
}

// formatDuration renders a wall clock reading for a finished run.
export function formatDuration(ms: number): string {
  if (!Number.isFinite(ms) || ms < 0) return "—";
  if (ms < 1000) return `${Math.round(ms)}ms`;
  const s = ms / 1000;
  if (s < 60) return `${s.toFixed(s < 10 ? 1 : 0)}s`;
  const m = Math.floor(s / 60);
  return `${m}m ${Math.round(s - m * 60)}s`;
}

// firstLine collapses a multi-line string to a single labelled line.
export function firstLine(s: string, max = 70): string {
  const line = (s || "").trim().split("\n")[0].trim();
  return line.length > max ? `${line.slice(0, max - 1)}…` : line;
}

function esc(s: string): string {
  return s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
}

// prettyJson stringifies a value (objects pretty-printed; scalars as-is).
export function prettyJson(value: any): string {
  if (typeof value === "string") return value;
  try {
    return JSON.stringify(value, null, 2);
  } catch {
    return String(value);
  }
}

// highlightJson returns an HTML string with classed spans for keys/strings/
// numbers/booleans/null — the classic regex JSON highlighter (input escaped).
export function highlightJson(value: any): string {
  const json = esc(prettyJson(value));
  return json.replace(
    /("(\\u[a-zA-Z0-9]{4}|\\[^u]|[^\\"])*"(\s*:)?|\b(true|false|null)\b|-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?)/g,
    (m) => {
      let cls = "j-num";
      if (/^"/.test(m)) cls = /:$/.test(m) ? "j-key" : "j-str";
      else if (m === "true" || m === "false") cls = "j-bool";
      else if (m === "null") cls = "j-null";
      return `<span class="${cls}">${m}</span>`;
    },
  );
}

/**
 * What a streaming answer should show, given everything received so far.
 *
 * The model's stream carries its programmatic tool calls inline, as
 * `<code>…</code>` blocks — that is the protocol, not prose. Appending each
 * delta verbatim put those scripts in the answer bubble, so a turn that used
 * tools read as `const res = callTool('bash', …)` until it finished and the
 * final answer replaced it. The Tool Trace panel already shows every script
 * with its result, which is where a script belongs.
 *
 * Takes the whole accumulated text rather than one delta because a block is
 * split across deltas at arbitrary points: an opening tag can arrive in one
 * chunk and its closing tag several later. Until the closing tag lands, the
 * script is suppressed rather than shown half-written.
 *
 * Markdown fences are left alone — ```js is how an answer legitimately shows
 * code, and only the <code> tags mean "run this".
 */
export function visibleAnswer(raw: string): string {
  let out = raw.replace(/<code>[\s\S]*?<\/code>/g, "\n");
  const open = out.lastIndexOf("<code>");
  if (open >= 0) out = out.slice(0, open);
  // A trailing "MOOD: happy" tag is protocol too — the backend splits it off
  // the completed answer and routes it to the avatar. A streamed bubble never
  // saw that split, so the tag used to flash at the end of an answer; on a
  // stopped one it stayed for good, because there is no final text to replace
  // it with. Only a short last line counts, so an answer that talks about the
  // tag in prose keeps it.
  out = out.replace(/\n?[ \t]*MOOD:[^\n]{0,20}[ \t]*$/, "");
  return out.trim();
}

/** What a person should see of a message: without the note a phone call adds
 *  to its first turn, or the marker the queen ends a call with. */
export function withoutCallNotes(text: string): string {
  const i = text.indexOf("\n\n(This is a voice call:");
  return (i >= 0 ? text.slice(0, i) : text).split("[end call]").join("").trim();
}

/** How a person reads a queen engine's name: "claude.mac" is "Claude Code · mac". */
export function engineLabel(engine: string): string {
  const dot = engine.indexOf(".");
  const agent = dot > 0 ? engine.slice(0, dot) : engine;
  const name = agent === "claude" ? "Claude Code" : agent;
  return dot > 0 && dot < engine.length - 1 ? `${name} · ${engine.slice(dot + 1)}` : name;
}

/** The engine thinking for the queen now, or "" while her turns are SuperLeo's
 *  own model's — no engine set, or SuperLeo answered the latest one. */
export function queenThinker(engine?: string, now?: string): string {
  if (!engine || now === "superleo") return "";
  return engineLabel(now || engine);
}

/** Who answered for the queen, as it goes under an answer: the engine
 *  ("Claude Code"), or "SuperLeo" when its own agent answered in the
 *  engine's place. Empty for SuperLeo's own turns, which need no name. */
export function engineByline(m: { engine?: string; standInFor?: string }): string {
  if (m.standInFor) return "SuperLeo";
  return m.engine ? engineLabel(m.engine) : "";
}

/** Who answers for the queen now when it is not her first engine: "SuperLeo",
 *  or the next engine in her list, answering in its place. Empty when the
 *  first engine is answering. */
export function queenEngineNote(engine?: string, now?: string): string {
  if (!engine || !now || now === engine) return "";
  return now === "superleo" ? "SuperLeo" : engineLabel(now);
}

/** Recalled memory the agent put in front of the model as a "user" message
 *  ("## Relevant Memory"): never something the person typed. */
export function isInjectedMemory(role: string, content: string): boolean {
  return role === "user" && /^\s*## Relevant Memory\b/.test(content || "");
}
