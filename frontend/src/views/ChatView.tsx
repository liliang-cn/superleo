import React, { useCallback, useEffect, useState } from "react";
import { useChat } from "../lib/useChat";
import { useRoom } from "../lib/useViewport";
import { useAttachments } from "../lib/useAttachments";
import { AppStatus, ChatMessage } from "../lib/types";
import { copyText, engineByline, withoutCallNotes } from "../lib/format";
import AttachmentChips from "../components/AttachmentChips";
import { SentMessage } from "../components/SentFiles";
import RunTracePanel from "../components/RunTracePanel";
import TraceChip from "../components/TraceChip";
import { AgentMenu, AddressedBanner } from "../components/AgentMenu";
import { useAgentMentions } from "../lib/useAgentMentions";
import {
  Conversation,
  ConversationContent,
  ConversationEmptyState,
} from "@/components/ai-elements/conversation";
import {
  Message,
  MessageByline,
  MessageContent,
} from "@/components/ai-elements/message";
import { Response } from "@/components/ai-elements/response";
import { Actions, Action } from "@/components/ai-elements/actions";
import {
  PromptInput,
  PromptInputTextarea,
  PromptInputToolbar,
  PromptInputTools,
  PromptInputSubmit,
} from "@/components/ai-elements/prompt-input";
import { Button } from "@/components/ui/button";
import {
  CopyIcon,
  RefreshCwIcon,
  MessageSquareIcon,
  Trash2Icon,
  PaperclipIcon,
  SquareIcon,
  LayoutDashboardIcon,
} from "lucide-react";
import { HistoryBar } from "../components/HistoryBar";
import SidePanel from "../components/SidePanel";
import DeliverablesBar from "../components/DeliverablesBar";
import ContextBlock from "../components/ContextBlock";
import NameDashboardModal from "../components/NameDashboardModal";
import { dashboards, hasRenderableBlock, suggestName } from "../lib/dashboards";
import AgentProgress from "../components/AgentProgress";
import { ChatTurnBrowser, newestBrowserTurn } from "../components/BrowserCard";
import { isBrowserStep } from "../lib/browser";
import { machine } from "../lib/machine";

/**
 * What a finished answer cost, for the line under its name: "34s · 6 tools".
 *
 * Only the finished shape is written here. While an answer streams, the
 * progress block directly beneath the byline is already counting — a second
 * live clock would be the same number twice, a frame out of step. And a
 * conversation restored from history has the steps but never had the clock, so
 * the duration is left out rather than printed as a zero.
 */
function bylineMeta(m: ChatMessage): string {
  if (m.streaming) return "";
  const bits: string[] = [];
  if (m.startedAt && m.finishedAt && m.finishedAt > m.startedAt) {
    const s = Math.max(1, Math.round((m.finishedAt - m.startedAt) / 1000));
    bits.push(s < 60 ? `${s}s` : `${Math.floor(s / 60)}m ${s % 60}s`);
  }
  const tools = m.progress?.filter((p) => p.kind === "tool").length ?? 0;
  if (tools) bits.push(`${tools} tool${tools === 1 ? "" : "s"}`);
  if (m.costUsd) bits.push(`$${m.costUsd.toFixed(3)}`);
  return bits.join(" · ");
}

