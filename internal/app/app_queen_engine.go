package app

// The queen's engine: who thinks for her when a person talks to her.
//
// By default that is this app's own model. Settings → Hive → Queen engine can
// hand it to an agent CLI instead — Claude Code, Codex, pi or Hermes, here
// ("codex") or on a linked machine ("codex.mac") — and SuperLeo keeps
// everything around the thinking:
// the conversation and its history, the stream to every client, approvals,
// notices, alarms and schedules. Clients see the same chat:event / chat:done
// they always did, with "engine" on them naming who answered.
//
// Only a person's own turns go to the engine, superleo_ask's among them.
// Schedules, dashboards, tiles, the hive's traffic, a voice call
// (SendCallChat) and a superleo_ask from the engine's own CLI stay with
// SuperLeo's agent: they are unattended, or cannot wait for a CLI to start,
// or would have the engine answering itself.
//
// Each turn is a run of the CLI (app_cliruns.go) given the queen's prompt
// (backend.ExternalQueenPrompt, the same text her own agent reads), the
// cluster-command tools on this app's /mcp under a queen-scoped token
// (mcp_scope.go), noted and the shared brain when this app has them, and the
// approval channel for its own permission prompts, each the way its CLI takes
// them (queen_engine_cli.go). The conversation's next turn resumes the same
// CLI session, for a CLI that keeps one.
//
// The engines are an ordered list (Hive.QueenEngines, queen_engine_chain.go).
// One that cannot take a turn — not installed, its machine asleep, signed
// out, failed before it said or did anything — hands the turn to the next
// one switched on, whose answer opens with a line saying so; SuperLeo's own
// agent is one of them. With none left the turn fails with the reasons.

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/liliang-cn/agent-go/v3/pkg/agent"
	"github.com/liliang-cn/superleo/internal/backend"
)

const (
	// queenHiveServer is what the engine calls this app's /mcp: its tools are
	// mcp__hive__superleo_hive_members and so on.
	queenHiveServer = "hive"
	// queenTokenEnv carries the queen token to the CLI. The MCP config names
	// it and the CLI fills it in, so the token is never on a command line.
	queenTokenEnv = "SUPERLEO_QUEEN_TOKEN"
	// queenEngineTokenName is the queen token this app mints for its engine.
	queenEngineTokenName = "queen engine (automatic)"
)

// QueenKit is what makes a CLI run the queen's.
type QueenKit struct {
	// SystemPrompt is appended to the CLI's own.
	SystemPrompt string `json:"systemPrompt"`
	// Servers are MCP server entries by name, as --mcp-config reads them.
	Servers map[string]any `json:"servers"`
	// Allowed are tool rules the CLI may use without a permission prompt.
	// The hive server is one: the cluster-changing calls on it pass this
	// app's own approval gate, so asking first as well would ask twice.
	Allowed []string `json:"allowed,omitempty"`
	// Env is set for the CLI alone: the tokens its servers' entries name,
	// and what keeps the run lean.
	Env map[string]string `json:"env,omitempty"`
	// Args go on Claude's command line as they are: queenLeanArgs. Every
	// other CLI's flags are its adapter's, on the machine it runs on.
	Args []string `json:"args,omitempty"`
}

// queenLeanArgs keep the person's own Claude Code out of a queen run: no
// user, project or local settings (so none of their hooks, plugins, model or
// permission rules), no CLAUDE.md from them, no skills or slash commands. The
// login is not a setting and still works. With --strict-mcp-config and
// queenLeanEnv the run is the queen's prompt and the servers it is handed.
var queenLeanArgs = []string{"--setting-sources", "", "--disable-slash-commands"}

// queenLeanEnv turns off Claude's auto-memory for the run, which would read
// and write notes under ~/.claude/projects for the workspace.
var queenLeanEnv = map[string]string{"CLAUDE_CODE_DISABLE_AUTO_MEMORY": "1"}

// QueenRunRequest is a queen run asked of another machine over the agent link
// or its API: StartCLIRun's arguments, the session to resume and the kit.
type QueenRunRequest struct {
	Agent   string   `json:"agent"`
	Prompt  string   `json:"prompt"`
	Cwd     string   `json:"cwd,omitempty"`
	Model   string   `json:"model,omitempty"`
	Ask     bool     `json:"ask"`
	Session string   `json:"session,omitempty"`
	Kit     QueenKit `json:"kit"`
}

