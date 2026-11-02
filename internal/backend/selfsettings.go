package backend

// Every setting, as the agent and the /mcp tools see and change it.
//
// settingsWritable (selftools.go) was a short whitelist, and everything off it
// could neither be read nor changed. The cost showed: the proxy was set and
// working, the queen could not see it, and told the person to go and set it.
//
// So now every key is readable, and every key is settable, in one of two
// tiers:
//
//   - writable: a preference. Validated and applied at once, the way the
//     settings page's save applies it.
//   - needs_approval: anything that widens what the agent may do or touches a
//     credential or an address data is sent to. A tool may ask for it; it is
//     applied only after the person says yes on an approval card, and that is
//     so whether or not the approval gate is on.
//
// The table below names the writable keys and describes the ones a model is
// likely to want. A settings path not in it is needs_approval: a field added
// later is never writable by accident.
//
// Secrets are never read back. A secret field reads "set" or "not set"; an
// address that can carry one (a proxy's user:password, a bot webhook whose
// path is its key) is shown without it.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
)

// Tiers of a settings key.
const (
	TierWritable      = "writable"
	TierNeedsApproval = "needs_approval"
)

// What a secret reads as.
const (
	SecretSet    = "set"
	SecretNotSet = "not set"
)

// SelfKey is one settings key as the self tools know it.
type SelfKey struct {
	// Key is the dotted JSON path ("proxy.url"); "*" stands for any one
	// segment (a channel's id, a map key).
	Key string `json:"key"`
	// Section is the settings_get section it is shown in.
	Section string `json:"section"`
	Tier    string `json:"tier"`
	Desc    string `json:"description"`
	// Virtual keys are not in settings.json: the app acts on them itself
	// (local_hive).
	Virtual bool `json:"virtual,omitempty"`

	// set replaces the generic setter, for a key with more to it than a
	// field (the queen's engine list keeps two legacy fields in step).
	set func(cfg *Settings, raw any) (any, error)
	// check validates the value after it is set; cfg is the whole result.
	check func(cfg *Settings) error
	// narrows says whether a change of this key only narrows what the agent
	// may do (approval back on, desktop control off): that needs nobody's
	// yes. Nil is never.
	narrows func(raw any) bool
}

