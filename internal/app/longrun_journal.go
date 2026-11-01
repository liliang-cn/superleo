package app

// Long runs that outlive SuperLeo.
//
// agent-go keeps a segmented task's plan and its checkpoints under the task
// id, so a task whose process went away can be started again from what it had
// finished. What it does not keep is why the task was running: the goal, the
// limits it was given, whether it ran unattended, and whether a person ended
// it. Without that, a restart finds a plan in the store and cannot tell a task
// that was cut off from one that was stopped on purpose, so the only way back
// was LongRunStart by hand with the old id.
//
// The intent is written beside the coding runs' journals, one small file per
// task under <data>/long-runs, and touched as the task goes (tokens spent and
// time on the clock, which a resume must count: agent-go starts its own
// budgets from zero for every call). SuperLeo starting reads them back:
//
//   - running: the process went away mid-task. It is marked interrupted and,
//     if its budget is not spent, started again under the same id and limits;
//   - stopped, done, failed: left alone;
//   - interrupted, budget spent: left as it is, shown on the Runs page.

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/liliang-cn/superleo/internal/backend"
)

// A long run's state in its journal.
const (
	longRunning     = "running"
	longDone        = "done"
	longFailed      = "failed"
	longStopped     = "stopped"
	longInterrupted = "interrupted"
)

// longMaxResumes is how many times a task is picked up before it is left
// alone: one that takes the process down every time it runs is not going to
// finish, and starting it at every launch would only repeat that.
const longMaxResumes = 5

