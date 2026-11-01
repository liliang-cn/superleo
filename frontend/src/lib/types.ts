/** Appearance: how dark the chassis is, and which finish is on it. */
export type Theme = "dark" | "light";
export type Accent = "signal" | "copper" | "cobalt" | "jade" | "rose" | "violet";

export type ViewKey =
  | "home"
  | "chat"
  | "conversations"
  | "stats"
  | "settings"
  | "knowledge"
  | "skills"
  | "mcp"
  | "records"
  | "hive"
  | "agents"
  | "coding"
  | "tasks"
  | "schedule"
  | "dashboards";

/**
 * Every streaming payload is tagged with the id `SendChat` returned for that
 * ask. One conversation can have several asks in flight, and the tag is the
 * only thing that says which bubble an event belongs to.
 */
export interface ChatEvent {
  requestId: string;
  /** The conversation, on a core that says (chat:turn's sessionId). */
  sessionId?: string;
  type: string;
  content: string;
  tool: string;
  args: Record<string, any>;
  result: any;
  debugType: string;
  /** The agent CLI thinking for the queen ("claude", "claude.mac"); absent for SuperLeo's own agent. */
  engine?: string;
  /** The engine SuperLeo's own agent answers for, when none in the chain could. */
  standInFor?: string;
}

/**
 * A turn the core took in a conversation, from any device: the words that
 * started it, before any of its events. A window on the same conversation
 * that did not send it follows it under its request id.
 */
export interface ChatTurn {
  requestId: string;
  sessionId: string;
  message: string;
  at?: string;
  images?: number;
}

export interface ChatDone {
  requestId: string;
  final: string;
  emotion: string;
  engine?: string;
  standInFor?: string;
  /** What the engine's turn cost, when its CLI said. */
  costUsd?: number;
}

export interface ChatError {
  requestId: string;
  error: string;
}

/**
 * The user stopped this ask. Its own terminal event rather than an error,
 * because a stop is an outcome the user chose — the partial answer stays and
 * nothing about it is a fault. `final` is normally empty and is only set in the
 * race where the turn completed as the stop landed.
 */
export interface ChatCancelled {
  requestId: string;
  final: string;
}

export interface AppStatus {
  ready: boolean;
  error: string;
  skills: string[];
  memoryMode: string;
  /** Running MCP servers, and the tools they add up to. */
  mcp: number;
  mcpTools: number;
  avatarPort: number;
  /** False only when the backend says so — see normalizeStatus. */
  scheduler: boolean;
  schedulerError: string;
}

/**
 * One line of "what the agent is doing" — the thinking and state_update events
 * the model emits between tokens. Without these the UI is silent for the whole
 * time a generated-code tool is being written and run.
 */
export interface ProgressStep {
  id: string;
  kind: "thinking" | "state" | "tool";
  text: string;
  /** Set for `tool` steps so the name can be rendered as code. */
  tool?: string;
}

export interface ChatMessage {
  id: string;
  role: "user" | "assistant";
  content: string;
  /**
   * "context" is what the agent was given to read before answering — recalled
   * memory, injected as a user message. Shown folded rather than as a bubble:
   * nobody typed it, but it is the reason the answer says what it says.
   *
   * "interim" is a standalone message the agent chose to send mid-turn via the
   * notify_user tool — a real bubble of its own, delivered before the final
   * answer of the ask that produced it.
   */
  kind?: "context" | "interim";
  emotion?: string;
  streaming?: boolean;
  /** Progress lines captured while this ask was running. */
  progress?: ProgressStep[];
  /** This ask failed. Kept per message so one bad ask cannot blank a sibling. */
  error?: string;
  /** The user stopped this ask. Distinct from `error`: nothing went wrong. */
  cancelled?: boolean;
  /** A stop has been asked for and the backend has not confirmed it yet. */
  stopping?: boolean;
  /**
   * Wall clock of the ask, used for the "worked for 21s" summary. Only bubbles
   * this client streamed have them, which also distinguishes a live ask from a
   * message restored out of history.
   */
  startedAt?: number;
  finishedAt?: number;
  /** Who thought for the queen in this answer, when it was not SuperLeo's own agent. */
  engine?: string;
  /** The engine SuperLeo's own agent answered for, when no engine could. */
  standInFor?: string;
  costUsd?: number;
  /** The flow this answer ran in: one trace id across its runs, tools,
   *  workers and CortexDB calls. */
  traceId?: string;
}

export interface TraceItem {
  id: string;
  /** The assistant message (i.e. the ask) whose turn ran this tool. */
  askId: string;
  tool: string;
  args: Record<string, any>;
  inner: boolean;
  status: "running" | "ok" | "fail";
  result?: any;
}

/** An ask, as far as anything outside the transcript needs to know about it. */
export interface AskSummary {
  id: string;
  /** The question that started it, for labelling its tool activity. */
  prompt: string;
  status: "streaming" | "done" | "error" | "cancelled";
}

export function normalizeStatus(raw: Record<string, any> | null): AppStatus {
  return {
    ready: Boolean(raw?.ready),
    error: String(raw?.error ?? ""),
    skills: Array.isArray(raw?.skills) ? raw!.skills : [],
    memoryMode: String(raw?.memoryMode ?? "—"),
    mcp: Number(raw?.mcp ?? 0),
    mcpTools: Number(raw?.mcpTools ?? 0),
    avatarPort: Number(raw?.avatarPort ?? 0),
    // Absent reads as running: the Schedules view warns when this is false, and
    // a warning about a healthy scheduler is worse than none at all.
    scheduler: raw?.scheduler !== false,
    schedulerError: String(raw?.schedulerError ?? ""),
  };
}

export function briefArgs(args: Record<string, any> | undefined | null): string {
  if (!args || typeof args !== "object") return "";
  const parts: string[] = [];
  for (const [k, v] of Object.entries(args)) {
    let val: string;
    if (v == null) val = "";
    else if (typeof v === "string") val = v;
    else if (typeof v === "object") val = JSON.stringify(v);
    else val = String(v);
    if (val.length > 48) val = val.slice(0, 45) + "…";
    parts.push(`${k}=${val}`);
    if (parts.join(" ").length > 70) break;
  }
  return parts.join(" ");
}