// selfKeys is the table. Order is the order settings_set lists them in.
func selfKeys() []SelfKey {
	enum := func(key string, get func(*Settings) string, allowed ...string) func(*Settings) error {
		return func(cfg *Settings) error {
			v := get(cfg)
			for _, a := range allowed {
				if v == a {
					return nil
				}
			}
			return fmt.Errorf("%s must be one of %s", key, quoteList(allowed))
		}
	}
	rangeOf := func(key string, get func(*Settings) int, lo, hi int) func(*Settings) error {
		return func(cfg *Settings) error {
			if n := get(cfg); n < lo || n > hi {
				return errBadRange(key, lo, hi, n)
			}
			return nil
		}
	}
	nonNeg := func(key string, get func(*Settings) int) func(*Settings) error {
		return rangeOf(key, get, 0, 1<<30)
	}
	isFalse := func(raw any) bool { b, err := strictBool(raw); return err == nil && !b }
	isTrue := func(raw any) bool { b, err := strictBool(raw); return err == nil && b }

	w, ap := TierWritable, TierNeedsApproval
	return []SelfKey{
		// --- model ---
		{Key: "llm_model", Section: "model", Tier: w, Desc: "The chat model, as the provider (or the signed-in accounts) name it."},
		{Key: "llm_reasoning_effort", Section: "model", Tier: w, Desc: `reasoning_effort sent on every call: "", "low", "medium" or "high".`,
			check: enum("llm_reasoning_effort", func(c *Settings) string { return c.LLMReasoningEffort }, "", "minimal", "low", "medium", "high")},
		{Key: "llm_context_tokens", Section: "model", Tier: w, Desc: "The model's context window in tokens; 0 takes the default.",
			check: nonNeg("llm_context_tokens", func(c *Settings) int { return c.LLMContextTokens })},
		{Key: "llm_max_output_tokens", Section: "model", Tier: w, Desc: "Most tokens one response may use; 0 takes the default.",
			check: nonNeg("llm_max_output_tokens", func(c *Settings) int { return c.LLMMaxOutputTokens })},
		{Key: "cliproxy_enabled", Section: "model", Tier: w, Desc: "Model source: true thinks with the signed-in accounts (Claude Code / Codex / Gemini subscriptions), false with the custom API (llm_base_url + llm_key)."},
		{Key: "cliproxy_port", Section: "model", Tier: w, Desc: "Loopback port of the signed-in accounts' proxy (1024-65535).",
			check: rangeOf("cliproxy_port", func(c *Settings) int { return c.CLIProxyPort }, 1024, 65535)},
		{Key: "search_model", Section: "model", Tier: w, Desc: "Model web_search asks; empty uses the brain's."},
		{Key: "search_native", Section: "model", Tier: w, Desc: `Which field turns search on at the search endpoint: "", google_search, openai, dashscope, anthropic or none.`,
			check: enum("search_native", func(c *Settings) string { return c.SearchNative }, "", "google_search", "openai", "dashscope", "anthropic", "none")},
		{Key: "tts_model", Section: "model", Tier: w, Desc: "Speech model for reading answers aloud."},
		{Key: "tts_voice", Section: "model", Tier: w, Desc: "Speech voice."},
		{Key: "stt_model", Section: "model", Tier: w, Desc: "Model that transcribes the phone's recordings in server recognition mode."},
		{Key: "stt_api", Section: "model", Tier: w, Desc: `How stt_model is called: "transcriptions" (/audio/transcriptions) or "chat" (audio in a chat message).`,
			check: enum("stt_api", func(c *Settings) string { return c.STTAPI }, "", "transcriptions", "chat")},
		{Key: "stt_base_url", Section: "model", Tier: ap, Desc: "Recognition endpoint; empty uses the brain's."},
		{Key: "stt_key", Section: "model", Tier: ap, Desc: "Recognition key (secret); empty uses the brain's."},
		{Key: "llm_base_url", Section: "model", Tier: ap, Desc: "The custom API's address: where every prompt is sent."},
		{Key: "llm_key", Section: "model", Tier: ap, Desc: "The custom API's key (secret)."},
		{Key: "search_base_url", Section: "model", Tier: ap, Desc: "web_search's endpoint."},
		{Key: "search_key", Section: "model", Tier: ap, Desc: "web_search's key (secret)."},
		{Key: "searxng_url", Section: "model", Tier: ap, Desc: "A self-hosted SearXNG for web search."},
		{Key: "tts_base_url", Section: "model", Tier: ap, Desc: "Speech endpoint."},
		{Key: "tts_key", Section: "model", Tier: ap, Desc: "Speech key (secret)."},

		// --- queen ---
		{Key: "hive.queen_engines", Section: "queen", Tier: w,
			Desc: `Who thinks for the queen, in the order tried: a list of {"engine","enabled"}; "superleo" is SuperLeo's own agent, "claude" a CLI here, "claude.<machine>" one on a linked machine. settings_get section queen lists the engines there are.`,
			set:  setQueenEngines},
		{Key: "hive.queen_engine", Section: "queen", Tier: w, Desc: "Legacy single engine (first of hive.queen_engines); prefer hive.queen_engines.",
			set: func(cfg *Settings, raw any) (any, error) {
				v := toStr(raw)
				fb := true
				if cfg.Hive.QueenFallback != nil {
					fb = *cfg.Hive.QueenFallback
				}
				cfg.Hive.PromoteQueenEngine(v, fb)
				return v, nil
			}},
		{Key: "hive.queen_fallback", Section: "queen", Tier: w, Desc: "Legacy: whether SuperLeo answers when the engine cannot; prefer hive.queen_engines.",
			set: func(cfg *Settings, raw any) (any, error) {
				b, err := strictBool(raw)
				if err != nil {
					return nil, fmt.Errorf("hive.queen_fallback: %v", err)
				}
				cfg.Hive.PromoteQueenEngine(cfg.Hive.QueenEngine, b)
				return b, nil
			}},
		{Key: "queen_name", Section: "queen", Tier: w, Desc: "What the person calls the queen (letters, digits, . _ -).",
			check: func(c *Settings) error { return ValidateQueenName(c.QueenName, c.QueenAliases) }},
		{Key: "queen_aliases", Section: "queen", Tier: w, Desc: "Other names she answers to (a list).",
			check: func(c *Settings) error { return ValidateQueenName(c.QueenName, c.QueenAliases) }},
		{Key: "hive.delegation", Section: "queen", Tier: w, Desc: `How much a queen hands to her workers: "max", "auto" or "off" ("" is the default).`,
			check: enum("hive.delegation", func(c *Settings) string { return strings.ToLower(c.Hive.Delegation) }, "", DelegationMax, DelegationAuto, DelegationOff)},
		{Key: "hive.delegate_timeout_seconds", Section: "queen", Tier: w, Desc: "Seconds one delegated piece of work may take on a worker; 0 is the default.",
			check: nonNeg("hive.delegate_timeout_seconds", func(c *Settings) int { return c.Hive.DelegateTimeoutSeconds })},
		{Key: "hive.max_concurrent_orders", Section: "queen", Tier: w, Desc: "Turns a worker runs at once; 0 is the default.",
			check: nonNeg("hive.max_concurrent_orders", func(c *Settings) int { return c.Hive.MaxConcurrentOrders })},

		// --- proxy ---
		{Key: "proxy", Section: "proxy", Tier: w, Desc: `The network proxy as {"mode","url"}.`,
			check: func(c *Settings) error { return c.Proxy.Validate() }},
		{Key: "proxy.mode", Section: "proxy", Tier: w, Desc: `"" follows the environment, "off" goes direct, "system" uses the system proxy, "manual" uses proxy.url.`,
			check: func(c *Settings) error { return c.Proxy.Validate() }},
		{Key: "proxy.url", Section: "proxy", Tier: w, Desc: "The manual proxy, e.g. http://127.0.0.1:7890 (http, https or socks5).",
			check: func(c *Settings) error { return c.Proxy.Validate() }},

		// --- browser ---
		{Key: "browser_mode", Section: "browser", Tier: w, Desc: `The built-in browser: "chrome" (the person's own Chrome), "own" (one it launches) or "off".`,
			check: enum("browser_mode", func(c *Settings) string { return c.BrowserMode }, "", BrowserModeChrome, BrowserModeOwn, BrowserModeOff)},
		{Key: "headless", Section: "browser", Tier: w, Desc: "Run the browser without a visible window."},

		// --- desktop ---
		{Key: "desktop_control", Section: "desktop", Tier: ap, Desc: "Let the agent operate this machine's desktop apps. Turning it off needs no approval.",
			narrows: isFalse},
		{Key: "desktop_blocked_apps", Section: "desktop", Tier: ap, Desc: "The apps desktop control leaves alone (a list); null puts the default list back.",
			set: setDesktopBlocked},

		// --- memory ---
		{Key: "memory_backend", Section: "memory", Tier: w, Desc: `"local" (this machine) or "shared" (the shared CortexDB at shared_memory_endpoint).`,
			check: enum("memory_backend", func(c *Settings) string { return c.MemoryBackend }, "", "local", MemoryBackendShared)},
		{Key: "shared_memory_namespace", Section: "memory", Tier: w, Desc: "Namespace inside the shared CortexDB."},
		{Key: "embed_model", Section: "memory", Tier: w, Desc: "The embedding model."},
		{Key: "shared_memory_endpoint", Section: "memory", Tier: ap, Desc: "host:port of the shared CortexDB: where memories are sent."},
		{Key: "shared_memory_token", Section: "memory", Tier: ap, Desc: "The shared CortexDB's token (secret)."},
		{Key: "embed_base_url", Section: "memory", Tier: ap, Desc: "The embedding endpoint."},
		{Key: "embed_key", Section: "memory", Tier: ap, Desc: "The embedding key (secret)."},

		// --- coding tools ---
		{Key: "external_agents.enabled", Section: "coding_tools", Tier: w, Desc: "Coding tools on/off: whether SuperLeo may hand work to the agent CLIs on this machine (each run still asks as the approval gate says)."},
		{Key: "external_agents.disabled", Section: "coding_tools", Tier: w, Desc: "Coding tools switched off one by one, by name (a list)."},
		{Key: "external_agents.models", Section: "coding_tools", Tier: w, Desc: "Model per coding tool, {name: model}."},
		{Key: "external_agents.models.*", Section: "coding_tools", Tier: w, Desc: "One coding tool's model."},
		{Key: "external_agents.timeout_seconds", Section: "coding_tools", Tier: w, Desc: "Seconds one delegated run may take; 0 is the default.",
			check: nonNeg("external_agents.timeout_seconds", func(c *Settings) int { return c.ExternalAgents.TimeoutSeconds })},
		{Key: "external_agents.unattended", Section: "coding_tools", Tier: ap, Desc: "Delegated runs skip their own approval prompts. Turning it off needs no approval.",
			narrows: isFalse},
		{Key: "external_agents.roots", Section: "coding_tools", Tier: ap, Desc: "Directories delegated runs may work in."},
		{Key: "external_agents.binaries", Section: "coding_tools", Tier: ap, Desc: "Which binary runs each coding tool."},
		{Key: "external_agents.custom", Section: "coding_tools", Tier: ap, Desc: "Coding tools added by command."},

		// --- remote agents ---
		{Key: "remote_agents.enabled", Section: "remote_agents", Tier: w, Desc: "Remote agents on/off: whether SuperLeo may ask the agents configured in remote_agents.agents."},
		{Key: "remote_agents.timeout_seconds", Section: "remote_agents", Tier: w, Desc: "Seconds one remote run may take; 0 is the default.",
			check: nonNeg("remote_agents.timeout_seconds", func(c *Settings) int { return c.RemoteAgents.TimeoutSeconds })},
		{Key: "remote_agents.agents", Section: "remote_agents", Tier: ap, Desc: "The remote agents themselves: hosts, commands, tokens."},
		{Key: "agent_link.relay_only", Section: "remote_agents", Tier: w, Desc: "Linked agents that are relays, not machines (a list of names).",
			set: func(cfg *Settings, raw any) (any, error) {
				return SetWritableSetting(cfg, "agent_link.relay_only", raw)
			}},

		// --- hive ---
		{Key: "hive.role", Section: "hive", Tier: ap, Desc: `This instance's part in a hive: "queen", "worker" or "" (standalone).`},
		{Key: "hive.join_url", Section: "hive", Tier: ap, Desc: "The queen a worker joins."},
		{Key: "hive.join_token", Section: "hive", Tier: ap, Desc: "The token a worker joins with (secret)."},
		{Key: "local_hive", Section: "hive", Tier: ap, Virtual: true, Desc: `This machine runs a hive of local workers: {"enabled", "min_workers", "max_workers"}.`},

		// --- bees ---
		// A bee acts on its own when it wakes: what it is for and what it
		// looks at are what it does unasked, so a change is the person's.
		{Key: "bees.*.goal", Section: "bees", Tier: ap, Virtual: true, Desc: "One bee's goal, what it keeps true, by the id standing_agents gives (standing_agent_update sets it)."},
		{Key: "bees.*.watch", Section: "bees", Tier: ap, Virtual: true, Desc: `One bee's scope, what it watches: a list, e.g. ["pods in namespace superleo"]; null clears it.`},

		// --- notifications ---
		{Key: "webhook_url", Section: "notifications", Tier: w, Desc: "Where a notice is POSTed (mirrors the first webhook channel). Blank turns it off."},
		{Key: "channels.*.enabled", Section: "notifications", Tier: w, Desc: "Switch one notification channel on or off, by its id."},
		{Key: "channels.*.events", Section: "notifications", Tier: w, Desc: `What one channel is sent: a list of "reminder", "schedule", "needs_you", "failed".`,
			check: checkChannelEvents},
		{Key: "channels.*.name", Section: "notifications", Tier: w, Desc: "One channel's label."},
		{Key: "channels", Section: "notifications", Tier: ap, Desc: "The whole channel list (adding or removing a channel). Secrets left as \"set\" keep their value."},
		{Key: "webhook_secret", Section: "notifications", Tier: ap, Desc: "The webhook's bearer token (secret)."},
		{Key: "telegram_bot_token", Section: "notifications", Tier: ap, Desc: "The Telegram bot's token (secret)."},
		{Key: "telegram_allowed_chats", Section: "notifications", Tier: ap, Desc: "Who may talk to SuperLeo on Telegram (chat ids)."},

		// --- approval ---
		{Key: "disable_tool_approval", Section: "approval", Tier: ap, Desc: "true turns the tool approval gate off. Turning the gate back on (false) needs no approval.",
			narrows: isFalse},
		{Key: "disable_self_install", Section: "approval", Tier: ap, Desc: "true withholds the install-MCP-server / install-skill tools. Setting it true needs no approval.",
			narrows: isTrue},
		{Key: "tool_approval_decider_url", Section: "approval", Tier: ap, Desc: "A service that judges a call's arguments for the gate."},
		{Key: "tool_approval_decider_threshold", Section: "approval", Tier: ap, Desc: "Probability at or above which the decider asks."},
		{Key: "action_mcp_servers", Section: "approval", Tier: ap, Desc: "MCP servers a chat block's button may call without a turn."},

		// --- budgets / workspace / privacy ---
		{Key: "max_rounds", Section: "budgets", Tier: w, Desc: fmt.Sprintf("Tool-call rounds per task (1-%d).", selfMaxRoundsCeiling),
			check: rangeOf("max_rounds", func(c *Settings) int { return c.MaxRounds }, 1, selfMaxRoundsCeiling)},
		{Key: "turn_max_tokens", Section: "budgets", Tier: w, Desc: "Token cap for one turn; 0 is none.",
			check: nonNeg("turn_max_tokens", func(c *Settings) int { return c.TurnMaxTokens })},
		{Key: "long_task_max_tokens", Section: "budgets", Tier: w, Desc: "Token cap for a long task; 0 is none.",
			check: nonNeg("long_task_max_tokens", func(c *Settings) int { return c.LongTaskMaxTokens })},
		{Key: "workspace_dir", Section: "workspace", Tier: w, Desc: "Where the agent reads and writes deliverable files."},
		{Key: "pii_redaction", Section: "privacy", Tier: w, Desc: "Strip personal data before sending to the model."},
		{Key: "disable_ptc", Section: "privacy", Tier: w, Desc: "Turn off programmatic tool calling."},
		{Key: "avatar_port", Section: "privacy", Tier: w, Desc: "Local port of the avatar bridge; 0 turns it off.",
			check: func(c *Settings) error {
				if n := c.AvatarPort; n != 0 && (n < 1024 || n > 65535) {
					return errBadRange("avatar_port", 1024, 65535, n)
				}
				return nil
			}},
	}
}

