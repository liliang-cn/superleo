package app

// Every conversation the person has had, in one list.
//
// They live in four places: the queen's sessions (agent-go's session store),
// the person's direct chats with a worker or a named agent (directchats.go,
// kept here because AskRemoteAgent answers once and keeps nothing), the coding
// runs a page or an @mention started (cli-runs.json) and the terminal
// sessions the CLIs' hooks report. Conversations merges them, newest first;
// ConversationThread opens one whole.
//
// Continuing a conversation is not done here: the queen's goes through
// SendChat, a direct chat through AskIn, a coding run through FollowUpCLIRun
// and a terminal session through ReplyToCodingSession. The thread says which
// applies (Conversation.Kind / Source / Replyable).

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/liliang-cn/superleo/internal/backend"
)

// Conversation kinds.
const (
	ConvQueen  = "queen"  // with the queen, in one of her sessions
	ConvWorker = "worker" // asked directly: a worker of the hive
	ConvAgent  = "agent"  // asked directly: a named agent on a linked machine
	ConvCoding = "coding" // a coding CLI's session
)

// Conversation is one row of the list.
type Conversation struct {
	// ID is what ConversationThread opens: "queen:<session>", "direct:<id>",
	// "cli:<thread>" or "term:<machine>|<cli>|<session>".
	ID   string `json:"id"`
	Kind string `json:"kind"`
	// With is who it was with, as a filter key: "queen", "worker:<name>",
	// "agent:<name>" or "coding:<cli>". Name is the same for people to read;
	// empty for the queen, whose name the client knows.
	With    string `json:"with"`
	Name    string `json:"name,omitempty"`
	Machine string `json:"machine,omitempty"`
	Title   string `json:"title"`
	// Last is the final thing said, one line.
	Last  string `json:"last,omitempty"`
	Turns int    `json:"turns"`
	// At is when it was last touched (RFC 3339).
	At string `json:"at"`
	// State is "running" while a turn is going.
	State string `json:"state,omitempty"`
	// Source of a coding conversation: "superleo" (a run) or "terminal".
	Source string `json:"source,omitempty"`
	// Session is the underlying id: the chat session, the direct chat, the CLI
	// thread, or the CLI's own session.
	Session string `json:"session"`
	// Run is a coding thread's newest run, which a follow-up resumes; Agent
	// and Cwd are what a fresh conversation with the same agent starts from.
	Run   string `json:"run,omitempty"`
	Agent string `json:"agent,omitempty"`
	Cwd   string `json:"cwd,omitempty"`
	// Replyable says whether the person can keep talking in it; Why says what
	// stops them when not.
	Replyable bool   `json:"replyable"`
	Why       string `json:"why,omitempty"`

	text string // everything said, lower-cased, for searching
}

// ConversationParty is one who-filter: somebody conversations were had with.
type ConversationParty struct {
	With    string `json:"with"`
	Kind    string `json:"kind"`
	Name    string `json:"name,omitempty"`
	Machine string `json:"machine,omitempty"`
	Count   int    `json:"count"`
}

// ConversationFilter narrows the list. Both parts are optional.
type ConversationFilter struct {
	// With is a Conversation.With key.
	With string `json:"with"`
	// Query is searched for in the title, who it was with, and everything said.
	Query string `json:"query"`
	Limit int    `json:"limit"`
}

// ConversationList is the page: who there is to filter by (counted before the
// filter, so the choices stay while one is picked) and the matching rows.
type ConversationList struct {
	Parties []ConversationParty `json:"parties"`
	Items   []Conversation      `json:"items"`
}

// ConversationThread is one conversation, whole.
type ConversationThread struct {
	Conversation Conversation       `json:"conversation"`
	Turns        []backend.ChatTurn `json:"turns"`
}

