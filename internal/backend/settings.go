// Package backend hosts the AgentGo-powered SuperLeo service, the avatar event
// driver, and the persisted user settings for the SuperLeo app.
package backend

import (
	"context"
	"encoding/json"
	_ "github.com/liliang-cn/superleo/internal/brand" // the old SuperLeo names keep working
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/liliang-cn/agent-go/v3/pkg/agent"
	"github.com/liliang-cn/agent-go/v3/pkg/domain"
	"github.com/liliang-cn/agent-go/v3/pkg/pool"
)

// Settings is the user-editable configuration, persisted as JSON at
// ~/.superleo/settings.json.
type Settings struct {
	// Brain (LLM) — any OpenAI-compatible endpoint.
	LLMBaseURL string `json:"llm_base_url"`
	LLMKey     string `json:"llm_key"`
	LLMModel   string `json:"llm_model"`
	// LLMContextTokens is LLMModel's context window, and LLMMaxOutputTokens the
	// most one response may use. agent-go derives when to compact from the
	// window and knows none by itself, so unset it is DefaultLLMContextTokens:
	// agent-go's own fallback, a fixed 60k, is a sixteenth of a 1M-token model.
	LLMContextTokens   int `json:"llm_context_tokens,omitempty"`
	LLMMaxOutputTokens int `json:"llm_max_output_tokens,omitempty"`
	// LLMReasoningEffort is sent as reasoning_effort on every call to LLMModel
	// ("low", "medium", "high"); empty sends nothing. On a thinking model
	// behind a gateway it is most of a turn's wait: gemini-3.8-flash-high
	// took 5–8s to its first tool call, 4–4.7s at "low".
	LLMReasoningEffort string `json:"llm_reasoning_effort,omitempty"`

	// Embeddings (optional). If EmbedKey is empty or "none", SuperLeo falls back
	// to file memory so chat-only proxies still work.
	EmbedBaseURL string `json:"embed_base_url"`
	EmbedKey     string `json:"embed_key"`
	EmbedModel   string `json:"embed_model"`

	// Speech (optional). An OpenAI-compatible /v1/audio/speech endpoint, used
	// to read an answer aloud in a real voice rather than the browser's own
	// synthesiser.
	//
	// The base URL and key fall back to the embedding endpoint's when left
	// empty, because on this install they are the same gateway and asking for
	// the same two secrets twice is how one of them ends up stale. TTSModel
	// has no default: a wrong model id there fails every request with a 404
	// that says nothing, so it is better to be plainly unconfigured.
	TTSBaseURL string `json:"tts_base_url"`
	TTSKey     string `json:"tts_key"`
	TTSModel   string `json:"tts_model"`
	// TTSVoice is the provider's voice name. Empty sends none, which the
	// providers that have no voices require and the ones that do accept.
	TTSVoice string `json:"tts_voice"`

	// Speech recognition (optional): the phone's 「服务器」 mode sends the
	// recorded audio here after the user stops. STTAPI picks the protocol:
	// "transcriptions" (default) is OpenAI's /v1/audio/transcriptions, "chat"
	// puts the audio into a /v1/chat/completions message as input_audio, which
	// is how an audio-capable chat model (Gemini behind a gateway) is used when
	// no transcription model is served. The base URL and key fall back to the
	// brain's endpoint; STTModel has no default, so an install without one
	// reports plainly that it has no server recognition.
	STTBaseURL string `json:"stt_base_url"`
	STTKey     string `json:"stt_key"`
	STTModel   string `json:"stt_model"`
	STTAPI     string `json:"stt_api"`
	// STTPrompt overrides the built-in hint (Mandarin with English tech terms).
	STTPrompt string `json:"stt_prompt"`

	// Web search. The built-in web_search tool asks a search-capable,
	// OpenAI-compatible chat endpoint for a grounded answer. Left empty, these
	// fall back to the brain's own endpoint — a gateway that serves a
	// search-grounded model answers the same request, and an agent that cannot
	// look anything up is stuck on any question about now.
	SearchBaseURL string `json:"search_base_url"`
	SearchKey     string `json:"search_key"`
	SearchModel   string `json:"search_model"`
	// SearchNative names the field that turns that endpoint's search on:
	// google_search (a Gemini gateway), openai, dashscope, anthropic or none.
	// Empty sends both the OpenAI and the DashScope field, which a Gemini
	// gateway accepts and ignores — the call then answers without searching;
	// with the brain on api.anthropic.com and no search endpoint of its own,
	// empty means anthropic (BrainSearchesItself).
	SearchNative string `json:"search_native"`

	// SearXNGURL points the builtin websearch MCP at a self-hosted SearXNG
	// instance (its JSON API must be enabled). Configured, that instance is
	// tried before the scraping engines — a stable aggregator beats parsing
	// search sites' HTML. Empty, the scrapers behave as before.
	SearXNGURL string `json:"searxng_url"`

	// DisableSelfInstall withholds the self-extension toolset —
	// search_mcp_servers, add_mcp_server, search_skills, install_skill and the
	// URL installer. Those tools let the agent go and acquire a capability it
	// lacks, which is right for a desktop conversation and wrong for a
	// one-shot invocation: a run that answers one question should not be
	// installing software, and the tools cost a schema slot and a tempting
	// detour on every task that will never use them. cmd/superleo sets it.
	DisableSelfInstall bool `json:"disable_self_install"`

	// DisableToolApproval turns off the tool approval gate: shell commands,
	// self-installs and deletions then run without asking (they are still
	// written to the audit log).
	//
	// Phrased as a disable so the safe state is the zero value. A settings.json
	// written before this field existed unmarshals to false, which means the
	// gate is on — an upgrade cannot silently leave a machine ungated, and
	// neither can a hand-edited file that forgot the key. The same reasoning
	// the DisableSelfInstall / DisableBrowser fields use, applied to the one
	// setting where getting the default backwards actually costs something.
	DisableToolApproval bool `json:"disable_tool_approval"`

	// ToolApprovalDeciderURL points at a laya-serve instance that judges a
	// call's arguments, not just its name. Empty is off, which is the gate
	// exactly as it was: the name rules alone.
	//
	// It can only add an ask. The model is fast and is confidently wrong on
	// negated text, so it is never allowed to wave a call through, and an
	// unreachable service leaves today's behaviour rather than opening or
	// closing the gate wholesale.
	ToolApprovalDeciderURL string `json:"tool_approval_decider_url,omitempty"`

	// ToolApprovalDeciderThreshold is the probability at or above which a call
	// is treated as needing approval. Zero means DefaultDeciderThreshold.
	//
	// Worth re-measuring against your own audit log before changing: the
	// default was chosen on fifteen labelled calls, and the checkpoint's
	// probabilities sit low enough that a value that reads as "confident"
	// lets a write to ~/.ssh/authorized_keys through.
	ToolApprovalDeciderThreshold float64 `json:"tool_approval_decider_threshold,omitempty"`

	// Agent workspace (sandbox root).
	WorkspaceDir string `json:"workspace_dir"`

	// Autonomy / runtime.
	//
	// MaxRounds is the tool-call round budget for one turn. agent.UnlimitedRounds
	// (-1) removes the budget entirely, which also removes the only thing that
	// stops a model looping on a failing tool — there is no wall-clock deadline
	// on a turn, so unlimited means the stop button is the backstop.
	MaxRounds int  `json:"max_rounds"`
	Headless  bool `json:"headless"`

	// Token budgets, optional: zero means no cap. TurnMaxTokens bounds one
	// turn, prompt plus completion over all its rounds; LongTaskMaxTokens is
	// what a long task may use across all its segments when whoever starts it
	// does not say.
	TurnMaxTokens     int `json:"turn_max_tokens,omitempty"`
	LongTaskMaxTokens int `json:"long_task_max_tokens,omitempty"`

	// DisableBrowser is retained for settings-file compatibility; the
	// browser is BrowserMode's.
	DisableBrowser bool `json:"disable_browser"`
	// BrowserMode is the built-in browser (browser.go): "chrome" works in the
	// person's own Chrome, "own" in one it launches, "off" — the default —
	// has none.
	BrowserMode string `json:"browser_mode,omitempty"`
	// DesktopControl lets the agent operate this machine's desktop apps
	// through their accessibility trees (desktop.go). Off unless chosen.
	DesktopControl bool `json:"desktop_control,omitempty"`
	// DesktopBlockedApps are the apps desktop control leaves alone; null,
	// desktopBlockedDefault (messages, mail, password stores, terminals,
	// System Settings). Not omitempty: an emptied list is the person saying
	// "none", and dropped from the file it would come back as the default.
	DesktopBlockedApps []string `json:"desktop_blocked_apps"`

	// StartAtLogin opens the desktop app when this person logs in (a login
	// item, app/desktop_resident.go), and KeepRunning leaves it running with
	// its window closed, so this Mac stays in its hive. Both are the
	// desktop app's own and off by default; KeepRunning is read when the
	// app starts.
	StartAtLogin bool `json:"start_at_login,omitempty"`
	KeepRunning  bool `json:"desktop_keep_running,omitempty"`

	// DisablePTC turns off Programmatic Tool Calling (the model writes JS that
	// drives tools in a sandbox, so one round does several tool calls).
	//
	// Default OFF — PTC is on. Measured with gpt-5.5 on 2026-07-29
	// (backend/ptc_rounds_test.go counts provider calls through a proxy):
	//
	//	                 PTC on   direct
	//	provider calls        5        8
	//	prompt tokens    39,957   96,614
	//	completion tok      638      415
	//	wall clock         26.8s    33.4s
	//
	// Every round re-sends the whole history plus every tool definition, so
	// fewer rounds is mostly a prompt-token win: ~59% cheaper here, and faster
	// too. Set this true for a model that rejects PTC's format.
	DisablePTC bool `json:"disable_ptc"`

	// PIIRedaction is currently a no-op: agent-go v3 removed the built-in PII
	// guardrail along with pkg/browser. The field is kept so existing settings
	// files still parse; re-implement redaction here (or in a provider proxy)
	// before advertising it again.
	PIIRedaction bool `json:"pii_redaction"`

	// Avatar driver server port (127.0.0.1:AvatarPort).
	AvatarPort int `json:"avatar_port"`

	// CLIProxyEnabled runs an embedded CLIProxyAPI (127.0.0.1:CLIProxyPort) and
	// routes the brain through it, so the agent uses the Claude Code / Codex /
	// Gemini CLI subscriptions whose credentials live in <data>/cliproxy/auths
	// instead of LLMBaseURL + LLMKey. LLMModel still selects the model — it must
	// be one the proxy serves (see the model list in Settings).
	CLIProxyEnabled bool `json:"cliproxy_enabled"`

	// CLIProxyPort is the local port the embedded proxy binds (loopback only).
	CLIProxyPort int `json:"cliproxy_port"`

	// Proxy is the network proxy SuperLeo's own outside requests and the
	// agent CLIs it starts go through (proxy.go): the environment by
	// default, off, the system's, or a URL.
	Proxy ProxySettings `json:"proxy,omitempty"`

	// MemoryBackend selects where durable memory lives:
	//
	//	"local"  (default) — this machine's own store under <data>/data.
	//	"shared"           — a remote CortexDB (the "shared brain") that other
	//	                     agents also read and write, over gRPC.
	//
	// This is a one-of-two choice on purpose. A capability should have exactly
	// one route: when the shared brain is the memory backend, any MCP server
	// pointed at the same endpoint is dropped from the tool surface, because
	// two names for one store is how a model ends up calling both and
	// reporting "not found".
	MemoryBackend string `json:"memory_backend"`

	// SharedMemoryEndpoint is host:port of the shared CortexDB gRPC server.
	// Used only when MemoryBackend is "shared"; falls back to $CORTEXDB_REMOTE.
	SharedMemoryEndpoint string `json:"shared_memory_endpoint"`

	// SharedMemoryToken is the shared brain's bearer token. Leave it empty to
	// read $CORTEXDB_GRPC_TOKEN instead, which keeps the secret out of
	// settings.json.
	SharedMemoryToken string `json:"shared_memory_token"`

	// SharedMemoryNamespace scopes reads and writes inside the shared brain.
	// Defaults to "default", which is the namespace the other clients use.
	SharedMemoryNamespace string `json:"shared_memory_namespace"`

	// WebhookURL receives a JSON POST for every notification the agent
	// produces: notify_user mid-task, and each scheduled run as it finishes.
	//
	// It exists because the other two surfaces both need someone already
	// looking — notify_user rides the stream to an open page, and the desktop
	// notification is macOS-only and absent in serve mode. On a headless box a
	// reminder that fired at 08:00 reached a log and nobody. One POST is what
	// Telegram, a WeCom bot, bark, ntfy and a home-made receiver all accept
	// behind a short adapter, which is why this is a URL and not a list of
	// per-service integrations.
	//
	// Empty disables it, which is the default.
	WebhookURL string `json:"webhook_url"`

	// WebhookSecret, when set, is sent as `Authorization: Bearer <secret>` so a
	// receiver on the open internet can tell the agent's calls from anyone
	// else's. A bearer token rather than a request signature: the receiver is
	// usually a few lines of script, and a token it can compare is the check it
	// will actually implement.
	WebhookSecret string `json:"webhook_secret"`

	// TelegramBotToken turns on the inbound Telegram bridge (telegram.go): the
	// bot is long-polled and every message from an allowed chat runs a turn,
	// with the answer sent back. Empty disables it, which is the default.
	//
	// This is the one per-service integration in the app, and the comment on
	// WebhookURL above explains why the outbound side is not: a notice is one
	// message with no state, a conversation is not.
	TelegramBotToken string `json:"telegram_bot_token"`

	// TelegramAllowedChats is who may talk to it — Telegram chat ids, which for
	// a private chat are the person's own user id. There is no wildcard and no
	// default: a bot's username is public and searchable, and this agent runs
	// shell commands, so an empty list refuses to start the poller rather than
	// starting it open to whoever finds the bot.
	TelegramAllowedChats []int64 `json:"telegram_allowed_chats"`

	// Channels is where notices go and where messages come in from: Telegram,
	// Feishu, WeCom, DingTalk, Slack, Bark, generic webhooks (channel.go). The
	// four fields above mirror its first webhook and first Telegram channel
	// for clients that predate it. Also kept in channels.json.
	Channels []Channel `json:"channels"`
	// channelLegacy is what the four fields above said when the channels
	// were last settled, so a save can tell which side changed.
	channelLegacy *channelLegacy

	// ExternalAgents lets SuperLeo hand a task to an agent CLI installed on
	// this machine. Nested rather than flattened into a dozen top-level keys
	// so the whole feature is one object a person can read, delete or diff in
	// settings.json — and so nothing here can be reached by a settings key the
	// agent is allowed to write (see settingsWritable in selftools.go).
	ExternalAgents ExternalAgents `json:"external_agents"`
	// Onboarded is set once the desktop app's first start has looked for a
	// signed-in coding CLI to think with (app/first_run.go), so it looks once
	// and what the person sets afterwards stands.
	Onboarded bool `json:"onboarded,omitempty"`

	// Hive makes this instance a queen or a worker in one — see hive.go.
	// The zero value is a standalone install.
	Hive HiveSettings `json:"hive"`

	// RemoteAgents lets SuperLeo ask an agent on another machine — see
	// remoteagents.go. Separate from ExternalAgents because the two differ in
	// what they can hurt: one spends a subscription on this laptop, the other
	// runs commands on a cluster.
	RemoteAgents RemoteAgents `json:"remote_agents"`

	// ActionMCPServers are the MCP servers a button in a chat block may call
	// directly, without a model turn (CallTool). Unset means just noted. Each
	// one listed hands every client holding the token a way to run its tools,
	// so the list is short on purpose; an empty list (not unset) turns
	// direct calls off.
	ActionMCPServers *[]string `json:"action_mcp_servers,omitempty"`

	// APNs is how this instance reaches Apple's push service, for the silent
	// push that tells the iOS app its alarms changed. See PushSettings.
	APNs APNsSettings `json:"apns,omitzero"`

	// QueenName is what the person calls the queen, shown by every client and
	// told to the model; QueenAliases are other names she answers to. Only
	// what people see changes: the role, the agent and the RPCs stay "queen".
	// See queen_name.go.
	QueenName    string   `json:"queen_name,omitempty"`
	QueenAliases []string `json:"queen_aliases,omitempty"`

	// AgentLink is how core treats the agents that dial in: which of them are
	// relays rather than machines. See agentlink_settings.go.
	AgentLink AgentLinkSettings `json:"agent_link,omitzero"`
	// HA is where core learns about HA services (one active copy moving
	// between hosts behind a service IP), like openclaw kept up by haify.
	HA HASettings `json:"ha,omitzero"`
}