// SelfKeys is the table, for settings_get's keys section and the
// settings_set description.
func SelfKeys() []SelfKey { return selfKeys() }

// LookupSelfKey finds the entry governing key: the exact one, else the most
// specific pattern. ok is false when no entry names it (it is then
// needs_approval, if it is a settings path at all).
func LookupSelfKey(key string) (SelfKey, bool) {
	segs := strings.Split(key, ".")
	var best SelfKey
	bestScore := -1
	for _, k := range selfKeys() {
		ks := strings.Split(k.Key, ".")
		if len(ks) != len(segs) {
			continue
		}
		score, ok := 0, true
		for i := range ks {
			switch {
			case ks[i] == segs[i]:
				score += 2
			case ks[i] == "*":
				score++
			default:
				ok = false
			}
		}
		if ok && score > bestScore {
			best, bestScore = k, score
		}
	}
	return best, bestScore >= 0
}

// SelfKeyTier is key's tier, and whether this particular value only narrows
// what the agent may do (and so needs no approval even on a boundary key).
func SelfKeyTier(key string, raw any) string {
	k, ok := LookupSelfKey(key)
	if !ok {
		return TierNeedsApproval
	}
	if k.Tier == TierNeedsApproval && k.narrows != nil && k.narrows(raw) {
		return TierWritable
	}
	return k.Tier
}

