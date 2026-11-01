import React, { useCallback, useEffect, useState } from "react";
import { Navigate, Route, Routes, useLocation, useNavigate } from "react-router-dom";
import { PATHS, viewOf } from "./lib/routes";
import Sidebar from "./components/Sidebar";
import Pet from "./components/Pet";
import { useRoom } from "./lib/useViewport";
import StatusBar from "./components/StatusBar";
import ChatView from "./views/ChatView";
import StatsView from "./views/StatsView";
import HiveView from "./views/HiveView";
import AgentsView from "./views/AgentsView";
import StandingView from "./views/StandingView";
import SchedulesView from "./views/SchedulesView";
import SettingsView from "./views/SettingsView";
import KnowledgeView from "./views/KnowledgeView";
import ExtensionsView from "./views/ExtensionsView";
import SkillsView from "./views/SkillsView";
import MCPView from "./views/MCPView";
import { useAttentionState } from "./canvas/attention";
import DashboardsPanel from "./components/DashboardsPanel";
import TasksView from "./views/TasksView";
import RecordsView from "./views/RecordsView";
import ConversationsView from "./views/ConversationsView";
import ScheduleView from "./views/ScheduleView";
import CanvasView from "./canvas/CanvasView";
import DeskView from "./desk/DeskView";
import { BrowserPanelHost } from "./components/BrowserPanel";
import DeskShell from "./desk/DeskShell";
import { clientKind } from "./lib/hivelink";
import { ScheduleRunToasts } from "./components/ScheduleRuns";
import { Toaster } from "./components/Toaster";
import SetupBanner from "./components/SetupBanner";
import { setReady } from "./lib/ready";
import { useBackendToasts } from "./lib/toasts";
import { EventsOff, EventsOn } from "../wailsjs/runtime/runtime";
import ToolApprovals from "./components/ToolApprovals";
import { ChatFab, FabStack } from "./components/Fabs";
import { Accent, AppStatus, Theme, ViewKey, normalizeStatus } from "./lib/types";
import { useScheduleRuns } from "./lib/useScheduleRuns";
import { useToolApprovals } from "./lib/useToolApprovals";
import { uiRules } from "./lib/aigui";
import { GetStatus, SetUIRules, SetWindowTheme } from "../wailsjs/go/app/App";

/** Where the sidebar remembers being expanded. Shared with the drawer on a
 *  phone, which is the same state seen through a different layout. */
const NAV_OPEN_KEY = "superleo-sidebar-open";

