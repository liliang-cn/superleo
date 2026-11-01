import React, { createContext, useContext, useEffect, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import {
  ListTodoIcon, LayoutDashboardIcon, ChartColumnIcon, PanelRightOpenIcon, PanelRightCloseIcon, BookOpenIcon, BotIcon, HexagonIcon, MessageSquareIcon, PaletteIcon,
  NotebookTabsIcon, MessagesSquareIcon, PuzzleIcon, SlidersHorizontalIcon, TerminalIcon, XIcon,
  MoonIcon, SunIcon, CalendarDaysIcon, GlobeIcon,
} from "lucide-react";
import DeskStatusBar from "./DeskStatusBar";
import World from "../world/World";
import { CanvasTheme, loadTheme, saveTheme, themeVars } from "../canvas/theme";
import { useCodingRuns } from "../canvas/data";
import { PATHS } from "../lib/routes";
import { AppStatus, ViewKey } from "../lib/types";
import { SetWindowTheme } from "../../wailsjs/go/app/App";
import { useI18n } from "../lib/i18n";
import { queenTitle, useQueenName } from "../lib/queenName";
import NotificationCenter from "../components/NotificationCenter";
import ThemePanel from "../canvas/ThemePanel";
import { ChatFab, ConversationsFab, FabStack, ModeFab, SettingsFab } from "../components/Fabs";
import { askToCompose } from "../lib/compose";
import { openBrowserPanel, useBrowserPanelOpen } from "../lib/browser";
import "../canvas/canvas.css";
import "./desk.css";

const served = Boolean((window as unknown as Record<string, unknown>).superleoServed);
export const drag = { "--wails-draggable": "drag" } as React.CSSProperties;
export const noDrag = { "--wails-draggable": "no-drag" } as React.CSSProperties;

/** The desktop's look, shared by the shell and the page in it. */
const ThemeCtx = createContext<{ theme: CanvasTheme; setTheme: (t: CanvasTheme) => void }>({ theme: loadTheme(), setTheme: () => {} });
export const useDeskTheme = () => useContext(ThemeCtx);

/** Whether the 3D world is up. In Menu mode it is not drawn at all, and the
 *  pages that showed it through themselves show something else. */
const WorldCtx = createContext(true);
export const useDeskWorld = () => useContext(WorldCtx);

/** The home's right pane: whether it is folded away, kept the way it was
 *  left. The shell holds it so the toolbar's toggle works from every page;
 *  the home reports whether anything is busy, for the toggle's dot. */
type Pane = { folded: boolean; fold: (on: boolean) => void; setBusy: (busy: boolean) => void };
const PaneCtx = createContext<Pane>({ folded: false, fold: () => {}, setBusy: () => {} });
export const useDeskPane = () => useContext(PaneCtx);
const FOLD_KEY = "superleo-desk-live-folded";
// In a narrow window, where it would float over the conversation, it starts folded.
const loadFolded = () => {
  try { const v = localStorage.getItem(FOLD_KEY); if (v !== null) return v === "1"; } catch { /* fine */ }
  return window.matchMedia("(max-width: 1240px)").matches;
};

const MODE_KEY = "superleo-desk-mode";
const loadMode = (): "world" | "menu" => {
  try { return localStorage.getItem(MODE_KEY) === "menu" ? "menu" : "world"; } catch { return "world"; }
};

const RAIL: { key: ViewKey; label: string; Icon: typeof HexagonIcon; cls: string }[] = [
  { key: "home", label: "The queen", Icon: MessageSquareIcon, cls: "dk-a-chat" },
  { key: "conversations", label: "Conversations", Icon: MessagesSquareIcon, cls: "dk-a-rec" },
  { key: "schedule", label: "Schedule", Icon: CalendarDaysIcon, cls: "dk-a-sched" },
  { key: "tasks", label: "Needs your attention", Icon: ListTodoIcon, cls: "dk-a-later" },
  { key: "hive", label: "Hive", Icon: HexagonIcon, cls: "dk-a-hive" },
  { key: "agents", label: "Bees", Icon: BotIcon, cls: "dk-a-bees" },
  { key: "coding", label: "Coding", Icon: TerminalIcon, cls: "dk-a-code" },
  { key: "knowledge", label: "Knowledge", Icon: BookOpenIcon, cls: "dk-a-know" },
  { key: "records", label: "Records", Icon: NotebookTabsIcon, cls: "dk-a-rec" },
  { key: "skills", label: "Extensions", Icon: PuzzleIcon, cls: "dk-a-later" },
];

/** The pages the toolbar opens; the open one's button closes it again. */
const SIDE: ViewKey[] = ["dashboards", "stats"];

export const TITLES: Record<ViewKey, string> = {
  home: "", chat: "Chat", conversations: "Conversations", tasks: "Needs your attention", schedule: "Schedule", dashboards: "Dashboards", hive: "Hive", agents: "Bees", coding: "Coding", knowledge: "Knowledge",
  skills: "Skills", mcp: "MCP", stats: "Stats", records: "Records", settings: "Settings",
};

/**
 * The desktop window around every page: the hive's sky behind, a bar the
 * window is moved by, and the rail of places down the left. The page itself
 * is the shell's child — the home's three panes, or any other screen on a
 * pane of glass.
 */
export default function DeskShell({ view, badges, children, onOpenConversation, status, loading }: { view: ViewKey; badges?: Partial<Record<ViewKey, number>>; children: React.ReactNode; onOpenConversation?: (session: string) => void; status:AppStatus|null; loading:boolean }) {
  const { t } = useI18n();
  const browserOn = useBrowserPanelOpen();
  const queen = useQueenName();
  const navigate = useNavigate();
  const [theme, setThemeState] = useState<CanvasTheme>(loadTheme);
  const setTheme = (t: CanvasTheme) => { setThemeState(t); saveTheme(t); };
  const [theming, setTheming] = useState(false);
  // Two modes, switched: the 3D world, where a page opens full size over it
  // as the screen of the thing it belongs to ("window"); or the menus and
  // lists with no world at all ("menu"). The choice is kept.
  const [mode, setMode] = useState<"world" | "menu" | "window">(loadMode);
  const modeRef = useRef(mode);
  modeRef.current = mode;
  const switchTo = (m: "world" | "menu") => {
    setMode(m);
    try { localStorage.setItem(MODE_KEY, m); } catch { /* fine */ }
  };
  const pendingWindow = useRef(false);
  useEffect(() => {
    // Read before the reset: the updater may run later, at render.
    const pending = pendingWindow.current;
    pendingWindow.current = false;
    setMode((m) => (pending ? "window" : m === "menu" ? "menu" : "world"));
  }, [view]);
  useEffect(() => {
    const open = (e: Event) => {
      const path = (e as CustomEvent<string>).detail;
      // In Menu mode a page is already a page: just go there.
      if (modeRef.current === "menu") { navigate(path); return; }
      // Same page: the view does not change, so open it here. Not on a
      // timer: a navigation can take longer to land than any timer, and the
      // window opened early is closed again when it does.
      if ((window.location.hash.slice(1) || "/") === path) { setMode("window"); return; }
      pendingWindow.current = true;
      navigate(path);
    };
    const theme = () => setTheming(true);
    // Esc closes the window, unless someone is typing in it.
    const esc = (e: KeyboardEvent) => {
      if (e.key === "Escape" && !(e.target as HTMLElement)?.closest?.("input,textarea,[contenteditable=true]")) setMode((m) => (m === "window" ? "world" : m));
    };
    window.addEventListener("keydown", esc);
    window.addEventListener("superleo:window", open);
    window.addEventListener("superleo:theme", theme);
    return () => { window.removeEventListener("keydown", esc); window.removeEventListener("superleo:window", open); window.removeEventListener("superleo:theme", theme); };
  }, [navigate]);
  const world = mode !== "menu";
  // The mode changes how the page in front of you is framed, never which page
  // it is: a page switched to 3D stays open, as a window over its part of the
  // world; the home becomes the world itself.
  const toggleMode = () => {
    if (world) { switchTo("menu"); return; }
    switchTo("world");
    if (view !== "home") setMode("window");
  };
  // The conversation with the queen: the home's middle column in Menu, a
  // window over the world in 3D, with the cursor in its box either way.
  const openChat = () => {
    if (mode === "menu") navigate(PATHS.home);
    else window.dispatchEvent(new CustomEvent("superleo:window", { detail: PATHS.home }));
    askToCompose();
  };
  const runs = useCodingRuns();
  const liveRuns = runs.filter((r) => r.state === "running").length;
  const [folded, setFolded] = useState(loadFolded);
  const [paneBusy, setPaneBusy] = useState(false);
  const pane: Pane = {
    folded,
    fold: (on) => { setFolded(on); try { localStorage.setItem(FOLD_KEY, on ? "1" : "0"); } catch { /* fine */ } },
    setBusy: setPaneBusy,
  };
  // The pane is the Menu home's; from any other page its toggle brings you
  // home with it open. In 3D there is no pane, so no toggle.
  const withPane = view === "home" || view === "chat";
  const paneShown = mode === "menu" && withPane && !folded;
  const togglePane = () => {
    if (paneShown) { pane.fold(true); return; }
    pane.fold(false);
    if (!withPane) navigate(PATHS.home);
  };
  // Dashboards and Stats: a page in Menu mode, a window over the world in 3D;
  // pressed again, back to where you were.
  const sideOn = (k: ViewKey) => view === k && mode !== "world";
  const openSide = (k: ViewKey) => {
    if (sideOn(k)) {
      if (mode === "window") setMode("world");
      else navigate(PATHS.home);
      return;
    }
    if (mode === "menu") navigate(PATHS[k]);
    else window.dispatchEvent(new CustomEvent("superleo:window", { detail: PATHS[k] }));
  };

  // The older screens paint with the app-wide tokens; desk.css gives them the
  // hive's while this shell is up.
  useEffect(() => {
    document.documentElement.dataset.desk = "1";
    return () => { delete document.documentElement.dataset.desk; };
  }, []);

  // The older screens read the app-wide light/dark flag; keep it in step.
  useEffect(() => {
    document.documentElement.dataset.theme = theme.mode;
    try { localStorage.setItem("superleo-theme", theme.mode); } catch { /* fine */ }
    if (!served) void SetWindowTheme(theme.mode === "dark").catch(() => {});
  }, [theme.mode]);

  const count = (k: ViewKey) => (k === "coding" ? liveRuns : badges?.[k] ?? 0);

  return (
    <ThemeCtx.Provider value={{ theme, setTheme }}>
    <WorldCtx.Provider value={world}>
    <PaneCtx.Provider value={pane}>
      {/* Menu mode keeps the world's styling, which every page is tuned
          for, and shows the rail and pages full width with no world drawn. */}
      <div className={`cv-root dk-shell dk-world${world ? "" : " panel-on dk-flat"}${served ? " served" : ""}${view === "home" ? " on-home" : ""}${mode === "window" ? " window-on" : ""} ${theme.mode === "dark" ? "dk-dark" : ""}`} style={themeVars(theme)}>
        {world && <World view={view} onFlipTheme={() => setTheme({ ...theme, mode: theme.mode === "dark" ? "light" : "dark" })} />}
        <div className="dk-bar" style={drag} />
        {/* One toolbar, top right, the same in every mode and on every page:
            the pane's toggle at the inner end (absent in 3D, where there is no
            pane), then Dashboards, Stats, light or dark, and the bell at the edge. */}
        {/* In 3D the world has the right edge to itself. */}
        {mode === "menu" && <div className="dk-toolbar" style={noDrag} role="toolbar">
          {mode === "menu" && (
            <button type="button" className={`dk-tool dk-tool-pane${paneShown ? " on" : ""}`} aria-pressed={paneShown}
              title={paneShown ? t("Hide this pane") : t("Show the hive")} aria-label={paneShown ? t("Hide this pane") : t("Show the hive")} onClick={togglePane}>
              {paneShown ? <PanelRightCloseIcon size={18} /> : <PanelRightOpenIcon size={18} />}
              {!paneShown && (paneBusy || liveRuns > 0) && <i className="dk-livedot" />}
            </button>
          )}
          {SIDE.map((k) => {
            const Icon = k === "dashboards" ? LayoutDashboardIcon : ChartColumnIcon;
            return (
              <button key={k} type="button" className={`dk-tool${sideOn(k) ? " on" : ""}`} aria-pressed={sideOn(k)}
                title={t(TITLES[k])} aria-label={t(TITLES[k])} onClick={() => openSide(k)}>
                <Icon size={18} />
              </button>
            );
          })}
          <button type="button" className="dk-tool dk-tool-mode" aria-pressed={theme.mode === "dark"}
            title={theme.mode === "dark" ? t("Light") : t("Dark")} aria-label={theme.mode === "dark" ? t("Light") : t("Dark")}
            onClick={() => setTheme({ ...theme, mode: theme.mode === "dark" ? "light" : "dark" })}>
            {theme.mode === "dark" ? <SunIcon size={18} /> : <MoonIcon size={18} />}
          </button>
          <NotificationCenter variant="desk" onOpenConversation={onOpenConversation} />
        </div>}
        <nav className="dk-rail" style={drag}>
          {RAIL.map(({ key, label, Icon, cls }) => (
            <button key={key} title={key === "home" ? queenTitle(queen) : t(label)} style={noDrag} className={`dk-app ${cls} ${view === key || (key === "skills" && view === "mcp") || (key === "home" && view === "chat") ? "on" : ""}`}
              onClick={() => navigate(PATHS[key])}>
              <Icon size={21} strokeWidth={2} />
              {count(key) > 0 && <span className="dk-badge">{count(key)}</span>}
            </button>
          ))}
          {/* The browser the agent drives, live, whenever you want to look
              or take it over; a panel over whatever page you are on. */}
          <button title={t("Browser")} style={noDrag} className={`dk-app dk-a-web${browserOn ? " on" : ""}`} onClick={openBrowserPanel} data-testid="rail-browser">
            <GlobeIcon size={20} strokeWidth={2} />
          </button>
          <span className="dk-sp" />
          <button title={t("Theme")} style={noDrag} className={`dk-app dk-a-mode${theming ? " on" : ""}`} onClick={() => setTheming((v) => !v)}>
            <PaletteIcon size={19} />
          </button>
          <button title={t("Settings")} style={noDrag} className={`dk-app dk-a-set ${view === "settings" ? "on" : ""}`} onClick={() => navigate(PATHS.settings)}>
            <SlidersHorizontalIcon size={20} />
          </button>
        </nav>
        {theming && <ThemePanel className="dk-theme" theme={theme} onChange={setTheme} onClose={() => setTheming(false)} />}
        {view === "home" ? children : (
          <main className={`cv-glass dk-page page-${view}`}>
            <div className="content">{children}</div>
          </main>
        )}
        {mode === "window" && (
          <button className="dk-win-x" style={noDrag} title={t("Back to the world")} onClick={() => setMode("world")}><XIcon size={18} /></button>
        )}
        <FabStack style={noDrag}>
          {world && <ConversationsFab onClick={() => window.dispatchEvent(new CustomEvent("superleo:window", { detail: PATHS.conversations }))} />}
          {world && <SettingsFab onClick={() => window.dispatchEvent(new CustomEvent("superleo:window", { detail: PATHS.settings }))} />}
          <ModeFab world={world} onClick={toggleMode} />
          <ChatFab onClick={openChat} />
        </FabStack>
        <DeskStatusBar status={status} loading={loading} codingRuns={liveRuns}/>
      </div>
    </PaneCtx.Provider>
    </WorldCtx.Provider>
    </ThemeCtx.Provider>
  );
}