export default function ChatView({
  status,
  openSession,
  onSessionOpened,
}: {
  status: AppStatus | null;
  /** A conversation to restore on arrival — a scheduled run's, for instance. */
  openSession?: string;
  onSessionOpened?: () => void;
}) {
  const chat = useChat();
  const [traceOpen, setTraceOpen] = useState(false);
  const attach = useAttachments();
  const notReady = status !== null && !status.ready;
  const messages = chat.messages;
  const newestBrowser = newestBrowserTurn(messages, chat.trace);
  const room = useRoom();
  const [filesKey, setFilesKey] = useState(0);
  // The composer is controlled so the send button can know whether there is
  // anything to send — which is what decides between "send" and "stop" below.
  const [draft, setDraft] = useState("");
  // The @ menu, and whether this message is addressed to another agent.
  const mentions = useAgentMentions(draft, setDraft);

  /**
   * A name picked from the Agents panel.
   *
   * It goes to the front of the message rather than to the caret, because the
   * front is the position that means something: that is what makes the message
   * an address. Someone who wants a mid-sentence mention is already typing and
   * has the @ menu under their fingers.
   *
   * An address already there is replaced, not stacked — picking a second name
   * is changing your mind about who to ask, and "@pi @hermes what" is not
   * something the parser or the person means.
   */
  const mentionFromPanel = useCallback((name: string) => {
    setDraft((prev) => {
      const body = prev.replace(/^[ \t]*@[\p{L}\p{N}._-]+(?:[ \t]*[:,：，][ \t]*|\s+|$)/u, "");
      return `@${name} ${body}`;
    });
    requestAnimationFrame(() => {
      const el = document.querySelector<HTMLTextAreaElement>("textarea[name=message]");
      if (!el) return;
      el.focus();
      // After the name, before whatever was already written: the question is
      // what gets typed next.
      const at = name.length + 2;
      el.setSelectionRange(at, at);
    });
  }, []);
  // The message index whose save just landed, for a moment of green.
  const [saved, setSaved] = useState(-1);
  // The reply waiting to be named. Null when the dialog is closed.
  const [naming, setNaming] = useState<{ index: number; content: string; prompt: string } | null>(null);
  const [saveError, setSaveError] = useState("");

  // Loading it here rather than in App keeps the transcript the only thing that
  // owns a session id. Picking a run's conversation is the same act as picking
  // one out of history.
  useEffect(() => {
    if (!openSession) return;
    chat.loadSession(openSession).catch(() => {});
    onSessionOpened?.();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [openSession]);

  useEffect(() => {
    // A finished turn may have written files and definitely changed history.
    chat.onDone(() => {
      setFilesKey((k) => k + 1);
    });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // Asking again while an answer is still streaming is allowed: each ask gets
  // its own bubble, so there is nothing to wait for.
  /**
   * Ask again for the answer at `index`, by re-sending the question that
   * produced it.
   *
   * The previous answer stays. Replacing it in place would be the tidier
   * screen and the worse record: a model asked twice gives two different
   * answers, and which one a person acted on is not recoverable from a
   * transcript that kept only the last.
   */
  /** The ask a reply answered: the nearest user message above it. */
  const askBehind = useCallback(
    (index: number): string => {
      for (let i = index - 1; i >= 0; i--) {
        const prev = messages[i];
        if (prev.role === "user" && prev.kind !== "context" && prev.content) {
          return prev.content;
        }
      }
      return "";
    },
    [messages],
  );

  const regenerate = (index: number) => {
    const prompt = askBehind(index);
    if (prompt) chat.send(prompt);
  };

  /**
   * Keep a reply, and the question behind it.
   *
   * The question is what makes a saved dashboard more than a screenshot: it is
   * what Refresh re-asks. So it is saved even though nothing here shows it —
   * and a reply with no ask above it (a scheduled run opened from Records, say)
   * still saves, it just cannot refresh itself, which the panel says.
   */
  const saveDashboard = (index: number, content: string) =>
    setNaming({ index, content, prompt: askBehind(index) });

  const commitDashboard = async (name: string) => {
    if (!naming) return;
    const { index, content, prompt } = naming;
    setNaming(null);
    try {
      await dashboards.save(name, content, prompt);
      setSaved(index);
      setTimeout(() => setSaved(-1), 1600);
    } catch (e: any) {
      setSaveError(String(e?.message || e));
      setTimeout(() => setSaveError(""), 4000);
    }
  };

  const onSubmit = (msg: { text: string }) => {
    const text = msg.text.trim();
    if (!text && attach.paths.length === 0) return;
    // Images go straight to the vision model as multimodal input; docs are read
    // via read_document from their workspace path referenced in the text.
    const { payload, images } = attach.build(text);
    attach.clear();
    chat.send(payload, images);
    setDraft("");
  };

  return (
    <div className="view">
      <div className="chat-layout">
        <div
          className="chat-main"
          style={{ ["--wails-drop-target" as any]: "drop" }}
        >
          {/* Starting a new conversation mid-answer is safe: the running ask
              finishes into its own conversation's history. */}
          <HistoryBar
            onNew={chat.newSession}
            sessionId={chat.sessionId}
            onTrace={() => setTraceOpen(true)}
          />
          {traceOpen && (
            <RunTracePanel
              taskId={chat.sessionId}
              live={chat.sending}
              onClose={() => setTraceOpen(false)}
            />
          )}

          {(
            <Conversation className="transcript">
              {messages.length === 0 ? (
                <ConversationEmptyState
                  icon={<MessageSquareIcon className="size-10" />}
                  title="Start a conversation"
                  description="Chat with SuperLeo. It can use tools, search memory and the web, and stream its reasoning live."
                >
                  {notReady && (
                    <div className="empty-hint">
                      Sign in to Claude Code on this {machine}, or choose a model in Settings, to begin.
                    </div>
                  )}
                </ConversationEmptyState>
              ) : (
                <ConversationContent
                  // The column the whole conversation is set in: centred, one
                  // measure wide, with the messages spaced by whitespace rather
                  // than by bubble borders.
                  className="conv-stream"
                  autoScrollKey={messages
                    .map((m) => `${m.content}#${m.progress?.length ?? 0}`)
                    .join("|")}
                  // Opening a different conversation is an arrival, not a
                  // message arriving: land at the bottom rather than gliding
                  // there from the top.
                  jumpKey={chat.sessionId}
                >
                  {messages.map((m, mi) =>
                    m.kind === "context" ? (
                      <ContextBlock key={m.id} content={m.content} />
                    ) : (
                      <div
                        key={m.id}
                        className={`msg-turn ${m.role === "user" ? "is-user" : "is-assistant"}`}
                      >
                        <Message from={m.role}>
                          <MessageContent>
                            {m.role === "assistant" ? (
                              <>
                                {/* The copper rule says "not you"; the name
                                    says who. It goes above the progress block
                                    so the turn is signed before it reports. */}
                                <MessageByline
                                  name="SuperLeo"
                                  meta={bylineMeta(m)}
                                  engine={engineByline(m)}
                                  trace={m.traceId ? <TraceChip traceId={m.traceId} session={chat.sessionId} /> : undefined}
                                />
                                {/* Browser calls leave the step list for
                                    their own card, which shows the page. */}
                                {m.progress && m.progress.some((p) => !isBrowserStep(p)) && (
                                  <AgentProgress
                                    steps={m.progress.filter((p) => !isBrowserStep(p))}
                                    running={!!m.streaming}
                                    startedAt={m.startedAt}
                                    finishedAt={m.finishedAt}
                                  />
                                )}
                                <ChatTurnBrowser m={m} trace={chat.trace} newest={newestBrowser} />
                                {m.content ? (
                                  <Response>{withoutCallNotes(m.content)}</Response>
                                ) : m.streaming ? (
                                  // The progress block carries its own spinner, so
                                  // the caret is only needed before the first sign
                                  // of life.
                                  !m.progress?.length && (
                                    <span className="msg-cursor" />
                                  )
                                ) : (
                                  !m.error && !m.progress?.length && "…"
                                )}
                                {/* A stop is the user's own doing, so it is
                                    reported as an outcome and not as a fault —
                                    and whatever was already streamed stays. */}
                                {m.cancelled && (
                                  <div className="msg-stopped">
                                    ■ Stopped — you cancelled this answer.
                                  </div>
                                )}
                                {m.stopping && !m.cancelled && (
                                  <div className="msg-stopped">Stopping…</div>
                                )}
                                {m.error && !m.cancelled && (
                                  <div className="msg-error">⚠ {m.error}</div>
                                )}
                              </>
                            ) : (
                              <SentMessage content={withoutCallNotes(m.content)} />
                            )}
                          </MessageContent>
                        </Message>
                        {/* One row under the reply: the copy action and the
                            mood chip are both about the message that just
                            finished, and stacking them made two half-empty
                            lines out of one. */}
                        {m.role === "assistant" &&
                          !m.streaming &&
                          (m.content || m.emotion) && (
                            <div className="msg-footer">
                              {m.content && (
                                <Actions>
                                  <Action
                                    label="Copy"
                                    tooltip="Copy message"
                                    onClick={() => copyText(m.content)}
                                  >
                                    <CopyIcon className="size-3.5" />
                                  </Action>
                                  <Action
                                    label="Regenerate"
                                    tooltip="Ask again"
                                    disabled={chat.sending}
                                    onClick={() => regenerate(mi)}
                                  >
                                    <RefreshCwIcon className="size-3.5" />
                                  </Action>
                                  {/* Only on a reply that actually drew
                                      something. Prose is worth copying, not
                                      pinning to a wall. */}
                                  {hasRenderableBlock(m.content) && (
                                    <Action
                                      label="Save as dashboard"
                                      tooltip={
                                        saved === mi
                                          ? "Saved"
                                          : "Save as dashboard"
                                      }
                                      onClick={() => saveDashboard(mi, m.content)}
                                    >
                                      <LayoutDashboardIcon
                                        className="size-3.5"
                                        style={
                                          saved === mi
                                            ? { color: "var(--green)" }
                                            : undefined
                                        }
                                      />
                                    </Action>
                                  )}
                                </Actions>
                              )}
                              {m.emotion && (
                                <div className="emotion-chip">🎭 {m.emotion}</div>
                              )}
                            </div>
                          )}
                      </div>
                    ),
                  )}
                </ConversationContent>
              )}
            </Conversation>
          )}

          <div data-pet-spot="files" data-pet-label="the row of files this conversation produced">
            <DeliverablesBar sessionId={chat.sessionId} refreshKey={filesKey} />
          </div>

          <div className="composer" data-pet-spot="composer" data-pet-label="the box you type a message into">
            <AttachmentChips paths={attach.paths} onRemove={attach.remove} />
            <AddressedBanner agent={mentions.addressee} />
            {/* Outside the form, not inside it: PromptInput is rounded and
                carries overflow-hidden, which would clip a menu drawn above
                the input to nothing. */}
            <AgentMenu
              matches={mentions.matches}
              active={mentions.active}
              onPick={(a) => {
                const next = mentions.accept(a);
                // Put the caret after the name it just inserted, or the next
                // character typed lands wherever the browser left it.
                if (next) {
                  requestAnimationFrame(() => {
                    const el = document.querySelector<HTMLTextAreaElement>("textarea[name=message]");
                    el?.focus();
                    el?.setSelectionRange(next.caret, next.caret);
                  });
                }
              }}
            />
            <PromptInput onSubmit={onSubmit}>
              <PromptInputTextarea
                value={draft}
                onChange={(e) => {
                  setDraft(e.target.value);
                  mentions.update(e.target.value, e.target.selectionStart ?? e.target.value.length);
                }}
                // The menu gets the arrows and Enter first. It calls
                // preventDefault on the ones it takes, which is what stops the
                // textarea from also submitting — see prompt-input.tsx.
                onKeyDown={mentions.onKeyDown}
                onBlur={mentions.close}
                onPaste={attach.paste}
                placeholder={
                  notReady
                    ? "Choose a model in Settings first"
                    : room === "phone"
                      ? "Message SuperLeo"
                      : mentions.agents.length > 0
                        ? "Message SuperLeo. @ reaches another agent; drop files or paste a screenshot."
                        : "Message SuperLeo. Drop files or paste a screenshot."
                }
              />
              <PromptInputToolbar>
                <PromptInputTools>
                  <Button
                    type="button"
                    variant="ghost"
                    size="sm"
                    className="text-muted-foreground"
                    onClick={attach.pick}
                    disabled={attach.importing}
                    data-pet-spot="attach"
                    data-pet-label="the paperclip that attaches a file"
                    title="Attach files (Word / Excel / PPT / PDF / image)"
                  >
                    <PaperclipIcon className="size-3.5" />
                    {attach.importing ? "Importing…" : "Attach"}
                  </Button>
                  <Button
                    type="button"
                    variant="ghost"
                    size="sm"
                    className="text-muted-foreground"
                    onClick={() => chat.clear()}
                    disabled={chat.messages.length === 0}
                    title="Clear conversation"
                  >
                    <Trash2Icon className="size-3.5" />
                    Clear
                  </Button>
                </PromptInputTools>
                {/* While an answer is streaming and nothing has been typed, the
                    send button is a stop button — the same place ChatGPT and
                    Claude put it. Type anything and it turns back into send, so
                    asking a second question mid-answer (which this transcript
                    supports) is never taken away; ⏎ sends either way. */}
                {chat.sending && draft.trim() === "" ? (
                  <Button
                    type="button"
                    size="icon"
                    variant="secondary"
                    className="rounded-lg"
                    title="Stop generating"
                    aria-label="Stop generating"
                    onClick={() => chat.cancel()}
                  >
                    <SquareIcon className="size-3.5 fill-current" />
                  </Button>
                ) : (
                  <PromptInputSubmit status="ready" data-pet-spot="send" data-pet-label="the send button" />
                )}
              </PromptInputToolbar>
            </PromptInput>
          </div>
        </div>
        <SidePanel
          trace={chat.trace}
          asks={chat.asks}
          sessionId={chat.sessionId}
          draft={draft}
          onMention={mentionFromPanel}
        />
      </div>
      {naming && (
        <NameDashboardModal
          suggested={suggestName(naming.prompt)}
          prompt={naming.prompt}
          onCancel={() => setNaming(null)}
          onSave={commitDashboard}
        />
      )}
      {saveError && <div className="nd-error">⚠ {saveError}</div>}
    </div>
  );
}
