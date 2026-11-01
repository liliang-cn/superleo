package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/liliang-cn/agent-go/v3/pkg/agent"
	"github.com/liliang-cn/agent-go/v3/pkg/mcp"
	"github.com/liliang-cn/superleo/internal/backend"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// App is the Wails-bound application object. Its exported methods are callable
// from the React frontend.
type App struct {
	ctx context.Context
	// The desktop window's link to a hive's queen. See app_hivelink.go.
	hiveLink hiveLinkState
	// Agents connected to this core over the agent link, and, on an agent,
	// where its events go up the link. See agentlink_core.go / _agent.go.
	agentHubOnce sync.Once
	agentHubV    *agentHub
	// haCache is the HA services last read from haify, refreshed in the
	// background so listing hosts never waits on it.
	haCache  haCache
	agentTap atomic.Pointer[func(string, map[string]any)]
	// memberPulses is the latest meter summary from each linked agent.
	memberPulses sync.Map
	// ccSelfActivity is this machine's own Claude Code sessions when it is
	// not linked to a core (cchook_activity.go); a linked agent's are on its
	// agentConn.
	ccSelfActivity atomic.Pointer[CCActivity]
	// cliSelf is this machine's own other coding CLIs when it is not linked
	// (cli_activity.go); cliSelfMu orders the writes.
	cliSelfMu sync.Mutex
	cliSelf   atomic.Pointer[cliMap]
	// cliPoll, set by a test before the hook socket starts, replaces the
	// process look's clock and ps (cli_procs.go).
	cliPoll *cliPollConfig
	// openTurns are the turns still running, by request id, as the session
	// list shows them (ChatSessions).
	openTurns sync.Map
	// turnSess is each running turn's conversation, for its events
	// (queen_console.go).
	turnSess turnSessions
	// abilities is what the last sync of the queen's skills and MCP did.
	abilities AbilitiesState
	// taps are in-process listeners on every emitted event, by id. See
	// mcp_ask.go. askFn starts a turn for superleo_ask; nil means SendChat. A
	// test sets it to a stub queen.
	taps  sync.Map
	askFn func(sessionID, message string) string
	// gateForMCP, when set, is the approval gate /mcp's destructive tools
	// pass instead of the service's (mcp_scope.go); a test sets it.
	gateForMCP *backend.ToolGate
	// delegations are superleo_hive_delegate's runs (mcp_hive.go).
	delegations delegationStore
	// ownTurnFn, appendFn and turnsFn, when set, stand in for a turn with
	// this app's model, for writing an exchange into its conversation and for
	// reading one; a test sets them. queenToken is the queen engine's /mcp
	// token, minted on first use (app_queen_engine.go).
	ownTurnFn  func(requestID, sessionID, message, standIn string)
	appendFn   func(sessionID, question, answer string) error
	turnsFn    func(sessionID string) []backend.ChatTurn
	queenToken queenTokenCache
	// queenNow is the engine taking the queen's turns now (app_queen_engine.go).
	queenNow queenNowState
	// desktop is whether this is the windowed app, which thinks for core's
	// queen when she picks it; desktopTurnFn, when set, stands in for its
	// model's turn there (queen_engine_desktop.go). A test sets both.
	desktop       bool
	desktopTurnFn func(ctx context.Context, sessionID, message string, emit func(*agent.Event)) (backend.TurnResult, error)
	// hermesWarm is where getting hermes ready stands (hermes_warm.go).
	hermesWarm hermesWarmState
	// electedTerm is the term this instance was elected queen in, for the
	// roster made when it takes over (app_election.go).
	electedTerm int64
	// agentDoor is where agents come in (app_agentdoor.go); doorStop closes it.
	agentDoor agentDoor
	doorMu    sync.Mutex
	doorStop  context.CancelFunc
	// member is this process's place in a hive (hive_member.go).
	member hiveMemberState
	// launchedKeepRunning is the keep-running setting the window was made
	// with (desktop_resident.go).
	launchedKeepRunning atomic.Bool
	// lanHive is a member's last roster from its queen (app_lanhive.go).
	lanHive lanHive
	// queenFor serialises becoming queen for the machines that joined
	// (queenForWorkers).
	queenFor sync.Mutex
	// turnLog is every recent chat turn as it ran (turn_journal.go).
	turnLog turnJournal
	// Direct MCP calls from chat-block buttons and home tiles. See
	// app_actions.go.
	actionsOnce sync.Once
	actionsV    *backend.MCPActions
	// The home screen's layout and the market data its widgets show. See
	// app_home_layout.go.
	homeOnce   sync.Once
	homeStore  *backend.HomeLayoutStore
	quotesOnce sync.Once
	quotesV    *backend.QuoteService
	// The alarm watcher and the APNs client it pushes with. See
	// app_alarms.go.
	alarmsOnce sync.Once
	alarmsV    *alarmWatcher
	alarmsMu   sync.Mutex
	alarmsStop context.CancelFunc
	alarmsErr  string
	// The notification-mode fire watcher's last failure (app_noted_schedule.go).
	firesMu   sync.Mutex
	firesErr  string
	pushMu    sync.Mutex
	pushBuilt bool
	pushKey   string
	pushV     alarmPusher
	// Each year's holidays AlarmRings expanded with. See app_alarm_rings.go.
	ringHolidays holidayCache
	// Where the agents the settings reach over ssh run, and what their hosts
	// call themselves, looked up in the background. See remote_where.go.
	remoteWhere remoteWhere

	mu       sync.Mutex
	svc      *backend.Service
	settings *backend.Settings
	avatar   backend.AvatarDriver
	// What the window last said the character is standing on. See app_pet.go.
	pet petState
	// Asking agents on other machines. Built on first use and dropped when the
	// settings change; see app_remote.go.
	remoteOnce sync.Once
	remote     *backend.RemoteRunner
	sse        *backend.SSEServer
	proxy      *backend.CLIProxy

	// loginCh is non-nil while a provider login is waiting for the user; it
	// carries the manually pasted callback URL.
	loginCh chan string

	// runs holds the chat turns currently in flight, keyed by the request id
	// every one of their events is tagged with. It has its own lock rather than
	// sharing a.mu: a.mu is held across a rebuild (which constructs a whole
	// Service), and "stop" must not queue behind that — the moment it is worth
	// pressing is exactly the moment the app is busy.
	runMu sync.Mutex
	// Settings changes made by the agent or over MCP, not yet applied (selfmcp.go).
	selfPendingMu sync.Mutex
	selfPending   map[string]any
	runs          map[string]*chatRun

	// wall narrates every long run into state a window can draw. It outlives
	// a rebuild on purpose: a settings save must not erase the run wall.
	wallOnce sync.Once
	wall     *backend.RunWall
	// pulse is the live meter: every run this process makes, chat turns
	// included, folded into a per-second histogram. Like the wall it outlives
	// a rebuild, so a settings save does not reset the counters.
	// phoneBrowserState routes the phone_browser_* tools to the iPhone that
	// lends its browser (phone_browser.go).
	// browserRelay* is the desktop window's loopback relay to the browser's
	// stream (app_browser.go).
	browserRelayOnce  sync.Once
	browserRelayURL   string
	browserRelayErr   error
	phoneBrowserOnce  sync.Once
	phoneBrowserState *phoneBrowser
	pulseOnce         sync.Once
	pulse             *backend.Pulse
	pulseMu           sync.Mutex
	pulseStop         func() // ends the push loop after its last write; nil when not running
	// longRuns holds the segmented tasks in flight, by task id, so a stop
	// button has something to press.
	longMu   sync.Mutex
	longRuns map[string]context.CancelFunc

	// scheduler drives prompts that run on a clock. It is rebuilt with the
	// service, since a run is an agent turn against the current settings.
	scheduler    *agent.PromptScheduler
	schedulerErr string

	// telegram is the inbound Telegram bridge, when one is configured. Like the
	// scheduler it is rebuilt with the service and may be idle rather than
	// polling, because only one process gets the bot.
	telegram    *backend.TelegramBridge
	telegramErr string

	// hive is the queen's roster; nil on anything else. Built once and
	// kept across rebuilds, because a settings save must not make every worker
	// look newly joined. hiveStop ends a worker's announcing loop.
	hive    *backend.Hive
	hiveAnn *backend.Announcer
	// hiveBoard records the orders that cross the hive, on either side. See
	// backend/hive_tasks.go.
	hiveBoard     *backend.TaskBoard
	hiveBoardOnce sync.Once
	// taskBoard is the board's file and the checks left on long orders; see
	// app_hive_taskboard.go.
	taskBoard taskBoardState
	// hiveReports carries a worker's dealings with its peers to the queen, in
	// order, off the path of whatever produced them.
	hiveReports    chan any
	hiveReportOnce sync.Once
	// peerAsks bounds how many questions this worker has out to peers at once.
	peerAsks chan struct{}
	// hiveMail is this member's inbox. See backend/hive_mail.go.
	hiveMail     *backend.Mailbox
	hiveMailOnce sync.Once
	// hiveSeen is, on a queen, the messages between workers she has seen go
	// by: the copies they send her, and the ones she carried.
	hiveSeen     *backend.Mailbox
	hiveSeenOnce sync.Once
	// mailWaking is, per sender, whether a goroutine is already waiting to
	// take that sender's mail up in a turn.
	mailWaking map[string]bool
	mailWakeMu sync.Mutex
	// wakeFn starts a turn for incoming mail; nil means SendChat. A test sets
	// it to watch what would have been started.
	wakeFn func(session, prompt string)
	// steerFn puts a message into a running turn; nil means the service's
	// Steer. A test sets it.
	steerFn  func(session, content string) bool
	hiveStop context.CancelFunc
	hiveDone chan struct{}
	// localHive runs this queen's workers as processes on this machine, when
	// its spawner is "local" (app_localhive.go); localHiveKey is what they
	// were started with, so a save that changes nothing leaves them be.
	localHive    *backend.LocalSpawner
	localHiveKey []byte
	// serveURL is this instance's own loopback address in serve mode; empty
	// in the desktop app.
	serveURL string
	// workerInFlight and workerLastUse are, per worker, delegated work in
	// progress and the last time any started or ended: the idle check.
	workerUseMu    sync.Mutex
	workerInFlight map[string]int
	workerLastUse  map[string]time.Time
	// scheduleLock is held while this process owns firing schedules; nil means
	// another process (the daemon) owns them and this one only manages.
	scheduleLock *backend.ScheduleLock
	// notifyOK records whether the user granted notification permission, asked
	// for once at startup rather than when a timer fires and nobody is looking.
	notifyOK bool

	// approvals holds the tool calls currently parked in front of the user.
	// Its own lock, like runs above and for the same reason: an answer must
	// not queue behind a rebuild, and the agent goroutine waiting on one of
	// these is holding up a turn.
	approvalMu sync.Mutex
	// Agent CLI runs started from the Agents page or the phone, and the
	// loopback MCP server their permission prompts come through.
	cli     *cliRunStore
	cliOnce sync.Once
	// cliDetached is set while SuperLeo stops: its coding runs carry on
	// without it (cli_live.go).
	cliDetached atomic.Bool
	// linkCore is the core this agent last linked to (agentlink_agent.go).
	linkCore       atomic.Value
	cliApprove     *cliApprover
	cliApproveOnce sync.Once
	remoteCLI      remoteCLICache
	// Standing agents (app_standing.go).
	standingHost *standingHost
	standingOnce sync.Once
	approvals    map[string]*pendingApproval
	// abilitiesCache is localAbilities for a few seconds (abilities.go).
	abilitiesCache abilityCache
	delegateTool   delegateToolState

	// emitFn is an extra sink for frontend events. Tests set it, and serve mode
	// points it at the SSE hub, where it is the only sink there is: the Wails
	// runtime calls log.Fatalf when EventsEmit is handed a context it did not
	// create, so any path without a real window needs somewhere else to send
	// events. It does not stand in for the window — emit fans out to every
	// surface that is attached, and in the desktop app that includes both.
	emitFn func(name string, payload map[string]any)

	// graphs holds the live 3D view of the knowledge graph. It has its own lock
	// and is deliberately not started at boot: it is a loopback listener plus a
	// poller reading the whole brain every couple of seconds, and most sessions
	// never open that page. See GraphView.
	graphs backend.GraphViews

	// companion is the HTTP server behind "Open in Browser": this same app,
	// reachable from a real browser tab, started on demand. Guarded by a.mu.
	companion *companionServer
	// companionHub is that server's SSE fan-out, held separately and atomically
	// rather than reached through a.companion. emit reads it on every event,
	// from whatever goroutine a chat turn happens to be on, and a.mu is held
	// across a whole service rebuild — an event must never queue behind that.
	companionHub atomic.Pointer[eventHub]
	// phones is the phone link (phonelink.go), started once a phone is
	// paired with the desktop app. Guarded by a.mu.
	phones *phoneLink

	buildErr string
	proxyErr string
}