// StartQueenCLIRun starts a run here thinking for another machine's queen.
// Its servers must be ones this machine reaches by URL: a command in the kit
// would run here on that machine's say-so.
func (a *App) StartQueenCLIRun(r QueenRunRequest) (CLIRun, error) {
	for name, raw := range r.Kit.Servers {
		e, _ := raw.(map[string]any)
		if url, _ := e["url"].(string); url == "" || e["command"] != nil {
			return CLIRun{}, fmt.Errorf("MCP server %s is not one reached by URL", name)
		}
		if name == "superleo" || name == "superai" { // "superai": saved before the rename
			return CLIRun{}, errors.New("the superleo server name is this machine's approval channel")
		}
	}
	// Only the lean flags and the variables the servers name, or the lean
	// one: anything else on the command line or in the environment is that
	// machine running more than a Claude for its queen.
	if !slices.Equal(r.Kit.Args, queenLeanArgs) && len(r.Kit.Args) > 0 {
		return CLIRun{}, fmt.Errorf("arguments %q are not the queen's", r.Kit.Args)
	}
	named := map[string]bool{}
	if body, err := json.Marshal(r.Kit.Servers); err == nil {
		for _, name := range backend.MCPEnvRefs(string(body)) {
			named[name] = true
		}
	}
	for k := range r.Kit.Env {
		if _, lean := queenLeanEnv[k]; !lean && !named[k] {
			return CLIRun{}, fmt.Errorf("%s is not a variable the queen's servers use", k)
		}
	}
	// "superleo" is this app's own agent, on the desktop
	// (queen_engine_desktop.go).
	if r.Agent == desktopEngineAgent {
		return a.startDesktopQueenRun(r)
	}
	if !canThinkForQueen(r.Agent) {
		return CLIRun{}, fmt.Errorf("%s cannot think for the queen", r.Agent)
	}
	kit := r.Kit
	cwd, session := a.queenRunPlace(r)
	return a.startCLIRun(cliStart{Agent: r.Agent, Prompt: r.Prompt, Cwd: cwd, Model: r.Model, Ask: r.Ask,
		Session: session, Queen: &kit, Origin: "the queen, thinking in a coding agent"})
}

// queenRunPlace is where another machine's queen run works here, and the
// session it resumes. The queen's runs have no project: the other machine
// sends back the directory its last run here reported, to stay with that
// run's session. One this machine does not run in — reported by another
// SuperLeo that served this machine's name before (the Mac's node, before
// the desktop took over), or a root since taken out — is not a reason to
// fail every turn: the run works in this machine's workspace instead, as a
// new session, since the old one was kept under that other directory. The
// directory is never let in for it.
func (a *App) queenRunPlace(r QueenRunRequest) (cwd, session string) {
	if strings.TrimSpace(r.Cwd) == "" {
		return "", r.Session
	}
	a.mu.Lock()
	var roots []string
	if a.settings != nil {
		roots = a.settings.ExternalAgents.Roots
	}
	a.mu.Unlock()
	if _, err := a.cliCwd(r.Cwd, roots); err != nil {
		slog.Warn("queen run: working in the workspace as a new session", "cwd", r.Cwd, "reason", err)
		return "", ""
	}
	return r.Cwd, r.Session
}

// queenEngineFor is the engine a turn goes to, or "" for this app's model.
// SuperLeo first in the list with no model to answer with hands the turn to
// the first engine after it.
func (a *App) queenEngineFor(sessionID string, imagePaths []string) string {
	a.mu.Lock()
	s := a.settings
	a.mu.Unlock()
	if engine := queenEngineRoute(s, sessionID, len(imagePaths) > 0); engine != "" {
		return engine
	}
	if !queenTurn(s, sessionID, len(imagePaths) > 0) || a.hasModel() {
		return ""
	}
	for _, e := range s.Hive.QueenEnginesOn() {
		if e != backend.QueenEngineSuperLeo {
			return e
		}
	}
	return ""
}

