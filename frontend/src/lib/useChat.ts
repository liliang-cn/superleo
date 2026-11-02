import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { EventsOn } from "../../wailsjs/runtime";
import { CancelChat, ChatHistory, SendChat } from "../../wailsjs/go/app/App";
import {
  AskSummary,
  ChatCancelled,
  ChatDone,
  ChatError,
  ChatEvent,
  ChatMessage,
  ChatTurn,
  ProgressStep,
  TraceItem,
} from "./types";
import { isInjectedMemory, visibleAnswer } from "./format";
import { traceIdOf } from "./trace";

/**
 * Where the id of the conversation on screen is kept across reloads.
 *
 * The server keeps running a turn whether or not a tab is watching it, but a
 * reload used to open an empty new session, so a long task looked lost and the
 * only way back was to recognise your own question in the history list.
 */
const LAST_SESSION_KEY = "superleo.lastSession";

function rememberSession(id: string): void {
  try {
    window.localStorage.setItem(LAST_SESSION_KEY, id);
  } catch {
    /* private windows and blocked storage are not a reason to fail a chat */
  }
}

function recallSession(): string {
  try {
    return window.localStorage.getItem(LAST_SESSION_KEY) || "";
  } catch {
    return "";
  }
}

let sessionCounter = 0;
function makeId(): string {
  try {
    if (typeof crypto !== "undefined" && crypto.randomUUID)
      return crypto.randomUUID();
  } catch {
    /* ignore */
  }
  sessionCounter += 1;
  return `id-${Date.now()}-${sessionCounter}`;
}

/** A payload waiting for its request id to be bound to an ask. */
type Queued =
  | { kind: "event"; payload: ChatEvent }
  | { kind: "done"; payload: ChatDone }
  | { kind: "error"; payload: ChatError }
  | { kind: "cancelled"; payload: ChatCancelled };

/**
 * Progress text is written for a human but arrives with the model's markdown
 * emphasis still on it ("**Designing synchronous JS tool call**"), and a status
 * line is a single line.
 */
function cleanProgress(raw: string): string {
  return (raw || "")
    .replace(/\s+/g, " ")
    .replace(/^[*_`\s]+/, "")
    .replace(/[*_`\s]+$/, "")
    .trim();
}

function summarizeAsks(messages: ChatMessage[]): AskSummary[] {
  const out: AskSummary[] = [];
  messages.forEach((m, i) => {
    if (m.role !== "assistant" || !m.startedAt) return;
    const prev = messages[i - 1];
    out.push({
      id: m.id,
      prompt: prev && prev.role === "user" ? prev.content : "",
      status: m.streaming
        ? "streaming"
        : m.cancelled
          ? "cancelled"
          : m.error
            ? "error"
            : "done",
    });
  });
  return out;
}

interface UseChatResult {
  sessionId: string;
  messages: ChatMessage[];
  trace: TraceItem[];
  /** The asks this transcript owns, oldest first. */
  asks: AskSummary[];
  /** True while at least one ask is still streaming. Does not block sending. */
  sending: boolean;
  lastEmotion: string;
  send: (text: string, imagePaths?: string[]) => Promise<void>;
  /**
   * Stop a running ask, or every running ask when called with no id. The turn
   * is cancelled on the backend; whatever it already streamed stays in the
   * bubble, and the composer never stops accepting the next question.
   */
  cancel: (askId?: string) => Promise<void>;
  onDone: (cb: () => void) => void;
  reset: () => void;
  clear: () => void;
  /** Restore a past conversation into the transcript and continue it. */
  loadSession: (id: string) => Promise<void>;
  /** Start a fresh conversation, leaving the previous one in history. */
  newSession: () => void;
}