// NewApp creates a new App.
func NewApp() *App {
	return &App{avatar: backend.NoopDriver{}}
}

// startup loads settings, starts the avatar SSE server, and builds the backend
// Service. A build failure does not crash the app — it is surfaced via
// GetStatus so the Settings page can fix the config and rebuild.
func (a *App) Startup(ctx context.Context) {
	a.ctx = ctx
	a.desktop = true
	a.launchedKeepRunning.Store(KeepRunningWanted())
	a.refreshMember()
	a.startHiveLink()
	a.boot()
	a.holdResident()
	a.startMemberAtLaunch()
	go a.migrateHiveLink()
	// Terminal Claude Code and the coding CLIs report here too, as they do to
	// serve: without it a core on this Mac alone never sees them work. The
	// CLI plugins stay pointed at whoever installed them (the node).
	go a.serveCCHookSocket(ctx)
	go a.firstRun()
	go a.startPhoneLinkIfPaired()
	// Other machines join this one as agents (app_agentdoor.go), unless this
	// Mac is a member of a hive, whose queen has the door.
	a.startAgentDoor()
}

// startupHeadless boots the same backend with no Wails context: events go
// wherever emitFn points, and everything that needs a native window — dialogs,
// the notification permission prompt — stays off behind the existing nil-ctx
// guards. Serve mode must set emitFn before calling this.
func (a *App) startupHeadless() {
	a.boot()
}

func (a *App) boot() {
	a.refreshMember()
	s, _ := backend.LoadSettings() // returns defaults even on error
	a.settings = s
	// Before anything reaches out: every request from here on asks it.
	backend.InstallProxy()
	backend.ApplyProxy(s.Proxy)

	sse := backend.NewSSEServer(s.AvatarPort, avatarWeb())
	if err := sse.Start(); err != nil {
		a.avatar = backend.NoopDriver{}
	} else {
		a.sse = sse
		a.avatar = sse
	}

	a.syncProxy()
	a.rebuild()
	// The coding runs that were going when SuperLeo last stopped.
	a.recoverCLIRuns()
	// The orders the queen had out when she last stopped, and what became of them.
	a.openTaskBoard()
	// Hermes's environment, built now rather than in someone's first turn.
	a.warmHermes("start")

	// Permission is requested at startup, not when a timer fires: by then the
	// user is typically not at the machine, which is the whole point of a
	// schedule.
	a.initNotifications()
	// The queen's duties, unless this process is a member of a hive: its
	// queen is elsewhere, and a second set would fork the conversations.
	// The phone link and the agent door are the desktop's, started in Startup.
	if !a.isHiveMember() {
		a.startScheduler()
		a.startTelegram()
		a.startHive()
		a.startTrackers()
		a.startAlarmWatch()
		a.lookForRemoteAgents()
	}
}

// syncProxy brings the embedded CLIProxyAPI in line with the current settings:
// started when enabled (and restarted when the port changes), stopped otherwise.
// A failure here is non-fatal — it is reported through GetStatus and the agent
// falls back to the configured LLM endpoint.
func (a *App) syncProxy() {
	a.mu.Lock()
	defer a.mu.Unlock()

	want := a.settings != nil && a.settings.CLIProxyEnabled
	if !want {
		if a.proxy != nil {
			_ = a.proxy.Close()
			a.proxy = nil
		}
		a.proxyErr = ""
		return
	}
	if a.proxy != nil && a.proxy.Port() == a.settings.CLIProxyPort {
		return
	}
	if a.proxy != nil {
		_ = a.proxy.Close()
		a.proxy = nil
	}

	p, err := backend.StartCLIProxy(a.settings.CLIProxyPort)
	if err != nil {
		a.proxyErr = err.Error()
		return
	}
	a.proxyErr = ""
	a.proxy = p
}

