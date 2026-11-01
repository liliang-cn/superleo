package app

import (
	"strings"
	"testing"
	"time"
)

// The throwaway home is TestMain's; each test also gets its own so a direct
// chat from one never shows in another.
func convTestApp(t *testing.T) *App {
	t.Helper()
	t.Setenv("SUPERLEO_HOME", t.TempDir())
	return &App{}
}

func TestADirectChatIsKeptAndContinuedUnderOneId(t *testing.T) {
	a := convTestApp(t)
	// Nobody is called that, so the answers are failures: still words said,
	// still a conversation the person can open again.
	r1 := a.AskIn("", "worker", "orange1", "how is the disk?")
	if r1.Conversation == "" || !r1.Failed {
		t.Fatalf("first: %+v", r1)
	}
	r2 := a.AskIn(r1.Conversation, "worker", "someone-else", "and memory?")
	if r2.Conversation != r1.Conversation {
		t.Fatalf("a follow-up started a new conversation: %q vs %q", r2.Conversation, r1.Conversation)
	}
	list := a.Conversations(ConversationFilter{})
	if len(list.Items) != 1 {
		t.Fatalf("items: %+v", list.Items)
	}
	c := list.Items[0]
	if c.ID != "direct:"+r1.Conversation || c.Kind != "worker" || c.With != "worker:orange1" || c.Turns != 4 || !c.Replyable {
		t.Fatalf("row: %+v", c)
	}
	if c.Title != "how is the disk?" {
		t.Fatalf("title %q", c.Title)
	}
	th, err := a.ConversationThread(c.ID)
	if err != nil || len(th.Turns) != 4 || th.Turns[0].Role != "user" || th.Turns[0].Content != "how is the disk?" || th.Turns[1].Kind != "error" {
		t.Fatalf("thread: %+v %v", th, err)
	}
	// Kept across a restart: a fresh process reads the file.
	directStore.chats = nil
	if n := len(a.Conversations(ConversationFilter{}).Items); n != 1 {
		t.Fatalf("after reload %d", n)
	}
}

func TestConversationsMergeTheKindsFilterByWhoAndSearchTheText(t *testing.T) {
	a := convTestApp(t)
	now := time.Now()
	end := now.Add(-time.Hour)
	s := a.cliStore()
	s.runs = map[string]*CLIRun{
		"1": {ID: "1", Thread: "t1", Agent: "claude", Prompt: "refactor the parser", Summary: "done, 3 files", Session: "s1", State: "done", Origin: "a page", Started: now.Add(-2 * time.Hour), Ended: &end},
		"2": {ID: "2", Thread: "t1", Agent: "claude", Prompt: "now add tests", Summary: "added", Session: "s1", State: "done", Origin: "a page", Started: now.Add(-30 * time.Minute)},
		"3": {ID: "3", Thread: "t2", Agent: "codex", Prompt: "no session here", State: "done", Origin: "a page", Started: now.Add(-3 * time.Hour)},
		"4": {ID: "4", Thread: "t3", Agent: "claude", Prompt: "the queen thinking", Queen: true, State: "done", Started: now},
		"5": {ID: "5", Thread: "t4", Agent: "claude", Prompt: "a hive order", State: "done", Origin: "hive task", Started: now},
	}
	a.AskIn("", "agent", "pi.sds-a", "what is the weather")

	all := a.Conversations(ConversationFilter{})
	if len(all.Items) != 3 {
		t.Fatalf("want 3 rows (queen runs and hive orders are not conversations): %+v", all.Items)
	}
	// Newest first: the direct chat (now), then t1's last run, then t2.
	if all.Items[0].Kind != "agent" || all.Items[1].ID != "cli:t1" || all.Items[2].ID != "cli:t2" {
		t.Fatalf("order: %s %s %s", all.Items[0].ID, all.Items[1].ID, all.Items[2].ID)
	}
	t1 := all.Items[1]
	if t1.Run != "2" || !t1.Replyable || t1.Turns != 4 || t1.Title != "refactor the parser" {
		t.Fatalf("t1: %+v", t1)
	}
	if t2 := all.Items[2]; t2.Replyable || !strings.Contains(t2.Why, "session") {
		t.Fatalf("a run with no session cannot be continued, and says why: %+v", t2)
	}
	if len(all.Parties) != 3 {
		t.Fatalf("parties: %+v", all.Parties)
	}

	if got := a.Conversations(ConversationFilter{With: "coding:claude"}); len(got.Items) != 1 || len(got.Parties) != 3 {
		t.Fatalf("by who: %+v", got)
	}
	// Search reads what was said, not just the title.
	if got := a.Conversations(ConversationFilter{Query: "added"}); len(got.Items) != 1 || got.Items[0].ID != "cli:t1" {
		t.Fatalf("search answer: %+v", got.Items)
	}
	if got := a.Conversations(ConversationFilter{Query: "WEATHER"}); len(got.Items) != 1 || got.Items[0].Kind != "agent" {
		t.Fatalf("search chat: %+v", got.Items)
	}
	if got := a.Conversations(ConversationFilter{Query: "nothing like this"}); len(got.Items) != 0 {
		t.Fatalf("search miss: %+v", got.Items)
	}

	th, err := a.ConversationThread("cli:t1")
	if err != nil || len(th.Turns) != 4 || th.Turns[1].Content != "done, 3 files" || th.Turns[2].Content != "now add tests" {
		t.Fatalf("thread: %+v %v", th.Turns, err)
	}
	if _, err := a.ConversationThread("nope:1"); err == nil {
		t.Fatal("unknown kind opened")
	}
}

func TestOnlyThePersonsOwnSessionsAreTheirConversations(t *testing.T) {
	for id, want := range map[string]bool{
		"hive-console": true, "hive-console-web": true, "abc-123": true,
		"hive:1234": false, "tile-x": false, "dashboard:a": false, "hive-other": false,
	} {
		if got := personalChat(id, "hello"); got != want {
			t.Errorf("%s: %v", id, got)
		}
	}
	if personalChat("x", "You hold a standing responsibility for …") {
		t.Error("a bee's wake-up is not a conversation")
	}
}
