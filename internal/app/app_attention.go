package app

// What deserves the person's eye, in one list: what needs them (an approval
// waiting, a bee asking, an order that failed, a worker gone quiet) and what
// is coming up (a meeting, a reminder). Every screen asks this same question,
// so the answer is made here, once, from what the core already knows.

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// AttentionItem is one thing for the person.
type AttentionItem struct {
	// Level is "needs" (act on it) or "soon" (know about it).
	Level string `json:"level"`
	// Kind: approval, bee (waiting on a reply), report (a bee told you something), failed, lost, run, event, reminder.
	Kind   string     `json:"kind"`
	Title  string     `json:"title"`
	Detail string     `json:"detail,omitempty"`
	At     *time.Time `json:"at,omitempty"`
	// Zone is the event's own offset and place, when it was set in one
	// ("+02:00", "Vienna"), so a screen can show both its time and yours.
	Zone  string `json:"zone,omitempty"`
	Place string `json:"place,omitempty"`
	// Ref is the id of the thing (approval, task, run, schedule); Open says
	// which screen it lives on.
	Ref  string `json:"ref,omitempty"`
	Open string `json:"open,omitempty"`
}

// Attention lists what needs the person now, then what is coming up in the
// next week, soonest first.
func (a *App) Attention() []AttentionItem {
	now := time.Now()
	var needs, soon []AttentionItem

	for _, ap := range a.PendingToolApprovals() {
		who := fmt.Sprint(ap["by"])
		if who == "" || who == "<nil>" {
			who = "SuperLeo"
			// A coding agent's prompt names it in the tool: "claude.mac · Bash".
			if tool := fmt.Sprint(ap["tool"]); strings.Contains(tool, " · ") {
				who = strings.SplitN(tool, " · ", 2)[0]
			}
		}
		what := fmt.Sprint(ap["command"])
		if what == "" || what == "<nil>" {
			what = fmt.Sprint(ap["tool"])
		}
		needs = append(needs, AttentionItem{Level: "needs", Kind: "approval", Title: who + " asks to run something",
			Detail: firstLineOf(what, 120), Ref: fmt.Sprint(ap["id"]), Open: "approval"})
	}

	for _, b := range a.StandingAgents() {
		if b.WaitingFor != "" && !b.Paused {
			needs = append(needs, AttentionItem{Level: "needs", Kind: "bee", Title: b.Name + " is waiting for you",
				Detail: firstLineOf(b.WaitingFor, 140), Ref: b.ID, Open: "agents"})
		}
	}

	dayAgo := now.Add(-24 * time.Hour)

	// What a bee told the person in the last day: it speaks only when
	// something needs them, so its message is a thing to look at. Only its
	// latest, newest first: a bee on a half-hourly patrol that reports every
	// time would otherwise fill the list with one finding said forty ways.
	told := map[string]bool{}
	for _, r := range a.StandingReports("") {
		if r.Kind != "message" || r.At.Before(dayAgo) || told[r.Agent] {
			continue
		}
		told[r.Agent] = true
		at := r.At
		needs = append(needs, AttentionItem{Level: "needs", Kind: "report", Title: r.Name,
			Detail: firstLineOf(r.Message, 160), At: &at, Ref: r.Agent, Open: "agents"})
	}
	for _, t := range a.tasks().Recent() {
		if t.State == "failed" && t.EndedAt.After(dayAgo) {
			at := t.EndedAt
			needs = append(needs, AttentionItem{Level: "needs", Kind: "failed", Title: t.Worker + " could not finish an order",
				Detail: firstLineOf(t.Prompt, 140), At: &at, Ref: t.ID, Open: "hive"})
		}
	}

	a.mu.Lock()
	h := a.hive
	a.mu.Unlock()
	if h != nil {
		for _, m := range h.Members() {
			if m.State == "lost" {
				seen := m.LastSeen
				needs = append(needs, AttentionItem{Level: "needs", Kind: "lost", Title: m.Name + " is not answering",
					Detail: "Last heard " + ago(now, seen), At: &seen, Ref: m.Name, Open: "hive"})
			}
		}
	}

	// A worker's pod also dials the agent link; the roster above already
	// says when one is not answering.
	role := a.agents().declaredRole
	roleOf := a.agentRoles()
	for name, seen := range a.agents().lostAgents(now) {
		if roleOf(name, role(name)) == agentRoleWorker {
			continue
		}
		needs = append(needs, AttentionItem{Level: "needs", Kind: "lost", Title: name + " is not answering",
			Detail: "Last heard " + ago(now, seen), At: &seen, Ref: name, Open: "hive"})
	}

	for _, r := range a.CLIRuns() {
		// A queen run that failed before doing anything was handed to the
		// next engine or to SuperLeo (app_queen_engine.go): there is nothing
		// left to do, and the queen's label already says who stands in.
		if r.Queen && r.Tools == 0 {
			continue
		}
		if (r.State == "failed" || r.State == "error") && r.Ended != nil && r.Ended.After(dayAgo) {
			at := *r.Ended
			needs = append(needs, AttentionItem{Level: "needs", Kind: "run", Title: r.Agent + " stopped with an error",
				Detail: firstLineOf(firstNonEmpty(r.Error, r.Prompt), 140), At: &at, Ref: r.ID, Open: "coding"})
		}
	}

	life := a.Life()
	week := now.Add(7 * 24 * time.Hour)
	soon = append(soon, upcomingEvents(life.Schedules, now)...)

	// Reminders: their next time is the scheduler's.
	next := map[string]*time.Time{}
	what := map[string]string{}
	for _, u := range a.Upcoming() {
		next[u.ID] = u.Next
		what[u.ID] = u.What
	}
	for _, r := range life.Reminders {
		id := fmt.Sprint(r["id"])
		at := next[id]
		if at == nil || at.After(week) || at.Before(now) {
			continue
		}
		// A row saved without a title printed as "<nil>"; the scheduled
		// task it belongs to still says what it is for.
		title := firstNonEmpty(fmt.Sprint(orEmpty(r["title"])), what[id], "Reminder")
		soon = append(soon, AttentionItem{Level: "soon", Kind: "reminder", Title: title,
			Detail: strings.TrimSpace(fmt.Sprint(orEmpty(r["when"]))), At: at, Ref: id, Open: "records"})
	}

	sort.SliceStable(needs, func(i, j int) bool { return later(needs[i].At, needs[j].At) })
	sort.SliceStable(soon, func(i, j int) bool { return earlier(soon[i].At, soon[j].At) })
	return append(needs, soon...)
}

