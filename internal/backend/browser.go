package backend

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The built-in browser.
//
// agent-go v3 has no browser of its own, so the model reaches the web through
// agent-browser (vercel-labs, Apache-2.0): a single Rust binary that drives
// Chrome over CDP, serves its tools over MCP, and streams the page it is on as
// JPEG frames over a WebSocket that also takes mouse and keyboard input. That
// last part is what lets a person watch a page being worked on from the
// conversation and take it over.
//
// The binary is fetched from the project's GitHub release for this platform
// and checked against the digest pinned here before it is ever run. It is
// not installed through npm or Homebrew: those wrap the same binary in a
// runtime that a clean machine may not have.

// AgentBrowserVersion is the agent-browser release SuperLeo runs.
const AgentBrowserVersion = "0.40.0"

// agentBrowserDigests are the release assets' SHA-256, as GitHub published
// them for v0.40.0. A download that does not match is discarded.
var agentBrowserDigests = map[string]string{
	"agent-browser-darwin-arm64":     "b064c9107a0431c76d8916e47c2c8ac322f49c2b2720d6c9036ab538ec1ed911",
	"agent-browser-darwin-x64":       "478cba056938c5cde286fbeea3994a8b8144f2e18f054684b93abb80c0340d25",
	"agent-browser-linux-arm64":      "f33b4dd9bd7707ad952125937f9de18f93548c5d51283b18092fe21ced6b9e53",
	"agent-browser-linux-musl-arm64": "3d4352c0b39f173896aa12af0e63d5905ac8ce7c95575fe6f1f93ce21f9b9e26",
	"agent-browser-linux-musl-x64":   "aeebf976eb429064efb10179ccdf737ec7d798c019bae7067ce2751c9c6e176a",
	"agent-browser-linux-x64":        "da26cc8029c004325b960fa955951063380cb61e622cc580fd5adf3aa89e4676",
	"agent-browser-win32-x64.exe":    "b142ca8946efda2d506e0947b232596e7949f08296e6b953b57f63eb50fee751",
}

// BrowserMCPName is the MCP server the browser is mounted as; its tools reach
// the model as mcp_browser_agent_browser_<op>.
const BrowserMCPName = "browser"

// Browser modes, the browser_mode setting.
const (
	// BrowserModeChrome works in the person's own running Chrome, with its
	// logins (agent-browser --auto-connect; Chrome 144+ lets it attach once
	// remote debugging is switched on at chrome://inspect/#remote-debugging).
	BrowserModeChrome = "chrome"
	// BrowserModeOwn runs a separate Chrome agent-browser launches itself.
	BrowserModeOwn = "own"
	// BrowserModeOff leaves the browser out.
	BrowserModeOff = "off"
)

// browserMode is what the environment or the settings ask for. The browser
// is opt-in: unset, there is none — it acts on the web with the person's own
// logins, and nothing is downloaded or run until they choose it.
func (s *Settings) browserMode() string {
	// SUPERLEO_BROWSER_MODE wins over the file: a test run, or one launch
	// that must not open a browser, says so without editing settings.
	for _, v := range []string{os.Getenv("SUPERLEO_BROWSER_MODE"), s.BrowserMode} {
		switch m := strings.ToLower(strings.TrimSpace(v)); m {
		case BrowserModeChrome, BrowserModeOwn, BrowserModeOff:
			return m
		}
	}
	return BrowserModeOff
}

// BrowserStatus is what the page needs to show the browser: whether there is
// one, whether it is ready, and where its stream is.
type BrowserStatus struct {
	Enabled bool   `json:"enabled"`
	Ready   bool   `json:"ready"`
	Version string `json:"version"`
	Mode    string `json:"mode"`
	// StreamURL is the agent-browser stream on this machine, for a page
	// running here (the desktop window). A page elsewhere uses the relay at
	// /api/browser/stream, which forwards to the same place.
	StreamURL string `json:"streamUrl"`
	// Connected is whether the session is attached to its browser. In the
	// person's own Chrome nothing attaches it but a deliberate connect, so a
	// page shows a connect button instead of dialling the stream in a loop.
	Connected bool   `json:"connected"`
	Error     string `json:"error,omitempty"`
	// Queue is which run has the browser and which wait for it.
	Queue *BrowserQueueStatus `json:"queue,omitempty"`
	// Bridge is the Chrome extension's side of "my Chrome": whether it is
	// connected and what to paste into it.
	Bridge *BridgeStatus `json:"bridge,omitempty"`
}

