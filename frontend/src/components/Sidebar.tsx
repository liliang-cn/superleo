import { useI18n } from "../lib/i18n";
import React, { useEffect } from "react";
import {
  LayoutDashboardIcon,
  ListTodoIcon,
  BrainIcon,
  CalendarDaysIcon,
  ChartColumnIcon,
  GlobeIcon,
  HexagonIcon,
  LayoutGridIcon,
  LogOutIcon,
  BotIcon,
  TerminalIcon,
  MessageSquareIcon,
  MessagesSquareIcon,
  PanelLeftCloseIcon,
  PanelLeftOpenIcon,
  PlugIcon,
  PuzzleIcon,
  SettingsIcon,
} from "lucide-react";
import { ViewKey } from "../lib/types";
import { openBrowserPanel } from "../lib/browser";
import { useRoom } from "../lib/useViewport";

const served = Boolean((window as unknown as Record<string, unknown>).superleoServed);

// The nav used to carry one emoji per row (💬📊🧠🧩🔌🗂️⚙️). On the old slate
// ground they passed for icons; on warm paper they are seven colour stickers
// stuck to a sheet of card, each drawn by a different hand at a different
// weight, and none of them the copper the rest of the room is finished in.
// Lucide is one hand, one weight, and inherits `color` — so a row's icon goes
// copper when the row is active instead of staying a coloured decal on top of
// it.
//
// `shortcut` is the key that reaches this view with ⌘ held. It lives on the
// item rather than in a second table beside the handler, because the hint that
// is painted and the key that is bound drifting apart is the one bug a
// shortcut hint can have that is worse than having no hint at all.
const NAV: {
  section: string;
  // "browser" is not a page: it opens the live browser over whichever page is up.
  items: { key: ViewKey | "browser"; label: string; Icon: typeof MessageSquareIcon; shortcut?: string }[];
}[] = [
  {
    section: "Workspace",
    items: [
      { key: "home", label: "Home", Icon: LayoutGridIcon },
      { key: "dashboards", label: "Dashboards", Icon: LayoutDashboardIcon },
      { key: "schedule", label: "Schedule", Icon: CalendarDaysIcon },
      { key: "tasks", label: "Needs your attention", Icon: ListTodoIcon },
      { key: "chat", label: "Chat", Icon: MessageSquareIcon, shortcut: "1" },
      { key: "conversations", label: "Conversations", Icon: MessagesSquareIcon },
      { key: "stats", label: "Stats", Icon: ChartColumnIcon, shortcut: "2" },
      { key: "hive", label: "Hive", Icon: HexagonIcon },
      { key: "agents", label: "Bees", Icon: BotIcon },
      { key: "coding", label: "Coding", Icon: TerminalIcon },
      { key: "browser", label: "Browser", Icon: GlobeIcon },
    ],
  },
  {
    section: "Knowledge",
    items: [
      // Next to Memory on purpose: it is the same store, seen whole instead of
      // one recall at a time.
      { key: "knowledge", label: "Knowledge", Icon: BrainIcon },
      { key: "skills", label: "Skills", Icon: PuzzleIcon },
      { key: "mcp", label: "MCP", Icon: PlugIcon },
    ],
  },
  {
    section: "System",
    items: [
      { key: "settings", label: "Settings", Icon: SettingsIcon, shortcut: "," },
    ],
  },
];

/** Every bound key, flattened once at module load rather than per keystroke. */
const BY_SHORTCUT = new Map<string, ViewKey>(
  NAV.flatMap((g) => g.items)
    .filter((it) => it.shortcut)
    .map((it) => [it.shortcut as string, it.key as ViewKey]),
);