export default function App() {
  // Draw every notice the backend publishes. Mounted here so it survives every
  // view change: a toast about a run that just failed must not disappear
  // because the user clicked to another screen to look into it.
  useBackendToasts();

  // The screen is the address. See lib/routes.ts.
  const location = useLocation();
  const navigate = useNavigate();
  const view = viewOf(location.pathname);
  const setView = useCallback((v: ViewKey) => navigate(PATHS[v]), [navigate]);
  const [status, setStatus] = useState<AppStatus | null>(null);
  // Off until asked for. Something walking across the window is charming when
  // you let it out and an interruption when it arrives on its own.
  const [petOpen, setPetOpen] = useState(false);
  // The sidebar: expanded on a desktop, a drawer on a phone. It starts closed
  // on a narrow screen whatever was stored, because a drawer covering the app
  // on arrival is not a navigation aid.
  // How much room there is. Narrow puts the sidebar into its icon rail and the
  // side panel over the conversation instead of beside it — applied on top of
  // what the person chose, never written back over it.
  const room = useRoom();
  const [navOpen, setNavOpen] = useState(() => {
    try {
      if (window.matchMedia("(max-width: 640px)").matches) return false;
      return localStorage.getItem(NAV_OPEN_KEY) !== "0";
    } catch {
      return true;
    }
  });
  useEffect(() => {
    try {
      // Only a desktop writes this down. On a phone the same flag is a drawer
      // being out, and closing a drawer — which navigating does on its own — is
      // not a statement about how you like your sidebar. Sharing the key
      // without this guard means one visit on a phone leaves the desktop
      // sidebar collapsed for good.
      if (window.matchMedia("(max-width: 640px)").matches) return;
      localStorage.setItem(NAV_OPEN_KEY, navOpen ? "1" : "0");
    } catch {
      // A private window. It simply opens on its default next time.
    }
  }, [navOpen]);
  const [loading, setLoading] = useState(true);
  const [theme, setTheme] = useState<Theme>(
    () => (localStorage.getItem("superleo-theme") as Theme) || "light"
  );
  // The accent finish. Remembered per browser rather than in settings.json:
  // it changes nothing the agent does, and a preference the backend has to be
  // rebuilt to apply is a preference nobody flips twice.
  const [accent, setAccent] = useState<Accent>(
    () => (localStorage.getItem("superleo-accent") as Accent) || "signal"
  );
  // The conversation a run belongs to, handed to the chat view to open. Cleared
  // as soon as it has been taken so asking for the same one twice works.
  const [pendingSession, setPendingSession] = useState("");

  const openConversation = useCallback((session: string) => {
    if (!session) return;
    setPendingSession(session);
    setView("chat");
  }, [setView]);

  // Scheduled runs are listened for here, not in the Schedules view: a timer
  // fires while the user is somewhere else, which is the whole point of a timer.
  // The opener goes with it so a clicked notification lands in the right place.
  const runs = useScheduleRuns(openConversation);

  // A click on a native banner. The desktop build carries the conversation on
  // the notification itself and asks for it back here, because a banner that
  // says a run finished and leaves you to find it is a dead end — a reminder
  // that fired hours ago means scrolling a list to work out which one it meant.
  useEffect(() => {
    EventsOn("open:conversation", (payload: { session?: string }) => {
      if (payload?.session) openConversation(payload.session);
    });
    return () => EventsOff("open:conversation");
  }, [openConversation]);

  // Tool approvals live at the root for a stronger version of the same reason:
  // the agent's turn is blocked until one is answered, and the prompt has to
  // find the user wherever they are — including on the Settings page they went
  // to in order to look at the audit log.
  const approvals = useToolApprovals();
  const attention = useAttentionState();

  useEffect(() => {
    // On the desktop the shell owns light and dark (the canvas theme).
    if (clientKind() === "desktop") return;
    document.documentElement.dataset.theme = theme;
    localStorage.setItem("superleo-theme", theme);
    // The window chrome is drawn by the OS, not by this stylesheet. Without
    // this the title bar keeps whatever colour the app started with, and a
    // light page sits under a dark bar looking half-converted. Runs on mount
    // too, so a remembered theme is matched before the first paint rather than
    // only after the next toggle. Harmless in a browser tab, which has no
    // chrome of its own to paint.
    void SetWindowTheme(theme === "dark").catch(() => {});
  }, [theme]);

  useEffect(() => {
    document.documentElement.dataset.accent = accent;
    localStorage.setItem("superleo-accent", accent);
  }, [accent]);

  // Tell the agent which rich blocks this transcript can render. The rules come
  // from the same registry + plugins the renderer uses, so they cannot drift;
  // the backend only rebuilds when they actually changed.
  useEffect(() => {
    SetUIRules(uiRules()).catch(() => {});
  }, []);
  const refreshStatus = useCallback(async () => {
    try {
      const raw = await GetStatus();
      const st = normalizeStatus(raw);
      setStatus(st);
      setReady(st.ready);
    } catch (e: any) {
      setStatus(normalizeStatus({ ready: false, error: String(e?.message || e) }));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    refreshStatus();
    const t = setInterval(refreshStatus, 15000);
    return () => clearInterval(t);
  }, [refreshStatus]);

  // Home is the canvas: the whole window, no sidebar and no status bar; the
  // other screens are a menu away. What needs you is on the canvas itself, so
  // the approval cards stay away from it.
  const routes = (
    <Routes>
      <Route
        path="/chat"
        element={
          <ChatView
            status={status}
            openSession={pendingSession}
            onSessionOpened={() => setPendingSession("")}
          />
        }
      />
      <Route path="/conversations" element={<ConversationsView />} />
      <Route path="/dashboards" element={<div className="dashboards-view"><DashboardsPanel /></div>} />
      <Route path="/tasks" element={<TasksView attention={attention} approvals={approvals.pending} onOpenConversation={openConversation} onResolve={approvals.resolve} />} />
      <Route path="/schedule" element={<ScheduleView />} />
      <Route path="/stats" element={<StatsView />} />
      {/* Everything under /hive is the Hive screen's own to route:
          the overview, and one page per task. */}
      <Route path="/hive/*" element={<HiveView />} />
      <Route path="/agents" element={<StandingView><SchedulesView embedded status={status} log={runs} onOpenConversation={openConversation} /></StandingView>} />
      <Route path="/coding" element={<AgentsView />} />
      <Route path="/settings" element={<SettingsView onSaved={refreshStatus} theme={theme} onTheme={setTheme} accent={accent} onAccent={setAccent} />} />
      <Route path="/knowledge" element={<KnowledgeView />} />
      <Route path="/skills" element={clientKind() === "desktop" ? <ExtensionsView /> : <SkillsView />} />
      <Route path="/mcp" element={clientKind() === "desktop" ? <ExtensionsView /> : <MCPView />} />
      <Route
        path="/records"
        element={<RecordsView status={status} log={runs} onOpenConversation={openConversation} />}
      />
      <Route path="*" element={<Navigate to="/" replace />} />
    </Routes>
  );

  // The desktop window: every page inside the same shell — the rail, the sky,
  // the glass. Its home is the conversation with the queen beside the live
  // hive, so a conversation opened from anywhere opens there.
  if (clientKind() === "desktop") {
    return (
      <>
        <DeskShell status={status} loading={loading} view={view} badges={{ records: runs.unseen, tasks: attention.items.filter(i => i.level === "needs").length }} onOpenConversation={openConversation}>
          {view === "home" || view === "chat" ? (
            <DeskView attentionItems={attention.items} attentionError={attention.error} approvals={approvals} openSession={pendingSession} onSessionOpened={() => setPendingSession("")} />
          ) : routes}
        </DeskShell>
        {view !== "records" && <ScheduleRunToasts log={runs} onOpenConversation={openConversation} />}
        <Toaster onOpenConversation={openConversation} />
        <SetupBanner status={status} loading={loading} />
        <BrowserPanelHost />
        <ToolApprovals
          pending={view === "home" || view === "chat" || view === "tasks" ? [] : approvals.pending}
          note={approvals.note}
          onResolve={approvals.resolve}
          onDismissNote={approvals.dismissNote}
        />
      </>
    );
  }

  if (view === "home") {
    return (
      <>
        <CanvasView approvals={approvals} />
        <ScheduleRunToasts log={runs} onOpenConversation={openConversation} />
        <Toaster onOpenConversation={openConversation} />
        <SetupBanner status={status} loading={loading} />
        <BrowserPanelHost />
      </>
    );
  }

  return (
    <>
      <div className="app">
        <Sidebar
          current={view}
          onNavigate={(v) => {
            setView(v);
            // On a phone the sidebar is a drawer over the page, so navigating
            // has to close it or the destination is behind it.
            if (window.matchMedia("(max-width: 640px)").matches) setNavOpen(false);
          }}
          badges={{ records: runs.unseen, tasks: attention.items.filter(i => i.level === "needs").length }}
          open={navOpen && room !== "narrow"}
          onToggle={() => setNavOpen((v) => !v)}
        />
        <div className="main">
          <StatusBar
            status={status}
            loading={loading}
            theme={theme}
            onTheme={setTheme}
            accent={accent}
            onAccent={setAccent}
            petOpen={petOpen}
            onTogglePet={() => setPetOpen((open) => !open)}
            onOpenNav={() => setNavOpen((v) => !v)}
            onOpenConversation={openConversation}
          />
          <div className="content">
            {routes}
          </div>
        </div>
      </div>
      {/* Loose in the window, over everything, catching nothing but its own
          clicks. Needs the avatar server for its sprites and its state, which
          is also the only condition under which the pill that opens it shows. */}
      {petOpen && status && status.avatarPort > 0 && (
        <Pet port={status.avatarPort} view={view} onDismiss={() => setPetOpen(false)} />
      )}
      {/* Records lists the same runs, so a toast there would only repeat what
          is already on screen. */}
      {view !== "records" && (
        <ScheduleRunToasts log={runs} onOpenConversation={openConversation} />
      )}
      {/* The conversation, a thumb away from every page but its own. A phone
          has no 3D, so the mode button stays with the desktop shell. */}
      {view !== "chat" && <FabStack lifted><ChatFab onClick={() => setView("chat")} /></FabStack>}
      {/* Everything the backend publishes, whatever raised it. */}
      <Toaster onOpenConversation={openConversation} />
      <SetupBanner status={status} loading={loading} />
      <BrowserPanelHost />
      <ToolApprovals
        pending={approvals.pending}
        note={approvals.note}
        onResolve={approvals.resolve}
        onDismissNote={approvals.dismissNote}
      />
    </>
  );
}