// Conversations lists every conversation, newest first.
func (a *App) Conversations(f ConversationFilter) ConversationList {
	all := a.allConversations()
	sort.SliceStable(all, func(i, j int) bool { return all[i].At > all[j].At })

	parties := map[string]*ConversationParty{}
	var order []string
	for _, c := range all {
		p := parties[c.With]
		if p == nil {
			p = &ConversationParty{With: c.With, Kind: c.Kind, Name: c.Name, Machine: c.Machine}
			parties[c.With] = p
			order = append(order, c.With)
		}
		p.Count++
	}
	out := ConversationList{Parties: []ConversationParty{}, Items: []Conversation{}}
	for _, k := range order {
		out.Parties = append(out.Parties, *parties[k])
	}
	// The queen first, then by how much was said.
	sort.SliceStable(out.Parties, func(i, j int) bool {
		a, b := out.Parties[i], out.Parties[j]
		if (a.Kind == ConvQueen) != (b.Kind == ConvQueen) {
			return a.Kind == ConvQueen
		}
		return a.Count > b.Count
	})

	q := strings.ToLower(strings.TrimSpace(f.Query))
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	for _, c := range all {
		if f.With != "" && c.With != f.With {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(c.Title+"\n"+c.Name+"\n"+c.Machine+"\n"+c.Last)+"\n"+c.text, q) {
			continue
		}
		out.Items = append(out.Items, c)
		if len(out.Items) >= limit {
			break
		}
	}
	return out
}

func (a *App) allConversations() []Conversation {
	var all []Conversation
	all = append(all, a.queenConversations()...)
	all = append(all, directConversations()...)
	all = append(all, a.codingConversations()...)
	return all
}

func lowerAll(parts ...string) string { return strings.ToLower(strings.Join(parts, "\n")) }

// queenConversations are the person's own sessions: the queen's console and
// the chats started beside it. What the hive and the screens keep for
// themselves (orders, tiles, dashboards, a bee's wake-up) is not theirs.
func (a *App) queenConversations() []Conversation {
	var out []Conversation
	for _, s := range a.ChatSessions() {
		if !personalChat(s.ID, s.Title) {
			continue
		}
		out = append(out, Conversation{
			ID: "queen:" + s.ID, Kind: ConvQueen, With: "queen",
			Title: s.Title, Last: s.Last, Turns: s.Turns, At: s.UpdatedAt,
			Session: s.ID, Replyable: true, text: strings.ToLower(s.Text),
		})
	}
	return out
}

func personalChat(id, title string) bool {
	for _, p := range []string{"tile-", "hive-", backend.HiveSessionPrefix, "dashboard:"} {
		// The old per-device consoles are the person's.
		if strings.HasPrefix(id, p) && !strings.HasPrefix(id, "hive-console") {
			return false
		}
	}
	return !strings.HasPrefix(title, "You hold a standing responsibility")
}

// --- direct chats ---

type directTurn struct {
	Role   string    `json:"role"` // user | assistant
	Text   string    `json:"text"`
	At     time.Time `json:"at"`
	Failed bool      `json:"failed,omitempty"`
	TaskID string    `json:"task_id,omitempty"`
	Tools  []string  `json:"tools,omitempty"`
}

type directChat struct {
	ID      string       `json:"id"`
	Kind    string       `json:"kind"` // worker | agent
	Name    string       `json:"name"`
	Machine string       `json:"machine,omitempty"`
	Updated time.Time    `json:"updated"`
	Turns   []directTurn `json:"turns"`
}

const (
	directChatsKept = 300
	directTurnMax   = 20000
)

// directStore is the person's direct chats, in direct-chats.json. One per
// process, re-read when the data directory is not the one it was read from
// (a test with its own home).
var directStore struct {
	mu    sync.Mutex
	path  string
	chats map[string]*directChat
}

func directChatsPath() string { return filepath.Join(backend.DataDir(), "direct-chats.json") }

// directLocked returns the loaded store; the caller holds directStore.mu.
func directLocked() map[string]*directChat {
	p := directChatsPath()
	if directStore.chats != nil && directStore.path == p {
		return directStore.chats
	}
	directStore.path, directStore.chats = p, map[string]*directChat{}
	if raw, err := os.ReadFile(p); err == nil {
		var list []*directChat
		if json.Unmarshal(raw, &list) == nil {
			for _, c := range list {
				if c != nil && c.ID != "" {
					directStore.chats[c.ID] = c
				}
			}
		}
	}
	return directStore.chats
}

func saveDirectLocked() {
	list := make([]*directChat, 0, len(directStore.chats))
	for _, c := range directStore.chats {
		list = append(list, c)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Updated.After(list[j].Updated) })
	if len(list) > directChatsKept {
		for _, c := range list[directChatsKept:] {
			delete(directStore.chats, c.ID)
		}
		list = list[:directChatsKept]
	}
	raw, err := json.Marshal(list)
	if err != nil {
		return
	}
	tmp := directStore.path + ".tmp"
	if os.WriteFile(tmp, raw, 0o600) == nil {
		_ = os.Rename(tmp, directStore.path)
	}
}

