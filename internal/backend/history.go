package backend

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/liliang-cn/agent-go/v3/pkg/domain"
	"github.com/liliang-cn/agent-go/v3/pkg/store"
)

// ChatSessionInfo is one past conversation, as listed in the history panel.
type ChatSessionInfo struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Turns     int    `json:"turns"`
	UpdatedAt string `json:"updated_at"`
	// Last is the final thing said, one line; Text is everything said, for
	// searching a list. Text never goes over the wire.
	Last string `json:"last,omitempty"`
	Text string `json:"-"`
}

// ChatTurn is a single visible message of a past conversation.
type ChatTurn struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	Emotion string `json:"emotion,omitempty"`
	// Kind marks a turn that is not part of the conversation proper.
	// "" is a normal message; TurnKindContext is what the agent was given to read
	// before answering — shown, but folded away, since it is neither a question
	// nor an answer.
	Kind string `json:"kind,omitempty"`
	// Steps is what the agent did on the way to this answer, one line each, for
	// the progress block a reopened conversation would otherwise lose.
	//
	// The scripts a turn ran are not shown as prose (see assistantText), and the
	// live progress block is built from stream events that are gone by the time
	// the conversation is reopened. Rather than drop the working entirely, the
	// transcript that is being hidden is summarised into these lines.
	Steps []string `json:"steps,omitempty"`
	// At is when the ask was made, or when the answer came (RFC 3339); empty
	// for turns from before times were kept. ElapsedMs is how long an answer
	// took. Filled in by the app layer, which records them.
	At        string `json:"at,omitempty"`
	ElapsedMs int64  `json:"elapsed_ms,omitempty"`
	// Engine is the agent CLI that thought for the queen in this answer
	// ("claude", "claude.mac"); empty is SuperLeo's own agent. CostUSD is what
	// that turn cost, when the CLI said. Filled in by the app layer.
	Engine  string  `json:"engine,omitempty"`
	CostUSD float64 `json:"cost_usd,omitempty"`
	// StandInFor is the queen engine SuperLeo's own agent answered for, when
	// it could not.
	StandInFor string `json:"stand_in_for,omitempty"`
}

// maxTitle caps how much of the opening message becomes a session title.
const maxTitle = 60

// TurnKindContext marks the memory the agent was handed before answering.
const TurnKindContext = "context"

// TurnKindInterim marks a standalone message the agent sent mid-turn via the
// notify_user tool — a real bubble, delivered before that ask's final answer.
const TurnKindInterim = "interim"

// memoryContextHeader opens the block the agent injects as a user message when
// long-term memory turns something up.
const memoryContextHeader = "--- Relevant Context From Memory ---"

// memoryContextBody drops the framing that block carries for the model's benefit
// and returns the markdown inside it.
//
// The header becomes the collapsed block's own label, and the closing "Use the
// context above…" is an instruction to the model, not something to read.
func memoryContextBody(content string) string {
	body := strings.TrimSpace(strings.TrimPrefix(content, memoryContextHeader))
	if i := strings.LastIndex(body, "Use the context above"); i >= 0 {
		body = strings.TrimSpace(body[:i])
	}
	return body
}

// Sessions lists past conversations, most recently updated first. Sessions
// whose visible turns are all injected context (a run that never produced a
// real exchange) are skipped — they would list as blank rows.
func (s *Service) Sessions(limit int) []ChatSessionInfo {
	if s == nil || s.svc == nil {
		return []ChatSessionInfo{}
	}
	if limit <= 0 {
		limit = 100
	}
	sessions, err := s.svc.ListSessions(limit)
	if err != nil {
		return []ChatSessionInfo{}
	}

	out := make([]ChatSessionInfo, 0, len(sessions))
	for _, sess := range sessions {
		if sess == nil {
			continue
		}
		turns := visibleTurns(sess.Messages)
		// Counted over what was actually said: a recalled-memory block is neither
		// a question nor an answer, and a conversation of nothing but context is
		// not a conversation.
		said := 0
		last := ""
		var text strings.Builder
		for _, t := range turns {
			if t.Kind == "" {
				said++
				last = t.Content
				text.WriteString(t.Content)
				text.WriteByte('\n')
			}
		}
		if said == 0 {
			continue
		}
		out = append(out, ChatSessionInfo{
			ID:        sess.ID,
			Title:     sessionTitle(turns),
			Turns:     said,
			UpdatedAt: sess.UpdatedAt.Format(time.RFC3339),
			Last:      previewLine(last, 160),
			Text:      text.String(),
		})
	}

	// ListSessions already orders by updated_at, but the filtering above can
	// only remove entries — re-sort so callers never depend on that detail.
	sort.SliceStable(out, func(i, j int) bool { return out[i].UpdatedAt > out[j].UpdatedAt })
	return out
}