// shutdown releases backend resources.
func (a *App) Shutdown(ctx context.Context) {
	// Before the turns: a turn ending stops the coding run it waits on, and
	// those runs are to carry on and be picked up when SuperLeo is back.
	a.detachCLIRuns()
	// The turns first: what they started — shell commands above all — is
	// bound to their contexts, and the process going away does not end a
	// child that has a process group of its own.
	a.stopTurns(4 * time.Second)
	// Before the lock: stopping the cron loop must not race the service it runs
	// its turns against. The Telegram poller is stopped first and for exactly
	// the same reason — it may be in the middle of a turn.
	a.stopTelegram()
	// A queen going on purpose says she will be back, before anything of
	// her stops: her members wait instead of electing another.
	a.announceRestart()
	a.stopHive()
	a.stopTaskBoard()
	a.stopLocalHive()
	a.stopAlarmWatch()
	a.stopScheduler()
	a.stopPulse()
	// Also before the lock, and both for the same reason as the scheduler: each
	// takes a lock that is not a.mu — the view waits for its HTTP server to
	// drain, and stopCompanion takes a.mu itself to unhook the server. An open
	// browser tab must not outlive the app it is a window onto.
	_ = a.graphs.Close()
	a.stopPhoneLink()
	a.stopCompanion()
	a.actionsV.Close() // nil-safe: never built when no button was pressed

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.svc != nil {
		_ = a.svc.Close()
		a.svc = nil
	}
	if a.sse != nil {
		_ = a.sse.Close()
		a.sse = nil
	}
	if a.proxy != nil {
		_ = a.proxy.Close()
		a.proxy = nil
	}
}

// rebuild (re)constructs the backend Service from current settings.
func (a *App) rebuild() {
	a.mu.Lock()
	defer a.mu.Unlock()
	// YOLO lives in the gate, and a rebuild makes a new gate. Carried over,
	// or a settings save switches it off while the status bar still says
	// YOLO, and the next delete asks after all.
	yolo := false
	if a.svc != nil {
		yolo, _ = a.svc.ToolGate().YoloStatus()
		_ = a.svc.Close()
		a.svc = nil
	}
	cfg := a.effective()
	svc, err := backend.NewService(cfg)
	if err != nil {
		a.buildErr = err.Error()
		return
	}
	a.buildErr = ""
	a.svc = svc
	if yolo {
		svc.ToolGate().StartYolo()
	}
	// A rebuild is what a settings save produces, so this is where a changed
	// host list stops being ignored.
	a.resetRemote()
	// The run wall watches this service the way it watched the last one, and
	// so does the meter beside it.
	// And a noted alarm tool it runs sends the alarm watcher to look.
	svc.Agent().RegisterObserver(a.runWall(), a.livePulse(), alarmToolObserver{poke: a.alarmsMayHaveChanged})
	// The service is built with its approval gate already installed but with
	// nobody to ask; this is where the app volunteers. Done on every rebuild
	// because SaveSettings constructs a whole new Service, and a gate that
	// lost its approver would start denying every shell command with "no way
	// to ask" while the window is sitting right there.
	svc.ToolGate().SetApprover(a.askToolApproval)
	// Every notice the backend raises reaches whatever surface is attached.
	// Installed on each rebuild for the same reason the approver is: a settings
	// save constructs a whole new Service, and one that lost its emitter would
	// go on posting webhooks while the open window heard nothing.
	svc.Notices().SetEmitter(a.emit)
	// The desktop's own banner is a third surface, and it subscribes like any
	// other. Registered under a name so a rebuild replaces it rather than
	// stacking a second one that would draw every notification twice.
	svc.Notices().Subscribe("banner", a.bannerSink)
	// And the phones: a notice marked Push becomes a notification on every
	// paired phone that registered for them (postnotify.go).
	svc.Notices().Subscribe("apns", a.apnsNoticeSink())
	// The character on the window, and the two calls that let a turn use it.
	// Also per rebuild: these are registered on the agent, and a new agent
	// starts with none of them.
	a.registerPetTools(svc)
	// The agents on other machines, if the settings name any. Read from the
	// same settings this Service was built from, so the tool's catalogue and
	// the runner can never describe two different sets.
	a.registerRemoteTools(svc, cfg.RemoteAgents)
	a.registerCodingAgentTools(svc, cfg)
	a.registerScheduleTools(svc)
	a.registerSelfTools(svc)
	a.registerHomeLayoutTool(svc, cfg)
	a.registerHiveTools(svc, cfg)
	// A queen in max delegation keeps her work tools out of her own runs
	// while a worker is live (app_hive_delegate.go). Asked per run, so a
	// worker joining or leaving counts from the next turn.
	svc.SetRunToolFilter(a.queenRunFilter)
	a.registerLongTaskTools(svc, cfg)
	// The first question after a start paid for everything the service opens
	// lazily — the model's connection, the shared brain's connection and its
	// first search, the tool catalogue — about two seconds before its first
	// word, measured; four on the hive's queen. The start pays instead.
	go warmUp(svc)
}

// warmUp assembles one throwaway turn, so the first real one is not the one
// that opens every connection.
func warmUp(svc *backend.Service) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	start := time.Now()
	if err := svc.Warm(ctx); err != nil {
		log.Printf("superleo: warm-up: %v", err)
		return
	}
	log.Printf("superleo: warmed up in %s", time.Since(start).Round(time.Millisecond))
}

// restartScheduler rebinds the timers to the current service. Callers must not
// hold a.mu.
func (a *App) restartScheduler() {
	a.startScheduler()
}

// effective returns the settings the backend Service is actually built from:
// the user's settings, with the brain redirected at the embedded CLIProxyAPI
// when it is running. Callers must hold a.mu.
func (a *App) effective() *backend.Settings {
	if a.settings == nil {
		return nil
	}
	s := *a.settings
	if a.proxy != nil {
		s.LLMBaseURL = a.proxy.BaseURL()
		s.LLMKey = a.proxy.Key()
	}
	return &s
}

// GetSettings returns the current settings.
func (a *App) GetSettings() backend.Settings {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.settings == nil {
		return backend.Settings{}
	}
	return *a.settings
}

// SaveSettings persists new settings and rebuilds the Service.
func (a *App) SaveSettings(s backend.Settings) error {
	if err := backend.ValidateQueenName(s.QueenName, s.QueenAliases); err != nil {
		return err
	}
	if err := s.Proxy.Validate(); err != nil {
		return err
	}
	a.mu.Lock()
	before := a.settings
	prev := ""
	if before != nil {
		prev = before.Hive.QueenEngine
	}
	a.mu.Unlock()
	if err := a.validQueenEngine(&s, prev); err != nil {
		return err
	}
	if err := s.Save(); err != nil {
		return err
	}
	a.mu.Lock()
	a.settings = &s
	a.mu.Unlock()
	if s.Hive.QueenEngine != prev {
		// Who answered last was under the old engine; until a turn says
		// otherwise, the new one is who the queen thinks with.
		a.setQueenNow("")
	}
	// Another engine for the queen is read by the next turn as it starts;
	// rebuilding for it would close the service under the turns running on
	// it and stop them, and the coding runs they started with them.
	if backend.OnlyQueenEngineDiffers(before, &s) {
		a.emit("settings:changed", nil)
		return nil
	}
	backend.ApplyProxy(s.Proxy)
	a.syncProxy()
	a.rebuild()
	a.syncLoginItem(s.StartAtLogin)
	// The scheduler runs turns against the service that was just replaced, and
	// so does the Telegram bridge. Restarting the bridge here is also what
	// makes turning Telegram on a settings change rather than a restart.
	a.restartScheduler()
	a.startTelegram()
	a.startHive()
	// agent_link.relay_only may have moved an agent in or out of the machines.
	a.agentsChanged()
	// What the settings switch on or off shows elsewhere (the 3D world's
	// desktop control), and those views have nothing else to listen to.
	a.emit("settings:changed", nil)
	return nil
}

// CLIProxyStatus reports the embedded CLIProxyAPI state for the Settings page.
func (a *App) CLIProxyStatus() map[string]any {
	a.mu.Lock()
	p := a.proxy
	perr := a.proxyErr
	a.mu.Unlock()

	st := map[string]any{
		"running": p != nil,
		"error":   perr,
		"baseURL": p.BaseURL(),
		"authDir": p.AuthDir(),
		"config":  p.ConfigPath(),
		"models":  []string{},
	}
	if p != nil {
		models, err := p.Models(context.Background())
		if err != nil {
			st["error"] = err.Error()
		} else {
			st["models"] = models
		}
	}
	return st
}

