import React, { useState } from "react";
import { useNavigate } from "react-router-dom";
import {
  CheckIcon,
  HashIcon,
  HistoryIcon,
  PlusIcon,
  ScrollTextIcon,
} from "lucide-react";
import { copyText } from "../lib/format";
import { PATHS } from "../lib/routes";

/** The History / New chat buttons, and which conversation this is. */
export function HistoryBar({
  onNew,
  sessionId,
  onTrace,
}: {
  onNew: () => void;
  /** The conversation on screen. Shown so it can be named elsewhere. */
  sessionId?: string;
  /** Opens what this conversation's runs sent the model and what tools did. */
  onTrace?: () => void;
}) {
  const [copied, setCopied] = useState(false);
  const navigate = useNavigate();
  return (
    <div className="chat-toolbar">
      <button
        className="btn ghost sm"
        onClick={() => navigate(PATHS.conversations)}
        title="Conversations"
      >
        <HistoryIcon className="size-3.5" /> Conversations
      </button>
      <button
        className="btn ghost sm"
        onClick={onNew}
        title="Start a new conversation"
      >
        <PlusIcon className="size-3.5" /> New chat
      </button>
      {onTrace && sessionId && (
        <button
          className="btn ghost sm"
          onClick={onTrace}
          title="Every model call and tool call in this conversation"
        >
          <ScrollTextIcon className="size-3.5" /> Trace
        </button>
      )}
      {/* The id is what identifies a conversation to everything outside this
          window — a schedule appends its runs to one, the daemon logs by it —
          so it has to be readable and copyable, not only inferable from a
          screenshot of the URL. Truncated because the middle of a UUID tells
          nobody anything; the whole thing goes to the clipboard. */}
      {sessionId && (
        <button
          className="session-id"
          title={`Conversation ${sessionId} — click to copy`}
          onClick={() => {
            copyText(sessionId);
            setCopied(true);
            window.setTimeout(() => setCopied(false), 1200);
          }}
        >
          {copied ? (
            <CheckIcon className="size-3" />
          ) : (
            <HashIcon className="size-3" />
          )}
          <span className="session-id-text">{shortId(sessionId)}</span>
        </button>
      )}
    </div>
  );
}

/** Enough of an id to recognise it by, without the unreadable middle. */
function shortId(id: string): string {
  return id.length > 13 ? `${id.slice(0, 8)}…${id.slice(-4)}` : id;
}
