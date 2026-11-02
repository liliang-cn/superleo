package backend

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// The Chrome bridge.
//
// "My Chrome" used to mean agent-browser auto-connecting to the person's
// Chrome over its remote-debugging port, which Chrome now gates behind a
// toggle and an Allow prompt on every connection. The bridge replaces that
// with a Chrome extension (extension/chrome): the extension dials SuperLeo,
// and SuperLeo hands agent-browser a CDP endpoint of its own (--cdp) whose
// other end is that extension. The extension runs the commands with
// chrome.debugger on tabs it keeps in a tab group named "SuperLeo" and shows
// nothing else of the browser, so there is no toggle and no prompt, and the
// person's own tabs are neither listed nor reachable.
//
//	agent-browser --CDP--> relay (this file) --WebSocket--> extension --> tabs in the group
//
// One listener on loopback serves both ends: /ext for the extension (a token
// the person pastes into the extension once), /json/* and /devtools/browser/*
// for agent-browser. The relay is a courier: the extension decides what is
// visible and allowed. The relay only adds a second lock on the calls that
// could take the whole browser down.

// BridgeGroupTitle is the name of the tab group every tab SuperLeo opens is in.
const BridgeGroupTitle = "SuperLeo"

// bridgeDefaultPort is tried first on a first run; taken, the OS picks one.
const bridgeDefaultPort = 43521

const (
	bridgeExtPath     = "/ext"
	bridgeBrowserPath = "/devtools/browser/superleo"
	bridgeCallTimeout = 60 * time.Second
)

// bridgeDebug logs the commands agent-browser sends (SUPERLEO_BRIDGE_DEBUG=1).
var bridgeDebug = os.Getenv("SUPERLEO_BRIDGE_DEBUG") != ""

// bridgeDenied are the commands the relay never forwards, whoever asks. The
// extension refuses them too.
var bridgeDenied = map[string]string{
	"Browser.close":               "closing Chrome is the person's to do",
	"Browser.crash":               "not allowed",
	"Browser.crashGpuProcess":     "not allowed",
	"Target.createBrowserContext": "tabs stay in the SuperLeo tab group",
}

// BridgeStatus is what the settings page shows of the bridge.
type BridgeStatus struct {
	// Connected is whether the extension is attached right now.
	Connected bool   `json:"connected"`
	Browser   string `json:"browser,omitempty"`
	Version   string `json:"version,omitempty"`
	// Code is what the person pastes into the extension: "<port>:<token>".
	Code string `json:"code"`
	// Dir is where the unpacked extension is, to load in chrome://extensions.
	Dir string `json:"dir"`
	// CDPURL is what agent-browser is given.
	CDPURL string `json:"cdpUrl"`
}

type bridgePending struct {
	client *bridgeClient   // reply goes to this client under origID...
	orig   json.RawMessage // ...
	ch     chan bridgeMsg  // or, for the bridge's own calls, here
}

