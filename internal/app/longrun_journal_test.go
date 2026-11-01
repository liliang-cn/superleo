package app

import (
	"testing"
	"time"

	"github.com/liliang-cn/superleo/internal/backend"
)

func TestLongResumeDecision(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	running := func(mut func(r *longRecord)) *longRecord {
		r := &longRecord{
			TaskID: "t", Goal: "g", State: longRunning,
			RunStarted: t0, UpdatedAt: t0.Add(10 * time.Minute), StartedAt: t0,
		}
		if mut != nil {
			mut(r)
		}
		return r
	}
	cases := []struct {
		name   string
		r      *longRecord
		resume bool
		why    string
	}{
		{"cut off mid-task, no limits", running(nil), true, ""},
		{"cut off, budget left", running(func(r *longRecord) {
			r.MaxMinutes, r.MaxTokens, r.Tokens = 60, 100000, 40000
		}), true, ""},
		{"tokens used up across incarnations", running(func(r *longRecord) {
			r.MaxTokens, r.TokensBefore, r.Tokens = 100000, 70000, 30000
		}), false, "token budget used up"},
		{"time used up across incarnations", running(func(r *longRecord) {
			r.MaxMinutes, r.SecondsBefore = 30, 20*60
		}), false, "time budget used up"},
		{"stopped by a person", running(func(r *longRecord) { r.State = longStopped }), false, "stopped by a person"},
		{"already done", running(func(r *longRecord) { r.State = longDone }), false, "already done"},
		{"failed", running(func(r *longRecord) { r.State = longFailed }), false, "failed"},
		{"already interrupted is not picked up twice", running(func(r *longRecord) { r.State = longInterrupted }), false, "already interrupted"},
		{"resumed too many times", running(func(r *longRecord) { r.Resumes = longMaxResumes }), false, "resumed too many times"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resume, why := longResumeDecision(c.r)
			if resume != c.resume || why != c.why {
				t.Fatalf("got (%v, %q), want (%v, %q)", resume, why, c.resume, c.why)
			}
		})
	}
}

func TestLongRecordRemaining(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	r := &longRecord{
		MaxMinutes: 60, MaxTokens: 100000,
		TokensBefore: 30000, Tokens: 20000,
		SecondsBefore: 15 * 60,
		RunStarted:    t0, UpdatedAt: t0.Add(10*time.Minute + 30*time.Second),
	}
	mins, toks := r.remaining()
	// 60 - (15 + 10.5) = 34.5, rounded up to whole minutes.
	if mins != 35 || toks != 50000 {
		t.Fatalf("remaining = (%d min, %d tokens), want (35, 50000)", mins, toks)
	}
	if m, k := (&longRecord{}).remaining(); m != 0 || k != 0 {
		t.Fatalf("no limits must stay none, got (%d, %d)", m, k)
	}
}

// The states a record goes through, as the journal functions write them.
func TestLongJournalTransitions(t *testing.T) {
	cases := []struct {
		name  string
		steps func(a *App)
		want  string
		stop  string
	}{
		{"begin is running", func(a *App) {}, longRunning, ""},
		{"done", func(a *App) { a.longJournalEnd("x", longDone, "done", 5) }, longDone, "done"},
		{"failed", func(a *App) { a.longJournalEnd("x", longFailed, "boom", 5) }, longFailed, "boom"},
		{"stopped", func(a *App) { a.longJournalEnd("x", longStopped, "cancelled", 5) }, longStopped, "cancelled"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := &App{}
			a.longJournalBegin(&longRecord{TaskID: "x", Goal: "g", MaxMinutes: 5})
			c.steps(a)
			got := readLongRecord("x")
			if got == nil || got.State != c.want || got.Stop != c.stop {
				t.Fatalf("record = %+v, want state %q stop %q", got, c.want, c.stop)
			}
		})
	}
}

// Touching an ended record must not bring it back to running.
func TestLongJournalTouchIgnoresEnded(t *testing.T) {
	a := &App{}
	a.longJournalBegin(&longRecord{TaskID: "y", Goal: "g"})
	a.longJournalEnd("y", longStopped, "cancelled", 1)
	a.longJournalTouch("y", 99, true)
	if got := readLongRecord("y"); got.State != longStopped || got.Tokens != 1 {
		t.Fatalf("record = %+v", got)
	}
}

// A record that was running and cannot be carried on is marked interrupted
// with the reason, and shown on the wall; a stopped one is left alone.
func TestRecoverLongRunsMarksAndLeaves(t *testing.T) {
	now := time.Now()
	writeLongRecord(&longRecord{TaskID: "spent", Goal: "g", State: longRunning, MaxTokens: 10, Tokens: 10, RunStarted: now, UpdatedAt: now, StartedAt: now})
	writeLongRecord(&longRecord{TaskID: "person", Goal: "g", State: longStopped, StartedAt: now})
	a := &App{}
	a.recoverLongRuns()

	if got := readLongRecord("spent"); got.State != longInterrupted || got.Stop != "token budget used up" {
		t.Fatalf("spent record = %+v", got)
	}
	if got := readLongRecord("person"); got.State != longStopped {
		t.Fatalf("stopped record = %+v", got)
	}
	// Run it twice: nothing changes the second time.
	a.recoverLongRuns()
	if got := readLongRecord("spent"); got.State != longInterrupted {
		t.Fatalf("second start changed it: %+v", got)
	}
	st := a.LongRunState("spent")
	if st == nil || st.State != longInterrupted {
		t.Fatalf("wall state = %+v", st)
	}
	if a.LongRunState("person") != nil {
		t.Fatal("a stopped run must stay off the page")
	}
	// A seeded task has no turns, and the page reads their length.
	if st.Segments == nil || st.Rounds == nil || st.Log == nil {
		t.Fatalf("nil lists in a seeded state: %+v", st)
	}
	// The page is told when it was cut off, in the fields and in the log.
	if st.InterruptedAt == "" {
		t.Fatalf("no interruptedAt: %+v", st)
	}
	var cut int
	for _, l := range st.Log {
		if l.Kind == "interrupt" {
			cut++
		}
	}
	if cut != 1 {
		t.Fatalf("interrupt lines = %d after two starts, want 1: %+v", cut, st.Log)
	}
	var sum *backend.TaskSummary
	for _, x := range a.LongRunList() {
		if x.TaskID == "spent" {
			x := x
			sum = &x
		}
	}
	if sum == nil || sum.State != longInterrupted || sum.InterruptedAt == "" {
		t.Fatalf("summary = %+v", sum)
	}
}

// A picked-up run carries how many times, and when, into the wall.
func TestWallResumedFields(t *testing.T) {
	w := backend.NewRunWall(nil, nil)
	cut := time.Now().Add(-time.Hour)
	w.Seed("r", "g", "m", longInterrupted, "", cut)
	w.Interrupted("r", 1, cut)
	w.Begin("r", "g", "m", 3)
	w.Resumed("r", 2)
	st := w.Snapshot(nil, "r")
	if st.Resumes != 2 || st.ResumedAt == "" || st.InterruptedAt == "" || st.State != "running" {
		t.Fatalf("state = %+v", st)
	}
	kinds := ""
	for _, l := range st.Log {
		kinds += l.Kind + " "
	}
	if kinds != "interrupt resume " {
		t.Fatalf("log kinds = %q", kinds)
	}
}
