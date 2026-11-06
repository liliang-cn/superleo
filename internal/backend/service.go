package backend

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	mimepkg "mime"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/liliang-cn/agent-go/v3/pkg/agent"
	"github.com/liliang-cn/agent-go/v3/pkg/config"
	"github.com/liliang-cn/agent-go/v3/pkg/cortexbridge"
	"github.com/liliang-cn/agent-go/v3/pkg/cortexbridge/connectorbridge"
	"github.com/liliang-cn/agent-go/v3/pkg/domain"
	"github.com/liliang-cn/agent-go/v3/pkg/pool"
	"github.com/liliang-cn/agent-go/v3/pkg/providers"
	"github.com/liliang-cn/agent-go/v3/pkg/sandbox"
	"github.com/liliang-cn/agent-go/v3/pkg/store"
	"github.com/liliang-cn/cortexdb/v2/pkg/connector"
	"github.com/liliang-cn/cortexdb/v2/pkg/cortexdb"
)

// UploadsSubdir is where attachments land inside the agent workspace, so
// read_document can resolve them through the sandbox as "uploads/<name>".
const UploadsSubdir = "uploads"

// Service wraps a maximally-configured AgentGo agent.Service plus the desktop
// app's supporting pieces (sandbox, cortexdb handle, life-assistant store,
// settings).
type Service struct {
	svc *agent.Service
	// browser is the built-in browser (browser.go); nil when it is off.
	browser *browserRuntime
	// browserEager is the browser's tools being among the eager ones
	// (refreshEagerTools), once it is mounted.
	browserEager atomic.Bool
	desktop      *desktopRuntime
	sb           sandbox.Sandbox
	cortex       *cortexdb.DB
	// sharedMem is the read-through store handed to agent-go, which does not
	// close an injected store; nil unless the namespace is a private one.
	sharedMem *readThroughMemoryStore
	settings  *Settings
	store     *lifeStore
	// files remembers which conversation produced which workspace file, so a
	// chat shows its own deliverables rather than everything ever written.
	files *sessionFiles
	// brain is the same generator the agent runs on, kept so tools that need a
	// single structured completion (reading an install page, say) can ask the
	// model directly instead of going through a whole agent turn.
	brain domain.Generator
	// promptScheduler is injected by whoever owns the process lifetime (the app,
	// or the background daemon), because a cron loop outlives a chat turn. Tools
	// that create schedules — set_reminder — go through it. Guarded by schedMu
	// because injection races tool calls already in flight.
	schedMu sync.Mutex
	// removedMCP holds the servers uninstalled while this service runs:
	// agent-go lists a loaded server until it is built again.
	removedMCP      sync.Map
	promptScheduler *agent.PromptScheduler
	// claims stops two schedule-writing tools from both answering one request.
	// See schedule_once.go.
	claims *scheduleClaims
	// ownedScheduler is a manage-only scheduler this service opened for itself
	// because nobody injected one (the single-shot CLI, most obviously). It
	// writes schedules to the shared store without firing any, and this service
	// closes it. Never the same object as promptScheduler.
	ownedScheduler *agent.PromptScheduler
	// dataDir is the agent's own data directory (<home>/data), where agent-go
	// keeps its sqlite database.
	dataDir string
	// gate authorizes tool calls. It is installed on the agent at build time
	// and stays reachable afterwards so whoever owns a UI can hand it an
	// approver — the service is built before the window knows about it, and a
	// gate with no approver denies rather than allows.
	gate *ToolGate
	// plans is the store the agent's scratchpad persists to, kept so the run
	// wall can read a task's plan back without going through the agent.
	plans *GraphPlanStore
	// dashboards are replies someone kept, with the question that produced each
	// one. See dashboards.go.
	dashboards *dashboardStore

	MemoryMode string

	// usage counts model-turn tokens across every entry point, for the
	// dashboard. Fed by an observer registered at build time; see dashboard.go.
	usage *usageTracker

	// traces writes a JSONL trace per long-run task. Another observer
	// registered at build time, beside the usage one — a trace is about the
	// run, not about whichever window happens to be open. See trace.go.
	traces *TraceStore

	// notifier posts the outbound webhook, if one is configured. Reachable
	// from the process that owns the scheduler as well as from notify_user,
	// because a reminder and a mid-task message are the same kind of thing to
	// whoever is not looking at the screen. See webhook.go.
	notifier *Notifier

	// notices fans one message out to every surface that is attached — the UI
	// and the webhook. Raised by notify_user, by a finished scheduled run, and
	// by anything else that has something to say. See notice.go.
	notices *Notices

	// inbox keeps every notice, so there is somewhere to look afterwards.
	inbox *Inbox

	// SharedMemoryMCP names the MCP servers that route to the same store the
	// memory backend already owns; their memory tools are kept out of runs.
	SharedMemoryMCP []string

	// runFilter names more tools a chat run must not have, decided per run by
	// whoever owns the process (a queen in max delegation withholds her work
	// tools while a worker is live). See SetRunToolFilter.
	runFilterMu sync.Mutex
	runFilter   RunToolFilter
}

// RunToolFilter names tools to withhold from one chat run, given the run's
// context, its conversation and the registry's tool names.
type RunToolFilter func(ctx context.Context, sessionID string, registry []string) []string

// SetRunToolFilter installs (or with nil removes) the per-run filter.
func (s *Service) SetRunToolFilter(f RunToolFilter) {
	if s == nil {
		return
	}
	s.runFilterMu.Lock()
	s.runFilter = f
	s.runFilterMu.Unlock()
}

// MCPServersWithTools names the MCP servers whose tools this agent can call
// now: configured and with tools in the registry. What a worker tells the
// queen it has.
func (s *Service) MCPServersWithTools() []string {
	if s == nil || s.svc == nil {
		return []string{}
	}
	names := s.svc.GetToolRegistry().Names()
	out := []string{}
	for _, st := range s.MCPServers() {
		prefix := "mcp_" + st.Name + "_"
		for _, n := range names {
			if strings.HasPrefix(n, prefix) {
				out = append(out, st.Name)
				break
			}
		}
	}
	slices.Sort(out)
	return out
}