// queenEngineRoute decides who thinks for a turn: the first engine in the
// list for a person's own words in a conversation of theirs; this app's
// model for everything else, and for pictures, which the CLI is not handed.
func queenEngineRoute(s *backend.Settings, sessionID string, pictures bool) string {
	if !queenTurn(s, sessionID, pictures) {
		return ""
	}
	on := s.Hive.QueenEnginesOn()
	if len(on) == 0 || on[0] == backend.QueenEngineSuperLeo {
		return ""
	}
	return on[0]
}

// queenTurn is whether a turn is one the queen's engines may take.
func queenTurn(s *backend.Settings, sessionID string, pictures bool) bool {
	return s != nil && s.Hive.Role != backend.HiveRoleWorker && !pictures && personalSession(sessionID)
}

// ownSessionPrefixes mark conversations no person is typing in: the hive's
// orders and mail, home tiles, terminal Claude Code sessions shown in the app.
var ownSessionPrefixes = []string{backend.HiveSessionPrefix, HiveMailSessionPrefix, "tile-", "cc:"}

func personalSession(id string) bool {
	id = strings.TrimSpace(id)
	if id == "" || id == "scheduled" {
		return false
	}
	if _, ok := backend.DashboardIDFromSession(id); ok {
		return false
	}
	for _, p := range ownSessionPrefixes {
		if strings.HasPrefix(id, p) {
			return false
		}
	}
	return true
}