// IsVirtualSelfKey reports a key the app acts on rather than a settings path.
func IsVirtualSelfKey(key string) bool {
	k, ok := LookupSelfKey(key)
	return ok && k.Virtual
}

// SetSettingKey changes one key of cfg in place: the key's own setter if it
// has one, else the field at that JSON path, with the value converted to the
// field's type. Validation of the whole result is CheckSettingKeys'. Nothing
// is saved.
//
// Values come from a model as whatever JSON it emitted, so "40" and 40 both
// mean forty. Secrets inside an object or list that read "set" (as
// settings_get showed them) keep the value they had.
func SetSettingKey(cfg *Settings, key string, raw any) (any, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, errors.New("no key given")
	}
	if k, ok := LookupSelfKey(key); ok {
		if k.Virtual {
			return nil, fmt.Errorf("%s is not a settings path", key)
		}
		if k.set != nil {
			return k.set(cfg, raw)
		}
	}
	segs := strings.Split(key, ".")
	if err := setPath(reflect.ValueOf(cfg).Elem(), segs, raw, key); err != nil {
		return nil, err
	}
	return raw, nil
}

// CheckSettingKeys runs the checks of every key given against cfg.
func CheckSettingKeys(cfg *Settings, keys []string) error {
	seen := map[string]bool{}
	for _, key := range keys {
		k, ok := LookupSelfKey(key)
		if !ok || k.check == nil || seen[k.Key] {
			continue
		}
		seen[k.Key] = true
		if err := k.check(cfg); err != nil {
			return err
		}
	}
	return nil
}