// NewService builds the full SuperLeo agent service from the provided settings.
func NewService(s *Settings) (*Service, error) {
	if strings.TrimSpace(s.LLMKey) == "" || strings.TrimSpace(s.LLMBaseURL) == "" {
		return nil, fmt.Errorf("no account yet: add one under Settings → Accounts")
	}
	if strings.TrimSpace(s.LLMModel) == "" {
		return nil, fmt.Errorf("no model selected: pick one under Settings → Accounts")
	}
	// Again here, not only on load: a model picked in the settings page
	// reaches this without the file being read again.
	s.registerWindow()

	// The builtin websearch MCP reads SEARXNG_BASE_URL at construction; the
	// setting is the explicit way to point this process at an instance.
	if u := strings.TrimSpace(s.SearXNGURL); u != "" {
		_ = os.Setenv("SEARXNG_BASE_URL", u)
	}

	// --- Brain (LLM): OpenAI-compatible pool. ---
	brain, err := pool.NewPool(pool.PoolConfig{
		Enabled:  true,
		Strategy: pool.StrategyRoundRobin,
		Providers: []pool.Provider{{
			Name: "brain", BaseURL: s.LLMBaseURL, Key: s.LLMKey,
			ModelName: s.LLMModel, MaxConcurrency: 5, Capability: 8,
			ReasoningEffort: s.LLMReasoningEffort,
			NativeWebSearch: brainNativeSearch(s),
		}},
	})
	if err != nil {
		return nil, fmt.Errorf("build brain: %w", err)
	}

	// --- Embedder (optional): graph memory when present, file memory otherwise. ---
	var embedder domain.Embedder
	if s.UseEmbeddings() {
		ep, eerr := providers.NewOpenAIEmbedderProvider(&domain.OpenAIProviderConfig{
			BaseURL: s.EmbedBaseURL, APIKey: s.EmbedKey, EmbeddingModel: s.EmbedModel,
		})
		if eerr != nil {
			slog.Warn("embedder init failed, falling back to file memory", "error", eerr)
		} else {
			embedder = ep
		}
	}

	// --- Config / home layout. ---
	cfg := &config.Config{Home: DataDir()}
	// Discovery, now that being deferred no longer means being invisible.
	//
	// This used to be off — the whole catalogue went straight into the schema —
	// because tool search cost more benchmark tasks than a large schema ever
	// did. That was true of a search the model had to guess at: nothing told it
	// what was behind the search tool, so it either shopped blindly or reported
	// a capability as missing. agent-go now carries a one-line index of
	// everything held back, so looking a tool up is reading a list rather than
	// guessing, and the old objection no longer applies.
	//
	// What it buys: measured on this install, the schema was 124KB of which
	// 94KB was the knowledge and graph surface — about 23k tokens on every
	// turn, for tools an ordinary conversation never calls.
	cfg.Tooling.DeferTools = deferredToolPatterns
	// Every tool goes out as its signature: 91 schemas were about 14k tokens
	// on every model call, most of it description written for a reader.
	cfg.Tooling.CompactSchemas = true
	// noted's everyday tools stay in the schema even when the catalogue is
	// big enough that the rest go behind search: "what's on tomorrow" cost a
	// worker two model calls just to find list_events — ten seconds of a
	// twenty-five second answer.
	cfg.Tooling.EagerTools = eagerTools(s, false)
	// Memory backend: local by default, or the shared CortexDB brain when the
	// user picked it and an endpoint is known. The shared backend is a remote
	// store, so the local embedder plays no part in it — the server owns the
	// embedding model.
	useShared := s.UseSharedMemory()
	switch {
	case useShared:
		applySharedMemoryConfig(cfg, s)
	case embedder != nil:
		cfg.Memory.StoreType = config.MemoryStoreTypeGraphFlow
	}
	cfg.ApplyHomeLayout()
	if err := os.MkdirAll(cfg.DataDir(), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir data dir: %w", err)
	}
	// Must run before the agent is built: that is when agent-go reads the
	// embedding pool out of agentgo.db, and settings.json is not one of its
	// sources. Never fatal — embeddings are an enhancement, and refusing to
	// start over one would be worse than starting without it.
	if err := syncEmbeddingProvider(cfg, s); err != nil {
		slog.Warn("embedding settings not applied to the pool", "error", err)
	}
	if err := os.MkdirAll(s.WorkspaceDir, 0o755); err != nil {
		slog.Warn("mkdir workspace", "error", err)
	}

	// --- Sandbox (local workspace). ---
	sb, err := sandbox.NewLocal(sandbox.WithWorkspace(s.WorkspaceDir))
	if err != nil {
		return nil, fmt.Errorf("build sandbox: %w", err)
	}

	// Browser control left the framework in agent-go v3 (pkg/browser is gone).
	// SuperLeo keeps the settings fields so existing config files still parse;
	// browsing is now expected to come from an MCP server instead.
	if s.DisableBrowser || strings.TrimSpace(os.Getenv("SUPERLEO_NO_BROWSER")) != "" {
		slog.Info("browser disabled by configuration")
	}

	// --- Tool approval gate. ---
	// Built before the agent because the handler is installed as part of the
	// build: agent-go only consults SetPermissionHandler's handler, and a
	// service that gets one later has already run whatever it ran in between.
	// Default ON — see Settings.DisableToolApproval for why the setting is
	// phrased as a disable.
	gate := NewToolGate(!s.DisableToolApproval, AuditLogPath())
	// Off unless a URL is configured, in which case the gate also judges a
	// call's arguments. See Settings.ToolApprovalDeciderURL.
	gate.SetDecider(s.toolApprovalDecider())

	// --- Plan persistence. ---
	//
	// Deliberately not the brain: a run's steps are what happened once, and a
	// few thousand of them would bury the knowledge they sit next to.
	planStore := NewGraphPlanStore(filepath.Join(cfg.DataDir(), "plans"))

	// --- Build the agent service. ---
	// With noted connected, personal data has a home of its own and the
	// prompt says so (notedSection); with Vita, health does (vitaSection).
	_, notedMissing, _, notedConfigured := findMCPServer(MCPConfigSources(), NotedServer, os.LookupEnv)
	notedConfigured = notedConfigured && len(notedMissing) == 0
	_, vitaMissing, _, vitaConfigured := findMCPServer(MCPConfigSources(), VitaServer, os.LookupEnv)
	vitaConfigured = vitaConfigured && len(vitaMissing) == 0
	b := agent.New("SuperLeo").
		WithPrompt(buildPersona(time.Now(), !s.DisableSelfInstall, s.ExternalAgents.Enabled) + browserSection(s.browserMode()) + desktopSection(s.desktopControl()) + hiveSection(s.Hive) + queenNameSection(s) + homeLayoutSection(s.Hive) + notedSectionFor(notedConfigured, s.Hive.Role) + vitaSection(vitaConfigured) + uiRulesSection()).
		WithConfig(cfg).
		WithLLM(brain).
		WithSandbox(sb).
		WithAutonomy(agent.AutonomyProfile{MaxRounds: s.MaxRounds, Scratchpad: true}).
		WithSkills().
		// A plan that outlives the process. Without it a long task that is
		// interrupted comes back having forgotten what it was doing — which is
		// the moment the plan is the only thing worth keeping. Each task gets
		// its own graph under <data>/plans, which is also what serve_graph_3d
		// renders, so a run can be watched filling in its own plan.
		WithPlanStore(planStore).
		WithOptions(agent.Options{
			Deliverables: true,
			// Both, always. agent-go treats a nil policy next to a non-nil
			// handler as "gate everything", which would put a prompt in front
			// of every memory write; and a nil handler makes authorizeTool a
			// no-op, which is the bug this closes.
			PermissionHandler: gate.Handler(),
			PermissionPolicy:  gate.Policy(),
		})
	if s.Hive.Role == HiveRoleWorker {
		// A worker takes orders from the queen, questions from peers and a
		// person's turns through one service. Past the cap a run is refused,
		// which the queen's scheduler reads as "try another worker".
		b = b.WithMaxConcurrentRuns(s.Hive.Concurrency())
	}
	// agent-go v3 removed PTC entirely — tools are always called directly, so
	// the DisablePTC setting no longer selects anything. It stays in Settings
	// only so existing settings files keep parsing.
	memMode := "file"
	var sharedMem *readThroughMemoryStore
	switch {
	case useShared:
		// The shared brain. Note there is no WithEmbedder here even when one is
		// configured: the remote server does its own embedding, and a local
		// vector would mean nothing to it.
		memOpts := []agent.MemoryOption{
			agent.WithMemoryStoreType(store.CortexRemoteStoreType),
			agent.WithMemoryDSN(cfg.Memory.DSN),
			agent.WithMemoryOptions(cfg.Memory.Options),
		}
		memMode = "shared:" + cfg.Memory.DSN
		// A private namespace (the hive's) still reads "default", where the
		// person's own facts live; see readThroughMemoryStore. Either way the
		// store is SuperLeo's own, so every call to the brain carries the
		// flow it was made in (cortex_trace.go).
		ns := strings.TrimSpace(s.SharedMemoryNamespace)
		var also []string
		if ns != "" && ns != sharedNamespaceDefault {
			also = []string{sharedNamespaceDefault}
		}
		useTLS, _ := strconv.ParseBool(strings.TrimSpace(cfg.Memory.Options["tls"]))
		rt, rerr := newSharedMemoryStore(cfg.Memory.DSN, cfg.Memory.Options["token"], ns, also, useTLS)
		if rerr != nil {
			return nil, rerr
		}
		sharedMem = rt
		memOpts = append(memOpts, agent.WithMemoryStore(rt))
		if len(also) > 0 {
			memMode += " (" + ns + ", reads " + sharedNamespaceDefault + ")"
		}
		b = b.WithMemory(memOpts...)
	case embedder != nil:
		b = b.WithEmbedder(embedder).WithGraphMemory()
		memMode = "graphflow"
	default:
		b = b.WithMemory(agent.WithMemoryStoreType("file"))
	}

	// MCP: any user-defined servers, from both files SuperLeo has ever read.
	// Web search does NOT come from here — it is the built-in web_search tool
	// registered below, so a fresh install with no MCP config can still look
	// something up.
	//
	// Two paths, because for a while there were two files and only one of them
	// was ever written to:
	//
	//   ~/.superleo/mcpServers.json       ← mcpConfigPath(), where
	//     InstallMCPServer writes and RemoveMCPServer deletes, and the path
	//     the add_mcp_server tool names in its own description
	//   ~/.superleo/data/mcpServers.json  ← cfg.DataDir(), the only one
	//     this function used to read
	//
	// So anything installed through the UI or by the agent started, worked for
	// the rest of the session, and was silently gone on the next launch. The
	// documented path is now read; the older one still is too, so an install
	// made back when it was the only one that counted keeps working.
	mcpOpts := []agent.MCPOption{}
	var sharedMCP []string
	// One capability, one route: if the memory backend now owns an endpoint,
	// an MCP server pointed at that same endpoint stays mounted for what else
	// it offers, but its memory tools are withheld (chatRunOptions).
	owned := ""
	if useShared {
		owned = cfg.Memory.DSN
	}
	var mcpPaths []string
	browserMode := s.browserMode()
	browserBuiltin.Store(browserMode != BrowserModeOff)
	// The third is what a worker took from its queen (hive_abilities.go).
	for _, src := range MCPConfigSources() {
		if _, statErr := os.Stat(src); statErr != nil {
			continue
		}
		shared, ferr := serversSharingMemory(src, owned)
		if ferr != nil {
			slog.Warn("mcp config", "path", src, "error", ferr)
		}
		if len(shared) > 0 {
			slog.Info("memory backend shared with MCP servers; their memory tools are withheld", "backend", owned, "servers", shared)
		}
		sharedMCP = append(sharedMCP, shared...)
		// ${NAME} in an entry comes from this process's environment; agent-go
		// reads an expanded copy (mcp_env.go).
		path, rerr := resolveMCPConfig(src, os.LookupEnv)
		if rerr != nil {
			slog.Warn("mcp config", "path", src, "error", rerr)
			path = src
		}
		mcpPaths = append(mcpPaths, path)
	}

	if len(mcpPaths) > 0 {
		mcpOpts = append(mcpOpts, agent.WithMCPConfigPaths(mcpPaths...))
	}
	// Build must not wait on the servers: the desktop's page loads only after
	// Startup returns, and one slow server (npx resolving mcp-remote on every
	// launch took 12-27s) left the window black for that long. Each server's
	// tools appear as it connects.
	mcpOpts = append(mcpOpts, agent.WithMCPBackgroundStart())
	b = b.WithMCP(mcpOpts...)
	// Each model call's request, the messages and the tools, reaches the
	// observers, so the trace store (trace.go) records what the model was
	// actually sent. It stays in this machine's own data directory.
	b = b.WithObserverPayloads()
	// SuperLeo's logger, at agent-go's own level (logging.go), and every run
	// of every service in the process traced into the flow files (flow.go).
	if l := AgentGoLogger(); l != nil {
		b = b.WithLogger(l)
	}
	b = b.WithObserver(FlowTraceWriter())

	svc, err := b.Build()
	if err != nil {
		_ = sb.Close()
		return nil, fmt.Errorf("build SuperLeo: %w", err)
	}

	out := &Service{
		sharedMem: sharedMem,
		svc:       svc, sb: sb, settings: s, MemoryMode: memMode, dataDir: cfg.DataDir(),
		claims: newScheduleClaims(),
		brain:  brain, SharedMemoryMCP: sharedMCP,
		gate: gate, plans: planStore,
	}
	// Token accounting for the dashboard: one observer sees every model turn,
	// whichever entry point started the run.
	out.usage = newUsageTracker(filepath.Join(cfg.DataDir(), "usage.json"))
	svc.RegisterObserver(&usageObserver{u: out.usage})
	// One writer per task, opened by StreamLong and closed when the task
	// stops. Registered here rather than in the app because every run reaches
	// this observer whichever entry point started it.
	out.traces = NewTraceStore(TracesDir())
	svc.RegisterObserver(out.traces)
	if browserMode != BrowserModeOff {
		out.startBrowser(browserMode)
	}
	if s.desktopControl() {
		out.startDesktop(s.desktopBlocked())
	}
	out.notifier = NewNotifier(s)
	out.notices = NewNotices(out.notifier)
	// The notification centre: the one subscriber that keeps what it is given,
	// so a run that finished at three in the morning is still there at nine.
	out.inbox = NewInbox(cfg.DataDir())
	out.notices.Subscribe("inbox", out.inbox.Sink())

	// --- Built-in framework tools. ---
	agent.RegisterDateTimeTool(svc)
	agent.RegisterFetchURLTool(svc)
	// Looking things up is not an optional extra. Without it the agent has no
	// answer to any question about now — the weather, a price, what happened
	// today — and correctly reports itself blocked, which is a worse outcome
	// than a grounded guess. On a fresh install with no MCP servers configured
	// this was the only route to the open web, and it was not wired.
	searchBase, searchKey, searchModel := s.WebSearch()
	searchNative, err := domain.ParseNativeWebSearchFormat(s.SearchNative)
	if err != nil {
		slog.Warn("search_native; sending both search fields", "error", err)
	}
	// The brain's thinking effort goes along only when the search runs on the
	// brain's own model; another model may not take the same values.
	searchEffort := ""
	if strings.TrimSpace(s.SearchModel) == "" {
		searchEffort = s.LLMReasoningEffort
	}
	// A brain that searches itself has the provider's web_search beside its
	// tools; ours would take the name and the provider's would be dropped.
	if !s.BrainSearchesItself() {
		agent.RegisterWebSearchTool(svc, agent.WebSearchConfig{
			BaseURL: searchBase, APIKey: searchKey, Model: searchModel,
			Native: searchNative, ReasoningEffort: searchEffort,
		})
	}

	// --- Life-assistant store + tools (ported from examples/superleo). ---
	out.dashboards = newDashboardStore(filepath.Join(cfg.DataDir(), "dashboards.json"))
	out.files = newSessionFiles(filepath.Join(cfg.DataDir(), "session-files.json"))
	out.store = newLifeStore(filepath.Join(cfg.DataDir(), "superleo-store.json"))
	out.store.load()
	out.registerLifeTools()
	out.registerNotifyTool()
	// Operating the app from inside the conversation: dashboards, and the
	// settings it is allowed to change about itself. Not gated on
	// DisableSelfInstall — that switch is about acquiring new capabilities from
	// the outside, and these are the app's own controls.
	out.registerSelfTools()
	// Handing work to another agent CLI on this machine — Claude Code, Codex,
	// Gemini, cursor-agent. Off unless the operator turned it on: a delegated
	// run spends someone else's subscription and writes files with that CLI's
	// own approval prompt bypassed, so it is not a capability an install
	// should acquire by upgrading. The roots come from settings and always
	// include the workspace; cli_agent_run is Destructive, so every call
	// meets this app's approval gate on the way out unless unattended is set.
	if s.ExternalAgents.Enabled {
		if err := agent.RegisterCLIAgentTools(svc, agent.CLIAgentConfig{
			Binaries:       s.ExternalAgents.Binaries,
			AllowedRoots:   s.ExternalAgentRoots(),
			DefaultTimeout: s.ExternalAgents.Timeout(),
		}); err != nil {
			// Not fatal. A machine with no agent CLI installed is a normal
			// machine, and the rest of the app has nothing to do with this.
			slog.Warn("external agents not registered", "error", err)
		}
	}

	// Self-extension: chat-driven install of skills and MCP servers, plus
	// "here's a URL, work out how to install it". Withheld for one-shot runs —
	// see Settings.DisableSelfInstall.
	if !s.DisableSelfInstall {
		out.registerInstallTools()
		out.registerURLInstallTools()
	}

	// --- CortexDB data-import + graphrag query + connector tools (best-effort). ---
	if db, derr := cortexdb.Open(cortexdb.DefaultConfig(filepath.Join(cfg.DataDir(), "cortex.db"))); derr != nil {
		slog.Warn("cortexdb disabled", "error", derr)
	} else {
		out.cortex = db
		if _, e := cortexbridge.RegisterImportFlow(svc, cortexbridge.NewImporter(db, brain)); e != nil {
			slog.Warn("import flow tools skipped", "error", e)
		}
		// Memory is agent-go's on both backends (WithMemory above), so
		// Register leaves CortexDB's own memory_* tools out by itself — one
		// memory path, the one that reconciles and writes to the configured
		// store. user_id fills the remaining CortexDB tools that take one:
		// this is a single-user app with no per-run memory user.
		// The knowledge and graph tools work on this file. When an MCP server
		// already reaches the shared brain the memory lives in, they would be
		// a second knowledge_save, into a private copy nobody else reads: in a
		// hive, three workers each "saving to the shared brain" into their own
		// pod. Then the shared one is the only one.
		if len(sharedMCP) > 0 {
			slog.Info("knowledge and graph tools come from the shared brain, not the local file", "servers", sharedMCP)
		} else if _, e := cortexbridge.Register(svc, db,
			cortexbridge.WithArgDefaults(map[string]interface{}{"user_id": singleUserID}),
		); e != nil {
			slog.Warn("graphrag tools skipped", "error", e)
		}
		if _, e := connectorbridge.Register(svc, db, connector.ToolboxOptions{}); e != nil {
			slog.Warn("connector tools skipped", "error", e)
		}
	}

	return out, nil
}