// routeToEngine answers a turn with the queen's engines, from engine on:
// one at a time, each that cannot take the turn handing it to the next one
// switched on (queenEnginesAfter). busy: the conversation's own session is
// still answering, so an engine starts a session of its own for this turn.
func (a *App) routeToEngine(requestID, sessionID, engine, message string, busy bool) {
	a.mu.Lock()
	var s backend.Settings
	if a.settings != nil {
		s = *a.settings
	}
	a.mu.Unlock()

	started := time.Now()
	a.openTurns.Store(requestID, backend.ChatSessionInfo{ID: sessionID, Title: firstLineOf(message, 80),
		UpdatedAt: started.UTC().Format(time.RFC3339)})
	// Stop works before the run has an id too: it is stopped as it starts,
	// and no engine after it is tried.
	var mu sync.Mutex
	runID, stopped := "", false
	a.trackRun(requestID, func() {
		mu.Lock()
		id := runID
		stopped = true
		mu.Unlock()
		if id != "" {
			a.CancelCLIRun(id)
		}
	})
	a.runSession(requestID, sessionID)
	end := func() {
		a.untrackRun(requestID)
		a.openTurns.Delete(requestID)
	}

	rest := queenEnginesAfter(&s, engine)
	var reasons []string
	var attempt func(eng string)

	// handOn gives the turn to the next engine switched on, or reports why
	// none could take it.
	handOn := func(failed, reason string) {
		reason = cliClip(strings.TrimSpace(reason), 300)
		reasons = append(reasons, engineLabel(failed)+": "+reason)
		for len(rest) > 0 {
			next := rest[0]
			rest = rest[1:]
			mu.Lock()
			halted := stopped
			mu.Unlock()
			if halted {
				end()
				a.emit("chat:cancelled", map[string]any{"requestId": requestID, "engine": failed, "final": ""})
				return
			}
			if next == backend.QueenEngineSuperLeo {
				if !a.hasModel() {
					continue
				}
				end()
				a.setQueenNow(queenNowSuperLeo)
				a.emit("chat:event", map[string]any{"requestId": requestID, "type": "state_update", "engine": "", "standInFor": engine, "failed": failed,
					"content": engineLabel(failed) + " could not take this turn (" + reason + "); SuperLeo answers it"})
				a.ownTurn(requestID, sessionID, message, nil, engine)
				return
			}
			a.emit("chat:event", map[string]any{"requestId": requestID, "type": "state_update", "engine": next, "failed": failed,
				"content": engineLabel(failed) + " could not take this turn (" + reason + "); " + engineLabel(next) + " answers it"})
			attempt(next)
			return
		}
		end()
		msg := engineLabel(failed) + " could not answer: " + reason
		if len(reasons) > 1 {
			msg = "No engine could answer: " + strings.Join(reasons, "; ")
		}
		a.emit("chat:error", map[string]any{"requestId": requestID, "engine": failed, "error": msg})
	}

	attempt = func(eng string) {
		tag := func(m map[string]any) map[string]any {
			m["requestId"], m["engine"] = requestID, eng
			return m
		}
		label := engineLabel(eng)
		// An engine answering in the place of the first one says so first.
		inPlaceOf := ""
		if eng != engine {
			inPlaceOf = engine
		}
		a.setQueenNow(eng)

		kit, err := a.queenKitFor(eng, &s)
		if err != nil {
			handOn(eng, err.Error())
			return
		}
		o := cliStart{Agent: eng, Prompt: message, Ask: !s.ExternalAgents.Unattended, Chat: sessionID, Queen: kit}
		// busy: the conversation's own session is still answering, so this
		// turn starts one of its own and is handed the conversation instead.
		// A CLI that keeps no session (hermes) is new to every turn.
		if prev := a.lastChatRun(sessionID, eng, true); prev != nil && !busy && queenEngineTraits(eng).Resumes {
			o.Thread, o.Session, o.Cwd, o.Model, o.Prev = prev.Thread, prev.Session, prev.Cwd, prev.Model, prev
		}
		// What was said here that the engine's session did not hear goes in
		// front of the question: the whole conversation for a session new to
		// it, the turns since its last answer for one it continues.
		if earlier := handoffText(a.unseenTurns(sessionID, eng, o.Prev != nil)); earlier != "" {
			o.Prompt = earlier + message
		}

		// The answer as it is written. Claude and pi send a message's text
		// twice, as deltas while they write and whole once they have; a
		// message whose deltas came is not written again.
		var said strings.Builder
		last, fresh := "", true
		write := func(chunk string) {
			if said.Len() == 0 && inPlaceOf != "" {
				a.emit("chat:event", tag(map[string]any{"type": "partial", "content": inPlaceNote(inPlaceOf, eng), "inPlaceOf": inPlaceOf}))
			}
			if said.Len() > 0 && fresh {
				chunk = "\n\n" + chunk
			}
			said.WriteString(chunk)
			a.emit("chat:event", tag(map[string]any{"type": "partial", "content": chunk}))
		}
		// A CLI that prints only its last word (hermes) prints its failure
		// the same way: what it says is held until it is known to be an
		// answer. So is a first whole message that came without deltas
		// before anything was done: a signed-out Claude writes its refusal
		// that way, then fails the turn (exiting 0), and the refusal is not
		// the person's answer.
		holds := !queenEngineTraits(eng).Streams
		held, holding := "", false
		flush := func() {
			if holding {
				holding = false
				write(held)
			}
		}
		o.Watch = func(e CLIRunEvent) {
			switch e.Kind {
			case "delta":
				flush()
				write(e.Text)
				fresh = false
			case "text":
				last = e.Text
				if fresh && !holds {
					if said.Len() == 0 && !holding {
						held, holding = e.Text, true
					} else {
						flush()
						write(e.Text)
					}
				}
				fresh = true
			case "tool":
				flush()
				a.emit("chat:event", tag(map[string]any{"type": "tool_call", "tool": shortTool(e.Tool),
					"args": map[string]any{"detail": e.Detail}}))
			case "result":
				flush()
				a.emit("chat:event", tag(map[string]any{"type": "tool_result", "tool": shortTool(e.Tool),
					"result": map[string]any{"ok": !e.Failed, "output": e.Text}}))
			case "note":
				if e.Text == "waiting for approval" {
					flush()
					a.emit("chat:event", tag(map[string]any{"type": "state_update",
						"content": fmt.Sprintf("%s wants to use %s — waiting for your approval", label, shortTool(e.Tool))}))
				}
			}
		}
		o.Done = func(r CLIRun) {
			switch r.State {
			case "cancelled":
				flush()
				end()
				a.emit("chat:cancelled", tag(map[string]any{"final": said.String()}))
				return
			case "failed":
				mu.Lock()
				byHand := stopped
				mu.Unlock()
				// Nothing said and nothing done: the engine never got going
				// (a login that ran out, a machine gone mid-start), and the
				// person still has a question for the next one.
				if said.Len() == 0 && r.Tools == 0 && !byHand {
					handOn(eng, firstNonEmpty(r.Error, held, "it failed"))
					return
				}
				flush()
				end()
				a.emit("chat:error", tag(map[string]any{"error": label + " did not finish: " + r.Error}))
				return
			}
			flush()
			end()
			answer := strings.TrimSpace(r.Summary)
			if answer == "" {
				answer = strings.TrimSpace(last)
			}
			if answer == "" {
				answer = strings.TrimSpace(said.String())
			}
			if holds && said.Len() == 0 && answer != "" {
				write(answer)
			}
			if err := a.appendExchange(sessionID, message, answer); err != nil {
				slog.Warn("could not record the queen engine's answer", "session", sessionID, "error", err)
			}
			// Claude reports what the whole session has cost so far; this
			// turn's share is what it added. The others report the turn's own.
			cost := r.CostUSD
			if engineCLI(eng) == "claude" && o.Prev != nil && r.Session == o.Prev.Session && cost >= o.Prev.CostUSD {
				cost -= o.Prev.CostUSD
			}
			recordTurnTimeBy(sessionID, message, started, time.Since(started), turnBy{Engine: eng, CostUSD: cost, InPlaceOf: inPlaceOf})
			done := tag(map[string]any{"final": answer})
			if inPlaceOf != "" {
				done["final"] = inPlaceNote(inPlaceOf, eng) + answer
				done["inPlaceOf"] = inPlaceOf
			}
			if cost > 0 {
				done["costUsd"] = cost
			}
			a.emit("chat:done", done)
		}

		a.emit("chat:event", tag(map[string]any{"type": "state_update", "content": "starting " + label + "…"}))
		if engineCLI(eng) == "hermes" && a.hermesWarming() {
			a.emit("chat:event", tag(map[string]any{"type": "state_update", "content": label + " is still getting its environment ready; the first answer takes longer"}))
		}
		mu.Lock()
		runID = ""
		mu.Unlock()
		run, err := queenStartRun(a, o)
		if err != nil {
			handOn(eng, err.Error())
			return
		}
		mu.Lock()
		runID = run.ID
		cancelNow := stopped
		mu.Unlock()
		if cancelNow {
			a.CancelCLIRun(run.ID)
		}
	}
	attempt(engine)
}