// IsSettingsPath reports whether key names a field of the settings ("*"
// or an id for an entry of a list or map).
func IsSettingsPath(key string) bool {
	_, err := settingsPathType(key)
	return err == nil
}

func settingsPathType(key string) (reflect.Type, error) {
	t := reflect.TypeOf(Settings{})
	for _, seg := range strings.Split(key, ".") {
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		switch t.Kind() {
		case reflect.Struct:
			f, ok := fieldByJSON(t, seg)
			if !ok {
				return nil, fmt.Errorf("unknown setting %q", key)
			}
			t = f.Type
		case reflect.Map:
			t = t.Elem()
		case reflect.Slice:
			if t.Elem().Kind() != reflect.Struct {
				return nil, fmt.Errorf("unknown setting %q", key)
			}
			t = t.Elem()
		default:
			return nil, fmt.Errorf("unknown setting %q", key)
		}
	}
	return t, nil
}

// --- the generic setter ---

func fieldByJSON(t reflect.Type, name string) (reflect.StructField, bool) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		tag := strings.Split(f.Tag.Get("json"), ",")[0]
		if tag == "-" {
			continue
		}
		if tag == "" {
			tag = f.Name
		}
		if tag == name {
			return f, true
		}
	}
	return reflect.StructField{}, false
}

// setPath sets the value at segs below v (addressable).
func setPath(v reflect.Value, segs []string, raw any, key string) error {
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			v.Set(reflect.New(v.Type().Elem()))
		}
		v = v.Elem()
	}
	if len(segs) == 0 {
		cv, err := convertTo(v.Type(), raw, key)
		if err != nil {
			return err
		}
		nv := reflect.New(v.Type()).Elem() // addressable, for keepRedacted
		nv.Set(cv)
		keepRedacted(nv, v, lastSeg(key), true)
		v.Set(nv)
		return nil
	}
	seg := segs[0]
	switch v.Kind() {
	case reflect.Struct:
		f, ok := fieldByJSON(v.Type(), seg)
		if !ok {
			return fmt.Errorf("unknown setting %q", key)
		}
		return setPath(v.FieldByIndex(f.Index), segs[1:], raw, key)
	case reflect.Map:
		if v.Type().Key().Kind() != reflect.String {
			return fmt.Errorf("unknown setting %q", key)
		}
		mk := reflect.ValueOf(seg).Convert(v.Type().Key())
		if len(segs) == 1 && raw == nil {
			if !v.IsNil() {
				v.SetMapIndex(mk, reflect.Value{})
			}
			return nil
		}
		elem := reflect.New(v.Type().Elem()).Elem()
		if !v.IsNil() {
			if cur := v.MapIndex(mk); cur.IsValid() {
				elem.Set(cur)
			}
		}
		if err := setPath(elem, segs[1:], raw, key); err != nil {
			return err
		}
		if v.IsNil() {
			v.Set(reflect.MakeMap(v.Type()))
		}
		v.SetMapIndex(mk, elem)
		return nil
	case reflect.Slice:
		// A list of objects is addressed by each one's id.
		for i := 0; i < v.Len(); i++ {
			if elemID(v.Index(i)) == seg {
				return setPath(v.Index(i), segs[1:], raw, key)
			}
		}
		if v.Type().Elem().Kind() == reflect.Struct {
			return fmt.Errorf("%s: no entry with id %q", strings.TrimSuffix(key, "."+strings.Join(segs, ".")), seg)
		}
	}
	return fmt.Errorf("unknown setting %q", key)
}

func lastSeg(key string) string {
	if i := strings.LastIndex(key, "."); i >= 0 {
		return key[i+1:]
	}
	return key
}

// elemID is a list entry's id: its "id" field, else "engine".
func elemID(v reflect.Value) string {
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return ""
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return ""
	}
	for _, name := range []string{"id", "engine"} {
		if f, ok := fieldByJSON(v.Type(), name); ok && f.Type.Kind() == reflect.String {
			return v.FieldByIndex(f.Index).String()
		}
	}
	return ""
}

