package watchdog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

type progress struct {
	Sig string    `json:"sig"`
	At  time.Time `json:"at"`
}

// State is what the watchdog remembers between ticks, kept in
// <home>/watchdog-state.json so a restart of the watchdog forgets nothing.
type State struct {
	Alerts     map[string]time.Time `json:"alerts"`     // "kind|target" -> raised
	Relaunches []time.Time          `json:"relaunches"` // recent relaunches
	HangSince  time.Time            `json:"hang_since"` // process up, API silent
	Progress   map[string]progress  `json:"progress"`   // run key -> last signature change
	Intervened map[string]time.Time `json:"intervened"` // run key -> stopped+resumed at
	Judged     map[string]time.Time `json:"judged"`     // run key -> last asked
	SchedLast  map[string]time.Time `json:"sched_last"` // schedule id -> last_run seen
}

func newState() *State {
	return &State{Alerts: map[string]time.Time{}, Progress: map[string]progress{},
		Intervened: map[string]time.Time{}, Judged: map[string]time.Time{}, SchedLast: map[string]time.Time{}}
}

func loadState(path string) *State {
	s := newState()
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, s)
	}
	if s.Alerts == nil {
		s.Alerts = map[string]time.Time{}
	}
	if s.Progress == nil {
		s.Progress = map[string]progress{}
	}
	if s.Intervened == nil {
		s.Intervened = map[string]time.Time{}
	}
	if s.Judged == nil {
		s.Judged = map[string]time.Time{}
	}
	if s.SchedLast == nil {
		s.SchedLast = map[string]time.Time{}
	}
	return s
}

func (s *State) save(path string) {
	b, err := json.Marshal(s)
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	tmp := path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, path)
	}
}
