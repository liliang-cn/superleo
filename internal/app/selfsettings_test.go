package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/liliang-cn/superleo/internal/backend"
)

// secretSettings holds a secret in every place one can be, each containing
// "SECRET" so a leak anywhere is found by one search.
func secretSettings() *backend.Settings {
	return &backend.Settings{
		LLMBaseURL: "https://gw.example.invalid/v1?key=SECRET-q1", LLMKey: "sk-SECRET-llm", LLMModel: "m1",
		EmbedKey: "SECRET-embed", TTSKey: "SECRET-tts", SearchKey: "SECRET-search",
		SharedMemoryEndpoint: "cortex.example.invalid:50051", SharedMemoryToken: "SECRET-cortex", SharedMemoryNamespace: "ns1",
		WebhookURL: "https://hooks.slack.com/services/T0/B0/SECRET-slackpath", WebhookSecret: "SECRET-webhook",
		TelegramBotToken: "SECRET-telegram", TelegramAllowedChats: []int64{7},
		Proxy:     backend.ProxySettings{Mode: backend.ProxyModeManual, URL: "http://user:SECRET-proxypw@127.0.0.1:7890"},
		MaxRounds: 30,
		Channels: []backend.Channel{
			{ID: "c1", Type: backend.ChannelTelegram, BotToken: "SECRET-bot", AllowedChats: []int64{7}, Events: []string{"reminder"}},
			{ID: "c2", Type: backend.ChannelFeishu, URL: "https://open.feishu.cn/open-apis/bot/v2/hook/SECRET-feishu", Secret: "SECRET-sign"},
			{ID: "c3", Type: backend.ChannelWeCom, URL: "SECRET-wecomkey-693a91f6"},
			{ID: "c4", Type: backend.ChannelWebhook, URL: "https://recv.example.invalid/in", InboundToken: "SECRET-inbound"},
			{ID: "c5", Type: backend.ChannelBark, DeviceKey: "SECRET-bark"},
		},
		Hive: backend.HiveSettings{JoinURL: "https://queen.example.invalid", JoinToken: "SECRET-join", PeerToken: "SECRET-peer"},
		RemoteAgents: backend.RemoteAgents{Agents: map[string]backend.RemoteAgent{
			"box": {URL: "https://box.example.invalid", Token: "SECRET-remote", Env: map[string]string{"API_KEY": "SECRET-env"}},
		}},
		HA: backend.HASettings{Haify: backend.HaifySource{URL: "http://haify.example.invalid:9431", Token: "SECRET-haify"}},
	}
}

func noSecret(t *testing.T, where, out string) {
	t.Helper()
	if i := strings.Index(out, "SECRET"); i >= 0 {
		t.Errorf("%s shows a secret: …%s…", where, out[max(0, i-80):min(len(out), i+40)])
	}
}