// convertTo turns a model's value into a value of type t.
func convertTo(t reflect.Type, raw any, key string) (reflect.Value, error) {
	if raw == nil {
		return reflect.Zero(t), nil
	}
	switch t.Kind() {
	case reflect.String:
		switch raw.(type) {
		case map[string]any, []any:
			return reflect.Value{}, fmt.Errorf("%s: want text", key)
		}
		return reflect.ValueOf(toStr(raw)).Convert(t), nil
	case reflect.Bool:
		b, err := strictBool(raw)
		if err != nil {
			return reflect.Value{}, fmt.Errorf("%s: %v", key, err)
		}
		return reflect.ValueOf(b).Convert(t), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strictInt(raw)
		if err != nil {
			return reflect.Value{}, fmt.Errorf("%s: %v", key, err)
		}
		return reflect.ValueOf(n).Convert(t), nil
	case reflect.Float32, reflect.Float64:
		f, err := strictFloat(raw)
		if err != nil {
			return reflect.Value{}, fmt.Errorf("%s: %v", key, err)
		}
		return reflect.ValueOf(f).Convert(t), nil
	case reflect.Pointer:
		inner, err := convertTo(t.Elem(), raw, key)
		if err != nil {
			return reflect.Value{}, err
		}
		p := reflect.New(t.Elem())
		p.Elem().Set(inner)
		return p, nil
	case reflect.Slice:
		// "a, b" for a list of words is what a model writes as often as a list.
		if s, ok := raw.(string); ok && t.Elem().Kind() == reflect.String {
			out := reflect.MakeSlice(t, 0, 4)
			for _, part := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '\n' }) {
				if part = strings.TrimSpace(part); part != "" {
					out = reflect.Append(out, reflect.ValueOf(part).Convert(t.Elem()))
				}
			}
			return out, nil
		}
		if items, ok := raw.([]any); ok && isScalarKind(t.Elem().Kind()) {
			out := reflect.MakeSlice(t, 0, len(items))
			for _, it := range items {
				ev, err := convertTo(t.Elem(), it, key)
				if err != nil {
					return reflect.Value{}, err
				}
				out = reflect.Append(out, ev)
			}
			return out, nil
		}
	}
	// Objects and lists of objects: through JSON, refusing a field the
	// settings do not have, so a typo is an error rather than nothing.
	b, err := json.Marshal(raw)
	if err != nil {
		return reflect.Value{}, fmt.Errorf("%s: %v", key, err)
	}
	p := reflect.New(t)
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(p.Interface()); err != nil {
		return reflect.Value{}, fmt.Errorf("%s: %v", key, jsonErrWithoutValues(err))
	}
	return p.Elem(), nil
}

func isScalarKind(k reflect.Kind) bool {
	switch k {
	case reflect.String, reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Float32, reflect.Float64:
		return true
	}
	return false
}

// jsonErrWithoutValues keeps a decode error's shape and drops anything it
// quoted from the input.
func jsonErrWithoutValues(err error) error {
	var te *json.UnmarshalTypeError
	if errors.As(err, &te) {
		return fmt.Errorf("%s has the wrong type (want %s)", te.Field, te.Type)
	}
	msg := err.Error()
	if strings.HasPrefix(msg, "json: unknown field") {
		return errors.New(strings.TrimPrefix(msg, "json: "))
	}
	return errors.New("not the right shape for this setting")
}

// keepRedacted puts back, inside a value about to replace cur, every secret
// that came in as it is shown ("set", or an address shown without its
// secret): a model that edits a list it read cannot know the secrets in it,
// and must not wipe them by sending the list back.
//
// top is the key's own value: there an empty secret clears it, as asked;
// inside an object or list, an empty secret is one the model did not know.
func keepRedacted(nv, cur reflect.Value, name string, top bool) {
	switch nv.Kind() {
	case reflect.String:
		if !cur.IsValid() || cur.Kind() != reflect.String {
			return
		}
		got, was := nv.String(), cur.String()
		if was == "" || got == was {
			return
		}
		if got == RedactValue(name, was) || (!top && IsSecretName(name) && (got == SecretSet || got == "")) {
			nv.SetString(was)
		}
	case reflect.Pointer:
		if nv.IsNil() || !cur.IsValid() || cur.Kind() != reflect.Pointer || cur.IsNil() {
			return
		}
		keepRedacted(nv.Elem(), cur.Elem(), name, top)
	case reflect.Struct:
		if !cur.IsValid() || cur.Type() != nv.Type() {
			return
		}
		for i := 0; i < nv.NumField(); i++ {
			f := nv.Type().Field(i)
			if !f.IsExported() {
				continue
			}
			keepRedacted(nv.Field(i), cur.Field(i), strings.Split(f.Tag.Get("json"), ",")[0], false)
		}
	case reflect.Slice:
		if !cur.IsValid() || cur.Kind() != reflect.Slice {
			return
		}
		for i := 0; i < nv.Len(); i++ {
			id := elemID(nv.Index(i))
			for j := 0; j < cur.Len(); j++ {
				if (id != "" && elemID(cur.Index(j)) == id) || (id == "" && i == j) {
					keepRedacted(nv.Index(i), cur.Index(j), name, false)
					break
				}
			}
		}
	case reflect.Map:
		if !cur.IsValid() || cur.Kind() != reflect.Map || nv.IsNil() || cur.IsNil() {
			return
		}
		for _, mk := range nv.MapKeys() {
			old := cur.MapIndex(mk)
			if !old.IsValid() {
				continue
			}
			ev := reflect.New(nv.Type().Elem()).Elem()
			ev.Set(nv.MapIndex(mk))
			elemName := name
			if nv.Type().Elem().Kind() == reflect.String && secretMapNames[name] {
				// env: every value is treated as a secret.
				elemName = "token"
			}
			keepRedacted(ev, old, elemName, false)
			nv.SetMapIndex(mk, ev)
		}
	}
}