// Defaults for APNsSettings: the developer team and the SuperLeo iOS app.
// The topic is the app's bundle id as registered with Apple, which kept its
// superai name through the rename: a push to any other topic is refused.
const (
	DefaultAPNsTeamID = "NUDL4XSA6J"
	DefaultAPNsTopic  = "cn.superleo.superai.ios"
)

// APNsSettings configures silent pushes to the paired iOS app. Every field
// falls back to an environment variable. The key itself can also arrive as
// PEM text in APNS_KEY (from a Secret); that text is never written to
// settings.json, which the settings page reads back.
type APNsSettings struct {
	// KeyPath is the .p8 file (APNS_KEY_PATH).
	KeyPath string `json:"key_path,omitempty"`
	// KeyID is the key's id from the developer account (APNS_KEY_ID).
	KeyID string `json:"key_id,omitempty"`
	// TeamID (APNS_TEAM_ID) defaults to DefaultAPNsTeamID.
	TeamID string `json:"team_id,omitempty"`
	// Topic is the app's bundle id (APNS_TOPIC), DefaultAPNsTopic by default.
	Topic string `json:"topic,omitempty"`
	// Env is "sandbox" or "production" (APNS_ENV), for a device that did not
	// say which build it runs. Production when unset.
	Env string `json:"env,omitempty"`
}