// Nothing settings_get, settings_set or status returns — the whole
// picture, any section, any key, a refusal, a waiting request, the approval
// card — ever holds a secret.
func TestNoSecretInAnySelfToolOutput(t *testing.T) {
	t.Setenv("SUPERLEO_HOME", t.TempDir())
	app := NewApp()
	app.settings = secretSettings()
	ss := selfSession(t, app)

	out, isErr := callSelf(t, ss, "superleo_settings_get", nil)
	if isErr {
		t.Fatal(out)
	}
	noSecret(t, "settings_get", out)
	for _, sec := range selfSettingsSections() {
		out, isErr := callSelf(t, ss, "superleo_settings_get", map[string]any{"section": sec})
		if isErr {
			t.Errorf("section %s: %s", sec, out)
		}
		noSecret(t, "section "+sec, out)
	}
	for _, key := range []string{"llm_key", "llm_base_url", "proxy", "proxy.url", "channels", "channels.c1", "channels.c2.url",
		"channels.c3.url", "hive", "hive.join_token", "remote_agents", "remote_agents.agents.box.env", "ha", "webhook_url", "telegram_bot_token"} {
		out, _ := callSelf(t, ss, "superleo_settings_get", map[string]any{"key": key})
		noSecret(t, "key "+key, out)
	}
	out, _ = callSelf(t, ss, "superleo_status", nil)
	noSecret(t, "status", out)

	// Refusals do not repeat what was given.
	for _, bad := range []map[string]any{
		{"key": "proxy.url", "value": "http://u:SECRET-pw@nohostport"},
		{"key": "proxy", "value": map[string]any{"mode": "manual", "url": "socks9://SECRET-x:1"}},
		{"key": "channels", "value": []any{map[string]any{"id": "c9", "bot_token": "SECRET-new", "nonsense": 1}}},
		{"key": "llm_key", "value": map[string]any{"oops": "SECRET-obj"}},
		{"changes": map[string]any{"llm_key": "SECRET-ok", "max_rounds": -5}},
	} {
		out, isErr := callSelf(t, ss, "superleo_settings_set", bad)
		if !isErr {
			t.Errorf("accepted %v: %s", bad, out)
		}
		noSecret(t, "refusal", out)
	}
	// A boundary request, its card and its record show no secret either.
	out, isErr = callSelf(t, ss, "superleo_settings_set", map[string]any{"changes": map[string]any{
		"llm_key": "SECRET-newkey", "hive.join_token": "SECRET-newjoin", "llm_base_url": "https://u:SECRET-pw@gw2.example.invalid/v1",
	}})
	if isErr || !strings.Contains(out, "needs_approval") {
		t.Fatalf("boundary request: %v %s", isErr, out)
	}
	noSecret(t, "boundary request", out)
	id := requestID(t, out)
	waitFor(t, "the card", func() bool { return cardFor(app, id) != nil })
	b, _ := json.Marshal(cardFor(app, id))
	noSecret(t, "approval card", string(b))
	out, _ = callSelf(t, ss, "superleo_settings_get", map[string]any{"request": id})
	noSecret(t, "request", out)
	app.ResolveToolApproval(id, false)
}