// ensureProxy returns the running proxy, starting it if needed. Login and the
// status panel both need it before the user has saved anything, so it must not
// depend on the persisted enable flag.
func (a *App) ensureProxy() (*backend.CLIProxy, error) {
	a.mu.Lock()
	if a.proxy != nil {
		p := a.proxy
		a.mu.Unlock()
		return p, nil
	}
	port := backend.DefaultCLIProxyPort
	if a.settings != nil && a.settings.CLIProxyPort > 0 {
		port = a.settings.CLIProxyPort
	}
	a.mu.Unlock()

	p, err := backend.StartCLIProxy(port)

	a.mu.Lock()
	defer a.mu.Unlock()
	if err != nil {
		a.proxyErr = err.Error()
		return nil, err
	}
	// Another goroutine may have won the race; keep the first one.
	if a.proxy != nil {
		go p.Close()
		return a.proxy, nil
	}
	a.proxyErr = ""
	a.proxy = p
	return p, nil
}

// SetUIRules receives the transcript's rendering rules from the frontend (which
// owns the AIGUI registry and plugin list) and rebuilds the agent only when they
// actually changed, so a restart with the same UI is not paying for a rebuild.
func (a *App) SetUIRules(rules string) string {
	changed, err := backend.SaveUIRules(rules)
	if err != nil {
		return err.Error()
	}
	if changed {
		a.rebuild()
		// rebuild() closes the old agent.Service, and a closed Service owns a
		// closed store. The scheduler holds the Service it was started with, so
		// leaving it bound to the old one means every scheduled run from here on
		// writes its history nowhere — silently, until agent-go v3.5.x started
		// refusing to run on a closed Service. SaveSettings has always paired the
		// two; this path was the one that forgot.
		a.restartScheduler()
	}
	return "ok"
}

// CLIProxyProviders lists the providers the user can log into.
func (a *App) CLIProxyProviders() []backend.CLIProxyProvider {
	return backend.CLIProxyProviders()
}

// CLIProxyLogin runs a provider OAuth flow for the embedded proxy. It returns
// immediately; progress is delivered as "cliproxy:login" events with a status of
// started / prompt / done / error. On success the credential lands in the auth
// dir and the proxy hot-loads it, so the new models appear without a restart.
//
// Only one login may be in flight at a time.
func (a *App) CLIProxyLogin(provider, projectID string) string {
	a.mu.Lock()
	busy := a.loginCh != nil
	a.mu.Unlock()

	// Logging in only needs the proxy's config and auth dir, so start it on
	// demand rather than making the user save settings first.
	p, err := a.ensureProxy()
	if err != nil {
		a.emitLogin("error", provider, "could not start the CLI proxy: "+err.Error())
		return "error"
	}
	if busy {
		a.emitLogin("error", provider, "another login is already in progress")
		return "error"
	}

	// Buffered so a late submission never blocks the frontend call.
	ch := make(chan string, 1)
	a.mu.Lock()
	a.loginCh = ch
	a.mu.Unlock()

	go func() {
		defer func() {
			a.mu.Lock()
			a.loginCh = nil
			a.mu.Unlock()
		}()

		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
		defer cancel()

		a.emitLogin("started", provider, "Opening your browser to sign in…")

		prompt := func(text string) (string, error) {
			a.emitLogin("prompt", provider, text)
			select {
			case v := <-ch:
				return v, nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}

		saved, err := p.Login(ctx, provider, projectID, prompt)
		if err != nil {
			a.emitLogin("error", provider, err.Error())
			return
		}
		_ = saved // the path is an implementation detail; the UI just needs the outcome
		a.emitLogin("done", provider, "Signed in.")
	}()

	return "ok"
}

// CLIProxyAccounts lists the signed-in provider credentials (no tokens).
func (a *App) CLIProxyAccounts() []backend.CLIProxyAccount {
	a.mu.Lock()
	p := a.proxy
	a.mu.Unlock()
	if p == nil {
		return []backend.CLIProxyAccount{}
	}
	accounts, err := p.Accounts()
	if err != nil {
		return []backend.CLIProxyAccount{}
	}
	return accounts
}

// CLIProxySetAccountEnabled enables or disables one credential without deleting
// it — the way to park an account or switch which one serves a shared model.
func (a *App) CLIProxySetAccountEnabled(file string, enabled bool) string {
	a.mu.Lock()
	p := a.proxy
	a.mu.Unlock()
	if p == nil {
		return "CLI proxy is not running"
	}
	if err := p.SetAccountDisabled(file, !enabled); err != nil {
		return err.Error()
	}
	return "ok"
}

// CLIProxySignOut deletes a credential, signing that provider account out.
func (a *App) CLIProxySignOut(file string) string {
	a.mu.Lock()
	p := a.proxy
	a.mu.Unlock()
	if p == nil {
		return "CLI proxy is not running"
	}
	if err := p.RemoveAccount(file); err != nil {
		return err.Error()
	}
	return "ok"
}

// CLIProxySubmitPrompt answers the pending login prompt (the pasted callback URL).
func (a *App) CLIProxySubmitPrompt(value string) string {
	a.mu.Lock()
	ch := a.loginCh
	a.mu.Unlock()
	if ch == nil {
		return "no login in progress"
	}
	select {
	case ch <- value:
		return "ok"
	default:
		return "not waiting for input"
	}
}

// windowEmit is the Wails leg of emit, held in a variable only so a test can
// stand in for a window it has no way to create: the real runtime calls
// log.Fatalf if it is handed a context it did not make, so the fan-out below
// cannot otherwise be checked at all.
var windowEmit = func(ctx context.Context, name string, payload map[string]any) {
	runtime.EventsEmit(ctx, name, payload)
}

// emit delivers one payload to every surface currently attached to this app.
//
// Every surface, not the first one found. This used to return after emitFn, on
// the assumption that a process has either a window or a socket. "Open in
// Browser" breaks that assumption: the desktop app keeps its window and gains a
// companion server, and the same chat has to stream into both at once. An
// early return there is silent — nothing errors, the window simply stops
// updating the moment a browser tab exists.
//
// Each leg carries its own guard for the case where it is not attached. Before
// startup there is no window, so the Wails call is skipped rather than made
// against a nil context (which the Wails runtime treats as fatal).
func (a *App) emit(name string, payload map[string]any) {
	payload = a.withTurnSession(name, payload)
	a.journal().record(name, payload)
	if a.emitFn != nil {
		a.emitFn(name, payload)
	}
	// In-process listeners: an MCP caller waiting on its answer (mcp_ask.go).
	a.taps.Range(func(_, fn any) bool {
		fn.(func(string, map[string]any))(name, payload)
		return true
	})
	if hub := a.companionHub.Load(); hub != nil {
		hub.broadcast(name, payload)
	}
	if tap := a.agentTap.Load(); tap != nil {
		(*tap)(name, payload)
	}
	if a.ctx != nil && !a.quietWhileLinked(name) {
		windowEmit(a.ctx, name, payload)
	}
}

func (a *App) emitLogin(status, provider, message string) {
	a.emit("cliproxy:login", map[string]any{
		"status":   status,
		"provider": provider,
		"message":  message,
	})
}

// GetStatus reports backend readiness for the frontend.
func (a *App) GetStatus() map[string]any {
	a.mu.Lock()
	st := map[string]any{
		"ready":          a.svc != nil || engineThinks(a.settings),
		"error":          a.buildErr,
		"skills":         []string{},
		"memoryMode":     "",
		"mcp":            0,
		"mcpTools":       0,
		"avatarPort":     0,
		"cliproxy":       a.proxy != nil,
		"cliproxyError":  a.proxyErr,
		"scheduler":      a.scheduler != nil,
		"schedulerError": a.schedulerErr,
		"telegram":       a.telegram != nil,
		"telegramError":  a.telegramErr,
		"notifications":  a.notifyOK,
	}
	if a.settings != nil {
		st["avatarPort"] = a.settings.AvatarPort
	}
	st["queenName"] = a.settings.QueenDisplayName()
	st["queenAliases"] = a.settings.QueenAliasList()
	svc := a.svc
	a.mu.Unlock()
	if svc != nil {
		// Read without a.mu: asking the service waits on its own locks, and
		// every message sent takes a.mu first, so a poll held up here would
		// hold up the next message with it.
		st["skills"] = svc.InstalledSkills()
		st["memoryMode"] = svc.MemoryMode
		// Counted here rather than shipped as the whole list: the status bar
		// only needs a number, and this is polled every fifteen seconds.
		running, tools := 0, 0
		for _, srv := range svc.MCPServers() {
			if !srv.Running {
				continue
			}
			running++
			tools += srv.ToolCount
		}
		st["mcp"] = running
		st["mcpTools"] = tools
	}
	return st
}

// Dashboard is one call with everything the dashboard page shows: the
// configured brain (never its key), runs in flight, token usage, and what
// task memory holds. Polled by the frontend, so it must stay cheap.
func (a *App) Dashboard() map[string]any {
	a.mu.Lock()
	svc := a.svc
	s := a.settings
	a.mu.Unlock()

	out := map[string]any{"ready": svc != nil || engineThinks(s)}
	if s != nil {
		model := s.LLMModel
		if model == "" && engineThinks(s) {
			model = engineLabel(s.Hive.QueenEngine)
		}
		out["llm"] = map[string]any{
			"model":      model,
			"baseURL":    s.LLMBaseURL,
			"embedModel": s.EmbedModel,
			"maxRounds":  s.MaxRounds,
			"workspace":  s.WorkspaceDir,
		}
	}
	if svc != nil {
		for k, v := range svc.DashboardData() {
			out[k] = v
		}
	}
	return out
}

// GraphView starts the live 3D view of the knowledge graph and reports where it
// is: a URL on this machine's loopback serving the brain SuperLeo actually uses,
// updating itself as the graph changes.
//
// Started here rather than at boot. The view is a listener and a poller that
// reads the whole graph every couple of seconds, which is a fair price for a
// page someone is looking at and a poor one for every session that never opens
// it. Once started it stays up for the life of the process — reopening the page
// returns the same URL instead of binding another port.
//
// A failure comes back inside the payload rather than as an error, because the
// reason is what the page has to show: "the shared brain is not answering" and
// "there is nothing in the local one yet" are opposite problems and the user
// can only act on the difference.
func (a *App) GraphView() map[string]any {
	// The settings are read under a.mu and the view is started without it:
	// starting one reads the whole brain first, and against a shared brain that
	// is a network call. Holding the lock that a settings save also takes would
	// mean a slow or unreachable server freezes the Settings page too.
	a.mu.Lock()
	s := a.settings
	a.mu.Unlock()

	ctx := a.ctx
	if ctx == nil {
		// Serve mode and tests have no Wails context. The view's lifetime is
		// owned by a.graphs.Close either way, so the parent context only has to
		// be a valid one.
		ctx = context.Background()
	}
	return a.graphs.GraphStatus(ctx, s).Map()
}

// hiveTurnNote is what a worker adds to the words of a hive: turn, given
// how much hive mail is waiting. An order gets the mail and the report it
// owes the queen; the person asking the worker directly gets told it is
// them, so the answer is to them and not a brief for the queen to pass on.
func hiveTurnNote(sessionID string, unread int) string {
	if backend.IsDirectAsk(sessionID) {
		return "\n\n(The person is talking to you directly, not the queen and not through her. " +
			"Answer them yourself, as you; do not address the queen or suggest what to reply.)"
	}
	note := ""
	// Mail waiting is mentioned with the order, not put into it: the order
	// stands alone, and whether the notes bear on it is the worker's call.
	if unread > 0 {
		note += fmt.Sprintf("\n\n(You have %d unread hive message(s); hive_inbox shows them.)", unread)
	}
	// The queen reads the report and answers the person herself, while
	// they wait on both: what was asked, without a write-up around it.
	return note + "\n\n(Report to the queen: what was asked, plainly, without padding. She answers the person.)"
}

// SendChat streams a chat turn. Streaming output is delivered via Wails events
// ("chat:event", "chat:done", "chat:error", "chat:cancelled"); avatar
// lifecycle/emotion events are pushed through the avatar driver.
//
// Exactly one of chat:done / chat:error / chat:cancelled ends a turn.
//
// The return value is the request id every one of those events is tagged with
// under "requestId" — one conversation may have several asks in flight, and
// without the tag the second ask's stream lands in the first one's bubble. An
// empty string means nothing was started (the error event carries the reason).
//
// With a queen engine set (Settings → Hive), a person's turn here is thought
// by that agent CLI instead of this app's model; see app_queen_engine.go.
func (a *App) SendChat(sessionID, message string, imagePaths []string) string {
	return a.sendChat(sessionID, message, imagePaths, false)
}

// SendCallChat is SendChat for a turn said in a voice call. SuperLeo's own
// agent answers it whatever the queen engine is: an engine takes seconds to
// start, and a call cannot wait them out.
func (a *App) SendCallChat(sessionID, message string, imagePaths []string) string {
	return a.sendChat(sessionID, message, imagePaths, true)
}

// sendChat is SendChat; own keeps the turn with this app's model, never the
// queen engine.
func (a *App) sendChat(sessionID, message string, imagePaths []string, own bool) string {
	requestID := uuid.NewString()
	a.announceTurn(requestID, sessionID, message, len(imagePaths))

	// A message addressed to another agent goes there instead, and this app's
	// model never runs. Checked before the readiness gate below on purpose: an
	// unconfigured LLM is no reason not to forward a question to a machine that
	// has one.
	//
	// The queen's own names come first: "@queen" or "@<her name>" is this
	// app's turn even when a worker or an agent happens to share the name.
	toQueen := a.addressedToQueen(message)
	if name, rest, ok := addressedTo(message, a.addressable); ok && !toQueen {
		if a.isDrivenCLI(name) {
			a.routeToCLI(requestID, sessionID, name, rest)
		} else {
			a.routeToRemote(requestID, sessionID, name, rest)
		}
		return requestID
	}

	// Asked again while this conversation is still answering: it starts now
	// all the same (see turn_at_once.go).
	a.startTurn(requestID, sessionID, message, imagePaths, own)
	return requestID
}

// startTurn runs a turn now: with the queen's engine, when one is set and this
// is a person talking to her, and with this app's model otherwise. An engine
// session takes one turn at a time, so while the conversation is busy this
// app's model stands in for the engine rather than the turn waiting for it.
func (a *App) startTurn(requestID, sessionID, message string, imagePaths []string, own bool) {
	if !own {
		if engine := a.queenEngineFor(sessionID, imagePaths); engine != "" {
			busy := a.sessionBusy(sessionID)
			if busy && a.hasModel() {
				a.ownTurn(requestID, sessionID, message, imagePaths, engine)
				return
			}
			// Free, or busy with no model of its own to stand in: the
			// engine, in a session of its own when the conversation's is
			// still answering.
			a.routeToEngine(requestID, sessionID, engine, message, busy)
			return
		}
	}
	a.ownTurn(requestID, sessionID, message, imagePaths, "")
}

// hasModel is whether this app has a model of its own to answer with (a
// test's ownTurnFn stands for one).
func (a *App) hasModel() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.svc != nil || a.ownTurnFn != nil
}