// PushSettings is the APNs configuration with the environment and defaults
// applied, and the key's PEM text when APNS_KEY carries it. Never log keyPEM.
func (s *Settings) PushSettings() (cfg APNsSettings, keyPEM string) {
	if s != nil {
		cfg = s.APNs
	}
	pick := func(v *string, env, def string) {
		*v = firstNonEmpty(*v, os.Getenv(env), def)
	}
	pick(&cfg.KeyPath, "APNS_KEY_PATH", "")
	pick(&cfg.KeyID, "APNS_KEY_ID", "")
	pick(&cfg.TeamID, "APNS_TEAM_ID", DefaultAPNsTeamID)
	pick(&cfg.Topic, "APNS_TOPIC", DefaultAPNsTopic)
	pick(&cfg.Env, "APNS_ENV", "production")
	cfg.KeyPath = expandHome(cfg.KeyPath)
	return cfg, strings.TrimSpace(os.Getenv("APNS_KEY"))
}

// ActionServers is the effective allowlist for direct MCP calls.
func (s *Settings) ActionServers() []string {
	if s == nil || s.ActionMCPServers == nil {
		return []string{NotedServer}
	}
	return append([]string(nil), (*s.ActionMCPServers)...)
}

// ExternalAgents configures handing work to agent CLIs installed on this
// machine. Off by default: it spends money through someone else's
// subscription and writes files with a bypassed approval prompt.
type ExternalAgents struct {
	Enabled bool `json:"enabled"`
	// Roots bounds the directories a delegated run may work in. Empty means
	// the workspace only.
	Roots []string `json:"roots,omitempty"`
	// Unattended lets a delegated run through this app's approval gate
	// without asking. Meant for scheduled work with nobody watching; every
	// call it lets through is still audited.
	Unattended bool `json:"unattended"`
	// Binaries overrides where an agent's CLI lives, by name.
	Binaries map[string]string `json:"binaries,omitempty"`
	// Disabled names CLIs that are installed but not offered: not listed,
	// not linked to core, not run.
	Disabled []string `json:"disabled,omitempty"`
	// Models overrides which model an agent is asked for, by name.
	Models map[string]string `json:"models,omitempty"`
	// TimeoutSeconds bounds one delegated run. Zero takes the default.
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
	// Custom are the coding tools a person added by command (codingtool.go).
	Custom []CodingTool `json:"custom,omitempty"`
}

