package backend

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The binary is only ever installed when it is the release that was pinned:
// a wrong body is refused and leaves nothing behind, and a binary already in
// place that no longer matches is fetched again rather than run.
func TestAgentBrowserIsInstalledOnlyWhenItMatchesItsDigest(t *testing.T) {
	asset := agentBrowserAsset()
	if asset == "" {
		t.Skip("no agent-browser release for this platform")
	}
	t.Setenv("SUPERLEO_HOME", t.TempDir())
	body := []byte("agent-browser test build")
	sum := sha256.Sum256(body)
	served := body
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/"+asset) {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(served)
	}))
	t.Cleanup(srv.Close)
	oldURL, oldDigest := agentBrowserURL, agentBrowserDigests[asset]
	agentBrowserURL = func(a string) string { return srv.URL + "/v" + AgentBrowserVersion + "/" + a }
	agentBrowserDigests[asset] = hex.EncodeToString(sum[:])
	t.Cleanup(func() { agentBrowserURL, agentBrowserDigests[asset] = oldURL, oldDigest })

	served = []byte("something else")
	if _, err := EnsureAgentBrowser(context.Background()); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("a mismatched download was accepted: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(AgentBrowserPath())); len(entries) != 0 {
		t.Fatalf("a refused download left %d file(s) behind", len(entries))
	}

	served = body
	path, err := EnsureAgentBrowser(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != string(body) {
		t.Fatalf("installed %q", got)
	}
	if runtime.GOOS != "windows" {
		if info, _ := os.Stat(path); info.Mode()&0o111 == 0 {
			t.Fatalf("installed without the execute bit: %v", info.Mode())
		}
	}

	if err := os.WriteFile(path, []byte("tampered"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureAgentBrowser(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != string(body) {
		t.Fatalf("a changed binary was kept: %q", got)
	}
}

// The browser is opt-in: unset, there is none on any platform.
func TestTheBrowserIsOffUntilChosen(t *testing.T) {
	t.Setenv("SUPERLEO_BROWSER_MODE", "")
	if got := (&Settings{}).browserMode(); got != BrowserModeOff {
		t.Fatalf("default = %q, want off", got)
	}
	for in, out := range map[string]string{"own": "own", " Chrome ": "chrome", "off": "off", "bogus": "off"} {
		if got := (&Settings{BrowserMode: in}).browserMode(); got != out {
			t.Errorf("%q → %q, want %q", in, got, out)
		}
	}
	t.Setenv("SUPERLEO_BROWSER_MODE", "off")
	if got := (&Settings{BrowserMode: "chrome"}).browserMode(); got != BrowserModeOff {
		t.Errorf("the environment did not win over the file: %q", got)
	}
}

// With the built-in browser mounted, a user's own agent-browser entry is left
// out of the config agent-go reads, and every other server stays.
func TestTheBuiltInBrowserReplacesAUsersAgentBrowser(t *testing.T) {
	t.Setenv("SUPERLEO_HOME", t.TempDir())
	src := filepath.Join(t.TempDir(), "mcpServers.json")
	raw := `{"mcpServers":{"agent-browser":{"command":"/opt/homebrew/bin/agent-browser","args":["mcp"]},` +
		`"web":{"command":"/usr/local/bin/agent-browser"},"noted":{"type":"http","url":"http://x/mcp"}}}`
	if err := os.WriteFile(src, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	lookup := func(string) (string, bool) { return "", false }

	browserBuiltin.Store(false)
	if got, err := resolveMCPConfig(src, lookup); err != nil || got != src {
		t.Fatalf("without a built-in browser the file is read as is: %q, %v", got, err)
	}

	browserBuiltin.Store(true)
	t.Cleanup(func() { browserBuiltin.Store(false) })
	got, err := resolveMCPConfig(src, lookup)
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		MCPServers map[string]any `json:"mcpServers"`
	}
	b, _ := os.ReadFile(got)
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.MCPServers["agent-browser"]; ok {
		t.Error("the user's agent-browser is still there")
	}
	if _, ok := f.MCPServers["web"]; ok {
		t.Error("an agent-browser under another name is still there")
	}
	if _, ok := f.MCPServers["noted"]; !ok {
		t.Error("an unrelated server was dropped")
	}
}

// In the person's own Chrome the browser works in a tab of its own and can
// neither quit Chrome nor close a tab: close sent there crashed it with a
// dozen tabs open. A browser of its own has none of those limits.
func TestThePersonsChromeCannotBeClosed(t *testing.T) {
	t.Setenv("SUPERLEO_HOME", t.TempDir())
	if err := os.MkdirAll(DataDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	s := &Service{gate: NewToolGate(false, filepath.Join(t.TempDir(), "audit.jsonl"))}
	b := &browserRuntime{mode: BrowserModeChrome, env: browserEnv(BrowserModeChrome)}
	s.guardPersonsChrome(b)
	for _, op := range []string{"close", "tab_close"} {
		if s.gate.forbiddenReason(BrowserTool(op)) == "" {
			t.Errorf("%s is not refused", op)
		}
	}
	if s.gate.forbiddenReason(BrowserTool("open")) != "" {
		t.Error("open is refused")
	}
	if b.env["AGENT_BROWSER_PIN_TAB"] != "1" || b.env["AGENT_BROWSER_AUTO_CONNECT"] != "1" {
		t.Errorf("env = %v", b.env)
	}
	raw, err := os.ReadFile(b.env["AGENT_BROWSER_ACTION_POLICY"])
	if err != nil || !strings.Contains(string(raw), `"tab_close"`) {
		t.Fatalf("policy = %s, %v", raw, err)
	}

	own := browserEnv(BrowserModeOwn)
	for _, k := range []string{"AGENT_BROWSER_PIN_TAB", "AGENT_BROWSER_AUTO_CONNECT", "AGENT_BROWSER_ACTION_POLICY"} {
		if _, ok := own[k]; ok {
			t.Errorf("a browser of its own has %s", k)
		}
	}
}

func TestAFailedBrowserCallSaysWhy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script stand-in")
	}
	t.Setenv("SUPERLEO_HOME", t.TempDir())
	bin := filepath.Join(t.TempDir(), "agent-browser")
	script := "#!/bin/sh\necho '{\"error\":\"All CDP discovery methods failed for 127.0.0.1:9222: Connection refused (os error 61)\",\"success\":false}'\nexit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	b := &browserRuntime{bin: bin, env: map[string]string{"AGENT_BROWSER_SESSION": "t"}, mode: BrowserModeChrome}
	err := b.ensureOwnTab(context.Background())
	if err == nil {
		t.Fatal("want an error")
	}
	got := err.Error()
	if !strings.Contains(got, "Connection refused") || strings.Contains(got, "exit status") || strings.Contains(got, `"success"`) {
		t.Fatalf("error does not carry agent-browser's reason: %q", got)
	}
}