// ownTurn runs a turn with this app's model. standIn names the queen engine
// it answers for when that engine could not take the turn: the answer then
// opens with a line saying so.
func (a *App) ownTurn(requestID, sessionID, message string, imagePaths []string, standIn string) {
	// The queen's figure and the status bar name who is thinking: with an
	// engine set, this turn is SuperLeo's all the same, and they say so.
	a.mu.Lock()
	engineSet := a.settings != nil && a.settings.Hive.Role != backend.HiveRoleWorker && a.settings.Hive.QueenEngine != ""
	a.mu.Unlock()
	if engineSet {
		a.setQueenNow(queenNowSuperLeo)
	}

	if a.ownTurnFn != nil {
		a.ownTurnFn(requestID, sessionID, message, standIn)
		return
	}
	a.mu.Lock()
	svc := a.svc
	driver := a.avatar
	buildErr := a.buildErr
	a.mu.Unlock()

	if svc == nil {
		// The id is returned even though nothing was started. Withholding it
		// (returning "") orphans the error event below: the caller cannot bind
		// an id it never received, so the reason the backend is unready — the
		// build error — becomes unreachable and the UI can only show a generic
		// message. Handing the id back costs nothing and lets the failure be
		// reported against the ask that caused it.
		a.emit("chat:error", map[string]any{
			"requestId": requestID,
			"error":     "backend not ready: " + buildErr,
		})
		return
	}

	// Registered here rather than inside the goroutine: SendChat returns the id
	// to the frontend, and a stop pressed before the goroutine got scheduled
	// would otherwise be told the run does not exist.
	// No deadline. A turn ends when the model stops, when the user presses
	// stop, or when the app shuts down — not when a clock says so. The ten
	// minutes that used to be here came in with the first commit and were
	// never a decision about this app: they cut off real work mid-sentence and
	// reported it as a turn the user had cancelled, which is a lie the
	// transcript then kept.
	ctx, cancel := context.WithCancel(withChatRequest(backend.WithChatSession(context.Background(), sessionID), requestID))
	a.trackRun(requestID, cancel)
	a.runSession(requestID, sessionID)
	a.openTurns.Store(requestID, backend.ChatSessionInfo{ID: sessionID, Title: firstLineOf(message, 80),
		UpdatedAt: time.Now().UTC().Format(time.RFC3339)})

	// An order from the hive is put on the task board, so the panel can show
	// this worker working. A person typing at this instance is not a task.
	taskID, board := "", (*backend.TaskBoard)(nil)
	if strings.HasPrefix(sessionID, backend.HiveSessionPrefix) {
		board = a.tasks()
		// The order's own id when the session carries one, so the queen's
		// record and this one are the same UUID.
		id, _ := backend.TaskIDFromSession(sessionID)
		taskID = board.StartAs(id, a.hiveName(), "", backend.TaskIn, message)
		board.OnStop(taskID, func() { a.CancelChat(requestID) })
		message += hiveTurnNote(sessionID, a.mailbox().Unread())
	}

	if taskID == "" {
		a.noteNoWorkers(requestID)
	}

	asked, askedAt := message, time.Now()
	go func() {
		defer cancel()
		defer a.untrackRun(requestID)
		defer a.openTurns.Delete(requestID)
		defer func() {
			recordTurnTimeBy(sessionID, asked, askedAt, time.Since(askedAt), turnBy{StoodInFor: standIn})
		}()

		driver.Emit(backend.AvatarEvent{Type: "state", State: backend.AvatarStateThinking})
		if standIn != "" {
			a.emit("chat:event", map[string]any{"requestId": requestID, "type": "partial", "content": standInNote(standIn),
				"engine": "", "standInFor": standIn})
		}

		pulser := backend.NewPulser(func(kind, tool string, n int) { board.Pulse(taskID, kind, tool, n) })
		// Every way out of this turn first closes the tool calls it left open,
		// so none keeps showing as running once the turn is over.
		var open openTools
		closeOpen := func(reason string) {
			for _, ev := range open.closeEvents(requestID, reason) {
				a.emit("chat:event", ev)
			}
		}
		turn, err := svc.StreamTurn(ctx, sessionID, message, imagePaths, func(ev *agent.Event) {
			switch ev.Type {
			case agent.EventTypeToolCall:
				open.call(ev.ToolName)
			case agent.EventTypeToolResult:
				open.result(ev.ToolName)
			}
			if taskID != "" {
				resultLen := 0
				if ev.ToolResult != nil {
					if b, e := json.Marshal(ev.ToolResult); e == nil {
						resultLen = len(b)
					}
				}
				pulser.Event(string(ev.Type), ev.ToolName, len(ev.Content), resultLen)
			}
			switch ev.Type {
			case agent.EventTypeToolCall:
				board.Live(taskID, backend.LiveTool(ev.ToolName, ev.ToolArgs))
			case agent.EventTypeToolResult:
				board.Live(taskID, backend.LiveResult(ev.ToolResult))
			case agent.EventTypePartial:
				board.Live(taskID, ev.Content)
			}
			switch ev.Type {
			case agent.EventTypeToolCall:
				board.Progress(taskID, backend.PhaseTool, ev.ToolName)
			case agent.EventTypeThinking:
				board.Progress(taskID, backend.PhaseThinking, "")
			case agent.EventTypePartial:
				board.Progress(taskID, backend.PhaseWriting, "")
			case agent.EventTypeSteerDropped:
				// Hive mail put into this turn that it ended without reading.
				a.steerDropped(ev.Content)
			case agent.EventTypeStateUpdate:
				// Waiting in line for the browser (backend/browser_queue.go):
				// the order shows as queued, not working, until its turn.
				switch {
				case strings.HasPrefix(ev.Content, backend.BrowserQueued):
					board.Progress(taskID, backend.PhaseQueued, "browser")
					board.Live(taskID, ev.Content)
				case strings.HasPrefix(ev.Content, backend.BrowserFree):
					board.Progress(taskID, backend.PhaseThinking, "")
				}
			}
			// Every type is forwarded, unfiltered: "thinking" and "state_update"
			// are the only progress the UI has while PTC writes its code.
			out := map[string]any{
				"requestId": requestID,
				"type":      string(ev.Type),
				"content":   ev.Content,
				"tool":      ev.ToolName,
				"args":      ev.ToolArgs,
				"result":    ev.ToolResult,
				"debugType": ev.DebugType,
			}
			// A model call's progress on the wire (agent-go's model_wire):
			// its step, attempt, HTTP status and wait, for the turn journal
			// and the trace.
			if ev.Type == "model_wire" {
				out["wire"] = ev.StateDelta
			}
			// A tool that returned an error has it in the event's content and
			// an empty result (agent-go's emitToolResult), which read alone
			// looks like a success.
			if ev.Type == agent.EventTypeToolResult && toolEventFailed(ev) {
				out["failed"] = true
			}
			a.emit("chat:event", out)
			switch ev.Type {
			case agent.EventTypeToolCall:
				driver.Emit(backend.AvatarEvent{Type: "state", State: backend.AvatarStateWorking, Tool: ev.ToolName})
			case agent.EventTypePartial:
				if strings.TrimSpace(ev.Content) != "" {
					driver.Emit(backend.AvatarEvent{Type: "state", State: backend.AvatarStateSpeaking})
				}
			}
		})

		// A stopped turn is not a failed one. Reported as its own event so the
		// transcript can say "you stopped this" and keep whatever was already
		// streamed, instead of painting a red error over a half-written answer.
		//
		// ctx.Err() is in the test as well as the stop flag: shutdown cancels
		// the same context the button does, and the agent loop reports both as
		// an ordinary workflow_cancelled event with a nil error — so without
		// it, a turn cut short would settle as a successful answer with
		// nothing in it.
		final := turn.Text
		if a.runCancelled(requestID) || errors.Is(err, context.Canceled) || ctx.Err() != nil {
			closeOpen("stopped")
			driver.Emit(backend.AvatarEvent{Type: "state", State: backend.AvatarStateIdle})
			partial, _ := backend.SplitEmotion(final)
			board.Finish(taskID, backend.TaskCancelled, partial, "")
			a.emit("chat:cancelled", map[string]any{"requestId": requestID, "final": partial})
			return
		}

		if err != nil {
			closeOpen("the turn failed before this finished")
			driver.Emit(backend.AvatarEvent{Type: "state", State: backend.AvatarStateIdle})
			board.Finish(taskID, backend.TaskFailed, "", err.Error())
			a.emit("chat:error", map[string]any{"requestId": requestID, "error": err.Error()})
			return
		}

		closeOpen("the turn ended before this finished")
		reply, emotion := backend.SplitEmotion(final)
		if emotion != "" {
			driver.Emit(backend.AvatarEvent{Type: "emotion", Emotion: emotion})
		}
		if strings.TrimSpace(reply) != "" {
			driver.Emit(backend.AvatarEvent{Type: "speech", Text: reply})
		}
		driver.Emit(backend.AvatarEvent{Type: "state", State: backend.AvatarStateIdle})
		board.Finish(taskID, backend.TaskDone, reply, "")
		// The tokens the turn used ride along, so a queen that ordered it can
		// add them to its own accounting.
		done := map[string]any{"requestId": requestID, "final": reply, "emotion": emotion}
		if turn.Usage != nil {
			done["usage"] = map[string]any{
				"prompt_tokens":        turn.Usage.PromptTokens,
				"completion_tokens":    turn.Usage.CompletionTokens,
				"cached_prompt_tokens": turn.Usage.CachedPromptTokens,
				"cache_write_tokens":   turn.Usage.CacheWriteTokens,
			}
		}
		if standIn != "" {
			done["final"] = standInNote(standIn) + reply
			done["standInFor"] = standIn
		}
		a.emit("chat:done", done)
	}()
}