// Timeout is how long one delegated run may take: TimeoutSeconds, or zero —
// no limit — when it is not set. A run handed to Claude Code is a task, and it
// ends when the task is done or someone stops it; a twenty-minute default
// killed the hive's development runs half way through.
func (e ExternalAgents) Timeout() time.Duration {
	if e.TimeoutSeconds <= 0 {
		return 0
	}
	return time.Duration(e.TimeoutSeconds) * time.Second
}

// WithLimit is ctx bounded by d, or only cancellable when d is zero: no limit.
func WithLimit(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, d)
}

// Binary returns the configured path to an agent's CLI, or the bare name for
// PATH lookup. A GUI app launched from Finder inherits a very short PATH — not
// the one the login shell builds — so a CLI that works in a terminal can be
// invisible here, and the override is how that gets fixed without asking
// anyone to relaunch the app from a shell.
func (e ExternalAgents) Binary(name string) string {
	if v := strings.TrimSpace(e.Binaries[name]); v != "" {
		return expandHome(v)
	}
	return name
}

// Model returns the configured model for an agent, or "" to let the CLI pick
// its own default.
func (e ExternalAgents) Model(name string) string {
	return strings.TrimSpace(e.Models[name])
}

// ExternalAgentRoots is the set of directories a delegated run may work in.
//
// Empty roots mean the workspace, not the whole disk: an unconfigured feature
// that would let another agent write anywhere is a footgun handed out by
// default. The workspace is always included even when roots are named, because
// a delegated run still reports back through the same deliverable directory
// every other tool writes to.
func (s *Settings) ExternalAgentRoots() []string {
	out := []string{}
	seen := map[string]bool{}
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" {
			return
		}
		p = expandHome(p)
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	add(s.WorkspaceDir)
	for _, r := range s.ExternalAgents.Roots {
		add(r)
	}
	return out
}

