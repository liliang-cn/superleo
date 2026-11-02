package app

import "github.com/liliang-cn/superleo/internal/backend"

// ChatPage is one page of a conversation, see ChatHistoryPage.
type ChatPage struct {
	Turns []backend.ChatTurn `json:"turns"`
	Start int                `json:"start"`
	Total int                `json:"total"`
	More  bool               `json:"more"`
}

const (
	defaultPageTurns = 40
	maxPageTurns     = 200
)

// pageTurns cuts a page out of a conversation. Indexes count the turns that
// remain once the agent's own reading (TurnKindContext) is dropped, so they
// do not move when the conversation grows at its end.
func pageTurns(all []backend.ChatTurn, before, limit int) ChatPage {
	if limit <= 0 {
		limit = defaultPageTurns
	}
	if limit > maxPageTurns {
		limit = maxPageTurns
	}
	turns := make([]backend.ChatTurn, 0, len(all))
	for _, t := range all {
		if t.Kind != backend.TurnKindContext {
			turns = append(turns, t)
		}
	}
	end := before
	if end < 0 || end > len(turns) {
		end = len(turns)
	}
	start := end - limit
	if start < 0 {
		start = 0
	}
	// Begin on a question, so a page never opens on half an exchange; the
	// search is bounded so a long run of answers cannot swell the page.
	for floor := start - limit; start > 0 && start > floor && !(turns[start].Role == "user" && turns[start].Kind == ""); start-- {
	}
	return ChatPage{Turns: turns[start:end:end], Start: start, Total: len(turns), More: start > 0}
}