// chatRun is one streaming turn, as far as stopping it is concerned.
//
// cancelled is remembered rather than inferred from the error, because there is
// nothing reliable to infer it from: a cancelled turn does not fail. The agent
// loop notices its context is gone, emits "Execution cancelled" as an ordinary
// event and closes the stream, so Service.Stream returns a nil error exactly as
// it does for a turn that finished. Only the side that pulled the plug knows.
type chatRun struct {
	cancel    context.CancelFunc
	cancelled bool
	// session is the conversation the turn belongs to, so a hive message for
	// that conversation can find the turn that is running in it.
	session string
}

// trackRun registers an in-flight turn so CancelChat can find it.
func (a *App) trackRun(requestID string, cancel context.CancelFunc) {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	if a.runs == nil {
		a.runs = make(map[string]*chatRun)
	}
	a.runs[requestID] = &chatRun{cancel: cancel}
}

// untrackRun forgets a finished turn. Every goroutine that calls trackRun must
// defer this, or the map grows for the lifetime of the process and a request id
// that is long over still answers "ok" to a stop.
func (a *App) untrackRun(requestID string) {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	delete(a.runs, requestID)
}

// runSession records which conversation a tracked turn belongs to.
func (a *App) runSession(requestID, sessionID string) {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	if r := a.runs[requestID]; r != nil {
		r.session = sessionID
	}
}