// queenNowSuperLeo is queenEngineNow while SuperLeo's own agent answers in
// the engine's place.
const queenNowSuperLeo = "superleo" // what the phone apps compare against

// queenNowState is the engine the queen's latest turn went to.
type queenNowState struct {
	mu     sync.Mutex
	engine string
}

// setQueenNow notes who takes the queen's turns now and tells every client
// when that changes, so the 3D queen and the settings show SuperLeo
// answering in the engine's place as it happens.
func (a *App) setQueenNow(engine string) {
	q := &a.queenNow
	q.mu.Lock()
	changed := q.engine != engine
	q.engine = engine
	q.mu.Unlock()
	if changed {
		a.emit("queen:engine", map[string]any{"engine": engine})
	}
}

// queenEngineNow is who took the queen's latest turn: an engine in her
// list, or queenNowSuperLeo when SuperLeo answered in an engine's place; ""
// before any turn, with SuperLeo first in the list, or once the engine
// taking it is no longer in the list.
func (a *App) queenEngineNow() string {
	q := &a.queenNow
	q.mu.Lock()
	now := q.engine
	q.mu.Unlock()
	a.mu.Lock()
	var h backend.HiveSettings
	if a.settings != nil {
		h = a.settings.Hive
	}
	a.mu.Unlock()
	if now == "" || strings.TrimSpace(h.QueenEngine) == "" {
		return ""
	}
	if now == queenNowSuperLeo || slices.Contains(h.QueenEnginesOn(), now) {
		return now
	}
	return ""
}