func directConversation(c *directChat) Conversation {
	var said int
	var last string
	var text strings.Builder
	title := ""
	for _, t := range c.Turns {
		said++
		last = t.Text
		text.WriteString(t.Text)
		text.WriteByte('\n')
		if title == "" && t.Role == "user" {
			title = t.Text
		}
	}
	return Conversation{
		ID: "direct:" + c.ID, Kind: c.Kind, With: c.Kind + ":" + c.Name, Name: c.Name, Machine: c.Machine,
		Title: cliClip(strings.Join(strings.Fields(title), " "), 80), Last: cliClip(strings.Join(strings.Fields(last), " "), 160),
		Turns: said, At: c.Updated.Format(time.RFC3339), Session: c.ID, Replyable: true,
		text: strings.ToLower(text.String()),
	}
}

func directConversations() []Conversation {
	directStore.mu.Lock()
	defer directStore.mu.Unlock()
	out := []Conversation{}
	for _, c := range directLocked() {
		out = append(out, directConversation(c))
	}
	return out
}

// DirectReply is an agent's answer in a direct chat.
type DirectReply struct {
	// Conversation is the chat's id: pass it back to continue.
	Conversation string   `json:"conversation"`
	Text         string   `json:"text"`
	Failed       bool     `json:"failed"`
	Tools        []string `json:"tools,omitempty"`
	TaskID       string   `json:"task_id,omitempty"`
	Host         string   `json:"host,omitempty"`
}

// AskIn asks a worker or a named agent something in a conversation that is
// kept: conv "" starts one, an id continues it, and the words and the answer
// are written down either way, so the conversation can be opened again from
// the list. kind is "worker" or "agent"; the agent is told it is the person
// asking, as AskRemoteAgent does, and is given what was said before.
func (a *App) AskIn(conv, kind, name, message string) DirectReply {
	name, message = strings.TrimSpace(name), strings.TrimSpace(message)
	if kind != ConvWorker {
		kind = ConvAgent
	}
	conv = strings.TrimSpace(conv)

	directStore.mu.Lock()
	chat := directLocked()[conv]
	if chat == nil {
		conv = uuid.NewString()
		chat = &directChat{ID: conv, Kind: kind, Name: name}
		directLocked()[conv] = chat
	}
	history := make([]directTurn, 0, len(chat.Turns))
	for _, t := range chat.Turns {
		if !t.Failed {
			history = append(history, t)
		}
	}
	name = chat.Name
	directStore.mu.Unlock()

	prompt := message
	if len(history) > 0 {
		var b strings.Builder
		for _, t := range history {
			who := "User"
			if t.Role == "assistant" {
				who = "Assistant"
			}
			b.WriteString(who + ": " + t.Text + "\n\n")
		}
		prompt = "Previous conversation:\n" + b.String() + "User: " + message + "\n\nContinue the conversation and answer the latest user message."
	}

	start := time.Now()
	res := a.askAgent(backend.AskedDirectly(context.Background()), name, prompt)
	var tools []string
	if res.TaskID != "" {
		if t, ok := a.tasks().Get(res.TaskID); ok {
			for _, e := range t.Events {
				if e.Phase == backend.PhaseTool {
					tools = append(tools, firstNonEmpty(e.Tool, "tool"))
				}
			}
		}
	}
	reply := DirectReply{Conversation: conv, Text: strings.TrimSpace(res.Text), Failed: res.Failed, Tools: tools, TaskID: res.TaskID, Host: res.Host}
	if res.Failed {
		reply.Text = firstNonEmpty(res.Reason, "failed")
	}

	now := time.Now()
	directStore.mu.Lock()
	defer directStore.mu.Unlock()
	chat = directLocked()[conv]
	if chat == nil { // pruned while the answer was coming
		chat = &directChat{ID: conv, Kind: kind, Name: name}
		directLocked()[conv] = chat
	}
	if chat.Machine == "" && res.Host != "" {
		chat.Machine = res.Host
	}
	chat.Turns = append(chat.Turns,
		directTurn{Role: "user", Text: clipText(message, directTurnMax), At: start},
		directTurn{Role: "assistant", Text: clipText(reply.Text, directTurnMax), At: now, Failed: res.Failed, TaskID: res.TaskID, Tools: tools})
	chat.Updated = now
	saveDirectLocked()
	return reply
}