// upcomingEvents is the meetings in the next week (and one that started
// within the hour). The same meeting saved twice — same start, same people —
// is shown once.
func upcomingEvents(schedules []map[string]any, now time.Time) []AttentionItem {
	week := now.Add(7 * 24 * time.Hour)
	seen := map[string]bool{}
	var out []AttentionItem
	for _, s := range schedules {
		start, err := time.Parse(time.RFC3339, fmt.Sprint(s["start_at"]))
		if err != nil || start.Before(now.Add(-time.Hour)) || start.After(week) {
			continue
		}
		people := stringsOf(s["participants"])
		key := start.UTC().Format(time.RFC3339) + "|" + strings.Join(sortedCopy(people), ",")
		if seen[key] {
			continue
		}
		seen[key] = true
		detail := ""
		if len(people) > 0 {
			detail = "With " + strings.Join(people, ", ")
		}
		_, off := start.Zone()
		out = append(out, AttentionItem{Level: "soon", Kind: "event", Title: fmt.Sprint(s["title"]), Detail: detail,
			At: &start, Zone: offsetString(off), Place: strings.TrimSpace(fmt.Sprint(orEmpty(s["location"]))),
			Ref: fmt.Sprint(s["id"]), Open: "records"})
	}
	return out
}

func firstLineOf(s string, max int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if r := []rune(s); len(r) > max {
		s = string(r[:max-1]) + "…"
	}
	return s
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

func orEmpty(v any) any {
	if v == nil {
		return ""
	}
	return v
}

func stringsOf(v any) []string {
	var out []string
	switch x := v.(type) {
	case []string:
		out = append(out, x...)
	case []any:
		for _, e := range x {
			if s := strings.TrimSpace(fmt.Sprint(e)); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

func sortedCopy(ss []string) []string {
	out := append([]string(nil), ss...)
	sort.Strings(out)
	return out
}

func offsetString(sec int) string {
	sign := "+"
	if sec < 0 {
		sign, sec = "-", -sec
	}
	return fmt.Sprintf("%s%02d:%02d", sign, sec/3600, sec%3600/60)
}

func ago(now, t time.Time) string {
	d := now.Sub(t)
	switch {
	case t.IsZero():
		return "never"
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func later(a, b *time.Time) bool {
	if a == nil {
		return false
	}
	if b == nil {
		return true
	}
	return a.After(*b)
}

func earlier(a, b *time.Time) bool {
	if a == nil {
		return false
	}
	if b == nil {
		return true
	}
	return a.Before(*b)
}
