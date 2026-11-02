package app

import (
	"encoding/json"
	"testing"
)

// Every client asks the core which conversation is the queen's, over the same
// RPC the browser and the phones use, and gets the one shared id.
func TestQueenConsoleNamesTheOneSharedConversation(t *testing.T) {
	got, err := callMethod(&App{}, "QueenConsole", nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(got)
	if string(b) != `{"session":"hive-console"}` {
		t.Fatalf("QueenConsole answered %s", b)
	}
}

// A turn taken in the shared conversation is announced with its words, and
// every event of it carries the conversation, so the iPhone can follow a turn
// the iPad started.
func TestATurnIsAnnouncedAndItsEventsCarryTheConversation(t *testing.T) {
	h := newEngineHarness(t, fakeClaudeTurn, 0)
	h.a.settings.Hive.QueenEngine = ""

	id := h.a.SendChat(QueenConsoleSession, "明天的会改到几点了？", []string{"/tmp/a.png"})
	if name, _ := h.ev.wait(t, id); name != "chat:done" {
		t.Fatalf("ended with %s", name)
	}
	turn := h.ev.find("chat:turn", id)
	if turn == nil {
		t.Fatal("no chat:turn")
	}
	if turn["sessionId"] != QueenConsoleSession || turn["message"] != sentImagesHeading+"\n- /tmp/a.png\n\n明天的会改到几点了？" || turn["images"] != 1 {
		t.Fatalf("chat:turn: %v", turn)
	}
	if done := h.ev.find("chat:done", id); done["sessionId"] != QueenConsoleSession {
		t.Fatalf("chat:done without the conversation: %v", done)
	}
	// The turn is forgotten once it ended: a stray event after the end is
	// not stamped, and nothing is kept per finished turn.
	if _, left := h.a.turnSess.m.Load(id); left {
		t.Fatal("the turn's conversation outlived the turn")
	}
}

// A turn started while another is running in the conversation is stamped
// too: the other device has to know which conversation it belongs to.
func TestAnOverlappingTurnCarriesTheConversation(t *testing.T) {
	h := newEngineHarness(t, fakeClaudeTurn, 0)
	h.a.settings.Hive.QueenEngine = ""
	end := busyWith(h.a, "call-1", QueenConsoleSession)
	defer end()

	id := h.a.SendChat(QueenConsoleSession, "还有一件事", nil)
	if name, done := h.ev.wait(t, id); name != "chat:done" || done["sessionId"] != QueenConsoleSession {
		t.Fatalf("ended with %s %v", name, done)
	}
}

// The stamp copies: a payload the caller still holds is left as it was.
func TestTheStampDoesNotChangeTheCallersPayload(t *testing.T) {
	a := &App{}
	a.turnSess.m.Store("r1", "s1")
	p := map[string]any{"requestId": "r1", "type": "partial"}
	out := a.withTurnSession("chat:event", p)
	if _, has := p["sessionId"]; has {
		t.Fatal("the caller's payload was changed")
	}
	if out["sessionId"] != "s1" {
		t.Fatalf("not stamped: %v", out)
	}
	if got := a.withTurnSession("hive:message", map[string]any{"requestId": "r1"}); got["sessionId"] != nil {
		t.Fatalf("a non-chat event was stamped: %v", got)
	}
}