// normalize drops what a hand-edited or UI-written settings file leaves
// behind: blank rows from an "add root" button nobody filled in, and a
// negative timeout, which would otherwise mean "already expired".
func (e *ExternalAgents) normalize() {
	roots := e.Roots[:0]
	for _, r := range e.Roots {
		if r = strings.TrimSpace(r); r != "" {
			roots = append(roots, r)
		}
	}
	e.Roots = roots
	if len(e.Roots) == 0 {
		e.Roots = nil
	}
	e.Binaries = trimmedPairs(e.Binaries)
	e.Models = trimmedPairs(e.Models)
	e.Custom = normalizeCodingTools(e.Custom)
	if e.TimeoutSeconds < 0 {
		e.TimeoutSeconds = 0
	}
}

// trimmedPairs drops entries whose value is blank. The settings UI writes a
// key the moment a field is focused, so an override someone typed and then
// cleared would otherwise persist as "" and shadow the real binary.
func trimmedPairs(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := map[string]string{}
	for k, v := range m {
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if k != "" && v != "" {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Memory backend choices for Settings.MemoryBackend.
const (
	MemoryBackendLocal  = "local"
	MemoryBackendShared = "shared"
)

// UseSharedMemory reports whether the shared brain is the configured backend
// and reachable (an endpoint is known).
func (s *Settings) UseSharedMemory() bool {
	return s.MemoryBackend == MemoryBackendShared && s.SharedMemoryEndpointResolved() != ""
}

// SharedMemoryEndpointResolved returns the configured endpoint, falling back to
// $CORTEXDB_REMOTE.
func (s *Settings) SharedMemoryEndpointResolved() string {
	if v := strings.TrimSpace(s.SharedMemoryEndpoint); v != "" {
		return v
	}
	return strings.TrimSpace(os.Getenv("CORTEXDB_REMOTE"))
}

// SharedMemoryTokenResolved returns the configured token, falling back to
// $CORTEXDB_GRPC_TOKEN. Never log the result.
func (s *Settings) SharedMemoryTokenResolved() string {
	if v := strings.TrimSpace(s.SharedMemoryToken); v != "" {
		return v
	}
	return strings.TrimSpace(os.Getenv("CORTEXDB_GRPC_TOKEN"))
}

const (
	defaultMaxRds  = 40
	defaultAvatarP = 47615
	// DefaultCLIProxyPort is deliberately off the well-known range to avoid
	// colliding with dev servers.
	DefaultCLIProxyPort = 43517
)

// DataDir returns the SuperLeo data directory (~/.superleo),
// honoring the SUPERLEO_HOME environment override.
func DataDir() string {
	if v := strings.TrimSpace(os.Getenv("SUPERLEO_HOME")); v != "" {
		return expandHome(v)
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ".superleo"
	}
	return filepath.Join(home, ".superleo")
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[2:])
		}
	}
	return p
}