// ToolGate is the approval gate this service's agent runs behind. Whoever has
// a user in front of them installs an approver on it; nobody else has to, and
// the gate denies gated calls when nobody does.
func (s *Service) ToolGate() *ToolGate {
	if s == nil {
		return nil
	}
	return s.gate
}

// Close releases the agent service, browser, sandbox, and cortexdb handle.
func (s *Service) Close() error {
	if s == nil {
		return nil
	}
	s.stopBrowser()
	s.stopDesktop()
	if s.store != nil {
		s.store.save()
	}
	// Only the scheduler this service opened for itself; an injected one belongs
	// to whoever injected it.
	s.schedMu.Lock()
	owned := s.ownedScheduler
	s.ownedScheduler = nil
	s.schedMu.Unlock()
	if owned != nil {
		_ = owned.Stop()
	}
	if s.svc != nil {
		_ = s.svc.Close()
		// agent-go's Close leaves the MCP servers it started running. A
		// settings change rebuilds this service, and every rebuild left one
		// more set of server processes behind (two noted mcp-remotes, two
		// browser servers) talking to nobody.
		if s.svc.MCP != nil {
			_ = s.svc.MCP.Close()
		}
	}
	if s.traces != nil {
		_ = s.traces.Close()
	}
	if s.sb != nil {
		_ = s.sb.Close()
	}
	if s.cortex != nil {
		_ = s.cortex.Close()
	}
	if s.sharedMem != nil {
		_ = s.sharedMem.Close()
	}
	return nil
}