// queenKitFor puts together what the engine is handed for one turn.
func (a *App) queenKitFor(engine string, s *backend.Settings) (*QueenKit, error) {
	// A desktop SuperLeo thinks with its own agent, prompt and tools: it is
	// handed the turn and nothing else.
	if isDesktopEngineID(engine) {
		return &QueenKit{}, nil
	}
	_, _, remote := splitRemoteCLI(engine)
	var base string
	if remote {
		// The CLI is on another machine, so this app's loopback is not where
		// it can reach /mcp.
		if base = s.Hive.Public(); base == "" {
			return nil, fmt.Errorf("%s is on another machine and this SuperLeo has no public address for it to reach (Settings → Hive → public address)", engine)
		}
	} else {
		u, err := a.localQueenURL()
		if err != nil {
			return nil, err
		}
		base = u
	}
	tok, err := a.queenEngineToken()
	if err != nil {
		return nil, fmt.Errorf("could not mint the queen's token: %w", err)
	}
	kit := &QueenKit{
		SystemPrompt: backend.ExternalQueenPrompt(s),
		Servers: map[string]any{queenHiveServer: map[string]any{
			"type": "http", "url": base + mcpPath,
			"headers": map[string]any{"Authorization": "Bearer ${" + queenTokenEnv + "}"},
		}},
		Allowed: []string{"mcp__" + queenHiveServer},
		Env:     map[string]string{queenTokenEnv: tok},
	}
	// Claude's lean flags travel with the kit, as they always have, so a
	// machine on an older SuperLeo still runs its Claude lean.
	if engineCLI(engine) == "claude" {
		kit.Args = append([]string(nil), queenLeanArgs...)
		for k, v := range queenLeanEnv {
			kit.Env[k] = v
		}
	}
	for _, name := range backend.QueenEngineServers {
		entry, env, why, ok := backend.EngineMCPServerWhy(name, remote)
		if !ok {
			// A server this instance does not have is ordinary; any other
			// reason costs the engine a tool, so it is said.
			if why != backend.EngineNotConfigured {
				slog.Warn("queen engine is not handed an MCP server", "engine", engine, "server", name, "reason", why)
			}
			continue
		}
		kit.Servers[name] = entry
		for k, v := range env {
			kit.Env[k] = v
		}
	}
	return kit, nil
}

// queenMayCall reports whether a queen run may call an MCP tool without the
// person being asked: whenever the queen's own call of it would not be. The
// tool arrives as Claude and pi name it, mcp__<server>__<tool>.
func (a *App) queenMayCall(tool string, args map[string]any) bool {
	rest, ok := strings.CutPrefix(tool, "mcp__")
	if !ok {
		return false
	}
	_, name, ok := strings.Cut(rest, "__")
	if !ok || name == "" {
		return false
	}
	a.mu.Lock()
	svc := a.svc
	a.mu.Unlock()
	gate := a.gateForMCP
	if gate == nil {
		gate = svc.ToolGate()
	}
	if gate == nil {
		return false
	}
	return !gate.Policy()(agent.PermissionRequest{ToolName: name, ToolArgs: args})
}

// shortTool is a CLI's tool name as the trace shows it: an MCP tool by its
// own name, without the mcp__<server>__ in front.
func shortTool(t string) string {
	if rest, ok := strings.CutPrefix(t, "mcp__"); ok {
		if _, name, ok := strings.Cut(rest, "__"); ok && name != "" {
			return name
		}
	}
	return t
}

// engineLabel is how a person reads an engine's name.
func engineLabel(engine string) string {
	if engine == backend.QueenEngineSuperLeo {
		return "SuperLeo"
	}
	agent, host, ok := splitRemoteCLI(engine)
	if !ok {
		agent = engine
	}
	name := agentLabel(agent)
	if ok && agent == desktopEngineAgent {
		name = "SuperLeo"
	}
	if ok {
		name += " · " + host
	}
	return name
}

// standInNote opens an answer SuperLeo's model gave in an engine's place.
func standInNote(engine string) string {
	return "> " + engineLabel(engine) + " 没能接这一条，由 SuperLeo 内置的蜂后回答。\n\n"
}

// queenTokenCache is the engine's queen token, in memory only.
type queenTokenCache struct {
	mu     sync.Mutex
	secret string
}