export default function Sidebar({
  current,
  onNavigate,
  badges,
  open,
  onToggle,
}: {
  current: ViewKey;
  onNavigate: (v: ViewKey) => void;
  /** Counts of things that happened on their own, per view. */
  badges?: Partial<Record<ViewKey, number>>;
  /** Expanded on a desktop; on a phone this is the drawer being out. The state
   *  lives in App because on a narrow screen the button that opens it cannot be
   *  in here — a closed drawer has translated itself off the screen, and its own
   *  toggle with it. */
  open: boolean;
  onToggle: () => void;
}) {
  const { t } = useI18n();
  // "Collapsed" means two different things depending on how much room there is,
  // and the component used to only know the desktop one. On a desktop it is the
  // 60px icon rail: navigation stays one click away, it just stops spending
  // width on labels. On a phone the very same flag means the drawer is shut —
  // and a shut drawer is off the screen entirely, so there is no width to save.
  // The stylesheet had already noticed this and put the label spacing back
  // under its 640px query, but CSS can only lay out text that was rendered, and
  // this tree rendered none: the drawer slid out 232px wide showing a column of
  // seven emoji and no words. So the room decides, not the flag.
  const room = useRoom();
  const labels = open || room === "phone";

  // ⌘1 / ⌘2 / ⌘3 / ⌘, — the hints in the rows, made real.
  //
  // Three things this deliberately does not do:
  //
  // 1. It does not refuse to fire while a text field has focus. The whole
  //    point is leaving the composer mid-sentence to check Stats, and a
  //    shortcut that stops working exactly where you spend your time is a
  //    shortcut nobody learns. It is safe here because every binding requires
  //    ⌘: with Command held, none of "1" "2" "3" "," puts a character into an
  //    input, so there is no keystroke left to swallow.
  // 2. It does not fire mid-composition. A Chinese IME uses the digit row to
  //    pick candidates, and while WKWebView is reordering composition events
  //    (see lib/ime.ts) a stray modifier read could turn "选第 2 个候选" into
  //    "go to Stats". isComposing plus the legacy 229 keyCode is the same
  //    guard the composer uses.
  // 3. It does not bind outside the desktop window. In a browser tab ⌘1–⌘3
  //    switch the browser's own tabs, and preventDefault on that is taking
  //    something away from the user that was never ours. The body class is the
  //    same switch the hint's CSS keys on — read at keystroke time, because
  //    main.tsx sets it in an effect that runs after this one.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (!document.body.classList.contains("desktop-shell")) return;
      if (!e.metaKey || e.ctrlKey || e.altKey || e.shiftKey) return;
      if (e.repeat || e.defaultPrevented) return;
      if (e.isComposing || e.keyCode === 229) return;
      const view = BY_SHORTCUT.get(e.key);
      if (!view) return;
      e.preventDefault();
      onNavigate(view);
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onNavigate]);

  return (
    <>
      {/* The scrim behind the drawer. A real element rather than the sidebar's
          own ::before, because a pseudo-element cannot take a click — made that
          way it looks dismissable and is not, and the only way out of the
          drawer is a small button at the bottom of it. Display:none above the
          drawer breakpoint. */}
      {open && <div className="sidebar-scrim" onClick={onToggle} aria-hidden="true" />}
      <aside className={`sidebar${open ? "" : " collapsed"}`}>
      {/* The traffic lights are drawn over this strip by the OS, and it is what
          the window is dragged by — see .sidebar-drag. */}
      <div className="sidebar-drag" />
      <div className="brand">
        <svg className="brand-mark" viewBox="0 0 32 32" aria-hidden="true">
          {/* A reticle: the instrument's own mark. */}
          <circle cx="16" cy="16" r="10.5" fill="none" stroke="currentColor" strokeWidth="1.6" />
          <path d="M16 2.5v5M16 24.5v5M2.5 16h5M24.5 16h5" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" />
          <circle className="brand-mark-dot" cx="16" cy="16" r="3.2" />
        </svg>
        {labels && (
          <div className="brand-text">
            <div className="brand-name">SuperLeo</div>
          </div>
        )}
      </div>
      <nav className="nav">
        {NAV.map((group) => (
          <div key={t(group.section)}>
            {labels && <div className="nav-section">{t(group.section)}</div>}
            {group.items.map((it) => {
              const badge = it.key === "browser" ? 0 : badges?.[it.key] ?? 0;
              const Icon = it.Icon;
              return (
                <button
                  key={it.key}
                  className={`nav-item${current === it.key ? " active" : ""}`}
                  data-pet-spot={`nav-${it.key}`}
                  data-pet-label={`the ${t(it.label)} link in the left sidebar`}
                  onClick={() => (it.key === "browser" ? openBrowserPanel() : onNavigate(it.key))}
                  title={labels ? undefined : t(it.label)}
                >
                  <span className="ic">
                    <Icon className="size-4" />
                  </span>
                  {/* The label is an element rather than a bare text node so it
                      can be the row's elastic middle. Everything to its right
                      used to reach the right edge with margin-left:auto, which
                      works for exactly one such thing — two of them split the
                      free space and both end up floating in the middle. */}
                  {labels && <span className="nav-label">{t(it.label)}</span>}
                  {/* A desktop app should show its own breathing. Hidden off
                      the desktop build by CSS, not here: in a browser tab
                      these keys belong to the browser. */}
                  {labels && it.shortcut && <span className="nav-key">⌘{it.shortcut}</span>}
                  {/* Collapsed there is no room for a count, but "something
                      happened" still has to be visible. */}
                  {badge > 0 && (
                    <span className={`nav-badge${labels ? "" : " dot"}`}>{labels ? badge : ""}</span>
                  )}
                </button>
              );
            })}
          </div>
        ))}
      </nav>
      <div className="sidebar-footer">
        {/* On a phone the top bar has no room for sign-out; it lives here,
            next to the way out of the drawer. */}
        {served && room === "phone" && (
          <button
            type="button"
            className="nav-item drawer-sign-out"
            onClick={() => {
              void fetch("/api/logout", { method: "POST" }).finally(() => window.location.reload());
            }}
          >
            <LogOutIcon className="size-4" />
            <span className="nav-label">Sign out</span>
          </button>
        )}
        <button
          type="button"
          className="panel-toggle"
          onClick={onToggle}
          // On a phone this button is the way out of the drawer, not a width
          // control, and "Collapse sidebar" describes a rail that does not
          // exist there.
          title={room === "phone" ? "Close menu" : open ? "Collapse sidebar" : "Expand sidebar"}
          aria-label={room === "phone" ? "Close menu" : open ? "Collapse sidebar" : "Expand sidebar"}
        >
          {open ? <PanelLeftCloseIcon className="size-4" /> : <PanelLeftOpenIcon className="size-4" />}
        </button>
      </div>
      </aside>
    </>
  );
}