type bridgeMsg struct {
	ID        json.RawMessage `json:"id,omitempty"`
	Method    string          `json:"method,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     json.RawMessage `json:"error,omitempty"`
	// Type marks the bridge's own messages (hello, ping, pong, clients).
	Type    string `json:"type,omitempty"`
	Version string `json:"version,omitempty"`
	Browser string `json:"browser,omitempty"`
	N       int    `json:"n,omitempty"`
}

type bridgeConn struct {
	ws *websocket.Conn
	mu sync.Mutex
}

func (c *bridgeConn) write(v any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return c.ws.WriteJSON(v)
}

type bridgeClient struct{ bridgeConn }

// BrowserBridge is the relay between agent-browser and the Chrome extension.
type BrowserBridge struct {
	dir string

	mu      sync.Mutex
	ln      net.Listener
	srv     *http.Server
	port    int
	token   string
	ext     *bridgeConn
	extInfo bridgeMsg
	clients map[*bridgeClient]struct{}
	pending map[int64]bridgePending
	seq     int64
}

var (
	sharedBridgeMu sync.Mutex
	sharedBridgeV  *BrowserBridge
)

// SharedBridge is this process's bridge. The browser is rebuilt whenever
// settings are saved, and the bridge must outlive that: the extension and
// agent-browser's daemon both hold connections to it.
func SharedBridge() *BrowserBridge {
	sharedBridgeMu.Lock()
	defer sharedBridgeMu.Unlock()
	if sharedBridgeV == nil || sharedBridgeV.dir != DataDir() {
		sharedBridgeV = newBrowserBridge(DataDir())
	}
	return sharedBridgeV
}

func newBrowserBridge(dir string) *BrowserBridge {
	return &BrowserBridge{dir: dir, clients: map[*bridgeClient]struct{}{}, pending: map[int64]bridgePending{}}
}

type bridgeFile struct {
	Port  int    `json:"port"`
	Token string `json:"token"`
}

func (b *BrowserBridge) filePath() string { return filepath.Join(b.dir, "browser-bridge.json") }

// Start opens the listener, once. The port and token are kept in the data
// directory so a restart of SuperLeo comes back at the same address: the
// extension and agent-browser's long-lived daemon both know it.
func (b *BrowserBridge) Start() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ln != nil {
		return nil
	}
	var f bridgeFile
	if raw, err := os.ReadFile(b.filePath()); err == nil {
		_ = json.Unmarshal(raw, &f)
	}
	if len(f.Token) < 32 {
		var t [16]byte
		if _, err := rand.Read(t[:]); err != nil {
			return err
		}
		f.Token = hex.EncodeToString(t[:])
	}
	if f.Port == 0 {
		f.Port = bridgeDefaultPort
	}
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(f.Port)))
	if err != nil {
		if ln, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
			return err
		}
	}
	f.Port = ln.Addr().(*net.TCPAddr).Port
	raw, _ := json.Marshal(f)
	if err := os.MkdirAll(b.dir, 0o700); err == nil {
		_ = os.WriteFile(b.filePath(), raw, 0o600)
	}
	b.ln, b.port, b.token = ln, f.Port, f.Token
	mux := http.NewServeMux()
	mux.HandleFunc(bridgeExtPath, b.serveExtension)
	mux.HandleFunc("/chat/send", b.serveChatSend)
	mux.HandleFunc("/chat/state", b.serveChatState)
	mux.HandleFunc("/json/version", b.serveVersion)
	mux.HandleFunc("/json/list", b.serveList)
	mux.HandleFunc("/json", b.serveList)
	mux.HandleFunc(bridgeBrowserPath, b.serveClient)
	srv := &http.Server{Handler: b.guard(mux), ReadHeaderTimeout: 10 * time.Second}
	b.srv = srv
	go func() { _ = srv.Serve(ln) }()
	return nil
}

// Stop closes the listener and every connection; for tests.
func (b *BrowserBridge) Stop() {
	b.mu.Lock()
	srv := b.srv
	b.srv, b.ln = nil, nil
	b.mu.Unlock()
	if srv != nil {
		_ = srv.Close()
	}
}

// guard turns away anything a web page could send: a page in the person's
// browser always names its origin, and a DNS-rebound one names a host other
// than loopback. The extension's own origin is the one exception, on /ext.
func (b *BrowserBridge) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil || (host != "127.0.0.1" && host != "localhost") {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if o := r.Header.Get("Origin"); o != "" {
			ours := r.URL.Path == bridgeExtPath || strings.HasPrefix(r.URL.Path, "/chat/")
			if !(ours && strings.HasPrefix(o, "chrome-extension://")) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// Port is the port of the listener (0 before Start).
func (b *BrowserBridge) Port() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.port
}

// Token is what the extension must present.
func (b *BrowserBridge) Token() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.token
}

// CDPURL is the endpoint agent-browser attaches to.
func (b *BrowserBridge) CDPURL() string {
	return fmt.Sprintf("ws://127.0.0.1:%d%s", b.Port(), bridgeBrowserPath)
}

// Connected is whether the extension is attached.
func (b *BrowserBridge) Connected() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ext != nil
}

// Status is the bridge as the settings page shows it.
func (b *BrowserBridge) Status() BridgeStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	return BridgeStatus{
		Connected: b.ext != nil,
		Browser:   b.extInfo.Browser,
		Version:   b.extInfo.Version,
		Code:      fmt.Sprintf("%d:%s", b.port, b.token),
		Dir:       ChromeExtensionDir(),
		CDPURL:    fmt.Sprintf("ws://127.0.0.1:%d%s", b.port, bridgeBrowserPath),
	}
}

var bridgeUpgrader = websocket.Upgrader{
	ReadBufferSize: 16 << 10, WriteBufferSize: 64 << 10,
	// guard has already refused every origin but the extension's.
	CheckOrigin: func(*http.Request) bool { return true },
}

// serveExtension is the extension's end. A newer connection replaces an older
// one: a service worker that restarted dials again before the old socket has
// noticed it is dead.
func (b *BrowserBridge) serveExtension(w http.ResponseWriter, r *http.Request) {
	if subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("token")), []byte(b.Token())) != 1 {
		http.Error(w, "wrong token", http.StatusForbidden)
		return
	}
	ws, err := bridgeUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	ws.SetReadLimit(64 << 20)
	conn := &bridgeConn{ws: ws}
	b.mu.Lock()
	old := b.ext
	b.ext = conn
	b.extInfo = bridgeMsg{}
	if old != nil {
		b.failPendingLocked("the Chrome extension reconnected")
	}
	b.mu.Unlock()
	if old != nil {
		_ = old.ws.Close()
	}
	log.Printf("superleo: browser bridge: extension connected")
	b.tellClients()
	defer func() {
		b.mu.Lock()
		if b.ext == conn {
			b.ext = nil
			b.failPendingLocked("the Chrome extension disconnected")
		}
		b.mu.Unlock()
		_ = ws.Close()
		log.Printf("superleo: browser bridge: extension gone")
	}()
	for {
		_ = ws.SetReadDeadline(time.Now().Add(90 * time.Second))
		_, raw, err := ws.ReadMessage()
		if err != nil {
			return
		}
		var m bridgeMsg
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		switch {
		case m.Type == "hello":
			b.mu.Lock()
			if b.ext == conn {
				b.extInfo = m
			}
			b.mu.Unlock()
		case m.Type == "ping":
			_ = conn.write(map[string]string{"type": "pong"})
		case len(m.ID) > 0 && m.Method == "":
			b.reply(m, raw)
		case m.Method != "":
			b.broadcast(raw)
		}
	}
}

// reply routes an answer from the extension to whoever asked.
func (b *BrowserBridge) reply(m bridgeMsg, raw []byte) {
	id, err := strconv.ParseInt(string(m.ID), 10, 64)
	if err != nil {
		return
	}
	b.mu.Lock()
	p, ok := b.pending[id]
	delete(b.pending, id)
	b.mu.Unlock()
	if !ok {
		return
	}
	if p.ch != nil {
		p.ch <- m
		return
	}
	m.ID = p.orig
	_ = p.client.write(m)
}

// failPendingLocked answers every call in flight with an error.
func (b *BrowserBridge) failPendingLocked(why string) {
	for id, p := range b.pending {
		delete(b.pending, id)
		m := bridgeMsg{ID: p.orig, Error: bridgeErr(why)}
		if p.ch != nil {
			p.ch <- m
		} else {
			go func(c *bridgeClient, m bridgeMsg) { _ = c.write(m) }(p.client, m)
		}
	}
}

func bridgeErr(msg string) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{"code": -32000, "message": msg})
	return raw
}

// broadcast sends an event from the extension to every client.
func (b *BrowserBridge) broadcast(raw []byte) {
	b.mu.Lock()
	cs := make([]*bridgeClient, 0, len(b.clients))
	for c := range b.clients {
		cs = append(cs, c)
	}
	b.mu.Unlock()
	for _, c := range cs {
		c.mu.Lock()
		_ = c.ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
		_ = c.ws.WriteMessage(websocket.TextMessage, raw)
		c.mu.Unlock()
	}
}

// tellClients tells the extension how many clients are attached, so it can
// let go of the tabs (and take the "debugging" bar off them) when none is.
func (b *BrowserBridge) tellClients() {
	b.mu.Lock()
	ext, n := b.ext, len(b.clients)
	b.mu.Unlock()
	if ext != nil {
		_ = ext.write(map[string]any{"type": "clients", "n": n})
	}
}

// send forwards one command to the extension under an id of the bridge's own.
func (b *BrowserBridge) send(p bridgePending, method string, params json.RawMessage, session string) error {
	b.mu.Lock()
	ext := b.ext
	if ext == nil {
		b.mu.Unlock()
		return errors.New("the SuperLeo Chrome extension is not connected")
	}
	b.seq++
	id := b.seq
	b.pending[id] = p
	b.mu.Unlock()
	msg := map[string]any{"id": id, "method": method}
	if len(params) > 0 {
		msg["params"] = params
	}
	if session != "" {
		msg["sessionId"] = session
	}
	if err := ext.write(msg); err != nil {
		b.mu.Lock()
		delete(b.pending, id)
		b.mu.Unlock()
		return err
	}
	return nil
}

// Call runs one browser-level CDP command in Chrome through the extension.
func (b *BrowserBridge) Call(method string, params any) (json.RawMessage, error) {
	var raw json.RawMessage
	if params != nil {
		var err error
		if raw, err = json.Marshal(params); err != nil {
			return nil, err
		}
	}
	ch := make(chan bridgeMsg, 1)
	if err := b.send(bridgePending{ch: ch}, method, raw, ""); err != nil {
		return nil, err
	}
	select {
	case m := <-ch:
		if len(m.Error) > 0 {
			var e struct {
				Message string `json:"message"`
			}
			_ = json.Unmarshal(m.Error, &e)
			return nil, errors.New(e.Message)
		}
		return m.Result, nil
	case <-time.After(bridgeCallTimeout):
		return nil, errors.New("Chrome did not answer")
	}
}

// serveClient is agent-browser's end: one CDP connection at the browser level.
func (b *BrowserBridge) serveClient(w http.ResponseWriter, r *http.Request) {
	if !b.Connected() {
		http.Error(w, "the SuperLeo Chrome extension is not connected: install it in Chrome and paste the code from Settings", http.StatusServiceUnavailable)
		return
	}
	ws, err := bridgeUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	ws.SetReadLimit(64 << 20)
	c := &bridgeClient{bridgeConn{ws: ws}}
	b.mu.Lock()
	b.clients[c] = struct{}{}
	b.mu.Unlock()
	b.tellClients()
	defer func() {
		b.mu.Lock()
		delete(b.clients, c)
		for id, p := range b.pending {
			if p.client == c {
				delete(b.pending, id)
			}
		}
		b.mu.Unlock()
		_ = ws.Close()
		b.tellClients()
	}()
	for {
		_, raw, err := ws.ReadMessage()
		if err != nil {
			return
		}
		var m bridgeMsg
		if json.Unmarshal(raw, &m) != nil || m.Method == "" {
			continue
		}
		if bridgeDebug {
			log.Printf("superleo: bridge <- %s session=%s %.200s", m.Method, m.SessionID, m.Params)
		}
		if why, no := bridgeDenied[m.Method]; no {
			_ = c.write(bridgeMsg{ID: m.ID, Error: bridgeErr(m.Method + ": " + why)})
			continue
		}
		if err := b.send(bridgePending{client: c, orig: m.ID}, m.Method, m.Params, m.SessionID); err != nil {
			_ = c.write(bridgeMsg{ID: m.ID, Error: bridgeErr(err.Error())})
		}
	}
}

func (b *BrowserBridge) serveVersion(w http.ResponseWriter, r *http.Request) {
	if !b.Connected() {
		http.Error(w, "the SuperLeo Chrome extension is not connected", http.StatusServiceUnavailable)
		return
	}
	b.mu.Lock()
	browser := b.extInfo.Browser
	b.mu.Unlock()
	if browser == "" {
		browser = "Chrome"
	}
	writeBridgeJSON(w, map[string]any{
		"Browser":              browser,
		"Protocol-Version":     "1.3",
		"webSocketDebuggerUrl": b.CDPURL(),
	})
}

// serveList lists the tabs in the group, the way Chrome's own /json/list does.
func (b *BrowserBridge) serveList(w http.ResponseWriter, r *http.Request) {
	raw, err := b.Call("Target.getTargets", nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	var res struct {
		TargetInfos []struct {
			TargetID string `json:"targetId"`
			Type     string `json:"type"`
			Title    string `json:"title"`
			URL      string `json:"url"`
		} `json:"targetInfos"`
	}
	_ = json.Unmarshal(raw, &res)
	out := []map[string]any{}
	for _, t := range res.TargetInfos {
		out = append(out, map[string]any{
			"id": t.TargetID, "type": t.Type, "title": t.Title, "url": t.URL,
			"webSocketDebuggerUrl": b.CDPURL(),
		})
	}
	writeBridgeJSON(w, out)
}

func writeBridgeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