// --- special setters and checks ---

func setQueenEngines(cfg *Settings, raw any) (any, error) {
	v, err := convertTo(reflect.TypeOf([]QueenEngineEntry{}), raw, "hive.queen_engines")
	if err != nil {
		return nil, err
	}
	list := v.Interface().([]QueenEngineEntry)
	if len(list) == 0 {
		return nil, errors.New("hive.queen_engines: give at least one engine, e.g. [{\"engine\":\"superleo\",\"enabled\":true}]")
	}
	cfg.Hive.SetQueenChain(list)
	return cfg.Hive.QueenEngines, nil
}

func setDesktopBlocked(cfg *Settings, raw any) (any, error) {
	if raw == nil {
		cfg.DesktopBlockedApps = nil
		return nil, nil
	}
	v, err := convertTo(reflect.TypeOf([]string{}), raw, "desktop_blocked_apps")
	if err != nil {
		return nil, err
	}
	list, seen := []string{}, map[string]bool{}
	for _, app := range v.Interface().([]string) {
		app = strings.TrimSpace(app)
		if app == "" || seen[strings.ToLower(app)] {
			continue
		}
		seen[strings.ToLower(app)] = true
		list = append(list, app)
	}
	cfg.DesktopBlockedApps = list
	return list, nil
}

func checkChannelEvents(cfg *Settings) error {
	known := map[string]bool{}
	for _, e := range ChannelEvents {
		known[e] = true
	}
	for _, c := range cfg.Channels {
		for _, e := range c.Events {
			if !known[e] {
				return fmt.Errorf("channel %s: unknown event %q; use %s", c.ID, e, quoteList(ChannelEvents))
			}
		}
	}
	return nil
}

func quoteList(items []string) string {
	q := make([]string, len(items))
	for i, s := range items {
		q[i] = strconv.Quote(s)
	}
	return strings.Join(q, ", ")
}

// --- reading ---

// secretParts are the words that make a field name a secret's.
var secretParts = map[string]bool{
	"token": true, "secret": true, "password": true, "passwd": true, "key": true,
	"apikey": true, "cookie": true, "credential": true, "credentials": true, "pem": true,
	"private": true, "bearer": true, "auth": true, "authorization": true,
}

// notSecret are names with a secret word in them that hold none: a path to
// a key file, a key's public id.
var notSecret = map[string]bool{"key_path": true, "key_id": true, "token_file": true}

// secretMapNames are maps whose every value is treated as a secret.
var secretMapNames = map[string]bool{"env": true, "headers": true, "environment": true}

// IsSecretName reports whether a JSON field name holds a secret.
func IsSecretName(name string) bool {
	name = strings.ToLower(camelToSnake(strings.TrimSpace(name)))
	if notSecret[name] {
		return false
	}
	for _, part := range strings.FieldsFunc(name, func(r rune) bool { return r == '_' || r == '-' || r == '.' }) {
		if secretParts[part] {
			return true
		}
	}
	return false
}

// camelToSnake splits joinToken into join_Token, so a state struct's camel
// case names are judged like the settings' snake case ones.
func camelToSnake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if i > 0 && r >= 'A' && r <= 'Z' {
			b.WriteByte('_')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// hostOnlyNames are addresses whose path or query is itself the secret: a
// Slack, Feishu, WeCom or DingTalk bot's webhook, a generic webhook URL.
var hostOnlyNames = map[string]bool{"webhook_url": true, "url": true, "server": true}

// RedactValue is a string setting as it may be shown: a secret as set or not
// set, an address without its user:password and query, a bot webhook as its
// host alone.
func RedactValue(name, v string) string {
	if IsSecretName(name) {
		if strings.TrimSpace(v) == "" {
			return SecretNotSet
		}
		return SecretSet
	}
	if v == "" {
		return v
	}
	return redactAddress(name, v)
}

func redactAddress(name, v string) string {
	if !strings.Contains(v, "://") {
		if at := strings.LastIndex(v, "@"); at > 0 && strings.Contains(v[:at], ":") && !strings.ContainsAny(v, " ") {
			// user:pass@host:port with the scheme left off.
			return "…@" + v[at+1:]
		}
		// A bare key where a URL goes (WeCom's key, DingTalk's
		// access_token) reads as set; host:port is shown.
		if hostOnlyNames[strings.ToLower(name)] && !hostPort.MatchString(v) {
			return SecretSet
		}
		return v
	}
	u, err := url.Parse(v)
	if err != nil {
		return SecretSet
	}
	u.User = nil
	u.Fragment = ""
	if u.RawQuery != "" {
		u.RawQuery = ""
		u.ForceQuery = false
		v = u.String() + "?…"
	} else {
		v = u.String()
	}
	if hostOnlyNames[strings.ToLower(name)] && u.Path != "" && u.Path != "/" {
		return u.Scheme + "://" + u.Host + "/…"
	}
	return v
}

// hostPort is an address written without a scheme: a dotted host or one
// with a port.
var hostPort = regexp.MustCompile(`^(?:[A-Za-z0-9-]+\.[A-Za-z0-9.-]+|[A-Za-z0-9.-]+:\d{1,5}|\[[0-9a-fA-F:]+\](?::\d{1,5})?)(?::\d{1,5})?$`)

func isAddressValue(v string) bool { return strings.Contains(v, "://") }

// RedactedSettings is the whole of cfg as a model may see it: every key,
// secrets as set or not set, addresses without what they carry.
func RedactedSettings(cfg *Settings) map[string]any {
	if cfg == nil {
		return map[string]any{}
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return map[string]any{}
	}
	var doc map[string]any
	if json.Unmarshal(b, &doc) != nil {
		return map[string]any{}
	}
	out, _ := redactTree("", doc).(map[string]any)
	return out
}

// RedactTree redacts any JSON-shaped value the way RedactedSettings does,
// with name the key it sits under.
func RedactTree(name string, v any) any { return redactTree(name, v) }

func redactTree(name string, v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if secretMapNames[strings.ToLower(name)] {
				out[k] = redactSecretLeaf(val)
				continue
			}
			if IsSecretName(k) {
				out[k] = redactSecretLeaf(val)
				continue
			}
			out[k] = redactTree(k, val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = redactTree(name, val)
		}
		return out
	case string:
		if IsSecretName(name) {
			return RedactValue(name, t)
		}
		if isAddressValue(t) || hostOnlyNames[strings.ToLower(name)] || strings.Contains(t, "@") {
			return RedactValue(name, t)
		}
		return t
	}
	return v
}