// --- coding ---

// personsRun says whether the person had a hand in a run: started from a page
// or an @mention, as opposed to one the queen or a hive order handed out.
func personsRun(r CLIRun) bool {
	return !r.Queen && (r.Origin == "" || r.Origin == "a page" || strings.HasPrefix(r.Origin, "@"))
}

// cliThreadsOf groups the person's runs by thread, each oldest first.
func (a *App) cliThreadsOf() map[string][]CLIRun {
	by := map[string][]CLIRun{}
	for _, r := range a.CLIRuns() {
		if personsRun(r) {
			by[r.Thread] = append(by[r.Thread], r)
		}
	}
	for _, rs := range by {
		sort.Slice(rs, func(i, j int) bool { return rs[i].Started.Before(rs[j].Started) })
	}
	return by
}

func runAnswer(r CLIRun) string {
	return firstNonEmpty(r.Summary, r.Error)
}

func codingFromThread(thread string, rs []CLIRun) Conversation {
	first, last := rs[0], rs[len(rs)-1]
	at := last.Started
	if last.Ended != nil {
		at = *last.Ended
	}
	var said int
	var text strings.Builder
	lastSaid := ""
	for _, r := range rs {
		said++
		text.WriteString(r.Prompt + "\n")
		if ans := runAnswer(r); ans != "" {
			said++
			text.WriteString(ans + "\n")
			lastSaid = ans
		}
	}
	if lastSaid == "" {
		lastSaid = last.Prompt
	}
	c := Conversation{
		ID: "cli:" + thread, Kind: ConvCoding, With: "coding:" + first.Agent, Name: first.Agent, Machine: first.Remote,
		Title: cliClip(strings.Join(strings.Fields(first.Prompt), " "), 80), Last: cliClip(strings.Join(strings.Fields(lastSaid), " "), 160),
		Turns: said, At: at.Format(time.RFC3339), Source: "superleo", Session: thread,
		Run: last.ID, Agent: first.Agent, Cwd: first.Cwd, text: strings.ToLower(text.String()),
	}
	switch {
	case last.State == "running":
		c.State = "running"
		c.Why = "the last turn is still running"
	case last.Session == "" && last.RemoteRun == "":
		c.Why = last.Agent + " did not report a session to continue"
	default:
		c.Replyable = true
	}
	return c
}

func termID(machine, cli, session string) string {
	return "term:" + machine + "|" + cli + "|" + session
}

func (a *App) codingConversations() []Conversation {
	var out []Conversation
	for thread, rs := range a.cliThreadsOf() {
		out = append(out, codingFromThread(thread, rs))
	}
	for _, s := range a.CodingSessions() {
		if s.Source != "terminal" {
			continue
		}
		c := Conversation{
			ID: termID(s.Machine, s.CLI, s.Session), Kind: ConvCoding, With: "coding:" + s.CLI, Name: s.CLI, Machine: s.Machine,
			Title: firstNonEmpty(s.Project, s.CLI+" session"), At: s.At.Format(time.RFC3339), State: "", Source: "terminal",
			Session: s.Session, Agent: s.CLI, Replyable: s.Replyable,
			text: lowerAll(s.Project, s.Title),
		}
		if s.State == "running" || s.State == "working" {
			c.State = "running"
		}
		if !s.Readable {
			c.Why = "its log cannot be read here"
		}
		if !s.Replyable {
			c.Why = firstNonEmpty(c.Why, "the session has ended")
		}
		out = append(out, c)
	}
	return out
}