// browserRuntime is one service's browser and how far getting it running has
// come. The stream's port is agent-browser's: its daemon outlives a settings
// save and a restart of SuperLeo, so a port chosen here went stale the first
// time the service was rebuilt. It is asked for instead (streamAddr).
type browserRuntime struct {
	mode   string
	bin    string
	env    map[string]string
	ready  atomic.Bool
	errMu  sync.Mutex
	err    string
	cancel context.CancelFunc

	addrMu    sync.Mutex
	addr      string
	connected bool
	addrAt    time.Time

	// lastAttach is the last time a connect reached for the person's Chrome;
	// attempts are spaced (BrowserConnect), since each one can stop Chrome
	// for a permission prompt.
	attachMu   sync.Mutex
	lastAttach time.Time

	// bridge is set in "my Chrome": the browser is the person's Chrome reached
	// through SuperLeo's extension, not agent-browser's own auto-connect.
	bridge *BrowserBridge

	tabMu        sync.Mutex // one ensureOwnTab at a time
	attachFailed time.Time  // when reaching Chrome last failed, under tabMu

	// queue lines up the runs that want this browser (browser_queue.go).
	queue *browserQueue
}

func (b *browserRuntime) setErr(err error) {
	b.errMu.Lock()
	defer b.errMu.Unlock()
	if err == nil {
		b.err = ""
		return
	}
	b.err = err.Error()
}

// BrowserStatus reports the browser of this service.
func (s *Service) BrowserStatus() BrowserStatus {
	st := BrowserStatus{Version: AgentBrowserVersion, Mode: BrowserModeOff}
	if s == nil || s.browser == nil {
		return st
	}
	b := s.browser
	st.Enabled, st.Mode = true, b.mode
	st.Ready = b.ready.Load()
	if b.bridge != nil {
		br := b.bridge.Status()
		st.Bridge = &br
	}
	if b.queue != nil {
		if q := b.queue.status(); q.Holder != "" || len(q.Waiting) > 0 {
			st.Queue = &q
		}
	}
	if st.Ready {
		if addr, connected, err := b.streamInfo(); err == nil {
			st.StreamURL = "ws://" + addr + "/"
			st.Connected = connected
			// A miss while the daemon was still starting is over.
			b.setErr(nil)
		} else {
			st.Ready = false
			b.setErr(err)
		}
	}
	b.errMu.Lock()
	st.Error = b.err
	b.errMu.Unlock()
	return st
}

// BrowserStreamAddr is the loopback address of the stream, for the relay;
// empty when there is no browser running.
func (s *Service) BrowserStreamAddr() string {
	if s == nil || s.browser == nil || !s.browser.ready.Load() {
		return ""
	}
	addr, connected, err := s.browser.streamInfo()
	if err != nil {
		log.Printf("superleo: browser stream: %v", err)
		return ""
	}
	// A page opening the stream makes agent-browser reach for the browser
	// when it is not attached. In the person's own Chrome every such reach is
	// one more thing laid on Chrome — retried by every page and phone in a
	// loop, it hung Chrome and the machine with it. Only a deliberate connect
	// attaches there; until then the stream is closed to everyone.
	if s.browser.mode == BrowserModeChrome && !connected {
		return ""
	}
	return addr
}

// BrowserConnect attaches the session to the browser now rather than at the
// model's first call: agent-browser connects to the person's Chrome only when
// a command needs it, so a panel opened first showed "not connected" with
// nothing to do about it. The command is a read; in the person's Chrome it
// opens the session's own tab (pin-tab) and nothing else.
func (s *Service) BrowserConnect(ctx context.Context) error {
	if s == nil || s.browser == nil || !s.browser.ready.Load() {
		return fmt.Errorf("the browser is not on")
	}
	b := s.browser
	defer b.forgetStream()
	if b.mode != BrowserModeChrome {
		_, err := b.run(ctx, "get", "url")
		return err
	}
	if b.bridge != nil {
		if err := b.bridgeReady(); err != nil {
			return err
		}
		// A read: agent-browser attaches through the relay and opens the
		// session's tab in the SuperLeo group if it has none.
		return b.ensureBridgeTab(ctx)
	}
	b.attachMu.Lock()
	if wait := browserAttachGap - time.Since(b.lastAttach); wait > 0 {
		b.attachMu.Unlock()
		return fmt.Errorf("tried a moment ago; try again in %ds", int(wait.Seconds())+1)
	}
	b.lastAttach = time.Now()
	b.attachMu.Unlock()
	return b.ensureOwnTab(ctx)
}