func requestID(t *testing.T, out string) string {
	t.Helper()
	var res struct {
		NeedsApproval struct {
			ID string `json:"id"`
		} `json:"needs_approval"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil || res.NeedsApproval.ID == "" {
		t.Fatalf("no request id in %s", out)
	}
	return res.NeedsApproval.ID
}

func cardFor(app *App, id string) map[string]any {
	for _, c := range app.PendingToolApprovals() {
		if c["id"] == id {
			return c
		}
	}
	return nil
}

func settingsJSON(app *App) string {
	s := app.GetSettings()
	b, _ := json.Marshal(s)
	return string(b)
}

// Every boundary key asks the person and changes nothing until they answer —
// with the approval gate switched off too — and a no leaves it unchanged.
func TestEveryBoundaryKeyWaitsForApproval(t *testing.T) {
	t.Setenv("SUPERLEO_HOME", t.TempDir())
	app := NewApp()
	cfg := secretSettings()
	cfg.DisableToolApproval = true // the gate is off: boundary changes still ask
	cfg.TelegramBotToken, cfg.Channels = "", nil
	app.settings = cfg
	ss := selfSession(t, app)

	samples := map[string]any{
		"llm_base_url": "https://llm.example.invalid/v1", "llm_key": "sk-NEW-1", "search_base_url": "https://s.example.invalid",
		"search_key": "sk-NEW-2", "searxng_url": "https://sx.example.invalid", "tts_base_url": "https://t.example.invalid", "tts_key": "sk-NEW-3", "stt_base_url": "https://s.example.invalid", "stt_key": "sk-NEW-4",
		"desktop_control": true, "desktop_blocked_apps": []any{"Mail"}, "shared_memory_endpoint": "c2.example.invalid:1",
		"shared_memory_token": "sk-NEW-4", "embed_base_url": "https://e.example.invalid", "embed_key": "sk-NEW-5",
		"external_agents.unattended": true, "external_agents.roots": []any{"/tmp"}, "external_agents.binaries": map[string]any{"claude": "/bin/echo"},
		"external_agents.custom": []any{}, "remote_agents.agents": map[string]any{"b2": map[string]any{"hosts": []any{"me@b2"}}},
		"hive.role": "queen", "hive.join_url": "https://q2.example.invalid", "hive.join_token": "sk-NEW-6",
		"local_hive": map[string]any{"enabled": true}, "channels": []any{},
		"webhook_secret": "sk-NEW-7", "telegram_bot_token": "sk-NEW-8", "telegram_allowed_chats": []any{42},
		"disable_tool_approval": true, "disable_self_install": false, "tool_approval_decider_url": "https://d.example.invalid",
		"tool_approval_decider_threshold": 0.5, "action_mcp_servers": []any{"noted"},
	}
	for _, k := range backend.SelfKeys() {
		if k.Tier != backend.TierNeedsApproval {
			continue
		}
		if !k.Virtual && !backend.IsSettingsPath(k.Key) {
			continue // a key whose field has not landed yet
		}
		if strings.Contains(k.Key, "*") {
			continue // one per bee: standing_update_test.go
		}
		v, ok := samples[k.Key]
		if !ok {
			t.Errorf("no sample value for boundary key %s", k.Key)
			continue
		}
		before := settingsJSON(app)
		out, isErr := callSelf(t, ss, "superleo_settings_set", map[string]any{"key": k.Key, "value": v})
		if isErr {
			t.Errorf("%s: %s", k.Key, out)
			continue
		}
		if strings.Contains(out, "NEW-") {
			t.Errorf("%s: the secret is repeated: %s", k.Key, out)
		}
		id := requestID(t, out)
		waitFor(t, "the card for "+k.Key, func() bool { return cardFor(app, id) != nil })
		if settingsJSON(app) != before {
			t.Errorf("%s changed before it was approved", k.Key)
		}
		app.ResolveToolApproval(id, false)
		waitFor(t, k.Key+" denied", func() bool { r, _ := app.selfRequest(id); return r.Status == reqDenied })
		if settingsJSON(app) != before {
			t.Errorf("%s changed though it was denied", k.Key)
		}
	}

	// Approved, they apply, together, and the outcome is readable.
	out, _ := callSelf(t, ss, "superleo_settings_set", map[string]any{"changes": map[string]any{
		"desktop_control": true, "llm_key": "sk-NEW-approved", "external_agents.roots": []any{"/tmp", "/srv"},
	}})
	id := requestID(t, out)
	waitFor(t, "the card", func() bool { return cardFor(app, id) != nil })
	if r := app.ResolveToolApproval(id, true); r != "ok" {
		t.Fatal(r)
	}
	waitFor(t, "applied", func() bool { r, _ := app.selfRequest(id); return r.Status == reqApplied })
	s := app.GetSettings()
	if !s.DesktopControl || s.LLMKey != "sk-NEW-approved" || len(s.ExternalAgents.Roots) != 2 || s.MaxRounds != 30 || s.SharedMemoryToken != "SECRET-cortex" {
		t.Fatalf("after approval: %+v", s)
	}
	out, _ = callSelf(t, ss, "superleo_settings_get", map[string]any{"request": id})
	if !strings.Contains(out, `"status":"applied"`) || strings.Contains(out, "sk-NEW") {
		t.Fatalf("request: %s", out)
	}
	// Narrowing applies at once: the gate back on, desktop control off.
	out, isErr := callSelf(t, ss, "superleo_settings_set", map[string]any{"changes": map[string]any{"disable_tool_approval": false, "desktop_control": false}})
	if isErr || strings.Contains(out, "needs_approval") {
		t.Fatalf("narrowing asked: %s", out)
	}
	if s := app.GetSettings(); s.DisableToolApproval || s.DesktopControl {
		t.Fatalf("narrowing did not apply: %+v", s)
	}
}

// A writable change applies at once, the proxy in use follows it, and
// nothing it does not name changes — not even what someone else saved a
// moment before.
func TestWritableChangesApplyAndTouchNothingElse(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SUPERLEO_HOME", home)
	t.Cleanup(func() { backend.ApplyProxy(backend.ProxySettings{}) })
	app := NewApp()
	cfg := secretSettings()
	cfg.Proxy = backend.ProxySettings{Mode: backend.ProxyModeManual, URL: "http://127.0.0.1:7890"}
	cfg.TelegramBotToken = "" // no bridge polling Telegram from a test
	app.settings = cfg
	ss := selfSession(t, app)

	// The proxy is visible, the motivating case.
	out, _ := callSelf(t, ss, "superleo_settings_get", nil)
	if !strings.Contains(out, `"proxy.url":"http://127.0.0.1:7890"`) {
		t.Fatalf("the proxy is not in settings_get: %s", out)
	}

	// Someone else (the settings page) saves a change of their own.
	page := app.GetSettings()
	page.MaxRounds = 77
	if err := app.SaveSettings(page); err != nil {
		t.Fatal(err)
	}

	out, isErr := callSelf(t, ss, "superleo_settings_set", map[string]any{"changes": map[string]any{
		"proxy.url": "http://127.0.0.1:43991", "browser_mode": "off", "hive.queen_engines": []any{map[string]any{"engine": "superleo", "enabled": true}},
		"channels.c1.events": []any{"failed"}, "llm_model": "m2",
	}})
	if isErr || !strings.Contains(out, `"applied"`) {
		t.Fatalf("settings_set: %v %s", isErr, out)
	}
	s := app.GetSettings()
	if s.Proxy.URL != "http://127.0.0.1:43991" || s.LLMModel != "m2" || ch(s, "c1").Events[0] != "failed" {
		t.Fatalf("not applied: %+v", s)
	}
	if s.MaxRounds != 77 || s.LLMKey != "sk-SECRET-llm" || ch(s, "c1").BotToken != "SECRET-bot" || s.Hive.JoinToken != "SECRET-join" ||
		s.RemoteAgents.Agents["box"].Token != "SECRET-remote" {
		t.Fatalf("an unnamed field changed: %+v", s)
	}
	if got := backend.CurrentProxyState().Using; got != "http://127.0.0.1:43991" {
		t.Fatalf("the proxy in use is %q", got)
	}
	out, _ = callSelf(t, ss, "superleo_settings_get", map[string]any{"section": "proxy"})
	if !strings.Contains(out, `"using":"http://127.0.0.1:43991"`) {
		t.Fatalf("proxy state: %s", out)
	}
	// And on disk.
	raw, err := os.ReadFile(filepath.Join(home, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var disk backend.Settings
	if err := json.Unmarshal(raw, &disk); err != nil || disk.MaxRounds != 77 || disk.Proxy.URL != "http://127.0.0.1:43991" || disk.LLMKey != "sk-SECRET-llm" {
		t.Fatalf("on disk: %v %+v", err, disk)
	}

	// All or nothing: one bad value and none applies.
	out, isErr = callSelf(t, ss, "superleo_settings_set", map[string]any{"changes": map[string]any{"llm_model": "m3", "browser_mode": "sometimes"}})
	if !isErr || app.GetSettings().LLMModel != "m2" {
		t.Fatalf("a bad batch applied: %v %s", isErr, out)
	}
}

// The old single-key form answers as it did, and the old flat fields are
// still in settings_get.
func TestSelfSettingsOldFormsStillWork(t *testing.T) {
	t.Setenv("SUPERLEO_HOME", t.TempDir())
	app := NewApp()
	app.settings = &backend.Settings{LLMModel: "m1", LLMKey: "k", MaxRounds: 30}
	ss := selfSession(t, app)
	out, isErr := callSelf(t, ss, "superleo_settings_set", map[string]any{"key": "max_rounds", "value": "60"})
	if isErr || !strings.Contains(out, `"key":"max_rounds"`) || app.GetSettings().MaxRounds != 60 {
		t.Fatalf("old form: %v %s", isErr, out)
	}
	out, _ = callSelf(t, ss, "superleo_settings_get", nil)
	var snap map[string]any
	if err := json.Unmarshal([]byte(out), &snap); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"llm_model", "llm_key_set", "max_rounds", "tool_approval_enabled", "writable_from_this_tool", "sections"} {
		if _, ok := snap[k]; !ok {
			t.Errorf("settings_get lost %s", k)
		}
	}
}

func ch(s backend.Settings, id string) backend.Channel {
	c, _ := backend.FindChannel(s.Channels, id)
	return c
}