// chatRunOptions are the RunOptions one chat turn is made with.
//
// Its own function because Preview takes the same list: a preview assembled
// from options the send path does not use is a picture of a run nobody is
// about to make. Anything added here reaches both.
func (s *Service) chatRunOptions(ctx context.Context, sessionID string, imagePaths []string) []agent.RunOption {
	maxRounds, maxTokens := 0, 0
	if s.settings != nil {
		maxRounds, maxTokens = s.settings.MaxRounds, s.settings.TurnMaxTokens
	}
	opts := []agent.RunOption{
		agent.WithSessionID(sessionID),
		agent.WithMaxTurns(maxRounds),
	}
	if maxTokens > 0 {
		opts = append(opts, agent.WithMaxBudgetTokens(maxTokens))
	}
	// An order from the hive: the order's UUID is the agent-go task id, so
	// the plan, the checkpoints and the journal on this worker are named by
	// the same id the queen's board shows, and a worker that dies mid-order
	// leaves something a replacement can resume. It runs under the hive
	// tenant so all orders stop together, and without the automatic memory
	// write — the queen already remembered giving the order, and N workers
	// given it would otherwise write N extractions into the memory they share.
	// One deny list: WithToolDenylist replaces, it does not add.
	var deny []string
	if len(s.SharedMemoryMCP) > 0 && s.svc != nil {
		deny = append(deny, memoryRouteDenylist(s.SharedMemoryMCP, s.svc.GetToolRegistry().Names())...)
	}
	s.runFilterMu.Lock()
	filter := s.runFilter
	s.runFilterMu.Unlock()
	if filter != nil && s.svc != nil {
		for _, n := range filter(ctx, sessionID, s.svc.GetToolRegistry().Names()) {
			if !slices.Contains(deny, n) {
				deny = append(deny, n)
			}
		}
	}
	if len(deny) > 0 {
		opts = append(opts, agent.WithToolDenylist(deny))
	}
	if id, ok := TaskIDFromSession(sessionID); ok {
		opts = append(opts,
			agent.WithTaskID(id),
			agent.WithTenant(HiveTenant),
			agent.WithoutMemoryAutoStore(),
		)
	}
	// Route dropped images straight to the vision model as multimodal input
	// (workspace-relative paths -> absolute, so the provider can read them).
	if len(imagePaths) > 0 {
		root := ""
		if s.sb != nil {
			root = s.sb.Workspace()
		}
		abs := make([]string, 0, len(imagePaths))
		for _, p := range imagePaths {
			if filepath.IsAbs(p) || root == "" {
				abs = append(abs, p)
			} else {
				abs = append(abs, filepath.Join(root, p))
			}
		}
		opts = append(opts, agent.WithInputImages(abs...))
	}
	return opts
}

// TurnResult is what one turn produced: the answer, and what it cost.
//
// The usage is here because a turn a worker runs for the queen is tokens the
// queen's own accounting cannot see any other way: it reaches the queen in
// chat:done and from there the observers on the queen's run. Usage is nil
// when the provider said nothing, which is an unknown and not a zero.
type TurnResult struct {
	Text  string
	Usage *domain.TokenUsage
	// TraceID is the flow the turn ran in.
	TraceID string
}

// Stream runs one turn, forwarding every agent event to emit, and returns the
// final completion text. StreamTurn is the same turn with its usage.
func (s *Service) Stream(ctx context.Context, sessionID, message string, imagePaths []string, emit func(ev *agent.Event)) (string, error) {
	out, err := s.StreamTurn(ctx, sessionID, message, imagePaths, emit)
	return out.Text, err
}

// StreamTurn runs one turn and returns the answer with the tokens it used.
func (s *Service) StreamTurn(ctx context.Context, sessionID, message string, imagePaths []string, emit func(ev *agent.Event)) (TurnResult, error) {
	var turn TurnResult
	// Whatever the turn started — a shell command, an MCP call — is bound to
	// this context, and the turn ending is the end of it: a caller that never
	// cancels (a channel, the hive, the MCP server) must not leave a tool call
	// running after its turn's answer, or failure, has gone out.
	ctx, endTurn := context.WithCancel(ctx)
	defer endTurn()
	// The browser this turn takes, if it takes it, is free again when it ends.
	emit = serialEmit(emit)
	ctx, releaseBrowser := s.withBrowserTurn(ctx, sessionID, emit)
	defer releaseBrowser()
	// The turn is an entry point's flow, or joins the one it was asked in
	// (flow.go). Whoever streams it hears the flow first, to show and link.
	ctx, flow := beginTurnFlow(ctx, sessionID, message)
	var runErr error
	defer func() {
		switch {
		case errors.Is(runErr, context.Canceled) || ctx.Err() != nil:
			flow.End("cancelled", nil)
		default:
			flow.End("", runErr)
		}
	}()
	if emit != nil && flow.Traceparent() != "" {
		emit(&agent.Event{Type: FlowEventType, Content: flow.Traceparent()})
	}
	turn.TraceID = flow.TraceID()
	opts := s.chatRunOptions(ctx, sessionID, imagePaths)
	// Whatever the turn writes into the workspace belongs to this conversation.
	root := ""
	if s.sb != nil {
		root = s.sb.Workspace()
	}
	before := snapshotWorkspace(root)
	defer func() {
		if s.files != nil {
			// An attachment imported during this turn also shows up as new, and it
			// is the one thing here the agent did not produce.
			changed := changedFiles(before, snapshotWorkspace(root))
			kept := changed[:0]
			for _, p := range changed {
				if !s.files.isImported(p) {
					kept = append(kept, p)
				}
			}
			s.files.record(sessionID, kept)
		}
	}()

	// The conversation this turn belongs to, carried down to the tools. A tool
	// asked to "save this" has to be able to reach the reply that is already on
	// screen, and agent-go keeps the session on the context under an unexported
	// key. This is the same fact under a key of ours.
	ch, err := s.svc.RunStreamWithOptions(withSessionID(ctx, sessionID), message, opts...)
	if err != nil {
		runErr = err
		return turn, err
	}
	var final string
	var lastErr string
	sawTerminal := false
	for ev := range ch {
		if emit != nil {
			emit(ev)
		}
		switch ev.Type {
		// A blocked run is an outcome, not an error: its explanation arrives as
		// EventTypeBlocked and is the answer the user should see.
		case agent.EventTypeComplete, agent.EventTypeBlocked:
			final = ev.Content
			sawTerminal = true
			turn.Usage = ev.Usage
		case agent.EventTypeError:
			lastErr = ev.Content
		}
	}
	// A run that ended on an error event and never reached a terminal state
	// used to come back as ("", nil) — the caller saw "no answer" with the
	// reason silently dropped (a gateway 502 looked identical to a model that
	// said nothing). The error event is the explanation; return it as one.
	if !sawTerminal && lastErr != "" {
		runErr = fmt.Errorf("%s", lastErr)
		return turn, runErr
	}
	turn.Text = final
	return turn, nil
}

