package backend

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
)

// The extension's side panel talks to the queen (蜂后) through the bridge's
// own listener, with the same token the extension pairs with, so the panel
// needs to know nothing of the API server's sign-in. The app plugs the real
// chat in (SetBridgeChat); the backend package has none of its own.

// BridgeChat is the chat the side panel reaches.
type BridgeChat struct {
	// Send starts a turn in a conversation and returns its request id.
	Send func(session, message string) string
	// History is the conversation's visible turns.
	History func(session string) any
	// Running is whether a request is still being answered.
	Running func(requestID string) bool
}

var bridgeChat atomic.Pointer[BridgeChat]

// SetBridgeChat sets what the side panel talks to.
func SetBridgeChat(c *BridgeChat) { bridgeChat.Store(c) }

// bridgeSidePanelSession is the one conversation the panel uses; it shows up
// on the Conversations page like any other.
const bridgeSidePanelSession = "chrome-sidepanel"

func (b *BrowserBridge) authorized(r *http.Request) bool {
	t, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(strings.TrimSpace(t)), []byte(b.Token())) == 1
}

func (b *BrowserBridge) serveChatSend(w http.ResponseWriter, r *http.Request) {
	c := bridgeChat.Load()
	if !b.authorized(r) || c == nil || r.Method != http.MethodPost {
		http.Error(w, "no", http.StatusForbidden)
		return
	}
	var in struct {
		Message string `json:"message"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in) != nil || strings.TrimSpace(in.Message) == "" {
		http.Error(w, "message", http.StatusBadRequest)
		return
	}
	writeBridgeJSON(w, map[string]any{"requestId": c.Send(bridgeSidePanelSession, in.Message), "session": bridgeSidePanelSession})
}

func (b *BrowserBridge) serveChatState(w http.ResponseWriter, r *http.Request) {
	c := bridgeChat.Load()
	if !b.authorized(r) || c == nil {
		http.Error(w, "no", http.StatusForbidden)
		return
	}
	writeBridgeJSON(w, map[string]any{
		"turns":   c.History(bridgeSidePanelSession),
		"running": r.URL.Query().Get("requestId") != "" && c.Running(r.URL.Query().Get("requestId")),
	})
}