// runningSessions lists the conversations with a turn running in them.
func (a *App) runningSessions() []string {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	seen := map[string]bool{}
	out := []string{}
	for _, r := range a.runs {
		if r.session != "" && !seen[r.session] {
			seen[r.session] = true
			out = append(out, r.session)
		}
	}
	sort.Strings(out)
	return out
}

// sessionBusy reports whether a turn is running in the conversation.
func (a *App) sessionBusy(sessionID string) bool {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	return a.sessionBusyLocked(sessionID)
}

// sessionBusyLocked is sessionBusy with runMu held.
func (a *App) sessionBusyLocked(sessionID string) bool {
	for _, r := range a.runs {
		if r.session == sessionID {
			return true
		}
	}
	return false
}

// runCancelled reports whether this turn was stopped by the user. It must be
// read before untrackRun — after that the answer is always false.
func (a *App) runCancelled(requestID string) bool {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	r := a.runs[requestID]
	return r != nil && r.cancelled
}

// CancelChat stops one streaming turn. The id is the one SendChat returned.
//
// The reply is "ok" when a turn was actually stopped, and a sentence explaining
// why not otherwise — a stop that lands after the answer did is not an error,
// and the caller needs to be able to tell the two apart.
func (a *App) CancelChat(requestID string) string {
	a.runMu.Lock()
	r := a.runs[requestID]
	if r != nil {
		r.cancelled = true
	}
	a.runMu.Unlock()

	if r == nil {
		return "that request is not running — it has already finished, or the id is unknown"
	}
	// Outside the lock: cancel runs the context's own callbacks, and nothing is
	// gained by holding up a second stop while they do.
	r.cancel()
	return "ok"
}