// SessionTurns returns a past conversation's visible messages, ready to drop
// back into the transcript.
func (s *Service) SessionTurns(id string) []ChatTurn {
	if s == nil || s.svc == nil || strings.TrimSpace(id) == "" {
		return []ChatTurn{}
	}
	sess, err := s.svc.GetSession(id)
	if err != nil || sess == nil {
		return []ChatTurn{}
	}
	return visibleTurns(sess.Messages)
}

// DeleteSession removes a past conversation for good.
//
// agent.Service exposes ListSessions and GetSession but not a delete, and its
// store field is unexported — so this opens the same database directly. The
// delete cascades to chat_messages. A short-lived connection keeps this away
// from the handle the running agent holds; sqlite is in WAL mode, so the two
// coexist, and deleting a conversation is rare enough not to contend.
func (s *Service) DeleteSession(id string) error {
	if s == nil || strings.TrimSpace(id) == "" {
		return errors.New("no session id")
	}
	// The files stay on disk (they may be useful), but stop claiming they belong
	// to a conversation that no longer exists.
	if s.files != nil {
		s.files.forget(id)
	}
	path := filepath.Join(s.dataDir, "agentgo.db")
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("agent database: %w", err)
	}
	db, err := store.NewAgentGoDB(path)
	if err != nil {
		return fmt.Errorf("open agent database: %w", err)
	}
	defer db.Close()
	return db.DeleteSession(id)
}

// visibleTurns strips everything the user never typed or saw: tool traffic, and
// the context the agent injects as extra user messages (date reminders, skill
// discovery, memory recall). Those are real messages to the model but noise in
// a transcript.
func visibleTurns(messages []domain.Message) []ChatTurn {
	turns := make([]ChatTurn, 0, len(messages))
	// Steps collected from transcripts that were hidden, waiting for the answer
	// they led to.
	var pending []string
	// The calls a turn handed to workers, by id: their reports say who called
	// what, which is the step a reopened conversation should show.
	delegations := map[string]bool{}
	for i := range messages {
		role := messages[i].Role
		if role == "tool" && delegations[messages[i].ToolCallID] {
			pending = append(pending, DelegatedSteps(messages[i].Content)...)
			continue
		}
		if role != "user" && role != "assistant" {
			continue
		}
		for _, c := range messages[i].ToolCalls {
			if c.Function.Name == DelegateToolName && c.ID != "" {
				delegations[c.ID] = true
			}
		}
		// An assistant turn that only carried tool calls has no text to show —
		// but it is the record of the agent working, so it becomes progress.
		// Except notify_user: its argument IS a message the user already saw
		// live, so it is restored as an interim bubble, not a step line.
		// Text written beside a tool call is the model working ("Need to find
		// the mac worker. Call hive_members?"), not something said to the
		// person, so it goes with the calls; only a turn that merely ends the
		// run (task_complete) can carry the answer.
		if len(messages[i].ToolCalls) > 0 && (strings.TrimSpace(messages[i].Content) == "" || callsWork(messages[i].ToolCalls)) {
			rest := make([]domain.ToolCall, 0, len(messages[i].ToolCalls))
			for _, c := range messages[i].ToolCalls {
				if c.Function.Name == "notify_user" {
					if msg, _ := c.Function.Arguments["message"].(string); strings.TrimSpace(msg) != "" {
						turns = append(turns, ChatTurn{Role: "assistant", Kind: TurnKindInterim, Content: strings.TrimSpace(msg)})
					}
					continue
				}
				rest = append(rest, c)
			}
			pending = append(pending, toolCallSteps(rest)...)
			continue
		}
		content := strings.TrimSpace(messages[i].Content)
		if content == "" || isInjectedContext(content) {
			continue
		}
		// Recalled memory is worth being able to look at — it is why the answer
		// says what it says — so it is kept as its own folded block rather than
		// dropped like the rest of the machinery or shown as a message the user
		// supposedly typed. Not a new question, so the pending steps stand.
		if role == "user" && strings.HasPrefix(content, memoryContextHeader) {
			if body := memoryContextBody(content); body != "" {
				turns = append(turns, ChatTurn{Role: role, Kind: TurnKindContext, Content: body})
			}
			continue
		}
		turn := ChatTurn{Role: role, Content: content}
		if role == "assistant" {
			content = assistantText(messages, i)
			if content == "" {
				// Hidden, but not for nothing: what it did becomes the progress
				// block on whichever answer comes next.
				pending = append(pending, scriptSteps(messages[i].Content)...)
				continue
			}
			turn.Content, turn.Emotion = SplitEmotion(content)
			if strings.TrimSpace(turn.Content) == "" {
				pending = append(pending, scriptSteps(messages[i].Content)...)
				continue
			}
			if len(pending) > 0 {
				turn.Steps = pending
				pending = nil
			}
		} else {
			// A new question ends the previous turn; working with no answer to
			// attach to is dropped rather than migrated onto the next one.
			pending = nil
		}
		turns = append(turns, turn)
	}
	return turns
}