export function useChat(): UseChatResult {
  // The session is state, not a fixed ref: picking a conversation out of
  // history means continuing that session id, not starting a new one.
  const [sessionId, setSessionId] = useState<string>(makeId);
  const [messages, setMessages] = useState<ChatMessage[]>([]);
  const [trace, setTrace] = useState<TraceItem[]>([]);
  const [lastEmotion, setLastEmotion] = useState("");
  const doneCb = useRef<(() => void) | null>(null);

  // An ask is identified by its assistant message id. Nothing about a stream
  // lives in a single "current" slot, so a second question asked while the
  // first is still answering gets its own bubble, trace and progress.
  const bound = useRef(new Map<string, string>()); // requestId -> ask id
  const request = useRef(new Map<string, string>()); // ask id -> requestId, the handle a stop needs
  const queue = useRef(new Map<string, Queued[]>()); // requestId -> events seen before the bind
  const unbound = useRef(0); // sends whose SendChat call has not returned an id yet
  const attached = useRef(new Set<string>()); // asks belonging to the transcript on screen
  const running = useRef(new Set<string>()); // asks that have not finished
  // Stopped asks. A cancelled turn keeps emitting for a moment — the agent loop
  // winds down with a tombstone and a workflow_cancelled on its way out — and
  // none of that must repaint a deliberate stop as a failure.
  const stopped = useRef(new Set<string>());
  // Stops asked for before SendChat returned the id they need. Replayed at the
  // bind, so a stop pressed in the first instant is not silently lost.
  const wantStop = useRef(new Set<string>());
  // Raw stream per ask. The bubble shows visibleAnswer() of this rather than the
  // deltas themselves, and stripping needs the whole text: a <code> block is cut
  // across deltas wherever the provider happens to flush.
  const streamed = useRef(new Map<string, string>());
  // The conversation on screen, for the handlers registered once below.
  const sessionRef = useRef("");
  // Turns another device (or this one, before SendChat answered) started in
  // the conversation on screen, waiting to learn whether they are ours.
  const turnsHeld = useRef(new Map<string, ChatTurn>());
  sessionRef.current = sessionId;

  const patchMessage = useCallback(
    (askId: string, patch: (m: ChatMessage) => ChatMessage) => {
      setMessages((prev) => prev.map((m) => (m.id === askId ? patch(m) : m)));
    },
    [],
  );

  /**
   * A notify_user call becomes its own assistant bubble, placed just before
   * the ask that sent it so the transcript reads in delivery order: interim
   * messages first, the (still-streaming) final answer last.
   */
  const insertInterim = useCallback((askId: string, text: string) => {
    setMessages((prev) => {
      const msg: ChatMessage = {
        id: makeId(),
        role: "assistant",
        kind: "interim",
        content: text,
      };
      const idx = prev.findIndex((m) => m.id === askId);
      if (idx === -1) return [...prev, msg];
      return [...prev.slice(0, idx), msg, ...prev.slice(idx)];
    });
  }, []);

  const pushProgress = useCallback(
    (askId: string, step: Omit<ProgressStep, "id">) => {
      patchMessage(askId, (m) => {
        const steps = m.progress || [];
        const last = steps[steps.length - 1];
        // Agents repeat themselves ("Thinking..." twice in a row); the repeat
        // carries no information, but the same line later in the run does.
        if (last && last.kind === step.kind && last.text === step.text)
          return m;
        return { ...m, progress: [...steps, { id: makeId(), ...step }] };
      });
    },
    [patchMessage],
  );

  const finishAsk = useCallback(
    (
      askId: string,
      opts: {
        final?: string;
        emotion?: string;
        error?: string;
        cancelled?: boolean;
        engine?: string;
        standInFor?: string;
        costUsd?: number;
      },
    ) => {
      running.current.delete(askId);
      streamed.current.delete(askId);
      wantStop.current.delete(askId);
      if (opts.cancelled) stopped.current.add(askId);
      patchMessage(askId, (m) => ({
        ...m,
        content: opts.final && opts.final.length ? opts.final : m.content,
        emotion: opts.emotion || m.emotion,
        error: opts.error || m.error,
        cancelled: opts.cancelled || m.cancelled,
        ...(opts.standInFor ? { engine: undefined, standInFor: opts.standInFor } : { engine: opts.engine || m.engine }),
        costUsd: opts.costUsd || m.costUsd,
        stopping: false,
        streaming: false,
        finishedAt: Date.now(),
      }));
      if (opts.emotion) setLastEmotion(opts.emotion);
      // A tool still marked running when the turn ends never reports back, so
      // resolve it here rather than leaving a clock spinning forever.
      setTrace((prev) =>
        prev.map((t) =>
          t.askId === askId && t.status === "running"
            ? { ...t, status: opts.error ? "fail" : "ok" }
            : t,
        ),
      );
    },
    [patchMessage],
  );

/**
 * One line for a rejected draft, out of the lint's own words.
 *
 * The lint's reason is written TO THE MODEL — "you never called it", "call it
 * now and report what it returned". Showing that to a person reads as an
 * accusation aimed at them. What is worth seeing is that a draft was refused
 * and by which rule; the rest is between the runtime and the model.
 */
function lintRetryLine(content: string | undefined): string {
  const name = /output lint ([a-z_]+)/i.exec(content ?? "")?.[1];
  return name ? `Draft answer rejected by ${name} — retrying` : "Draft answer rejected — retrying";
}

  const applyEvent = useCallback(
    (askId: string, ev: ChatEvent) => {
      if (ev.standInFor) {
        // No engine could take it: SuperLeo's own agent answers.
        const standInFor = ev.standInFor;
        patchMessage(askId, (m) => (m.standInFor === standInFor ? m : { ...m, engine: undefined, standInFor }));
      } else if (ev.engine) {
        const engine = ev.engine;
        patchMessage(askId, (m) => (m.engine === engine ? m : { ...m, engine }));
      }
      switch (ev.type) {
        case "flow": {
          // The turn's flow, first thing it says: its traceparent.
          const traceId = traceIdOf(ev.content);
          if (traceId) patchMessage(askId, (m) => (m.traceId === traceId ? m : { ...m, traceId }));
          break;
        }
        case "partial": {
          if (!ev.content) return;
          const raw = (streamed.current.get(askId) || "") + ev.content;
          streamed.current.set(askId, raw);
          const shown = visibleAnswer(raw);
          patchMessage(askId, (m) =>
            m.content === shown ? m : { ...m, content: shown },
          );
          break;
        }
        case "tombstone": {
          // What was streamed is withdrawn: a draft answer the output lint
          // turned down, or an attempt that failed and is being retried. The
          // next answer is written from scratch, so start the text over. An
          // unmarked tombstone ends the run (stopped, failed); what was
          // streamed is kept then, as it always was.
          if (ev.debugType !== "retry" || !streamed.current.get(askId)) return;
          streamed.current.set(askId, "");
          patchMessage(askId, (m) => (m.content ? { ...m, content: "" } : m));
          break;
        }
        case "thinking":
        case "state_update": {
          const text = cleanProgress(ev.content);
          if (!text) return;
          pushProgress(askId, {
            kind: ev.type === "thinking" ? "thinking" : "state",
            text,
          });
          break;
        }
        case "tool_call": {
          const inner = ev.debugType === "ptc_inner";
          const tool = ev.tool || "tool";
          setTrace((prev) => [
            ...prev,
            {
              id: makeId(),
              askId,
              tool,
              args: ev.args || {},
              inner,
              status: "running",
            },
          ]);
          // A notify_user call IS the message: surface it as its own bubble
          // instead of a "Calling notify_user" progress line.
          if (!inner && tool === "notify_user") {
            const text =
              typeof ev.args?.message === "string" ? ev.args.message.trim() : "";
            if (text) insertInterim(askId, text);
            break;
          }
          // Inner calls are the ones the model's own code makes; there can be
          // dozens of them and the trace panel already shows them nested, so
          // only top-level tools become a progress line.
          if (!inner)
            pushProgress(askId, {
              kind: "tool",
              text: `Calling ${tool}`,
              tool,
            });
          break;
        }
        case "tool_result": {
          setTrace((prev) => {
            // Mark this ask's most recent running match as resolved. Scoping to
            // the ask matters: a sibling turn may be running the same tool.
            const next = [...prev];
            for (let i = next.length - 1; i >= 0; i--) {
              const t = next[i];
              if (t.askId !== askId || t.status !== "running") continue;
              if (ev.tool && t.tool !== ev.tool) continue;
              const r = ev.result;
              const failed =
                r &&
                typeof r === "object" &&
                (r.ok === false || r.error || r.success === false);
              next[i] = { ...t, status: failed ? "fail" : "ok", result: r };
              break;
            }
            return next;
          });
          break;
        }
        case "workflow_error": {
          // Not every workflow_error ends the run. agent-go emits a rejected
          // draft answer with debugType "lint_retry" — the output lint refused
          // what the model was about to say, the model is being re-prompted,
          // and the run carries on and usually succeeds. Treating it as fatal
          // stapled a red error box under a correct answer: the model claimed
          // it had created a schedule without calling the tool, the lint made
          // it actually call the tool, and the user was shown the machinery
          // arguing with itself as if their request had failed.
          if (ev.debugType === "lint_retry") {
            pushProgress(askId, { kind: "state", text: lintRetryLine(ev.content) });
            break;
          }
          finishAsk(askId, { error: ev.content || "workflow error" });
          break;
        }
        case "workflow_cancelled": {
          // The run stopped before it finished — the stop button, or the
          // turn's own deadline. An outcome, not a failure: keep whatever was
          // streamed and settle the ask as cancelled. Normally the stop path
          // has already done this and dispatch drops the event; this is the
          // case where nobody on this side asked for it.
          finishAsk(askId, { cancelled: true });
          break;
        }
        case "workflow_blocked": {
          finishAsk(askId, { final: ev.content || undefined });
          break;
        }
        default:
          break;
      }
    },
    [finishAsk, insertInterim, patchMessage, pushProgress],
  );

  const dispatch = useCallback(
    (askId: string, item: Queued) => {
      // A stopped ask is over. The backend is still winding the turn down and
      // its parting events — a tombstone, a last partial, workflow_cancelled,
      // and finally chat:cancelled itself — would otherwise reopen an ask the
      // user has already finished with.
      if (stopped.current.has(askId)) return;
      switch (item.kind) {
        case "event":
          applyEvent(askId, item.payload);
          break;
        case "done":
          finishAsk(askId, {
            final: item.payload?.final,
            emotion: item.payload?.emotion,
            engine: item.payload?.engine,
            standInFor: item.payload?.standInFor,
            costUsd: item.payload?.costUsd,
          });
          if (doneCb.current) doneCb.current();
          break;
        case "error":
          finishAsk(askId, { error: item.payload?.error || "unknown error" });
          break;
        case "cancelled":
          finishAsk(askId, {
            final: item.payload?.final || undefined,
            cancelled: true,
          });
          break;
      }
    },
    [applyEvent, finishAsk],
  );

  // A turn of this conversation that nobody here asked for and no chat:turn
  // announced (it began while this view was away or hidden: the card in the
  // world, another window). Set below, once adoptTurn and loadSession exist.
  const missed = useRef<(requestId: string, item: Queued) => void>(() => {});

  const route = useCallback(
    (requestId: string, item: Queued) => {
      let askId = requestId ? bound.current.get(requestId) : undefined;
      // An untagged payload can still be placed while exactly one ask is
      // running. That keeps the transcript working against a backend build
      // that does not tag some event type yet, and is unambiguous by
      // construction — with two asks in flight it is dropped instead.
      if (!requestId && running.current.size === 1) {
        askId = running.current.values().next().value;
      }
      if (askId) {
        if (attached.current.has(askId)) dispatch(askId, item);
        return;
      }
      // The backend emits from a goroutine while the Wails call returns
      // separately, so events routinely arrive before we know their id. Hold
      // them until the bind, which replays them in arrival order. If no send is
      // still waiting for an id, nothing can ever claim this payload (the
      // "backend not ready" error is tagged with an id SendChat never returns),
      // so dropping it is the only way it does not leak.
      if (!requestId) return;
      if (unbound.current === 0) {
        missed.current(requestId, item);
        return;
      }
      const q = queue.current.get(requestId);
      if (q) q.push(item);
      else queue.current.set(requestId, [item]);
    },
    [dispatch],
  );

  useEffect(() => {
    // Wails runtime is only present inside the desktop webview; guard so the
    // SPA can also render in a plain browser (dev inspection) without crashing.
    if (typeof window === "undefined" || !(window as any).runtime) {
      return;
    }
    const offEvent = EventsOn("chat:event", (payload: ChatEvent) => {
      if (!payload) return;
      route(payload.requestId || "", { kind: "event", payload });
    });
    const offDone = EventsOn("chat:done", (payload: ChatDone) => {
      route(payload?.requestId || "", { kind: "done", payload });
    });
    const offErr = EventsOn("chat:error", (payload: ChatError) => {
      route(payload?.requestId || "", { kind: "error", payload });
    });
    const offCancel = EventsOn("chat:cancelled", (payload: ChatCancelled) => {
      route(payload?.requestId || "", { kind: "cancelled", payload });
    });
    return () => {
      offEvent();
      offDone();
      offErr();
      offCancel();
    };
  }, [route]);

  /**
   * Follow a turn this window did not send — the same conversation on the
   * phone, the iPad, another tab — in its own pair of bubbles, exactly as if
   * it had been asked here. Whatever of it arrived before this is replayed.
   */
  const adoptTurn = useCallback(
    (turn: ChatTurn) => {
      if (bound.current.has(turn.requestId)) return;
      const askId = makeId();
      const userMsg: ChatMessage = { id: makeId(), role: "user", content: turn.message };
      const assistantMsg: ChatMessage = {
        id: askId,
        role: "assistant",
        content: "",
        streaming: true,
        progress: [],
        startedAt: Date.now(),
      };
      attached.current.add(askId);
      running.current.add(askId);
      bound.current.set(turn.requestId, askId);
      request.current.set(askId, turn.requestId);
      setMessages((prev) => (turn.message ? [...prev, userMsg, assistantMsg] : [...prev, assistantMsg]));
      const held = queue.current.get(turn.requestId);
      queue.current.delete(turn.requestId);
      held?.forEach((item) => dispatch(askId, item));
    },
    [dispatch],
  );

  /** Every held turn that is not one of ours is somebody else's. */
  const adoptHeld = useCallback(() => {
    const held = Array.from(turnsHeld.current.values());
    turnsHeld.current.clear();
    held.forEach((t) => {
      if (t.sessionId === sessionRef.current) adoptTurn(t);
    });
  }, [adoptTurn]);

  /**
   * Tie a request id to the ask that started it and replay whatever arrived in
   * the meantime. Must stay synchronous: an event delivered between the bind
   * and the replay would otherwise overtake the buffered ones.
   */
  const bindAsk = useCallback(
    (askId: string, requestId: string) => {
      unbound.current -= 1;
      turnsHeld.current.delete(requestId);
      const held = queue.current.get(requestId);
      queue.current.delete(requestId);
      if (attached.current.has(askId)) {
        bound.current.set(requestId, askId);
        request.current.set(askId, requestId);
        held?.forEach((item) => dispatch(askId, item));
      }
      // Once every send knows its id, a turn still held was another
      // device's; anything else still held is unclaimable.
      if (unbound.current === 0) {
        adoptHeld();
        queue.current.clear();
      }
    },
    [adoptHeld, dispatch],
  );

  /**
   * Stop the turn behind a request id and settle the ask as cancelled once the
   * backend confirms it. A refusal ("already finished") is left alone: the real
   * terminal event is on its way and it, not this, decides how the ask ended.
   */
  const stopRequest = useCallback(
    async (askId: string, requestId: string) => {
      const clear = () =>
        patchMessage(askId, (m) => (m.stopping ? { ...m, stopping: false } : m));
      patchMessage(askId, (m) => (m.stopping ? m : { ...m, stopping: true }));
      try {
        const res = await CancelChat(requestId);
        if (res === "ok" && running.current.has(askId)) {
          finishAsk(askId, { cancelled: true });
          return;
        }
      } catch {
        /* the ask is ending one way or another; its own event will say how */
      }
      clear();
    },
    [finishAsk, patchMessage],
  );

  const cancel = useCallback(
    async (askId?: string) => {
      const targets = askId ? [askId] : Array.from(running.current);
      await Promise.all(
        targets.map((id) => {
          if (!running.current.has(id)) return Promise.resolve();
          const requestId = request.current.get(id);
          if (requestId) return stopRequest(id, requestId);
          // SendChat has not answered with an id yet. Remember the stop and
          // let the bind carry it through, rather than dropping it and leaving
          // a turn running that the user has already told us to abandon.
          wantStop.current.add(id);
          patchMessage(id, (m) => (m.stopping ? m : { ...m, stopping: true }));
          return Promise.resolve();
        }),
      );
    },
    [patchMessage, stopRequest],
  );

  const send = useCallback(
    async (text: string, imagePaths: string[] = []) => {
      const trimmed = text.trim();
      if (!trimmed && imagePaths.length === 0) return;
      const askId = makeId();
      const userMsg: ChatMessage = {
        id: makeId(),
        role: "user",
        content: trimmed,
      };
      const assistantMsg: ChatMessage = {
        id: askId,
        role: "assistant",
        content: "",
        streaming: true,
        progress: [],
        startedAt: Date.now(),
      };
      attached.current.add(askId);
      running.current.add(askId);
      unbound.current += 1;
      // A new ask retires the tool activity of asks that have already finished,
      // which keeps the panel about what is happening now — but never touches a
      // sibling that is still running.
      setTrace((prev) => prev.filter((t) => running.current.has(t.askId)));
      setMessages((prev) => [...prev, userMsg, assistantMsg]);
      // A session becomes worth restoring once it has been asked something.
      rememberSession(sessionId);
      try {
        const requestId = await SendChat(sessionId, trimmed, imagePaths);
        if (requestId) {
          bindAsk(askId, requestId);
          // A stop pressed before the id came back is honoured now that it can
          // name the run it meant.
          if (wantStop.current.delete(askId)) void stopRequest(askId, requestId);
          return;
        }
        // Nothing was started. The backend also emits a tagged error, but it is
        // tagged with an id we were never given, so report it from here.
        unbound.current -= 1;
        if (unbound.current === 0) {
          adoptHeld();
          queue.current.clear();
        }
        if (attached.current.has(askId)) {
          finishAsk(askId, {
            error: "backend not ready — configure your LLM in Settings",
          });
        }
      } catch (e: any) {
        unbound.current -= 1;
        if (unbound.current === 0) {
          adoptHeld();
          queue.current.clear();
        }
        if (attached.current.has(askId)) {
          finishAsk(askId, { error: String(e?.message || e) });
        }
      }
    },
    [sessionId, adoptHeld, bindAsk, finishAsk, stopRequest],
  );

  const onDone = useCallback((cb: () => void) => {
    doneCb.current = cb;
  }, []);

  /**
   * Let go of every ask on screen without stopping it. Leaving a conversation
   * is not the same act as pressing stop: the turn keeps running and its answer
   * still lands in that conversation's history — it simply stops writing into a
   * transcript it is no longer part of. Use cancel() to actually end one.
   */
  const detachAll = useCallback(() => {
    attached.current.clear();
    running.current.clear();
    bound.current.clear();
    request.current.clear();
    stopped.current.clear();
    wantStop.current.clear();
    queue.current.clear();
  }, []);

  const reset = useCallback(() => {
    detachAll();
    turnsHeld.current.clear();
    setMessages([]);
    setTrace([]);
    setLastEmotion("");
  }, [detachAll]);

  const loadSession = useCallback(
    async (id: string) => {
      const turns = await ChatHistory(id);
      detachAll();
      setMessages(
        (turns || []).map((t) => ({
          id: makeId(),
          role: t.role as ChatMessage["role"],
          content: t.content,
          kind:
            t.kind === "context" || isInjectedMemory(t.role, t.content)
              ? ("context" as const)
              : t.kind === "interim"
                ? ("interim" as const)
                : undefined,
          emotion: t.emotion || undefined,
          engine: t.engine || undefined,
          standInFor: t.stand_in_for || undefined,
          costUsd: t.cost_usd || undefined,
          // The working, recovered from the scripts the transcript hid, so a
          // reopened conversation still shows what the agent did and not just
          // what it concluded. Same progress block as a live turn, already
          // collapsed — the run is over.
          progress: (t.steps || []).map((text) => ({
            id: makeId(),
            kind: "tool" as const,
            text,
          })),
        })),
      );
      setTrace([]);
      setSessionId(id);
      rememberSession(id);
    },
    [detachAll],
  );

  // A turn started in this conversation somewhere else — the phone, the iPad,
  // another tab — is followed live, as if it had been asked here.
  useEffect(() => {
    if (typeof window === "undefined" || !(window as any).runtime) return;
    return EventsOn("chat:turn", (t: ChatTurn) => {
      if (!t?.requestId || !t.sessionId || t.sessionId !== sessionRef.current) return;
      if (bound.current.has(t.requestId)) return;
      // A send of ours has not learned its id yet, and this may be it. Held
      // until it does; its events are held under the same id meanwhile.
      if (unbound.current > 0) {
        turnsHeld.current.set(t.requestId, t);
        return;
      }
      adoptTurn(t);
    }) as unknown as () => void;
  }, [adoptTurn]);

  // Back from a dropped stream, whatever was said meanwhile — here or on
  // another device — is read again, unless an answer is streaming in here.
  const loadRef = useRef(loadSession);
  loadRef.current = loadSession;
  useEffect(() => {
    if (typeof window === "undefined" || !(window as any).runtime) return;
    const again = () => {
      if (running.current.size > 0 || unbound.current > 0) return;
      void loadRef.current(sessionRef.current).catch(() => {});
    };
    const offs = [
      EventsOn("stream:reconnect", again),
      // A linked desktop window: the queen's stream came back.
      EventsOn("hivelink:state", (p: { live?: boolean }) => { if (p?.live) again(); }),
    ];
    return () => offs.forEach((off) => typeof off === "function" && off());
  }, []);

  // The conversation is read again when the window is shown (the card in the
  // world maximized into it) or comes back to the front, and a turn it never
  // saw begin is followed from its first event or read when it ends, so what
  // was said in a card is never missing here.
  useEffect(() => {
    missed.current = (requestId, item) => {
      const sid = (item.payload as { sessionId?: string } | undefined)?.sessionId;
      if (!sid || sid !== sessionRef.current || bound.current.has(requestId)) return;
      if (item.kind === "event") {
        queue.current.set(requestId, [item]);
        adoptTurn({ requestId, sessionId: sid, message: "" });
      } else if (running.current.size === 0) {
        void loadRef.current(sid).catch(() => {});
      }
    };
  }, [adoptTurn]);
  useEffect(() => {
    if (typeof window === "undefined") return;
    const again = () => {
      if (running.current.size > 0 || unbound.current > 0) return;
      void loadRef.current(sessionRef.current).catch(() => {});
    };
    const onShow = () => { if (!document.hidden) again(); };
    window.addEventListener("superleo:window", again);
    window.addEventListener("focus", again);
    document.addEventListener("visibilitychange", onShow);
    return () => {
      window.removeEventListener("superleo:window", again);
      window.removeEventListener("focus", again);
      document.removeEventListener("visibilitychange", onShow);
    };
  }, []);

  // Restore the conversation that was on screen before the reload. Only the
  // transcript comes back: a turn still running on the server keeps running and
  // lands in history when it finishes, which beats showing an empty page and
  // implying the work was lost.
  const restored = useRef(false);
  useEffect(() => {
    if (restored.current) return;
    restored.current = true;
    const previous = recallSession();
    if (!previous || previous === sessionId) return;
    void loadSession(previous).catch(() => {
      /* a session the server no longer has just leaves the new empty one */
    });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const newSession = useCallback(() => {
    const id = makeId();
    setSessionId(id);
    rememberSession(id);
    reset();
  }, [reset]);

  const sending = messages.some((m) => m.streaming);
  // Keyed on a signature rather than the array so the trace panel is not
  // re-rendered by every streamed token; the prompts it reads never change
  // after their message is appended.
  const askSignature = messages
    .map((m) =>
      m.startedAt
        ? `${m.id}:${
            m.streaming ? "s" : m.cancelled ? "c" : m.error ? "e" : "d"
          }`
        : "",
    )
    .join("|");
  // eslint-disable-next-line react-hooks/exhaustive-deps
  const asks = useMemo(() => summarizeAsks(messages), [askSignature]);

  return {
    sessionId,
    messages,
    trace,
    asks,
    sending,
    lastEmotion,
    send,
    cancel,
    onDone,
    reset,
    clear: reset,
    loadSession,
    newSession,
  };
}