// longRecord is the durable intent of one long run.
type longRecord struct {
	TaskID           string `json:"taskId"`
	Goal             string `json:"goal"`
	Model            string `json:"model,omitempty"`
	MaxSegments      int    `json:"maxSegments"`
	RoundsPerSegment int    `json:"roundsPerSegment"`
	MaxMinutes       int    `json:"maxMinutes"`
	MaxTokens        int    `json:"maxTokens"`
	Unattended       bool   `json:"unattended"`

	State string `json:"state"`
	// Stop is how it ended, or why it was not picked up again.
	Stop string `json:"stop,omitempty"`
	// Resumes counts how many times it has been picked up after a restart.
	Resumes int `json:"resumes"`

	// Spent before this incarnation began: what earlier processes used.
	TokensBefore  int   `json:"tokensBefore"`
	SecondsBefore int64 `json:"secondsBefore"`
	// Spent by this incarnation so far, as of UpdatedAt.
	Tokens int `json:"tokens"`
	// RunStarted is when this incarnation began; UpdatedAt is its last sign
	// of life, which is as close as a killed process lets the end be known.
	RunStarted time.Time `json:"runStarted"`

	StartedAt time.Time `json:"startedAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// spent is what the task has used across every incarnation.
func (r *longRecord) spent() (tokens int, active time.Duration) {
	tokens = r.TokensBefore + r.Tokens
	active = time.Duration(r.SecondsBefore) * time.Second
	if !r.RunStarted.IsZero() && r.UpdatedAt.After(r.RunStarted) {
		active += r.UpdatedAt.Sub(r.RunStarted)
	}
	return
}

// longResumeDecision says whether a record found at start is to be picked up,
// and if not, why. Pure: the record alone decides, so it can be tabled.
func longResumeDecision(r *longRecord) (resume bool, why string) {
	switch r.State {
	case longRunning:
	case longStopped:
		return false, "stopped by a person"
	case longDone:
		return false, "already done"
	case longFailed:
		return false, "failed"
	default:
		// interrupted: it was left on an earlier start for the reason it
		// carries, and that stays the reason.
		return false, "already interrupted"
	}
	if r.Resumes >= longMaxResumes {
		return false, "resumed too many times"
	}
	tokens, active := r.spent()
	if r.MaxTokens > 0 && tokens >= r.MaxTokens {
		return false, "token budget used up"
	}
	if r.MaxMinutes > 0 && active >= time.Duration(r.MaxMinutes)*time.Minute {
		return false, "time budget used up"
	}
	return true, ""
}

// remaining is the limits a resumed task is given: agent-go counts a call's
// budget from zero, so what was already spent comes off here.
func (r *longRecord) remaining() (maxMinutes, maxTokens int) {
	tokens, active := r.spent()
	if r.MaxMinutes > 0 {
		left := time.Duration(r.MaxMinutes)*time.Minute - active
		maxMinutes = int((left + time.Minute - 1) / time.Minute)
		if maxMinutes < 1 {
			maxMinutes = 1
		}
	}
	if r.MaxTokens > 0 {
		maxTokens = r.MaxTokens - tokens
		if maxTokens < 1 {
			maxTokens = 1
		}
	}
	return
}

func longRunDir() string { return filepath.Join(backend.DataDir(), "long-runs") }

func longRunPath(id string) string {
	// A task id comes from a caller; keep it a file name.
	id = strings.NewReplacer("/", "_", "\\", "_", "..", "_").Replace(id)
	return filepath.Join(longRunDir(), id+".json")
}

func readLongRecord(id string) *longRecord {
	raw, err := os.ReadFile(longRunPath(id))
	if err != nil {
		return nil
	}
	var r longRecord
	if json.Unmarshal(raw, &r) != nil || r.TaskID == "" {
		return nil
	}
	return &r
}

// writeLongRecord replaces the file whole through a rename, so a process
// killed mid-write leaves the old record rather than half of a new one.
func writeLongRecord(r *longRecord) {
	if err := os.MkdirAll(longRunDir(), 0o700); err != nil {
		return
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return
	}
	p := longRunPath(r.TaskID)
	tmp := p + ".tmp"
	if os.WriteFile(tmp, raw, 0o600) != nil {
		return
	}
	_ = os.Rename(tmp, p)
}

// longJournalBegin writes the record for a task being started or resumed.
func (a *App) longJournalBegin(r *longRecord) {
	a.longJMu.Lock()
	defer a.longJMu.Unlock()
	if a.longRecs == nil {
		a.longRecs = map[string]*longRecord{}
		a.longTouched = map[string]time.Time{}
	}
	now := time.Now()
	r.State, r.Stop = longRunning, ""
	r.RunStarted, r.UpdatedAt = now, now
	r.Tokens = 0
	if r.StartedAt.IsZero() {
		r.StartedAt = now
	}
	a.longRecs[r.TaskID] = r
	writeLongRecord(r)
}

// longJournalTouch records that a running task is still going and what it has
// used. Called on every wall tick; the file is written at most every couple of
// seconds, which loses nothing that matters to a budget counted in minutes.
func (a *App) longJournalTouch(taskID string, tokens int, force bool) {
	a.longJMu.Lock()
	defer a.longJMu.Unlock()
	r := a.longRecs[taskID]
	if r == nil || r.State != longRunning {
		return
	}
	now := time.Now()
	if !force && now.Sub(a.longTouched[taskID]) < 2*time.Second {
		return
	}
	a.longTouched[taskID] = now
	r.UpdatedAt, r.Tokens = now, tokens
	writeLongRecord(r)
}

// longJournalEnd records how a task ended.
func (a *App) longJournalEnd(taskID, state, stop string, tokens int) {
	a.longJMu.Lock()
	defer a.longJMu.Unlock()
	r := a.longRecs[taskID]
	if r == nil {
		return
	}
	r.State, r.Stop = state, stop
	r.UpdatedAt, r.Tokens = time.Now(), tokens
	writeLongRecord(r)
}

// recoverLongRuns picks up the long runs that were going when SuperLeo last
// stopped.
func (a *App) recoverLongRuns() {
	paths, _ := filepath.Glob(filepath.Join(longRunDir(), "*.json"))
	for _, p := range paths {
		id := strings.TrimSuffix(filepath.Base(p), ".json")
		if r := readLongRecord(id); r != nil {
			a.recoverLongRun(r)
		}
	}
}

func (a *App) recoverLongRun(r *longRecord) {
	// One already in flight in this process is not a casualty of a restart.
	a.longMu.Lock()
	_, going := a.longRuns[r.TaskID]
	a.longMu.Unlock()
	if going {
		return
	}
	resume, why := longResumeDecision(r)
	if !resume {
		// A task that was running and is not picked up is marked as cut off,
		// with the reason; the finished ones are history and stay off the page.
		if r.State == longRunning {
			r.State, r.Stop = longInterrupted, why
			writeLongRecord(r)
			slog.Info("long run: left interrupted after a restart", "task", r.TaskID, "why", why)
		}
		if r.State == longInterrupted {
			a.runWall().Seed(r.TaskID, r.Goal, r.Model, r.State, r.Stop, r.StartedAt)
		}
		return
	}

	// Folded into the totals before the record is touched again: what the
	// dead incarnation used counts against the limits of this one.
	tokens, active := r.spent()
	r.TokensBefore, r.SecondsBefore = tokens, int64(active/time.Second)
	r.Resumes++
	r.State = longInterrupted
	writeLongRecord(r)
	maxMinutes, maxTokens := r.remaining()
	slog.Info("long run: picked back up after a restart",
		"task", r.TaskID, "resumes", r.Resumes, "tokens_spent", tokens,
		"active", active.Round(time.Second).String(),
		"max_minutes_left", maxMinutes, "max_tokens_left", maxTokens)
	a.runWall().Seed(r.TaskID, r.Goal, r.Model, r.State, r.Stop, r.StartedAt)
	a.startLongRun(r, maxMinutes, maxTokens, true)
}