// toolScript matches one <code>…</code> block — the agent's programmatic
// tool-calling protocol, not markdown. A fenced ```js block is a real reply
// showing code and must survive.
var toolScript = regexp.MustCompile(`(?s)<code>.*?</code>`)

// halfScript matches a transcript whose other tag went missing. Stored turns
// really do end up with a closing </code> and no opening one — the tag is lost
// on the way through the stream — and the result reads as `const res =
// callTool(…)` with no clue that it was ever a script.
var halfScript = regexp.MustCompile(`(?s)^.*?</code>|<code>.*$`)

// internalDirective matches the lines the runtime appends for the model's own
// benefit: the between-rounds word limit, and a described tool result. Neither is
// something the assistant said to anyone.
var internalDirective = regexp.MustCompile(`^(?i)(Constraint text:|(Object|Array) containing \d+ items?)`)

// callTool matches a tool call inside a script: callTool('name', …).
var callTool = regexp.MustCompile(`callTool\(\s*['"]([A-Za-z0-9_.-]+)['"]`)

// toolCallSteps summarises the tool calls an assistant turn carried.
//
// This is where the working actually lives on the streaming path: the turn's
// text is empty and the calls hang off the message. A programmatic call is one
// execute_javascript whose code calls the real tools, so naming that would say
// "Called execute_javascript" for every step of every run — the names worth
// showing are the ones inside the script.
func toolCallSteps(calls []domain.ToolCall) []string {
	out := []string{}
	add := func(line string) {
		if len(out) > 0 && out[len(out)-1] == line {
			return // a loop over one tool reads as one step
		}
		out = append(out, line)
	}
	for _, c := range calls {
		// The stored argument is the script itself, with no <code> wrapper.
		code, _ := c.Function.Arguments["code"].(string)
		inner := callTool.FindAllStringSubmatch(code, -1)
		for _, m := range inner {
			add("Called " + m[1])
		}
		if len(inner) > 0 {
			continue
		}
		if strings.TrimSpace(code) != "" {
			add("Ran code") // a script that computed rather than called anything
			continue
		}
		if name := strings.TrimSpace(c.Function.Name); name != "" {
			add("Called " + name)
		}
	}
	return out
}

// scriptSteps summarises a hidden transcript as progress lines.
//
// What is worth keeping is which tools ran, in order — the same thing the live
// progress block shows. The script itself is not: it is a means, and reading
// someone else's generated JavaScript a day later tells you nothing that
// "Called read_document" does not.
func scriptSteps(raw string) []string {
	scripts := toolScript.FindAllString(raw, -1)
	if len(scripts) == 0 && strings.Contains(raw, "</code>") {
		scripts = []string{raw}
	}
	out := make([]string, 0, len(scripts))
	for _, s := range scripts {
		for _, m := range callTool.FindAllStringSubmatch(s, -1) {
			line := "Called " + m[1]
			// A loop calling the same tool repeatedly reads as one step.
			if len(out) > 0 && out[len(out)-1] == line {
				continue
			}
			out = append(out, line)
		}
	}
	if len(out) == 0 && len(scripts) > 0 {
		// A script that computed rather than called anything still happened.
		out = append(out, "Ran code")
	}
	return out
}