// ConversationThread opens one conversation from the list whole.
func (a *App) ConversationThread(id string) (ConversationThread, error) {
	kind, rest, _ := strings.Cut(strings.TrimSpace(id), ":")
	switch kind {
	case "queen":
		for _, c := range a.queenConversations() {
			if c.ID == id {
				return ConversationThread{Conversation: c, Turns: visibleChat(a.ChatHistory(rest))}, nil
			}
		}
		// Not in the list yet (a first turn still being answered): still its own.
		c := Conversation{ID: id, Kind: ConvQueen, With: "queen", Session: rest, Replyable: true, At: time.Now().Format(time.RFC3339)}
		return ConversationThread{Conversation: c, Turns: visibleChat(a.ChatHistory(rest))}, nil
	case "direct":
		directStore.mu.Lock()
		defer directStore.mu.Unlock()
		chat := directLocked()[rest]
		if chat == nil {
			return ConversationThread{}, errors.New("no such conversation")
		}
		turns := make([]backend.ChatTurn, 0, len(chat.Turns))
		for _, t := range chat.Turns {
			ct := backend.ChatTurn{Role: t.Role, Content: t.Text, At: t.At.Format(time.RFC3339), Steps: t.Tools}
			if t.Failed {
				ct.Kind = "error"
			}
			turns = append(turns, ct)
		}
		return ConversationThread{Conversation: directConversation(chat), Turns: turns}, nil
	case "cli":
		rs := a.cliThreadsOf()[rest]
		if len(rs) == 0 {
			return ConversationThread{}, errors.New("no such conversation")
		}
		var turns []backend.ChatTurn
		for _, r := range rs {
			turns = append(turns, backend.ChatTurn{Role: "user", Content: r.Prompt, At: r.Started.Format(time.RFC3339)})
			if r.State == "running" {
				continue
			}
			at := r.Started
			if r.Ended != nil {
				at = *r.Ended
			}
			ans := backend.ChatTurn{Role: "assistant", Content: firstNonEmpty(runAnswer(r), r.State), At: at.Format(time.RFC3339)}
			if r.State != "done" {
				ans.Kind = "error"
			}
			if d, err := a.CLIRunDetail(r.ID); err == nil {
				ans.Steps = cliSteps(d.Events)
				if ans.Content == r.State {
					for i := len(d.Events) - 1; i >= 0; i-- {
						if d.Events[i].Kind == "text" && strings.TrimSpace(d.Events[i].Text) != "" {
							ans.Content = d.Events[i].Text
							break
						}
					}
				}
			}
			turns = append(turns, ans)
		}
		return ConversationThread{Conversation: codingFromThread(rest, rs), Turns: turns}, nil
	case "term":
		parts := strings.SplitN(rest, "|", 3)
		if len(parts) != 3 {
			return ConversationThread{}, errors.New("not a conversation")
		}
		machine, cli, session := parts[0], parts[1], parts[2]
		var conv Conversation
		for _, c := range a.codingConversations() {
			if c.ID == id {
				conv = c
			}
		}
		if conv.ID == "" {
			conv = Conversation{ID: id, Kind: ConvCoding, With: "coding:" + cli, Name: cli, Machine: machine, Source: "terminal", Session: session, Agent: cli}
		}
		log, err := a.CodingSessionLog(machine, cli, session)
		if err != nil {
			return ConversationThread{Conversation: conv}, err
		}
		return ConversationThread{Conversation: conv, Turns: logTurns(log.Events)}, nil
	}
	return ConversationThread{}, errors.New("not a conversation")
}

// visibleChat drops what the queen was handed to read; the rest is the
// conversation.
func visibleChat(turns []backend.ChatTurn) []backend.ChatTurn {
	out := make([]backend.ChatTurn, 0, len(turns))
	for _, t := range turns {
		if t.Kind != backend.TurnKindContext {
			out = append(out, t)
		}
	}
	return out
}

// cliSteps are the tools a run called, one line each, runs of the same tool
// folded.
func cliSteps(events []CLIRunEvent) []string {
	var steps []string
	for _, e := range events {
		if e.Kind != "tool" {
			continue
		}
		line := e.Tool
		if d := strings.TrimSpace(e.Detail); d != "" {
			line += " " + cliClip(strings.Join(strings.Fields(d), " "), 80)
		}
		steps = append(steps, line)
	}
	if len(steps) > 40 {
		steps = append(steps[:40], "…")
	}
	return steps
}

// logTurns reads a terminal session's log as a conversation: what the person
// typed, then what the agent said and did before the next prompt.
func logTurns(events []CLIRunEvent) []backend.ChatTurn {
	var turns []backend.ChatTurn
	var steps []string
	at := func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.Format(time.RFC3339)
	}
	for _, e := range events {
		switch e.Kind {
		case "you":
			turns = append(turns, backend.ChatTurn{Role: "user", Content: e.Text, At: at(e.At)})
			steps = nil
		case "tool":
			steps = append(steps, cliSteps([]CLIRunEvent{e})...)
		case "text":
			turns = append(turns, backend.ChatTurn{Role: "assistant", Content: e.Text, At: at(e.At), Steps: steps})
			steps = nil
		}
	}
	return turns
}