// Deliverables returns the agent's produced artifacts.
func (s *Service) Deliverables(ctx context.Context, sessionID string) ([]agent.Deliverable, error) {
	all, err := s.svc.Deliverables(ctx)
	if err != nil {
		return nil, err
	}
	// An empty session id means "everything" (used by tests and any caller that
	// has no conversation in hand).
	var owned map[string]bool
	if strings.TrimSpace(sessionID) != "" && s.files != nil {
		owned = map[string]bool{}
		for _, p := range s.files.forSession(sessionID) {
			owned[p] = true
		}
	}
	imported := func(string) bool { return false }
	if s.files != nil {
		imported = s.files.isImported
	}
	return keepDeliverables(all, owned, imported), nil
}

// keepDeliverables decides which artifacts belong to a conversation.
//
// What the user handed in is not something the agent produced, so it does not
// belong in the list. Judged per file rather than by directory: the agent writes
// its output beside its input, so excluding all of uploads/ also hid the
// converted copy of an attachment — the one file the user then goes looking for.
//
// owned is nil for the "everything" caller, which has no conversation to compare
// against; there, a path in uploads/ with no import record is still assumed to be
// an attachment, since it predates the record being kept.
func keepDeliverables(
	all []agent.Deliverable,
	owned map[string]bool,
	isImported func(string) bool,
) []agent.Deliverable {
	out := make([]agent.Deliverable, 0, len(all))
	for _, d := range all {
		rel := filepath.ToSlash(d.Path)
		if isImported(rel) {
			continue
		}
		if owned != nil {
			if !owned[rel] {
				continue
			}
		} else if strings.HasPrefix(rel, UploadsSubdir+"/") {
			continue
		}
		out = append(out, d)
	}
	return out
}