// assistantText decides what one assistant message contributes to a readable
// transcript, returning "" for the ones that contribute nothing.
//
// A PTC turn is stored as the model wrote it, script included, because that is
// the history the model needs next turn — and it is not only the script: the
// tool's results are pasted in around it, so a stripped transcript is still a
// page of "Object containing 2 items:" describing a search result set. The agent
// normally records the answer as its own message straight afterwards, so the
// whole transcript can go.
//
// "Normally" is why this looks ahead instead of assuming: a turn from before the
// agent learned to summarise its own runs can be the script *and* the only copy
// of the answer. Dropping that would erase the reply from the user's history, so
// when nothing follows it the scripts are merely stripped out.
// callsWork says whether a turn's tool calls do something, rather than only
// end the run.
func callsWork(calls []domain.ToolCall) bool {
	for _, c := range calls {
		switch c.Function.Name {
		case "task_complete", "task_blocked":
		default:
			return true
		}
	}
	return false
}

func assistantText(messages []domain.Message, i int) string {
	content := strings.TrimSpace(messages[i].Content)
	if strings.Contains(content, "<code>") || strings.Contains(content, "</code>") {
		if answerFollows(messages, i) {
			return ""
		}
		content = strings.TrimSpace(toolScript.ReplaceAllString(content, "\n"))
		content = strings.TrimSpace(halfScript.ReplaceAllString(content, "\n"))
		// Salvaging turns up one of two things: the reply the model wrote, or the
		// machinery that was pasted around the script. Only the first is worth
		// showing, and what separates them is how it opens.
		if internalDirective.MatchString(content) {
			return ""
		}
	}
	// A bare payload is not something anyone said either.
	if (strings.HasPrefix(content, "{") && strings.HasSuffix(content, "}")) ||
		(strings.HasPrefix(content, "[") && strings.HasSuffix(content, "]")) {
		return ""
	}
	return content
}

// answerFollows reports whether the answer to the transcript at i was recorded
// as its own message — which is what makes dropping the transcript lossless.
//
// Only the next message counts. The agent appends the answer immediately after
// the turn it summarises, and a session that collects many runs (the one every
// schedule writes to) is not reliably ordered beyond that, so scanning further
// would find some other run's answer and call this one redundant.
func answerFollows(messages []domain.Message, i int) bool {
	for j := i + 1; j < len(messages); j++ {
		if messages[j].Role == "tool" {
			continue // tool traffic sits between the two and is never shown
		}
		if messages[j].Role != "assistant" {
			return false
		}
		text := strings.TrimSpace(messages[j].Content)
		if toolScript.MatchString(text) {
			return false // another transcript, not an answer
		}
		body, _ := SplitEmotion(text)
		return strings.TrimSpace(body) != ""
	}
	return false
}

// isInjectedContext reports whether a "user" message was actually added by the
// agent rather than typed by the person.
func isInjectedContext(content string) bool {
	return strings.HasPrefix(content, "<system-reminder>") ||
		strings.HasPrefix(content, "<skill-discovery>") ||
		strings.HasPrefix(content, "<memory-recall>") ||
		// The runtime's own nudge between tool rounds, addressed to the model as a
		// user message. A tool-heavy turn accumulates one per round, so reopening
		// such a conversation showed this six times over as if the person had
		// typed it.
		strings.HasPrefix(content, "Analyze the tool results above.")
}

// sessionTitle names a conversation after its opening question.
func sessionTitle(turns []ChatTurn) string {
	for _, t := range turns {
		// Recalled memory arrives as a user message, and naming a conversation
		// "## Memory Index" tells nobody which conversation it was.
		if t.Role != "user" || t.Kind != "" {
			continue
		}
		// Attachment preambles bury the actual question on a later line.
		title := t.Content
		if strings.HasPrefix(title, "[") {
			if lines := strings.Split(title, "\n"); len(lines) > 0 {
				title = strings.TrimSpace(lines[len(lines)-1])
			}
		}
		title = strings.Join(strings.Fields(title), " ")
		if title == "" {
			continue
		}
		if len([]rune(title)) > maxTitle {
			title = string([]rune(title)[:maxTitle]) + "…"
		}
		return title
	}
	return "Untitled"
}

// previewLine is the first non-empty line of s, cut to n runes.
func previewLine(s string, n int) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			if r := []rune(l); len(r) > n {
				return string(r[:n]) + "…"
			}
			return l
		}
	}
	return ""
}