func settingsPath() string { return filepath.Join(DataDir(), "settings.json") }

// defaults returns a Settings populated with built-in defaults and environment
// fallbacks (LLM_BASE / LLM_KEY / LLM_MODEL, EMBED_BASE / EMBED_KEY /
// EMBED_MODEL). Out of the box SuperLeo runs on the embedded CLI proxy, so no
// cloud endpoint or API key is assumed.
func defaults() *Settings {
	s := &Settings{
		LLMBaseURL:      envOr("LLM_BASE", ""),
		LLMKey:          envOr("LLM_KEY", ""),
		LLMModel:        envOr("LLM_MODEL", ""),
		EmbedBaseURL:    envOr("EMBED_BASE", ""),
		EmbedKey:        envOr("EMBED_KEY", ""),
		EmbedModel:      envOr("EMBED_MODEL", ""),
		SearchBaseURL:   envOr("SEARCH_BASE", ""),
		SearchKey:       envOr("SEARCH_KEY", ""),
		SearchModel:     envOr("SEARCH_MODEL", ""),
		SearchNative:    envOr("SEARCH_NATIVE", ""),
		WorkspaceDir:    filepath.Join(DataDir(), "workspace"),
		MaxRounds:       defaultMaxRds,
		Headless:        true,
		AvatarPort:      defaultAvatarP,
		CLIProxyEnabled: true,
		CLIProxyPort:    DefaultCLIProxyPort,
		// Local memory is the default: a fresh install must not depend on a
		// server somewhere else on the network.
		MemoryBackend:         MemoryBackendLocal,
		SharedMemoryEndpoint:  envOr("CORTEXDB_REMOTE", ""),
		SharedMemoryNamespace: "default",
		QueenName:             DefaultQueenName,
	}
	return s
}

// LoadSettings reads settings.json, falling back to defaults when the file is
// missing. Any zero-valued fields are backfilled with defaults so older files
// keep working after upgrades.
func LoadSettings() (*Settings, error) {
	s, err := loadSettingsFrom(settingsPath())
	if s != nil {
		s.mergeLinked(LoadLinkedSuperLeos())
		s.mergeQueenIdentity()
		s.mergeAgentLink()
		s.mergeQueenEngine()
		s.mergeChannels()
	}
	return s, err
}

// linkedPath holds the other SuperLeos linked from Settings › Other SuperLeos.
// A file of its own because settings.json is not always this app's to keep:
// in the hive's containers the entrypoint rewrites it from a Secret on every
// start, and a link made in the page would be gone after the next restart.
func linkedPath() string { return filepath.Join(DataDir(), "linked.json") }

