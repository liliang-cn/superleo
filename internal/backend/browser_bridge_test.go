package backend

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func startTestBridge(t *testing.T) *BrowserBridge {
	t.Helper()
	b := newBrowserBridge(t.TempDir())
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Stop)
	return b
}

func dialExt(t *testing.T, b *BrowserBridge, token string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	h := http.Header{"Origin": {"chrome-extension://abcdef"}}
	return websocket.DefaultDialer.Dial(fmt.Sprintf("ws://127.0.0.1:%d/ext?token=%s", b.Port(), token), h)
}

func connectExt(t *testing.T, b *BrowserBridge) *websocket.Conn {
	t.Helper()
	c, _, err := dialExt(t, b, b.Token())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	_ = c.WriteJSON(map[string]string{"type": "hello", "version": "9", "browser": "Chrome/1"})
	bridgeWait(t, b.Connected)
	return c
}

func bridgeWait(t *testing.T, f func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if f() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out")
}

func dialClient(t *testing.T, b *BrowserBridge) *websocket.Conn {
	t.Helper()
	c, _, err := websocket.DefaultDialer.Dial(b.CDPURL(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// readMsg is the next message that is CDP, not the bridge's own bookkeeping.
func readMsg(t *testing.T, c *websocket.Conn) map[string]any {
	t.Helper()
	for {
		_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
		var m map[string]any
		if err := c.ReadJSON(&m); err != nil {
			t.Fatal(err)
		}
		if m["type"] == nil {
			return m
		}
	}
}

// A client's command reaches the extension under the bridge's own id, and the
// answer comes back under the client's.
func TestBridgeForwardsCommandsAndRestoresIDs(t *testing.T) {
	b := startTestBridge(t)
	ext := connectExt(t, b)
	cl := dialClient(t, b)
	_ = cl.WriteJSON(map[string]any{"id": 7, "method": "Target.getTargets"})
	got := readMsg(t, ext)
	if got["method"] != "Target.getTargets" {
		t.Fatalf("extension got %v", got)
	}
	// An event, then the answer.
	_ = ext.WriteJSON(map[string]any{"method": "Target.targetCreated", "params": map[string]any{"x": 1}})
	_ = ext.WriteJSON(map[string]any{"id": got["id"], "result": map[string]any{"targetInfos": []any{}}})
	ev := readMsg(t, cl)
	if ev["method"] != "Target.targetCreated" {
		t.Fatalf("event: %v", ev)
	}
	ans := readMsg(t, cl)
	if ans["id"].(float64) != 7 {
		t.Fatalf("answer under id %v, want the client's 7", ans["id"])
	}
}

// Closing Chrome never leaves the relay.
func TestBridgeRefusesClosingChrome(t *testing.T) {
	b := startTestBridge(t)
	ext := connectExt(t, b)
	cl := dialClient(t, b)
	_ = cl.WriteJSON(map[string]any{"id": 1, "method": "Browser.close"})
	ans := readMsg(t, cl)
	if ans["error"] == nil || ans["id"].(float64) != 1 {
		t.Fatalf("Browser.close answered %v", ans)
	}
	_ = ext.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	for {
		_, raw, err := ext.ReadMessage()
		if err != nil {
			break
		}
		if strings.Contains(string(raw), "Browser.close") {
			t.Fatal("Browser.close reached the extension")
		}
	}
}

// Without the extension there is nothing to attach to, and the error says why.
func TestBridgeWithoutExtension(t *testing.T) {
	b := startTestBridge(t)
	_, resp, err := websocket.DefaultDialer.Dial(b.CDPURL(), nil)
	if err == nil || resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("dial without extension: %v %v", err, resp)
	}
	r, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/json/version", b.Port()))
	if err != nil || r.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("/json/version: %v %v", err, r)
	}
}

// Only the extension, holding the token, gets in; a web page never does.
func TestBridgeGuardsTheExtensionDoor(t *testing.T) {
	b := startTestBridge(t)
	if _, resp, err := dialExt(t, b, "wrong"); err == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong token: %v", err)
	}
	h := http.Header{"Origin": {"https://evil.example"}}
	if _, resp, err := websocket.DefaultDialer.Dial(fmt.Sprintf("ws://127.0.0.1:%d/ext?token=%s", b.Port(), b.Token()), h); err == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("web page origin: %v", err)
	}
	connectExt(t, b)
	if _, resp, err := websocket.DefaultDialer.Dial(b.CDPURL(), http.Header{"Origin": {"https://evil.example"}}); err == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("web page on the CDP door: %v", err)
	}
	req, _ := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d/json/version", b.Port()), nil)
	req.Host = "evil.example:80"
	if r, err := http.DefaultClient.Do(req); err != nil || r.StatusCode != http.StatusForbidden {
		t.Fatalf("rebound host: %v %v", err, r)
	}
}

// /json/list is the extension's targets, nothing else.
func TestBridgeListsTheExtensionsTargets(t *testing.T) {
	b := startTestBridge(t)
	ext := connectExt(t, b)
	go func() {
		m := readMsg(t, ext)
		_ = ext.WriteJSON(map[string]any{"id": m["id"], "result": map[string]any{"targetInfos": []any{
			map[string]any{"targetId": "T1", "type": "page", "title": "HN", "url": "https://news.ycombinator.com/"}}}})
	}()
	r, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/json/list", b.Port()))
	if err != nil {
		t.Fatal(err)
	}
	var list []map[string]any
	_ = json.NewDecoder(r.Body).Decode(&list)
	if len(list) != 1 || list[0]["id"] != "T1" || !strings.Contains(list[0]["webSocketDebuggerUrl"].(string), "/devtools/browser/") {
		t.Fatalf("list = %v", list)
	}
}

// A reconnecting extension replaces its stale socket, and the calls that were
// waiting on the old one fail instead of hanging.
func TestBridgeReconnect(t *testing.T) {
	b := startTestBridge(t)
	old := connectExt(t, b)
	cl := dialClient(t, b)
	_ = cl.WriteJSON(map[string]any{"id": 3, "method": "Page.enable", "sessionId": "sl1"})
	readMsg(t, old)
	fresh := connectExt(t, b)
	ans := readMsg(t, cl)
	if ans["error"] == nil || ans["id"].(float64) != 3 {
		t.Fatalf("the call in flight: %v", ans)
	}
	// The client is still there and the new socket serves it.
	_ = cl.WriteJSON(map[string]any{"id": 4, "method": "Page.enable", "sessionId": "sl1"})
	m := readMsg(t, fresh)
	_ = fresh.WriteJSON(map[string]any{"id": m["id"], "result": map[string]any{}})
	if a := readMsg(t, cl); a["id"].(float64) != 4 || a["error"] != nil {
		t.Fatalf("after reconnect: %v", a)
	}
	// Extension gone: not connected, and the next command says so.
	fresh.Close()
	bridgeWait(t, func() bool { return !b.Connected() })
	_ = cl.WriteJSON(map[string]any{"id": 5, "method": "Page.enable", "sessionId": "sl1"})
	if a := readMsg(t, cl); a["error"] == nil {
		t.Fatalf("without the extension: %v", a)
	}
}

// The port and token survive a restart, so the extension's code stays valid.
func TestBridgeKeepsItsAddress(t *testing.T) {
	dir := t.TempDir()
	a := newBrowserBridge(dir)
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	port, tok := a.Port(), a.Token()
	a.Stop()
	b := newBrowserBridge(dir)
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	defer b.Stop()
	if b.Port() != port || b.Token() != tok {
		t.Fatalf("address changed: %d/%s -> %d/%s", port, tok, b.Port(), b.Token())
	}
}