// ReadWorkspaceFile reads a workspace-relative file via the sandbox.
func (s *Service) ReadWorkspaceFile(path string) (string, error) {
	data, err := s.svc.Sandbox().ReadFile(context.Background(), path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// ReadWorkspaceFileDataURL returns a workspace file as a data: URL, so the
// webview can render it natively — PDFs in an <embed>, images in an <img> —
// instead of the UI dumping raw bytes as text.
func (s *Service) ReadWorkspaceFileDataURL(path string) (string, error) {
	data, err := s.svc.Sandbox().ReadFile(context.Background(), path)
	if err != nil {
		return "", err
	}
	mime := mimeTypeFor(path)
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

// mimeTypeFor guesses a content type from the extension, falling back to a
// generic binary type the webview will simply offer to download.
func mimeTypeFor(path string) string {
	if t := mimepkg.TypeByExtension(strings.ToLower(filepath.Ext(path))); t != "" {
		return t
	}
	return "application/octet-stream"
}

// OpenWorkspaceFileExternal hands a workspace file to the OS default
// application — the escape hatch for formats the webview cannot render
// (Word, Excel, PowerPoint, archives).
func (s *Service) OpenWorkspaceFileExternal(path string) error {
	root := ""
	if s.sb != nil {
		root = s.sb.Workspace()
	}
	full := path
	if !filepath.IsAbs(full) && root != "" {
		full = filepath.Join(root, path)
	}
	if _, err := os.Stat(full); err != nil {
		return err
	}
	return exec.Command("open", full).Start()
}

// UsePromptScheduler hands the service the scheduler its tools should create
// schedules in — the one whose process owns the cron loop.
func (s *Service) UsePromptScheduler(sch *agent.PromptScheduler) {
	if s == nil {
		return
	}
	s.schedMu.Lock()
	s.promptScheduler = sch
	s.schedMu.Unlock()
}

// reminderScheduler returns something that can write a schedule.
//
// A reminder is data. Firing it is a cron loop's job, and the two are separate
// concerns that happen to share a database: agent-go keeps schedules in the
// same store as the sessions, so any process pointed at this home can create
// one and whichever process owns execution picks it up.
//
// This used to refuse outright when nobody had injected a running scheduler,
// which made "set a reminder" fail in the single-shot CLI — no window, no
// daemon, no cron loop, and therefore, absurdly, no way to write a row. A
// benchmark task was lost to it. Now the fallback opens the same store in
// manage-only mode: the reminder is stored, nothing is fired here, and the app
// or daemon rings it when the time comes.
func (s *Service) reminderScheduler() (*agent.PromptScheduler, error) {
	if s == nil {
		return nil, fmt.Errorf("no service")
	}
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	if s.promptScheduler != nil {
		return s.promptScheduler, nil
	}
	if s.ownedScheduler != nil {
		return s.ownedScheduler, nil
	}
	if s.svc == nil {
		return nil, fmt.Errorf("the scheduler is not running")
	}
	sch, err := s.svc.NewPromptScheduler(agent.WithPromptSessionID("scheduled"))
	if err != nil {
		return nil, err
	}
	// Manage-only: this process must not fire schedules. If it did, a CLI run
	// and the desktop app would both ring the same reminder.
	if err := sch.StartManageOnly(); err != nil {
		return nil, err
	}
	s.ownedScheduler = sch
	return sch, nil
}

// NoteImported records files the host copied into the workspace on the user's
// behalf, so the deliverables list can tell them from what the agent produced
// even when they share a directory.
func (s *Service) NoteImported(paths []string) {
	if s == nil || s.files == nil {
		return
	}
	s.files.noteImported(paths)
}

// Agent exposes the underlying agent service, for capabilities this package
// does not wrap — the prompt scheduler being the first, since its lifetime
// belongs to the host rather than to a chat turn.
func (s *Service) Agent() *agent.Service {
	if s == nil {
		return nil
	}
	return s.svc
}

// InstalledSkills returns the skills discovered/installed for this service.
func (s *Service) InstalledSkills() []string { return s.svc.InstalledSkills() }

// ----------------------------------------------------------------------------
// Persona
// ----------------------------------------------------------------------------

// selfInstallSection is the "go and acquire what you lack" instruction. It is
// only included when those tools are actually registered — a prompt that tells
// the model to call search_mcp_servers when the schema has no such tool is an
// invitation to waste a round discovering that.
const selfInstallSection = `
- Missing a capability? Get it, then resume the task: a tool → search_mcp_servers, then add_mcp_server with its command/args verbatim (ask for any required_env first, never install with empty values); know-how → search_skills, then install_skill with its source_path. Check the tools you have before installing anything.`

// delegateSection tells the model that another agent is a tool, and when it is
// the right one. Only appended when the operator has turned delegation on.
//
// The negative half is the half that matters. A coding agent given a general
// assistant's work does it worse and bills a subscription for the privilege,
// and the failure mode of a tool called "run another agent" is that it becomes
// the answer to everything.
const delegateSection = `
- cli_agent_list / cli_agent_run hand a coding job in a directory to another agent CLI on this machine. Wrong for anything you can do yourself or anything that is only a question: it takes minutes and another subscription. Say who did it; a run that failed to sign in returns text that reads like an answer — report that as a failure.`

func buildPersona(now time.Time, selfInstall, externalAgents bool) string {
	installHint := ""
	if selfInstall {
		installHint = selfInstallSection
	}
	delegateHint := ""
	if externalAgents {
		delegateHint = delegateSection
	}
	return fmt.Sprintf(`You are SuperLeo, a warm everyday assistant for life and work. `+answerLanguageRule+`
- Relative times (tomorrow, next Monday, 8 tonight…): work them out from the "Now:" line of the turn and pass RFC 3339 with its offset; resolve_datetime only for something you cannot work out.
- Tools that do not depend on each other go out together in one reply (the day's events and to-dos: both at once), never one per round — each round is seconds the person waits.
- Record unprompted, before replying: a plan→add_schedule, a person→upsert_person, work→add_record(work), life→add_record(diary), a note→add_record(note), a habit→add_record(habit).
- Reminders take one tool: daily at HH:MM or one moment→set_reminder; any other cadence→schedule_prompt (cron).
- `+investingRule+`
- Current facts→web_search; a page's content→fetch_url. "Save this as a dashboard"→dashboard_save; settings→settings_get, then settings_set.
- Report what you actually did. If a tool for part of the request is missing, say which part — never pass off a near neighbour as what was asked.
- Your answer is what the person reads, word by word — as reply text, or as task_complete's result. Never a note about the turn ("已回复问候", "完成"): to a "hi", say hello.%s%s
- You may end with MOOD: <neutral|happy|sad|thinking|excited|sleepy|confused|love|angry|surprised> alone on the last line. It is optional and never worth a worse answer.`,
		installHint, delegateHint)
}

// hiveSection tells a member of a hive who it is there. Without it every
// worker believed it was "SuperLeo": asked to save something under its own
// name, ten workers wrote to one key and each overwrote the last.
func hiveSection(h HiveSettings) string {
	switch h.Role {
	case HiveRoleWorker:
		return fmt.Sprintf(`

In the hive you are %[1]s, a worker. The queen's orders arrive as turns. Where a task wants your name or a per-member key, path or label, you are %[1]s, not SuperLeo — memory is shared, so another name overwrites someone. To settle what another member found, hive_ask it first and say what it told you. To wait for a message use hive_wait — never sleep, poll hive_inbox or ask whether it was sent.`, h.Self()) + queenChineseName
	case HiveRoleQueen:
		return queenSection(h)
	}
	return ""
}

// ExternalQueenPrompt is what an agent CLI thinking for the queen is told on
// every turn (app_queen_engine.go), appended to its own system prompt. The
// queen's part is the one her own agent is given, so the two cannot drift;
// only what differs about being outside is said here: where the replies go,
// and that the hive's tools carry the MCP server's names.
func ExternalQueenPrompt(s *Settings) string {
	if s == nil {
		s = &Settings{}
	}
	out := `You are the queen of SuperLeo, answering a person in the SuperLeo app: what you write is shown to them as her reply, word by word, and SuperLeo keeps the conversation. ` + answerLanguageRule + "\n" + investingRule
	if s.Hive.Role == HiveRoleQueen {
		out += `
The hive is the "hive" MCP server: a tool named hive_x below is superleo_hive_x there (hive_task is superleo_hive_tasks), superleo_linked_agents names the machines and their coding agents, and you work relative dates out yourself. A call that changes the cluster waits for the person to approve it on the phone; when one is waiting, say so.` + queenSection(s.Hive)
	} else {
		out += queenChineseName
	}
	return out + queenNameSection(s)
}

// answerLanguageRule is how every queen answers, her own agent or not.
const answerLanguageRule = `Answer in Chinese — short and natural; never English, Japanese or Korean.`

// investingRule is how a question about the markets or the person's money is
// answered, by her own agent or an outside engine. Asked whether to buy after
// a US market drop, the queen once said she could not know the person's
// positions — which are in noted — and gave general principles and a line that
// she is not their advisor.
const investingRule = `A question about the markets or the person's money (buy or sell, a drop, a position) is answered from their data, never with general principles: first read their holdings in noted (list_holdings) and today's prices and news — never say you cannot know their positions. Say what they hold in what is asked about, its weight and gain or loss, what moved and why, and what buying more would do to that weight; then your view in plain words, with the reason and what would change it. No line that you are not their advisor: the person knows the decision is theirs.`

// queenChineseName is the hive's word for its queen in Chinese. A CLI left to
// translate "queen" calls itself by the word for a monarch, which SuperLeo
// never calls her; the rule names that word so the model knows which one not
// to write, spelled in halves so no copy or prompt in the source has it
// (TestNoMonarchWordForTheQueen).
const queenChineseName = "\nIn Chinese the hive's queen is 蜂后 — when you name yourself or the queen, write 蜂后, never " + queenNotWord + "."

// queenNotWord is the word the queen is never called in Chinese.
const queenNotWord = "女" + "王"

// queenSection is the queen's part of the prompt.
func queenSection(h HiveSettings) string {
	return fmt.Sprintf(`

In the hive you are %[1]s, the queen. Run it through hive_members, hive_command, hive_map, hive_send and hive_inbox — never inspect your own machine, environment or API. Where reports disagree, say so and quote them; a difference no report explains is unexplained.
%[2]s A big job — research, a report, a check across several machines — is yours to split: see who you have (hive_members, coding_agent_list, remote_agent_list), then give every part out in ONE hive_command call — workers, coding agents, named agents — so they run at the same time; hive_map when there are more parts than workers. Calling coding_agent_run or remote_agent_run one by one makes the rest wait. Each order stands alone: what to find, how far, real sources; research saves to the shared brain with knowledge_save under a prefix for the job. Then write the result yourself — compare, cite, say where they disagree — and %[3]s.
When workers pass work by message, let them run; if what you wait for has not come, end the turn saying so.`, h.Self(), queenWorkRule(h.DelegationMode()), queenSaveRule(h.DelegationMode())) + queenChineseName
}

// queenWorkRule is the part of the queen's prompt that says who does the
// work, by delegation mode (hive_delegate.go). Off is the queen as she was.
func queenWorkRule(mode string) string {
	switch mode {
	case DelegationMax:
		return `You plan, split, route, follow up, merge, handle approvals and talk to the person; workers do the work. While a worker is live your work tools (noted, web search and pages, files, shell, CortexDB research) are withheld and hive_delegate runs them: give it every piece that needs a tool, standing alone, dates already resolved with resolve_datetime, and needs naming the MCP server its tools come from (noted for calendar, to-dos, notes, goals, time, holdings).` + workerKitRule + ` Independent pieces go in one call and run in parallel on different workers. Answer yourself only for conversation, a clarifying question, your own state (hive_members, hive_task) and composing the reply from the reports; say which worker did what in a few words. If a report says the queen did a piece herself, say why in one line. Without hive_delegate among your tools no worker is live: do the work yourself and say the hive had no free worker.`
	case DelegationAuto:
		return `Work that needs a tool (noted, web, files, research) goes to an idle worker with hive_delegate — independent pieces in one call, needs naming the MCP server (noted for calendar and to-dos).` + workerKitRule + ` Do it yourself only when no worker is free or it is one quick read. Conversation and your own state you answer yourself.`
	}
	return `Answer small questions yourself.`
}

// workerKitRule is how the queen uses a server or skill only some workers
// have (hive_delegate's description lists them, with their tools).
const workerKitRule = ` When a server or skill only some workers have fits the request, that is how it is done: write the piece for its tools and put its name in needs, rather than a shell or another way round to the same end.`

func queenSaveRule(mode string) string {
	if mode == DelegationMax {
		return "have a worker save it under the same prefix"
	}
	return "save it under the same prefix"
}

// uiRulesSection appends the transcript's rendering rules to the persona, so
// the model knows which rich blocks the UI can actually draw: the backend's own
// card / timeline / stat always, the frontend's once it has announced them
// (see uirules.go).
func uiRulesSection() string {
	names := uiBlockNames(AllUIRules())
	if len(names) == 0 {
		return ""
	}
	return "\n\nBesides markdown the screen draws these fenced blocks: " + strings.Join(names, ", ") +
		". Before writing one, call ui_block_rules once for its exact format."
}

// uiBlockNames lists the block kinds the rules describe, in order.
//
// The rules themselves run to 9KB — every block's JSON shape, a full example
// dashboard — and rode in every turn's prompt for answers that are nearly
// always plain text. Their names say what can be drawn; ui_block_rules hands
// over the rest when an answer actually draws one.
func uiBlockNames(rules string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range uiBlockName.FindAllStringSubmatch(rules, -1) {
		n := m[1]
		if n == "" {
			n = m[2]
		}
		if n != "" && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

var uiBlockName = regexp.MustCompile("```([a-z][a-z0-9-]*)|`([a-z][a-z0-9-]*)`（")

// ----------------------------------------------------------------------------
// Life-assistant store (ported from examples/superleo)
// ----------------------------------------------------------------------------

type lifeStore struct {
	mu        sync.Mutex
	path      string
	Schedules []map[string]any          `json:"schedules"`
	Records   []map[string]any          `json:"records"`
	Persons   map[string]map[string]any `json:"persons"`
	Reminders []map[string]any          `json:"reminders"`
}

func newLifeStore(path string) *lifeStore {
	return &lifeStore{path: path, Persons: map[string]map[string]any{}}
}

func (db *lifeStore) load() {
	raw, err := os.ReadFile(db.path)
	if err != nil {
		return
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	_ = json.Unmarshal(raw, db)
	if db.Persons == nil {
		db.Persons = map[string]map[string]any{}
	}
}

func (db *lifeStore) save() {
	db.mu.Lock()
	raw, err := json.MarshalIndent(db, "", "  ")
	db.mu.Unlock()
	if err != nil {
		return
	}
	_ = os.WriteFile(db.path, raw, 0o644)
}

func okData(data any) map[string]any { return map[string]any{"ok": true, "data": data} }

// ----------------------------------------------------------------------------
// Life-assistant tools
// ----------------------------------------------------------------------------

func (s *Service) registerLifeTools() {
	svc, db := s.svc, s.store

	str := func(a map[string]any, k string) string {
		if v, ok := a[k].(string); ok {
			return strings.TrimSpace(v)
		}
		return ""
	}
	strSlice := func(a map[string]any, k string) []string {
		out := []string{}
		if raw, ok := a[k].([]any); ok {
			for _, v := range raw {
				if sv, ok := v.(string); ok && strings.TrimSpace(sv) != "" {
					out = append(out, strings.TrimSpace(sv))
				}
			}
		}
		return out
	}
	write := agent.ToolMetadata{InterruptBehavior: agent.InterruptBehaviorBlock}
	read := agent.ToolMetadata{ReadOnly: true, ConcurrencySafe: true, InterruptBehavior: agent.InterruptBehaviorCancel}

	obj := func(props map[string]any, required ...string) map[string]any {
		m := map[string]any{"type": "object", "properties": props}
		if len(required) > 0 {
			m["required"] = required
		}
		return m
	}
	sp := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	arr := func(desc string) map[string]any {
		return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
	}

	svc.AddToolWithMetadata("add_schedule", "Create a calendar entry or appointment. Give the time as an absolute RFC3339 timestamp (resolve it with resolve_datetime first).",
		obj(map[string]any{
			"title": sp("Title of the entry"), "start_at": sp("Start time, RFC3339"),
			"location": sp("Location"), "participants": arr("Names of the participants"),
		}, "title", "start_at"),
		func(ctx context.Context, a map[string]any) (any, error) {
			rec, err := s.AddSchedule(str(a, "title"), str(a, "start_at"),
				str(a, "location"), strSlice(a, "participants"))
			if err != nil {
				return errResult(err.Error()), nil
			}
			return okData(rec), nil
		}, write)

	svc.AddToolWithMetadata("list_schedules", "List every calendar entry.", obj(map[string]any{}),
		func(ctx context.Context, a map[string]any) (any, error) {
			db.mu.Lock()
			defer db.mu.Unlock()
			return okData(db.Schedules), nil
		}, read)

	svc.AddToolWithMetadata("add_record", "Record one entry: diary, work, note or habit.",
		obj(map[string]any{
			"type": sp("Kind: diary|work|note|habit"), "title": sp("Short title"),
			"body": sp("Body text"), "tags": arr("Tags"), "project": sp("Project it belongs to (for work records)"),
		}, "type", "body"),
		func(ctx context.Context, a map[string]any) (any, error) {
			rec, err := s.AddRecord(str(a, "type"), str(a, "title"), str(a, "body"),
				strSlice(a, "tags"), str(a, "project"))
			if err != nil {
				return errResult(err.Error()), nil
			}
			return okData(rec), nil
		}, write)

	svc.AddToolWithMetadata("search_records", "Search records by keyword, optionally filtered by type.",
		obj(map[string]any{"query": sp("Keyword"), "type": sp("Optional: diary|work|note|habit")}, "query"),
		func(ctx context.Context, a map[string]any) (any, error) {
			db.mu.Lock()
			defer db.mu.Unlock()
			q, typ := strings.ToLower(str(a, "query")), str(a, "type")
			hits := []map[string]any{}
			for _, r := range db.Records {
				if typ != "" && r["type"] != typ {
					continue
				}
				blob := strings.ToLower(fmt.Sprintf("%v %v %v %v", r["title"], r["body"], r["tags"], r["project"]))
				if q == "" || strings.Contains(blob, q) {
					hits = append(hits, r)
				}
			}
			return okData(hits), nil
		}, read)

	svc.AddToolWithMetadata("upsert_person", "Create or update a person's profile (relationship, preferences, what they are up to).",
		obj(map[string]any{
			"name": sp("Name"), "relation": sp("Relationship, e.g. colleague/friend/flatmate"), "note": sp("Preferences or recent news"),
		}, "name"),
		func(ctx context.Context, a map[string]any) (any, error) {
			db.mu.Lock()
			name := str(a, "name")
			p := db.Persons[name]
			if p == nil {
				p = map[string]any{"name": name}
			}
			if v := str(a, "relation"); v != "" {
				p["relation"] = v
			}
			if v := str(a, "note"); v != "" {
				p["note"] = v
			}
			db.Persons[name] = p
			db.mu.Unlock()
			db.save()
			return okData(p), nil
		}, write)

	// A reminder is a scheduled prompt. Until the scheduler existed this tool
	// only appended a row to a JSON file while telling the model "SuperLeo will
	// remind you when it is due" — a promise nothing kept.
	svc.AddToolWithMetadata("set_reminder",
		"Set a reminder that really fires when it comes due (even with the app closed, as long as the background service is resident). Give the time as HH:MM (every day) or RFC3339.",
		obj(map[string]any{
			"title":      sp("What to remind about; it may also be something to do first, e.g. \"check whether yesterday's deploy is healthy\""),
			"remind_at":  sp("HH:MM for every day (e.g. 08:00); RFC3339 for one specific day"),
			"recurrence": sp("daily or none; HH:MM defaults to daily"),
		}, "title", "remind_at"),
		func(ctx context.Context, a map[string]any) (any, error) {
			title := strings.TrimSpace(str(a, "title"))
			if title == "" {
				return errResult("title is required"), nil
			}
			plan, err := ReminderToCron(str(a, "remind_at"), str(a, "recurrence"))
			if err != nil {
				return errResult(err.Error()), nil
			}

			sch, err := s.reminderScheduler()
			if err != nil {
				return errResult("reminders are unavailable: " + err.Error()), nil
			}
			// One request, one schedule — see schedule_once.go. What is already
			// scheduled is asked first: the in-memory claim below cannot see a
			// repeat from an hour ago or anything at all from before a restart.
			// A listing that fails is not a reason to refuse a reminder, so the
			// claim still stands behind it.
			if existing, lerr := sch.List(); lerr == nil {
				if held := alreadyScheduled(existing, title); held != nil {
					return errResult(duplicateScheduleMessage(held)), nil
				}
			}
			if held := s.claims.claim(title, plan.Cron); held != nil {
				return errResult(duplicateScheduleMessage(held)), nil
			}
			task, err := sch.Schedule(ReminderPrompt(title), plan.Cron, reminderNote(title, plan.OneShot), "reminders")
			if err != nil {
				s.claims.release(title)
				return errResult(err.Error()), nil
			}
			s.claims.record(title, task.ID, plan.Cron)

			data := map[string]any{
				"id": task.ID, "title": title, "schedule": plan.Cron, "when": plan.Note,
			}
			if task.NextRun != nil {
				data["next_run"] = task.NextRun.Local().Format("2006-01-02 15:04")
			}
			// Keep a row too, or list_reminders answers "none" for a reminder
			// the user just watched get created.
			db.mu.Lock()
			db.Reminders = append(db.Reminders, map[string]any{
				"id": task.ID, "title": title, "schedule": plan.Cron, "when": plan.Note,
				"created_at": time.Now().Format(time.RFC3339),
			})
			db.mu.Unlock()
			db.save()
			if plan.OneShot {
				// cron cannot say "once", so this is stored as the same day every
				// year and removed as soon as it has fired — see reminder_once.go.
				// Said plainly because a reminder that deletes itself is a
				// surprise if you go looking for it afterwards.
				data["note"] = "This fires once and is deleted automatically after it has fired."
			}
			return okData(data), nil
		}, write)

	// set_reminder covers "every day at HH:MM" and one specific moment, which is
	// most of what a person asks for and not all of it. "工作日下午三点"、"每四小时"、
	// "每月一号" have no shape in that tool, and a model asked for them either
	// picks the nearest daily time or gives up. This one takes the cron directly.
	svc.AddToolWithMetadata("schedule_prompt",
		"Run a prompt on any cadence: weekdays, every few hours, a day of the month. Use set_reminder instead only when the timing is every day at HH:MM, or one specific moment. Never call both for one request.",
		obj(map[string]any{
			"prompt":       sp("The instruction to run when it fires, written as you would type it into chat"),
			"cron":         sp("Five-field cron. Daily at 08:00 `0 8 * * *`; weekdays at 09:00 `0 9 * * 1-5`; every four hours `0 */4 * * *`; the 1st at 09:00 `0 9 1 * *`"),
			"name":         sp("Optional label for the list; defaults to the prompt"),
			"conversation": sp("Optional conversation the runs append to; blank shares one called scheduled"),
			"once": map[string]any{
				"type":        "boolean",
				"description": "True when the user named one specific moment rather than a repeating cadence. cron cannot say \"once\", so a single moment has to be stored as that day every year; with this set the schedule is deleted as soon as it has fired, instead of coming back a year later.",
			},
		}, "prompt", "cron"),
		func(ctx context.Context, a map[string]any) (any, error) {
			prompt := strings.TrimSpace(str(a, "prompt"))
			if prompt == "" {
				return errResult("prompt is required"), nil
			}
			sch, err := s.reminderScheduler()
			if err != nil {
				return errResult("scheduling is unavailable: " + err.Error()), nil
			}
			if existing, lerr := sch.List(); lerr == nil {
				if held := alreadyScheduled(existing, prompt); held != nil {
					return errResult(duplicateScheduleMessage(held)), nil
				}
			}
			if held := s.claims.claim(prompt, strings.TrimSpace(str(a, "cron"))); held != nil {
				return errResult(duplicateScheduleMessage(held)), nil
			}
			once, _ := a["once"].(bool)
			label := strings.TrimSpace(str(a, "name"))
			if label == "" {
				label = prompt
			}
			task, err := sch.Schedule(prompt, strings.TrimSpace(str(a, "cron")),
				markOneShot(label, once), strings.TrimSpace(str(a, "conversation")))
			if err != nil {
				s.claims.release(prompt)
				return errResult(err.Error()), nil
			}
			s.claims.record(prompt, task.ID, task.Schedule)
			data := map[string]any{"id": task.ID, "prompt": prompt, "schedule": task.Schedule}
			if once {
				data["note"] = "This fires once and is deleted automatically after it has fired."
			}
			// The next run is the only part a person can check against what they
			// meant. A cron they cannot read is not a confirmation.
			if task.NextRun != nil {
				data["next_run"] = task.NextRun.Local().Format("2006-01-02 15:04 MST")
			}
			return okData(data), nil
		}, write)

	svc.AddToolWithMetadata("list_reminders", "List every reminder.", obj(map[string]any{}),
		func(ctx context.Context, a map[string]any) (any, error) {
			db.mu.Lock()
			rows := append([]map[string]any(nil), db.Reminders...)
			db.mu.Unlock()
			return okData(s.reconcileReminders(rows)), nil
		}, read)
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// moodMarker is the tag the persona is asked for.
const moodMarker = "MOOD:"

// SplitEmotion peels a trailing mood tag off a reply, returning the cleaned
// reply plus the mood.
//
// The tag is optional, which is what makes the "last line" rule matter. When
// every reply ended with one, finding the marker anywhere was good enough. Now
// that a reply may simply not have one, a message that merely mentions the
// marker mid-sentence would be cut in half and the rest reported as a mood. So
// only the final line is considered, and only when the marker starts it.
func SplitEmotion(text string) (reply, emotion string) {
	trimmed := strings.TrimRight(text, " \t\r\n")
	cut := strings.LastIndexAny(trimmed, "\r\n")
	last := strings.TrimSpace(trimmed[cut+1:])

	rest, ok := strings.CutPrefix(last, moodMarker)
	if !ok {
		return splitInlineEmotion(text, trimmed)
	}
	emotion = strings.TrimSpace(rest)
	if emotion == "" {
		// "MOOD:" with nothing after it says nothing, and removing the line
		// would be its only visible effect. Leave the text alone.
		return text, ""
	}
	if cut < 0 {
		// The whole reply was the tag. There is no answer to keep.
		return "", emotion
	}
	reply = strings.TrimRight(trimmed[:cut], " \t\r\n")
	reply = strings.TrimSuffix(reply, "\\n")
	return strings.TrimRight(reply, " \t\r\n"), emotion
}

// splitInlineEmotion peels a tag the model wrote on the answer's own last
// line instead of below it — "其余时间空着。MOOD: neutral" reached a person
// that way, tag and all, from a final answer the model handed in through
// task_complete. Mid-line, only a tag that ends the reply, names one of the
// avatar's moods and follows the end of a sentence counts; anything looser
// would cut a reply that is talking about the format.
func splitInlineEmotion(text, trimmed string) (string, string) {
	i := strings.LastIndex(trimmed, moodMarker)
	if i <= 0 {
		return text, ""
	}
	emotion := strings.TrimSpace(trimmed[i+len(moodMarker):])
	if !slices.Contains(AvatarEmotions(), emotion) {
		return text, ""
	}
	before := strings.TrimRight(trimmed[:i], " \t")
	r, _ := utf8.DecodeLastRuneInString(before)
	if !strings.ContainsRune("。！？.!?", r) {
		return text, ""
	}
	return before, emotion
}

// Plan returns the current plan for a task, or nil.
//
// RunSegments scopes an unnamed plan to its task id under the scratchpad's
// default key, which is how two long tasks against one store stay apart.
func (s *Service) Plan(ctx context.Context, taskID string) []agent.PlanItem {
	if s == nil || s.plans == nil || taskID == "" {
		return nil
	}
	items, err := s.plans.LoadPlan(ctx, "default:"+taskID)
	if err != nil {
		return nil
	}
	return items
}

// singleUserID is the user_id CortexDB tools receive when they need one. The
// app has exactly one user, so any stable value is correct; it only has to
// stay the same across runs.
const singleUserID = "owner"

// eagerNotedTools are the noted tools an ordinary day of conversation calls:
// reading the day and the lists, and the few writes people dictate.
// eagerTools is every tool pattern a request offers up front: noted's
// everyday tools, the browser's when it is mounted, and on a worker the tools
// of its own MCP servers — the reason the queen sends it the work that needs
// them, and a worker that has to search for them finds the wrong ones.
func eagerTools(s *Settings, browser bool) []string {
	out := append([]string{}, eagerNotedTools...)
	if browser {
		out = append(out, browserEagerTools...)
	}
	if s != nil && s.Hive.Role == HiveRoleWorker {
		for name := range OwnMCPServers() {
			out = append(out, "mcp_"+sanitizeName(name)+"_*")
		}
	}
	return out
}

// refreshEagerTools puts eagerTools in force on the running agent, after a
// server was added or removed or the browser mounted.
func (s *Service) refreshEagerTools() {
	if s == nil || s.svc == nil {
		return
	}
	s.svc.SetEagerTools(eagerTools(s.settings, s.browserEager.Load())...)
}

var eagerNotedTools = []string{
	"mcp_noted_get_overview", "mcp_noted_list_events", "mcp_noted_list_tasks",
	"mcp_noted_list_holdings", "mcp_noted_create_event", "mcp_noted_create_task",
	"mcp_noted_complete_task", "mcp_noted_update_task", "mcp_noted_search_notes",
	"mcp_noted_create_note", "mcp_noted_log_time",
}

// deferredToolPatterns are the tools this app keeps behind the index rather
// than in every turn's schema.
//
// The rule is how often a conversation actually calls one. Saving a memory or
// searching for one happens in ordinary chat, so those four stay in the
// schema. Rebuilding an ontology, validating SHACL, resolving an object set or
// repairing a vector dimension does not — those are things the user asks for
// by name, at which point one search makes them callable for the rest of the
// conversation.
//
// Prefix patterns, so "knowledge_graph_*" holds back the graph surface while
// knowledge_search and knowledge_save stay where they are.
var deferredToolPatterns = []string{
	// The knowledge graph, its ontology, and everything that reasons over it.
	"knowledge_graph_*", "knowledge_memory_*", "ontology_*", "side_graph_*",
	"apply_inference", "build_context", "expand_graph", "extract_conversation",
	"fact_provenance", "uncited_facts", "object_set_resolve", "vector_dimension_repair",
	"upsert_entities", "upsert_relations", "delete_entities", "delete_document_graph",
	"ingest_document", "get_chunks", "get_nodes", "find_nodes", "graph_list_all",
	"render_graph_html", "serve_graph_3d",
	// Retrieval variants. The plain search stays; the specialised ones are
	// asked for by name.
	"cortex_query", "search_graphrag_*", "search_chunks_*", "search_text",
	// The rarely used half of the knowledge store. Kept eager:
	// knowledge_search, knowledge_save. Memory tools are agent-go's.
	"knowledge_update", "knowledge_delete", "knowledge_get",
	// Only live during an import, which is a thing the user starts.
	"importflow_*", "connector_*",
}