// queenEngineToken is the queen token the engine reaches /mcp with: minted on
// first use, the same one every turn after, and minted again if it was
// revoked. The secret is in memory alone; the store keeps its hash, as for
// every token. One minted by an earlier run of this app is revoked when this
// one mints its own, since nothing holds that one's secret any more.
func (a *App) queenEngineToken() (string, error) {
	c := &a.queenToken
	c.mu.Lock()
	defer c.mu.Unlock()
	s, err := openIntegrations(integrationsPath())
	if err != nil {
		return "", err
	}
	if c.secret != "" && s.holds(c.secret, scopeQueen) {
		return c.secret, nil
	}
	old, err := s.list()
	if err != nil {
		return "", err
	}
	for _, t := range old {
		if t.Name == queenEngineTokenName && t.Scope == scopeQueen {
			if _, err := s.revoke(t.ID); err != nil {
				return "", err
			}
		}
	}
	_, secret, err := s.createScoped(queenEngineTokenName, scopeQueen)
	if err != nil {
		return "", err
	}
	c.secret = secret
	return secret, nil
}

// QueenEngineChoice is one thing the queen can think with.
type QueenEngineChoice struct {
	// Name is what Hive.QueenEngine is set to; "" is SuperLeo's own agent.
	Name  string `json:"name"`
	Label string `json:"label"`
	// Online is whether it could take a turn now.
	Online bool `json:"online"`
}

// QueenEngines is what the queen thinks with, who took her latest turn (the
// engine, or "superleo" when SuperLeo answered in its place), whether SuperLeo
// stands in for an engine that cannot answer, and every engine there is
// to choose from:
//
//	POST /api/rpc/QueenEngines  []
//	→ {"engine":"claude","now":"claude","fallback":true,"public_url":"","choices":[{"name":"","label":"SuperLeo","online":true},…]}
func (a *App) QueenEngines() map[string]any {
	a.mu.Lock()
	var s backend.Settings
	if a.settings != nil {
		s = *a.settings
	}
	a.mu.Unlock()
	choices := []QueenEngineChoice{{Name: "", Label: "SuperLeo", Online: true}}
	here := map[string]bool{}
	for _, c := range a.localAgentNames() {
		here[c.Name] = true
	}
	for _, name := range queenEngineNames {
		if here[name] {
			choices = append(choices, QueenEngineChoice{Name: name, Label: engineLabel(name), Online: true})
		}
	}
	remote := a.remoteCLIs()
	hosts := make([]string, 0, len(remote))
	for h := range remote {
		hosts = append(hosts, h)
	}
	slices.Sort(hosts)
	for _, h := range hosts {
		for _, name := range queenEngineNames {
			if slices.Contains(remote[h], name) {
				choices = append(choices, QueenEngineChoice{Name: name + "." + h, Label: engineLabel(name + "." + h), Online: true})
			}
		}
	}
	for _, d := range a.desktopEngines() {
		choices = append(choices, QueenEngineChoice{Name: d.ID, Label: d.Label, Online: true})
	}
	cur := strings.TrimSpace(s.Hive.QueenEngine)
	if cur != "" && !slices.ContainsFunc(choices, func(c QueenEngineChoice) bool { return c.Name == cur }) {
		choices = append(choices, QueenEngineChoice{Name: cur, Label: engineLabel(cur)})
	}
	return map[string]any{"engine": cur, "now": a.queenEngineNow(), "fallback": s.Hive.QueenFallsBack(), "public_url": s.Hive.PublicURL, "choices": choices}
}

// SetQueenEngine picks what the queen thinks with ("" is SuperLeo's own agent)
// and whether SuperLeo answers a turn the engine cannot take: the engine goes
// first in her list, and SuperLeo is switched on or off in it. Saved like any
// other settings change. SetQueenEngineList sets the whole list.
func (a *App) SetQueenEngine(engine string, fallback bool) (map[string]any, error) {
	a.mu.Lock()
	var next backend.Settings
	if a.settings != nil {
		next = *a.settings
	} else if s, err := backend.LoadSettings(); err == nil {
		next = *s
	}
	a.mu.Unlock()
	next.Hive.PromoteQueenEngine(engine, fallback)
	if err := a.SaveSettings(next); err != nil {
		return nil, err
	}
	return a.QueenEngines(), nil
}