// redactSecretLeaf is a secret field's value: set or not set, for a list or a
// map of them too.
func redactSecretLeaf(v any) any {
	switch t := v.(type) {
	case nil:
		return SecretNotSet
	case string:
		if strings.TrimSpace(t) == "" {
			return SecretNotSet
		}
		return SecretSet
	case []any, map[string]any:
		if reflect.ValueOf(t).Len() == 0 {
			return SecretNotSet
		}
		return SecretSet
	case bool, float64:
		// A flag or a number under a secret-sounding name holds nothing.
		return t
	}
	return SecretSet
}

// SettingsSection is the part of the redacted settings shown under a
// settings_get section: the top-level keys whose table entries are in it.
func SettingsSection(cfg *Settings, section string) map[string]any {
	all := RedactedSettings(cfg)
	out := map[string]any{}
	for _, k := range selfKeys() {
		if k.Section != section || k.Virtual {
			continue
		}
		path := strings.Split(k.Key, ".")
		if v, ok := lookupTree(all, path); ok {
			out[strings.Join(path, ".")] = v
		}
	}
	return out
}

func lookupTree(doc map[string]any, path []string) (any, bool) {
	var cur any = doc
	for _, seg := range path {
		if seg == "*" {
			return nil, false
		}
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[seg]; !ok {
			return nil, true // a key not set reads as nothing, not as missing
		}
	}
	return cur, true
}

// SelfKeySections are the section names, in order.
func SelfKeySections() []string {
	seen := map[string]bool{}
	var out []string
	for _, k := range selfKeys() {
		if !seen[k.Section] {
			seen[k.Section] = true
			out = append(out, k.Section)
		}
	}
	return out
}

// SelfKeyTable describes every key, by tier, for a tool's description:
// writable keys with what they take, boundary keys by name (their meaning is
// in settings_get's keys section), to keep a description every turn carries
// short.
func SelfKeyTable() string {
	var wr, ap []string
	for _, k := range selfKeys() {
		if k.Tier == TierWritable {
			wr = append(wr, k.Key+" — "+k.Desc)
		} else {
			ap = append(ap, k.Key)
		}
	}
	return "WRITABLE (applied at once):\n- " + strings.Join(wr, "\n- ") +
		"\n\nNEEDS APPROVAL (asked on a card; nothing changes until the person says yes): " + strings.Join(ap, ", ") +
		", and any other settings path. Turning the approval gate back on, desktop control off or unattended off applies at once."
}

// --- strict conversions ---

func strictBool(v any) (bool, error) {
	switch b := v.(type) {
	case bool:
		return b, nil
	case string:
		switch strings.ToLower(strings.TrimSpace(b)) {
		case "true", "on", "yes", "1":
			return true, nil
		case "false", "off", "no", "0", "":
			return false, nil
		}
	case float64:
		return b != 0, nil
	case int:
		return b != 0, nil
	}
	return false, errors.New("want true or false")
}

func strictInt(v any) (int64, error) {
	switch n := v.(type) {
	case float64:
		if n != float64(int64(n)) {
			return 0, errors.New("want a whole number")
		}
		return int64(n), nil
	case int:
		return int64(n), nil
	case int64:
		return n, nil
	case json.Number:
		return n.Int64()
	case string:
		i, err := strconv.ParseInt(strings.TrimSpace(n), 10, 64)
		if err != nil {
			return 0, errors.New("want a whole number")
		}
		return i, nil
	}
	return 0, errors.New("want a whole number")
}

func strictFloat(v any) (float64, error) {
	switch n := v.(type) {
	case float64:
		return n, nil
	case int:
		return float64(n), nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		if err != nil {
			return 0, errors.New("want a number")
		}
		return f, nil
	}
	return 0, errors.New("want a number")
}