// stopTurns cancels every running turn and waits, up to wait, for them to wind
// down, which is when the commands they started have been signalled.
func (a *App) stopTurns(wait time.Duration) {
	a.CancelAllChats()
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		a.runMu.Lock()
		n := len(a.runs)
		a.runMu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// CancelAllChats stops every streaming turn — the "stop everything" the user
// wants when several asks are in flight and all of them are going nowhere.
func (a *App) CancelAllChats() string {
	a.runMu.Lock()
	pending := make([]context.CancelFunc, 0, len(a.runs))
	for _, r := range a.runs {
		r.cancelled = true
		pending = append(pending, r.cancel)
	}
	a.runMu.Unlock()

	if len(pending) == 0 {
		return "nothing is running"
	}
	for _, cancel := range pending {
		cancel()
	}
	return fmt.Sprintf("ok: stopped %d", len(pending))
}

// Deliverables returns the files a conversation produced. An empty session id
// returns everything in the workspace.
func (a *App) Deliverables(sessionID string) []agent.Deliverable {
	a.mu.Lock()
	svc := a.svc
	a.mu.Unlock()
	if svc == nil {
		return nil
	}
	d, err := svc.Deliverables(context.Background(), sessionID)
	if err != nil {
		return nil
	}
	return d
}

// ReadWorkspaceFile reads a workspace-relative file (empty string on error).
func (a *App) ReadWorkspaceFile(path string) string {
	a.mu.Lock()
	svc := a.svc
	a.mu.Unlock()
	if svc == nil {
		return ""
	}
	out, err := svc.ReadWorkspaceFile(path)
	if err != nil {
		return ""
	}
	return out
}

// ChatSessions lists past conversations, most recent first.
func (a *App) ChatSessions() []backend.ChatSessionInfo {
	a.mu.Lock()
	svc, queen := a.svc, a.hive != nil
	a.mu.Unlock()
	if svc == nil {
		return []backend.ChatSessionInfo{}
	}
	saved := svc.Sessions(200)
	if queen {
		// A queen is never given orders, so a hive: conversation on her is a
		// piece she did herself for one of the person's (app_hive_delegate.go),
		// not a conversation of theirs to list.
		kept := saved[:0]
		for _, c := range saved {
			if !strings.HasPrefix(c.ID, backend.HiveSessionPrefix) {
				kept = append(kept, c)
			}
		}
		saved = kept
	}
	return withOpenTurns(saved, &a.openTurns)
}

// withOpenTurns adds the conversations whose turn is still running and that
// the store does not list yet — a conversation is saved when its first answer
// is, and one asked from another device or by a long job would otherwise be
// missing from every list for as long as it works.
func withOpenTurns(saved []backend.ChatSessionInfo, open *sync.Map) []backend.ChatSessionInfo {
	have := map[string]bool{}
	for _, s := range saved {
		have[s.ID] = true
	}
	var live []backend.ChatSessionInfo
	open.Range(func(_, v any) bool {
		if s := v.(backend.ChatSessionInfo); !have[s.ID] {
			have[s.ID] = true
			live = append(live, s)
		}
		return true
	})
	return append(live, saved...)
}

// ChatHistory returns a past conversation's visible messages so the transcript
// can be restored.
func (a *App) ChatHistory(sessionID string) []backend.ChatTurn {
	a.mu.Lock()
	svc := a.svc
	a.mu.Unlock()
	if svc == nil {
		return []backend.ChatTurn{}
	}
	return withChatActions(sessionID, withTurnTimes(sessionID, svc.SessionTurns(sessionID)))
}

// SearchMCPServers finds installable MCP servers in the official registry, so
// the panel can offer the same catalogue the agent searches.
func (a *App) SearchMCPServers(query string) map[string]any {
	candidates, err := backend.SearchMCPServers(context.Background(), query, 20)
	if err != nil {
		return map[string]any{"error": err.Error(), "servers": []backend.MCPCandidate{}}
	}
	return map[string]any{"error": "", "servers": candidates}
}

// SearchSkills finds skills available on this machine.
func (a *App) SearchSkills(query string) []backend.SkillCandidate {
	return backend.SearchSkills(query, 50)
}

// InstallMCPServer adds a server from the registry and starts it.
func (a *App) InstallMCPServer(name, command string, args []string, env map[string]string) string {
	a.mu.Lock()
	svc := a.svc
	a.mu.Unlock()
	if svc == nil {
		return "backend not ready"
	}
	if err := svc.InstallMCPServer(context.Background(), name, command, args, env); err != nil {
		return err.Error()
	}
	a.announceNow()
	return "ok"
}

// InstallMCPURLServer adds a server reached by URL (streamable HTTP or SSE,
// with its headers) and connects to it.
func (a *App) InstallMCPURLServer(name, transport, url string, headers map[string]string) string {
	a.mu.Lock()
	svc := a.svc
	a.mu.Unlock()
	if svc == nil {
		return "backend not ready"
	}
	if err := svc.InstallMCPURLServer(context.Background(), name, transport, url, headers); err != nil {
		return err.Error()
	}
	a.emit("mcp:changed", map[string]any{"name": name})
	a.announceNow()
	return "ok"
}

// RemoveMCPServer forgets a server: stopped now, and gone on the next launch too.
func (a *App) RemoveMCPServer(name string) string {
	a.mu.Lock()
	svc := a.svc
	a.mu.Unlock()
	// Stopped and its tools withdrawn now, not on the next launch.
	if err := svc.UninstallMCPServer(name); err != nil {
		return err.Error()
	}
	a.emit("mcp:changed", map[string]any{"name": name})
	a.announceNow()
	return "ok"
}

// InstallSkill copies a skill found on this machine into SuperLeo.
func (a *App) InstallSkill(name, sourcePath string) string {
	a.mu.Lock()
	svc := a.svc
	a.mu.Unlock()
	if svc == nil {
		return "backend not ready"
	}
	if err := svc.InstallSkillFromPath(context.Background(), name, sourcePath); err != nil {
		return err.Error()
	}
	a.announceNow()
	return "ok"
}

// RemoveSkill uninstalls a skill.
func (a *App) RemoveSkill(name string) string {
	a.mu.Lock()
	svc := a.svc
	a.mu.Unlock()
	if err := backend.RemoveSkill(name); err != nil {
		return err.Error()
	}
	if svc != nil {
		svc.ReloadSkills(context.Background())
	}
	a.announceNow()
	return "ok"
}

// DeleteChatSession removes a past conversation permanently.
func (a *App) DeleteChatSession(sessionID string) string {
	a.mu.Lock()
	svc := a.svc
	a.mu.Unlock()
	if svc == nil {
		return "backend not ready"
	}
	if err := svc.DeleteSession(sessionID); err != nil {
		return err.Error()
	}
	return "ok"
}

// ReadWorkspaceFileDataURL returns a workspace file as a data: URL so the
// webview can render PDFs and images natively instead of showing raw bytes.
func (a *App) ReadWorkspaceFileDataURL(path string) string {
	a.mu.Lock()
	svc := a.svc
	a.mu.Unlock()
	if svc == nil {
		return ""
	}
	out, err := svc.ReadWorkspaceFileDataURL(path)
	if err != nil {
		return ""
	}
	return out
}

// OpenWorkspaceFileExternal opens a deliverable with the OS default app — the
// way out for formats the webview cannot preview (Word, Excel, archives).
func (a *App) OpenWorkspaceFileExternal(path string) string {
	a.mu.Lock()
	svc := a.svc
	a.mu.Unlock()
	if svc == nil {
		return "backend not ready"
	}
	if err := svc.OpenWorkspaceFileExternal(path); err != nil {
		return err.Error()
	}
	return "ok"
}

// EmitAvatarTest pushes a test emotion + speech event for the avatar test button.
// Life returns the life-assistant data (schedules/records/persons/reminders) for the Life panel.
func (a *App) Life() backend.LifeData {
	a.mu.Lock()
	svc := a.svc
	a.mu.Unlock()
	if svc == nil {
		return backend.LifeData{Persons: map[string]map[string]any{}}
	}
	return svc.Life()
}

// ForgetLife deletes one calendar entry ("schedule"), note ("record") or
// person ("person", by name) that SuperLeo got wrong.
func (a *App) ForgetLife(kind, key string) error {
	a.mu.Lock()
	svc := a.svc
	a.mu.Unlock()
	if svc == nil {
		return errors.New("not running yet")
	}
	if err := svc.Forget(kind, key); err != nil {
		return err
	}
	// What lists "coming up" refreshes on this.
	a.emit("schedule:changed", map[string]any{"forgot": kind, "key": key})
	return nil
}

// AddSchedule and AddRecord are the life-assistant writes a rendered `ui` block
// may trigger, and the only two. They exist as App methods so a button the
// model drew has somewhere bounded to land: the action names are registered in
// the frontend, the arguments are typed here, and the same Service methods the
// agent's own tools call do the work. A model that wants a third kind of write
// has to be given one deliberately, which is the point.
func (a *App) AddSchedule(title, startAt, location string, participants []string) map[string]any {
	a.mu.Lock()
	svc := a.svc
	a.mu.Unlock()
	if svc == nil {
		return map[string]any{"ok": false, "error": "agent is not ready"}
	}
	rec, err := svc.AddSchedule(title, startAt, location, participants)
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	return map[string]any{"ok": true, "data": rec}
}

// TestWebhook posts a sample notification so the settings screen can prove the
// URL works before a reminder depends on it. A webhook is configured once and
// then relied on at 08:00 while nobody is watching, which is the worst moment
// to discover a typo — so unlike the real sends, this one reports the failure.
func (a *App) TestWebhook() string {
	a.mu.Lock()
	svc := a.svc
	a.mu.Unlock()
	if svc == nil {
		return "agent is not ready"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := svc.Notifier().SendTest(ctx); err != nil {
		return err.Error()
	}
	return "ok"
}

// Notify raises a notice from the frontend through the same fan-out the backend
// uses, so a message the UI has to show follows one path rather than a second
// one that drifts from it.
//
// Push is not exposed: a toast the frontend raised is about something the user
// just did in front of the screen, and a person who is standing there does not
// need their phone to buzz about it. Anything worth sending onward is raised by
// the code that knows why.
func (a *App) Notify(level, title, message string) string {
	a.mu.Lock()
	svc := a.svc
	a.mu.Unlock()
	if svc == nil {
		// No agent yet is exactly when the frontend most needs to report
		// something, so the notice still goes to the window.
		a.emit(backend.NoticeEvent, map[string]any{
			"level": level, "title": title, "message": message,
		})
		return "ok"
	}
	svc.Notices().Raise(context.Background(), backend.Notice{
		Level: backend.Level(level), Title: title, Message: message,
	})
	return "ok"
}

// AddRecord writes one diary / work / note / habit entry.
func (a *App) AddRecord(kind, title, body string, tags []string, project string) map[string]any {
	a.mu.Lock()
	svc := a.svc
	a.mu.Unlock()
	if svc == nil {
		return map[string]any{"ok": false, "error": "agent is not ready"}
	}
	rec, err := svc.AddRecord(kind, title, body, tags, project)
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	return map[string]any{"ok": true, "data": rec}
}

// MemoryRecall queries long-term memory and returns the formatted recall context.
func (a *App) MemoryRecall(query string) string {
	a.mu.Lock()
	svc := a.svc
	a.mu.Unlock()
	if svc == nil {
		return ""
	}
	out, _ := svc.MemoryRecall(context.Background(), query)
	return out
}

// MemorySkills returns the installed skill IDs.
func (a *App) MemorySkills() []string {
	a.mu.Lock()
	svc := a.svc
	a.mu.Unlock()
	if svc == nil {
		return nil
	}
	return svc.InstalledSkills()
}

// Skills returns the installed skills with descriptions for the Skills panel.
func (a *App) Skills() []backend.SkillInfo {
	a.mu.Lock()
	svc := a.svc
	a.mu.Unlock()
	if svc == nil {
		return nil
	}
	return svc.SkillsDetailed()
}

// MCP returns the configured MCP servers (built-in + user) for the MCP panel.
func (a *App) MCP() []mcp.ServerStatus {
	a.mu.Lock()
	svc := a.svc
	a.mu.Unlock()
	if svc == nil {
		return nil
	}
	return svc.MCPServers()
}

// ImportCSV imports a CSV file into RAG+KG; returns a JSON report (or an error string).
func (a *App) ImportCSV(path, hint string) string {
	a.mu.Lock()
	svc := a.svc
	a.mu.Unlock()
	if svc == nil {
		return "backend not ready"
	}
	out, err := svc.ImportCSV(context.Background(), path, hint)
	if err != nil {
		return "error: " + err.Error()
	}
	return out
}

// EmitAvatarTest pushes a test emotion + speech event for the avatar test button.
//
// It also walks the character to something that is actually on the screen in
// front of you, which is the half of the protocol a static test event could
// never show: a face and a line of text prove the stream is connected, and
// only a walk proves the window is reporting where its landmarks are.
func (a *App) EmitAvatarTest(emotion string) {
	a.mu.Lock()
	driver := a.avatar
	a.mu.Unlock()
	if emotion == "" {
		emotion = "happy"
	}
	driver.Emit(backend.AvatarEvent{Type: backend.AvatarTypeEmotion, Emotion: emotion})
	driver.Emit(backend.AvatarEvent{Type: backend.AvatarTypeSpeech, Text: "这是一条头像测试事件 (" + emotion + ")"})
	driver.Emit(backend.AvatarEvent{Type: backend.AvatarTypeState, State: backend.AvatarStateSpeaking})
	if stage, fresh := a.pet.current(); fresh {
		if spot, ok := stage.showpiece(); ok {
			driver.Emit(backend.AvatarEvent{
				Type: backend.AvatarTypePoint,
				Spot: spot.Name,
				Text: "这里是 " + spot.Name,
			})
		}
	}
}

// SetWindowTheme paints the window chrome to match the UI's theme.
//
// The web view is only the middle of the window. On macOS the title bar and the
// area behind the page are drawn by the OS from the background colour Wails was
// given at startup, so switching the page to light left a dark bar above it and
// a dark edge around it — the app looked half-converted, which is worse than
// picking one and staying there.
//
// Called from the frontend whenever the theme changes, because the frontend is
// where that decision is made and remembered. A no-op without a window: serve
// mode has no chrome to paint, and the browser tab draws its own.
func (a *App) SetWindowTheme(dark bool) {
	if a.ctx == nil {
		return
	}
	// The same two colours the stylesheet uses for --bg-0, so the seam between
	// the page and the chrome around it does not show. They moved when the
	// palette did — warm dark #141210 and warm paper #f4f1ea — and this is the
	// one place outside the stylesheet that has to be told.
	if dark {
		runtime.WindowSetBackgroundColour(a.ctx, 20, 18, 16, 255)
		runtime.WindowSetDarkTheme(a.ctx)
		return
	}
	runtime.WindowSetBackgroundColour(a.ctx, 244, 241, 234, 255)
	runtime.WindowSetLightTheme(a.ctx)
}