// LoadLinkedSuperLeos reads the links; none is an empty map.
func LoadLinkedSuperLeos() map[string]RemoteAgent {
	out := map[string]RemoteAgent{}
	if raw, err := os.ReadFile(linkedPath()); err == nil {
		_ = json.Unmarshal(raw, &out)
	}
	return out
}

// SaveLinkedSuperLeos writes the links, readable by this user only: each
// carries a device key.
func SaveLinkedSuperLeos(links map[string]RemoteAgent) error {
	raw, err := json.MarshalIndent(links, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(linkedPath(), raw, 0o600)
}

// mergeLinked adds the links to Remote agents. A name settings.json already
// gives an agent keeps that one: what an operator wrote wins.
func (s *Settings) mergeLinked(links map[string]RemoteAgent) {
	if len(links) == 0 {
		return
	}
	if s.RemoteAgents.Agents == nil {
		s.RemoteAgents.Agents = map[string]RemoteAgent{}
	}
	for name, r := range links {
		if _, ok := s.RemoteAgents.Agents[name]; !ok {
			s.RemoteAgents.Agents[name] = r
		}
	}
	s.RemoteAgents.Enabled = true
}

// loadSettingsFrom is LoadSettings against a named file, for callers that
// look at a home other than the running app's.
func loadSettingsFrom(path string) (*Settings, error) {
	def := defaults()
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return def, nil
		}
		return def, err
	}
	s := *def
	if err := json.Unmarshal(raw, &s); err != nil {
		return def, err
	}
	s.backfill(def)
	return &s, nil
}

func (s *Settings) backfill(def *Settings) {
	if strings.TrimSpace(s.LLMBaseURL) == "" {
		s.LLMBaseURL = def.LLMBaseURL
	}
	if strings.TrimSpace(s.LLMModel) == "" {
		s.LLMModel = def.LLMModel
	}
	if strings.TrimSpace(s.EmbedBaseURL) == "" {
		s.EmbedBaseURL = def.EmbedBaseURL
	}
	if strings.TrimSpace(s.EmbedModel) == "" {
		s.EmbedModel = def.EmbedModel
	}
	if strings.TrimSpace(s.WorkspaceDir) == "" {
		s.WorkspaceDir = def.WorkspaceDir
	}
	// -1 is agent.UnlimitedRounds and is a choice; 0 is an unset field and
	// anything below -1 is a typo. Only the last two fall back.
	if s.MaxRounds == 0 || s.MaxRounds < agent.UnlimitedRounds {
		s.MaxRounds = def.MaxRounds
	}
	if s.AvatarPort <= 0 {
		s.AvatarPort = def.AvatarPort
	}
	if s.CLIProxyPort <= 0 {
		s.CLIProxyPort = def.CLIProxyPort
	}
	s.registerWindow()
	// An older settings file has no memory_backend; it was on local memory, so
	// that is what it stays on.
	if s.MemoryBackend != MemoryBackendShared {
		s.MemoryBackend = MemoryBackendLocal
	}
	if strings.TrimSpace(s.SharedMemoryEndpoint) == "" {
		s.SharedMemoryEndpoint = def.SharedMemoryEndpoint
	}
	if strings.TrimSpace(s.SharedMemoryNamespace) == "" {
		s.SharedMemoryNamespace = def.SharedMemoryNamespace
	}
	// Nothing here is backfilled from defaults. External agents are off in the
	// zero value, which is what a settings file written before this existed
	// unmarshals to, and an upgrade must not switch on a feature that spends
	// money — the same reasoning DisableToolApproval uses, from the other
	// direction.
	s.ExternalAgents.normalize()
	// Remote agents get their catalogue backfilled but not their switch: a
	// file written before this existed knows nothing about pi or hermes, and
	// filling the list in costs nothing while Enabled stays false.
	s.RemoteAgents.backfill()
	s.RemoteAgents.normalize()
	s.normalizeQueen()
}

// Save writes the settings to ~/.superleo/settings.json (creating the
// data directory if needed).
// DefaultLLMContextTokens is the window assumed when the settings state none.
const DefaultLLMContextTokens = 1_000_000

// registerWindow tells agent-go how much the brain's model holds: what the
// settings say, DefaultLLMContextTokens when they say nothing.
func (s *Settings) registerWindow() {
	model := strings.TrimSpace(s.LLMModel)
	if model == "" {
		return
	}
	window := s.LLMContextTokens
	if window <= 0 {
		window = DefaultLLMContextTokens
	}
	pool.RegisterModelWindow(model, pool.ModelWindow{
		ContextTokens:   window,
		MaxOutputTokens: s.LLMMaxOutputTokens,
	})
}