// How much of the earlier conversation an engine is handed: the last few
// turns, each and all of them cut to a size that is context and not a
// transcript.
const (
	handoffTurns    = 6
	handoffTurnMax  = 1200
	handoffTotalMax = 6000
)

// sessionTurns is a conversation as ChatHistory shows it, with who answered.
func (a *App) sessionTurns(sessionID string) []backend.ChatTurn {
	if a.turnsFn != nil {
		return withTurnTimes(sessionID, a.turnsFn(sessionID))
	}
	a.mu.Lock()
	svc := a.svc
	a.mu.Unlock()
	if svc == nil {
		return nil
	}
	return withTurnTimes(sessionID, svc.SessionTurns(sessionID))
}

// unseenTurns are the turns of a conversation the engine's session has not
// heard: all of them for a new session, those after its last answer for one
// it resumes. A resumed session whose last answer is not found (the history
// was compacted) is handed nothing rather than a guess.
func (a *App) unseenTurns(sessionID, engine string, resumed bool) []backend.ChatTurn {
	turns := a.sessionTurns(sessionID)
	if len(turns) == 0 && !resumed {
		turns = a.engineRunTurns(sessionID, engine)
	}
	from := 0
	if resumed {
		from = -1
		for i := len(turns) - 1; i >= 0; i-- {
			if turns[i].Role == "assistant" && turns[i].Engine == engine {
				from = i + 1
				break
			}
		}
		if from < 0 {
			return nil
		}
	}
	var out []backend.ChatTurn
	for _, t := range turns[from:] {
		if t.Kind == "" && (t.Role == "user" || t.Role == "assistant") && strings.TrimSpace(t.Content) != "" {
			out = append(out, t)
		}
	}
	return out
}

// handoffEnd closes the earlier conversation in front of a question.
const handoffEnd = "[End of the earlier conversation. The person now says:]\n\n"

// engineRunTurns is a conversation as the engine's own finished runs in it
// tell it, for an app with no history of its own to keep it in (no model
// set up): each run's question, without what it was handed in front of it,
// and its answer. A CLI that keeps no session of its own then still hears
// what was said.
func (a *App) engineRunTurns(sessionID, engine string) []backend.ChatTurn {
	s := a.cliStore()
	s.mu.Lock()
	var runs []CLIRun
	for _, r := range s.runs {
		if r.Chat == sessionID && r.Agent == engine && r.Queen && r.State == "done" && strings.TrimSpace(r.Summary) != "" {
			runs = append(runs, summaryOf(r))
		}
	}
	s.mu.Unlock()
	slices.SortFunc(runs, func(x, y CLIRun) int { return x.Started.Compare(y.Started) })
	var out []backend.ChatTurn
	for _, r := range runs {
		q := r.Prompt
		if i := strings.LastIndex(q, handoffEnd); i >= 0 {
			q = q[i+len(handoffEnd):]
		}
		out = append(out, backend.ChatTurn{Role: "user", Content: q}, backend.ChatTurn{Role: "assistant", Content: r.Summary, Engine: engine})
	}
	return out
}

// handoffText is the earlier conversation as it goes in front of a question,
// oldest first and marked as what it is.
func handoffText(turns []backend.ChatTurn) string {
	if len(turns) > handoffTurns {
		turns = turns[len(turns)-handoffTurns:]
	}
	var lines []string
	size := 0
	for i := len(turns) - 1; i >= 0; i-- {
		who := "Person"
		if turns[i].Role == "assistant" {
			who = "Queen"
		}
		line := who + ": " + cliClip(strings.TrimSpace(turns[i].Content), handoffTurnMax)
		if size+len(line) > handoffTotalMax {
			break
		}
		size += len(line)
		lines = append([]string{line}, lines...)
	}
	if len(lines) == 0 {
		return ""
	}
	return "[Earlier in this conversation, oldest first. You did not see these turns; they are context, not requests.]\n" +
		strings.Join(lines, "\n\n") + "\n" + handoffEnd
}