// browserAttachGap is the least time between two connects to the person's
// Chrome: each can stop Chrome on a permission prompt.
const browserAttachGap = 20 * time.Second

// ensureOwnTab makes sure the session works in a tab it opened itself. In the
// person's Chrome, pin-tab binds the session to whichever tab was in front
// when it attached — measured: the person's Gemini tab, after the daemon had
// restarted — and the model's next open would have navigated it away. So the
// tab is checked before every browser call (the gate's guard) and, unless it
// is the one recorded as the session's own, a new one is opened and recorded.
func (b *browserRuntime) ensureOwnTab(ctx context.Context) error {
	b.tabMu.Lock()
	defer b.tabMu.Unlock()
	// After Chrome failed to answer, nothing reaches for it again for a while:
	// a model retrying its call would otherwise knock on Chrome once a round.
	if since := time.Since(b.attachFailed); since < browserAttachGap {
		return fmt.Errorf("the person's Chrome did not answer a moment ago; try again in %ds, or ask them to check Chrome", int((browserAttachGap-since).Seconds())+1)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	own := b.ownTab()
	cur, err := b.boundTab(ctx)
	if err != nil {
		b.attachFailed = time.Now()
		return fmt.Errorf("reach the person's Chrome: %v", err)
	}
	if cur != "" && cur == own {
		return nil
	}
	if out, err := b.run(ctx, "tab", "new"); err != nil {
		return fmt.Errorf("open a tab of its own in Chrome: %s", out)
	}
	cur, err = b.boundTab(ctx)
	if err != nil || cur == "" {
		return fmt.Errorf("could not tell which tab the browser is in: %v", err)
	}
	if err := os.WriteFile(b.ownTabPath(), []byte(cur), 0o600); err != nil {
		return err
	}
	return nil
}

// bridgeReady is the gate's check in "my Chrome": the extension must be there.
func (b *browserRuntime) bridgeReady() error {
	if b.bridge.Connected() {
		return nil
	}
	return fmt.Errorf("the SuperLeo Chrome extension is not connected: ask the person to load it in Chrome (Settings, Browser, My Chrome) and paste the code")
}

// ensureBridgeTab makes sure the session has a tab to work in. Through the
// bridge every tab it can see is one of its own, in the SuperLeo group.
func (b *browserRuntime) ensureBridgeTab(ctx context.Context) error {
	b.tabMu.Lock()
	defer b.tabMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cur, err := b.boundTab(ctx)
	if err != nil {
		return fmt.Errorf("reach Chrome through the extension: %v", err)
	}
	if cur != "" {
		return nil
	}
	if out, err := b.run(ctx, "tab", "new"); err != nil {
		return fmt.Errorf("open a tab in the SuperLeo group: %s", out)
	}
	return nil
}

// boundTab is the target id of the tab the session is in, "" when none.
func (b *browserRuntime) boundTab(ctx context.Context) (string, error) {
	out, err := b.run(ctx, "--json", "tab", "list")
	if err != nil {
		return "", err
	}
	var r struct {
		Data struct {
			Tabs []struct {
				TargetID string `json:"targetId"`
				Active   bool   `json:"active"`
			} `json:"tabs"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		return "", err
	}
	for _, t := range r.Data.Tabs {
		if t.Active {
			return t.TargetID, nil
		}
	}
	return "", nil
}

func (b *browserRuntime) ownTabPath() string {
	return filepath.Join(DataDir(), "browser-tab-"+b.env["AGENT_BROWSER_SESSION"])
}

func (b *browserRuntime) ownTab() string {
	raw, _ := os.ReadFile(b.ownTabPath())
	return strings.TrimSpace(string(raw))
}

// run runs one agent-browser command in this session and returns what it
// printed. A command that reports failure in its JSON is an error too.
func (b *browserRuntime) run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, b.bin, args...)
	cmd.Env = os.Environ()
	for k, v := range b.env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	raw, err := cmd.CombinedOutput()
	out := strings.TrimSpace(string(raw))
	if err == nil && strings.Contains(out, `"success":false`) {
		err = fmt.Errorf("agent-browser: %s", out)
	}
	if err != nil && out == "" {
		out = err.Error()
	}
	return out, err
}

// streamAddr asks agent-browser where this session's stream is. The answer
// is kept for a moment, since a page opening a card and its modal asks twice.
func (b *browserRuntime) streamAddr() (string, error) {
	addr, _, err := b.streamInfo()
	return addr, err
}

// streamInfo is where the stream is and whether the session is attached to a
// browser. It is asked without the attach settings (auto-connect, pin-tab),
// so asking can never be what reaches into the person's Chrome: a status
// query must not become one more attempt on it.
func (b *browserRuntime) streamInfo() (string, bool, error) {
	b.addrMu.Lock()
	defer b.addrMu.Unlock()
	if b.addr != "" && time.Since(b.addrAt) < 2*time.Second {
		return b.addr, b.connected, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, b.bin, "stream", "status", "--json")
	cmd.Env = os.Environ()
	for k, v := range b.env {
		if k == "AGENT_BROWSER_AUTO_CONNECT" || k == "AGENT_BROWSER_PIN_TAB" {
			continue
		}
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.Output()
	if err != nil {
		return "", false, fmt.Errorf("agent-browser stream status: %w", err)
	}
	var st struct {
		Success bool `json:"success"`
		Data    struct {
			Enabled   bool `json:"enabled"`
			Connected bool `json:"connected"`
			Port      int  `json:"port"`
		} `json:"data"`
		Error any `json:"error"`
	}
	if err := json.Unmarshal(out, &st); err != nil {
		return "", false, fmt.Errorf("agent-browser stream status: %w", err)
	}
	if !st.Success || !st.Data.Enabled || st.Data.Port <= 0 {
		return "", false, fmt.Errorf("agent-browser has no stream for this session (%v)", st.Error)
	}
	b.addr, b.connected, b.addrAt = fmt.Sprintf("127.0.0.1:%d", st.Data.Port), st.Data.Connected, time.Now()
	return b.addr, b.connected, nil
}

// forgetStream drops the kept answer, so the next one is asked afresh: after
// a connect, the page must see it at once.
func (b *browserRuntime) forgetStream() {
	b.addrMu.Lock()
	b.addrAt = time.Time{}
	b.addrMu.Unlock()
}

// startBrowser gets the binary and mounts it, off the caller's path: Build
// must not wait on a download, so the browser's tools join the agent when
// they are ready, the way any MCP server's do.
func (s *Service) startBrowser(mode string) {
	ctx, cancel := context.WithCancel(context.Background())
	b := &browserRuntime{mode: mode, env: browserEnv(mode), cancel: cancel}
	if mode == BrowserModeChrome && os.Getenv("SUPERLEO_BROWSER_CHROME_LEGACY") == "" {
		// The extension's bridge replaces auto-connect (browser_bridge.go).
		if err := InstallChromeExtension(); err != nil {
			log.Printf("superleo: browser: unpack the Chrome extension: %v", err)
		}
		br := SharedBridge()
		if err := br.Start(); err != nil {
			log.Printf("superleo: browser: bridge: %v", err)
			b.setErr(err)
		} else {
			b.bridge = br
			delete(b.env, "AGENT_BROWSER_AUTO_CONNECT")
			b.env["AGENT_BROWSER_CDP"] = br.CDPURL()
			// Its own session: a daemon left over from auto-connect would
			// keep that connection whatever it is told now.
			b.env["AGENT_BROWSER_SESSION"] += fmt.Sprintf("-b%d", br.Port())
		}
	}
	b.queue = browserQueueFor(browserKey(mode, b.env))
	s.browser = b
	if mode == BrowserModeChrome {
		s.guardPersonsChrome(b)
	}
	if s.gate != nil {
		// Every browser call first waits until its run has the browser to
		// itself (takeBrowser); in the person's Chrome it then makes sure the
		// session is in its own tab and not the one the person has in front
		// (ensureOwnTab).
		s.gate.Guard(BrowserTool(""), func(ctx context.Context) error {
			if err := b.takeBrowser(ctx); err != nil {
				return err
			}
			if b.bridge != nil {
				return b.bridgeReady()
			}
			if b.mode == BrowserModeChrome {
				return b.ensureOwnTab(ctx)
			}
			return nil
		})
	}
	go func() {
		bin, err := EnsureAgentBrowser(ctx)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("superleo: browser: %v", err)
				b.setErr(err)
			}
			return
		}
		b.bin = bin
		if s.svc == nil {
			b.setErr(fmt.Errorf("no agent to mount the browser on"))
			return
		}
		s.browserEager.Store(true)
		s.refreshEagerTools()
		// AddMCPServer, not MCP's own add: that starts the process but leaves
		// its tools out of the search index the model finds MCP tools by.
		if err := s.svc.AddMCPServer(ctx, BrowserMCPName, bin, []string{"mcp"}, b.env); err != nil {
			if ctx.Err() == nil {
				log.Printf("superleo: browser: %v", err)
				b.setErr(err)
			}
			return
		}
		b.setErr(nil)
		b.ready.Store(true)
		addr, _ := b.streamAddr()
		log.Printf("superleo: browser %s ready (%s, stream on %s)", AgentBrowserVersion, mode, addr)
	}()
}

func (s *Service) stopBrowser() {
	if s != nil && s.browser != nil && s.browser.cancel != nil {
		s.browser.cancel()
	}
}

// guardPersonsChrome keeps the browser from taking the person's own Chrome
// down. agent-browser's close, sent there, crashed Chrome with a dozen tabs
// open, and a tab close right before it was the last thing it did; neither
// runs here. The session works in a tab of its own (pin-tab), not in the one
// the person is reading, and the tab is left for the person to close.
//
// close is refused at the gate because agent-browser's own action policy
// cannot refuse it: a policy always lets close through. tab_close is refused
// at both, so a call that reached the daemon some other way still stops.
func (s *Service) guardPersonsChrome(b *browserRuntime) {
	const why = "this is the person's own Chrome: closing it or its tabs is theirs to do"
	if s.gate != nil {
		s.gate.Forbid(BrowserTool("close"), why)
		// Through the bridge the session can only see the tabs in the
		// SuperLeo group, and the extension refuses to close any other, so
		// closing its own is safe. The old auto-connect saw every tab.
		if b.bridge == nil {
			s.gate.Forbid(BrowserTool("tab_close"), why)
		}
	}
	b.env["AGENT_BROWSER_PIN_TAB"] = "1"
	policy := filepath.Join(DataDir(), "browser-policy.json")
	rules := `{"default":"allow","deny":["tab_close"]}`
	if b.bridge != nil {
		rules = `{"default":"allow"}`
	}
	if err := os.WriteFile(policy, []byte(rules), 0o600); err != nil {
		log.Printf("superleo: browser: action policy: %v", err)
		return
	}
	b.env["AGENT_BROWSER_ACTION_POLICY"] = policy
}

// BrowserTool is the name a browser op reaches the model as.
func BrowserTool(op string) string {
	return "mcp_" + BrowserMCPName + "_agent_browser_" + op
}

// browserEnv configures the agent-browser MCP server. The session is named
// after this SuperLeo's data directory, so the desktop and a node on the same
// machine each keep their own tabs instead of driving one another's. The
// stream's port is left to agent-browser (streamAddr).
func browserEnv(mode string) map[string]string {
	sum := sha256.Sum256([]byte(DataDir()))
	env := map[string]string{
		"AGENT_BROWSER_SESSION":          "superleo-" + hex.EncodeToString(sum[:4]),
		"AGENT_BROWSER_STREAM_QUALITY":   "60",
		"AGENT_BROWSER_STREAM_MAX_WIDTH": "1280",
	}
	if mode == BrowserModeChrome {
		env["AGENT_BROWSER_AUTO_CONNECT"] = "1"
	}
	return env
}

// agentBrowserAsset is the release asset for this platform, or "" when
// agent-browser publishes none.
func agentBrowserAsset() string {
	arch := map[string]string{"arm64": "arm64", "amd64": "x64"}[runtime.GOARCH]
	if arch == "" {
		return ""
	}
	switch runtime.GOOS {
	case "darwin":
		return "agent-browser-darwin-" + arch
	case "windows":
		if arch == "x64" {
			return "agent-browser-win32-x64.exe"
		}
	case "linux":
		// Alpine — the hive image — has no glibc; the musl build runs there.
		if matches, _ := filepath.Glob("/lib/ld-musl-*.so.1"); len(matches) > 0 {
			return "agent-browser-linux-musl-" + arch
		}
		return "agent-browser-linux-" + arch
	}
	return ""
}

// AgentBrowserPath is where the binary lives once fetched.
func AgentBrowserPath() string {
	name := "agent-browser-" + AgentBrowserVersion
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(DataDir(), "bin", name)
}

var agentBrowserFetch sync.Mutex // one download at a time, per process

// agentBrowserURL is where a release asset is downloaded from; a variable so
// a test can serve it.
var agentBrowserURL = func(asset string) string {
	return "https://github.com/vercel-labs/agent-browser/releases/download/v" + AgentBrowserVersion + "/" + asset
}

// EnsureAgentBrowser returns the path of a verified agent-browser binary,
// downloading it first if it is not there. A file already in place is checked
// again too: a half-written or swapped binary is fetched afresh, not run.
func EnsureAgentBrowser(ctx context.Context) (string, error) {
	asset := agentBrowserAsset()
	want := agentBrowserDigests[asset]
	if want == "" {
		return "", fmt.Errorf("agent-browser publishes no binary for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	agentBrowserFetch.Lock()
	defer agentBrowserFetch.Unlock()
	path := AgentBrowserPath()
	if fileDigest(path) == want {
		return path, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, agentBrowserURL(asset), nil)
	if err != nil {
		return "", err
	}
	// The default transport honours HTTPS_PROXY, which is how a machine
	// behind one reaches GitHub at all.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("download agent-browser: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download agent-browser: HTTP %d", resp.StatusCode)
	}
	tmp := path + ".download"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, 64<<20))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("download agent-browser: %w", err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("agent-browser %s: digest %s, want %s; not installed", asset, got, want)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	log.Printf("superleo: agent-browser %s installed at %s", AgentBrowserVersion, path)
	return path, nil
}

func fileDigest(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

// browserBuiltin is set while this process mounts its own browser, so a
// user's mcpServers.json entry for agent-browser is left out rather than
// running a second browser beside it (resolveMCPConfig).
var browserBuiltin atomic.Bool

// isUserBrowserEntry reports whether an mcpServers entry runs agent-browser.
func isUserBrowserEntry(name string, entry map[string]any) bool {
	if strings.EqualFold(name, "agent-browser") {
		return true
	}
	cmd, _ := entry["command"].(string)
	base := strings.TrimSuffix(filepath.Base(cmd), ".exe")
	return base == "agent-browser"
}

// browserEagerTools are the browser tools every request offers. MCP tools
// otherwise wait behind the search index, and a model that guessed these
// names wrong searched once, found nothing and fetched the page instead. The
// rest — checkboxes, waits, eval, tab housekeeping — stay findable by search.
var browserEagerTools = []string{
	"mcp_browser_agent_browser_open",
	"mcp_browser_agent_browser_snapshot",
	"mcp_browser_agent_browser_click",
	"mcp_browser_agent_browser_fill",
	"mcp_browser_agent_browser_type",
	"mcp_browser_agent_browser_press",
	"mcp_browser_agent_browser_scroll",
	"mcp_browser_agent_browser_get_text",
	"mcp_browser_agent_browser_get_url",
	"mcp_browser_agent_browser_screenshot",
	"mcp_browser_agent_browser_back",
	"mcp_browser_agent_browser_wait_for_load",
	"mcp_browser_agent_browser_tab_list",
	// A tab closed under the session (tab_gone) is replaced with this.
	"mcp_browser_agent_browser_tab_new",
	"mcp_browser_agent_browser_tab_switch",
}

// browserSection tells the model the browser is there and when it is the
// right tool. In the person's own Chrome it acts as them, signed in to their
// accounts, so anything that cannot be taken back waits for a yes.
func browserSection(mode string) string {
	switch mode {
	case BrowserModeChrome:
		return `
- A real browser is yours (mcp_browser_agent_browser_*: open, snapshot for refs, click, fill/type, press, scroll, get_text, screenshot, back, tab_*). Use it when a page needs a login, clicks, a form or scripts, or the person wants to watch; to read a public page, fetch_url is faster. The person sees it live and can take over.
- It is the person's own Chrome, signed in as them. Before anything that buys, sends, posts, deletes, or changes an account, stop and ask.`
	case BrowserModeOwn:
		return `
- A real browser is yours (mcp_browser_agent_browser_*: open, snapshot for refs, click, fill/type, press, scroll, get_text, screenshot, back, tab_*). Use it when a page needs clicks, a form or scripts, or the person wants to watch; to read a public page, fetch_url is faster. The person sees it live and can take over.`
	}
	return ""
}