func (s *Settings) Save() error {
	if err := os.MkdirAll(DataDir(), 0o755); err != nil {
		return err
	}
	// On the way in as well as on the way out. The settings page appends an
	// empty row the moment "Add a directory" is pressed, and a file full of
	// blank roots is a file nobody can read to answer "where may it write".
	s.ExternalAgents.normalize()
	s.RemoteAgents.normalize()
	s.normalizeQueen()
	s.AgentLink.normalize()
	s.prepareChannels()
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(settingsPath(), raw, 0o644); err != nil {
		return err
	}
	if err := s.saveQueenIdentity(); err != nil {
		return err
	}
	if err := s.saveQueenEngine(); err != nil {
		return err
	}
	if err := s.saveChannels(); err != nil {
		return err
	}
	return s.saveAgentLink()
}

// WebSearch returns the endpoint the built-in web_search tool should use,
// defaulting to the brain's own endpoint field by field. Returning the brain is
// deliberate: it is the one endpoint SuperLeo is guaranteed to have credentials
// for, and a search-grounded model behind it answers a grounded request. A
// dedicated search endpoint is configured by filling the Search* fields (or
// SEARCH_BASE / SEARCH_KEY / SEARCH_MODEL).
// BrainSearchesItself reports whether the brain's own requests carry the
// provider's web search: Anthropic's API takes its search tool beside the
// function tools, so there the model searches in the same call instead of
// through a web_search tool of ours. search_native, when set, decides.
func (s *Settings) BrainSearchesItself() bool {
	if s == nil {
		return false
	}
	switch strings.TrimSpace(s.SearchNative) {
	case string(domain.NativeWebSearchAnthropic):
		return true
	case "":
		u, err := url.Parse(strings.TrimSpace(s.LLMBaseURL))
		return err == nil && strings.EqualFold(u.Hostname(), "api.anthropic.com") && strings.TrimSpace(s.SearchBaseURL) == ""
	}
	return false
}

// brainNativeSearch is what the brain declares to its provider: "anthropic"
// when it searches itself, otherwise undeclared, as it has always been.
func brainNativeSearch(s *Settings) string {
	if s.BrainSearchesItself() {
		return string(domain.NativeWebSearchAnthropic)
	}
	return ""
}

func (s *Settings) WebSearch() (baseURL, key, model string) {
	if s == nil {
		return "", "", ""
	}
	return firstNonEmpty(s.SearchBaseURL, s.LLMBaseURL),
		firstNonEmpty(s.SearchKey, s.LLMKey),
		firstNonEmpty(s.SearchModel, s.LLMModel)
}

// UseEmbeddings reports whether an embedder should be built (graph memory) or
// SuperLeo should fall back to file memory.
// Speech reports the endpoint, key and model to synthesise with, and whether
// there is one at all. A missing model means no: see the field comment.
func (s *Settings) Speech() (baseURL, key, model, voice string, ok bool) {
	if s == nil {
		return "", "", "", "", false
	}
	model = strings.TrimSpace(s.TTSModel)
	if model == "" {
		return "", "", "", "", false
	}
	baseURL = firstNonEmpty(s.TTSBaseURL, s.EmbedBaseURL)
	key = firstNonEmpty(s.TTSKey, s.EmbedKey)
	if baseURL == "" {
		return "", "", "", "", false
	}
	// "none" is how EmbedKey says "no embeddings"; inheriting it as a literal
	// bearer token would send the word none as a credential.
	if strings.TrimSpace(key) == "none" {
		key = ""
	}
	return baseURL, key, model, strings.TrimSpace(s.TTSVoice), true
}

// Recognition reports the endpoint, key, model and protocol to transcribe
// with, and whether there is one at all.
func (s *Settings) Recognition() (baseURL, key, model, api string, ok bool) {
	if s == nil {
		return "", "", "", "", false
	}
	model = strings.TrimSpace(s.STTModel)
	baseURL = firstNonEmpty(s.STTBaseURL, s.LLMBaseURL)
	if model == "" || baseURL == "" {
		return "", "", "", "", false
	}
	key = firstNonEmpty(s.STTKey, s.LLMKey)
	if strings.TrimSpace(key) == "none" {
		key = ""
	}
	api = strings.ToLower(strings.TrimSpace(s.STTAPI))
	if api != "chat" {
		api = "transcriptions"
	}
	return baseURL, key, model, api, true
}

func (s *Settings) UseEmbeddings() bool {
	k := strings.TrimSpace(s.EmbedKey)
	return k != "" && k != "none"
}

func envOr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}
