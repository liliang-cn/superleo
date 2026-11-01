package backend

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openTestStore(t *testing.T) (*TaskStore, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "taskboard.db")
	s, err := OpenTaskStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

// An order written down comes back whole after the board is built again, with
// the ones still running handed back for the caller to settle.
func TestTheBoardSurvivesARestart(t *testing.T) {
	s, path := openTestStore(t)
	b := NewTaskBoard(nil)
	b.AttachStore(s)

	done := b.Start("w1", TaskOut, "first order")
	b.InSession(done, "chat-1")
	b.Progress(done, PhaseTool, "web_search")
	b.Finish(done, TaskDone, "the answer", "")
	run := b.Start("w2", TaskOut, "second order")
	b.Progress(run, PhaseTool, "shell")
	b.SetRun(run, "run-7")
	b.Live(run, "writing words that are not kept")
	b.Flush() // what a clean stop does

	s.Close()
	s2, err := OpenTaskStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	b2 := NewTaskBoard(nil)
	running := b2.AttachStore(s2)

	if len(running) != 1 || running[0].ID != run || running[0].Run != "run-7" || running[0].Tools != 1 {
		t.Fatalf("running after restart: %+v", running)
	}
	got, ok := b2.Get(done)
	if !ok || got.State != TaskDone || got.Result != "the answer" || got.Session != "chat-1" || got.Worker != "w1" ||
		got.Prompt != "first order" || len(got.Events) < 4 || got.EndedAt.IsZero() {
		t.Fatalf("finished order: %+v", got)
	}
	if r, _ := b2.Get(run); r.Live != "" {
		t.Fatalf("live text outlived the process: %q", r.Live)
	}
}

// What the board has forgotten from memory is still found, by id and by the
// start of one.
func TestAnOrderOlderThanTheBoardIsStillFound(t *testing.T) {
	s, _ := openTestStore(t)
	b := NewTaskBoard(nil)
	b.AttachStore(s)
	first := b.Start("w", TaskOut, "the oldest")
	b.Finish(first, TaskDone, "ok", "")
	for i := 0; i < boardKeep+5; i++ {
		b.Finish(b.Start("w", TaskOut, "filler"), TaskDone, "", "")
	}
	if len(b.Recent()) > boardKeep {
		t.Fatalf("memory holds %d", len(b.Recent()))
	}
	if got, ok := b.Get(first); !ok || got.Prompt != "the oldest" {
		t.Fatalf("Get after trim: %+v %v", got, ok)
	}
	if ids := b.Resolve(first[:8]); len(ids) != 1 || ids[0] != first {
		t.Fatalf("Resolve: %v", ids)
	}
	if err := b.Stop(first); err == nil || !strings.Contains(err.Error(), "already ended") {
		t.Fatalf("stopping an old finished order: %v", err)
	}
	if n := len(b.List(1000, false)); n != boardKeep+6 {
		t.Fatalf("List sees %d orders, want %d", n, boardKeep+6)
	}
	if n := len(b.List(1000, true)); n != 0 {
		t.Fatalf("running: %d", n)
	}
}

func TestPruning(t *testing.T) {
	now := time.Now()
	day := 24 * time.Hour
	tests := []struct {
		name string
		put  func(s *TaskStore)
		left []string
	}{
		{"older than a month goes, newer stays", func(s *TaskStore) {
			s.Put(HiveTask{ID: "old", Worker: "w", Dir: TaskOut, State: TaskDone, StartedAt: now.Add(-41 * day), EndedAt: now.Add(-40 * day)})
			s.Put(HiveTask{ID: "fresh", Worker: "w", Dir: TaskOut, State: TaskDone, StartedAt: now.Add(-3 * day), EndedAt: now.Add(-3 * day)})
		}, []string{"fresh"}},
		{"a running order is never pruned, however old", func(s *TaskStore) {
			s.Put(HiveTask{ID: "ancient-running", Worker: "w", Dir: TaskOut, State: TaskRunning, StartedAt: now.Add(-90 * day)})
		}, []string{"ancient-running"}},
		{"a lost one ages like a finished one", func(s *TaskStore) {
			s.Put(HiveTask{ID: "lost-old", Worker: "w", Dir: TaskOut, State: TaskLost, StartedAt: now.Add(-60 * day), EndedAt: now.Add(-59 * day)})
			s.Put(HiveTask{ID: "lost-new", Worker: "w", Dir: TaskOut, State: TaskLost, StartedAt: now.Add(-2 * day), EndedAt: now.Add(-day)})
		}, []string{"lost-new"}},
		{"one that never ended is as old as its start", func(s *TaskStore) {
			s.Put(HiveTask{ID: "no-end", Worker: "w", Dir: TaskOut, State: TaskFailed, StartedAt: now.Add(-45 * day)})
		}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := openTestStore(t)
			tc.put(s)
			s.Prune(now)
			var got []string
			for _, x := range s.List(100, false) {
				got = append(got, x.ID)
			}
			if strings.Join(got, ",") != strings.Join(tc.left, ",") {
				t.Fatalf("left %v, want %v", got, tc.left)
			}
		})
	}

	t.Run("the oldest finished go past the row limit", func(t *testing.T) {
		s, _ := openTestStore(t)
		for i := 0; i < taskKeepRows+10; i++ {
			s.Put(HiveTask{ID: "t" + time.Duration(i).String(), Worker: "w", Dir: TaskOut, State: TaskDone,
				StartedAt: now.Add(-time.Duration(taskKeepRows+10-i) * time.Second), EndedAt: now})
		}
		s.Put(HiveTask{ID: "running", Worker: "w", Dir: TaskOut, State: TaskRunning, StartedAt: now.Add(-time.Hour)})
		if n := s.Prune(now); n != 10 {
			t.Fatalf("pruned %d, want 10", n)
		}
		if _, ok := s.Get("running"); !ok {
			t.Fatal("the running order was pruned")
		}
		if n := s.Count(); n != taskKeepRows+1 {
			t.Fatalf("%d rows", n)
		}
	})
}

// A lost order stays lost until the worker itself reports on it.
func TestLostOrdersAndLateReports(t *testing.T) {
	s, _ := openTestStore(t)
	b := NewTaskBoard(nil)
	b.AttachStore(s)
	id := b.StartAs("", "w", "w", TaskPeer, "p")
	b.MarkLost(id, "restarted")
	got, _ := b.Get(id)
	if got.State != TaskLost || got.Error != "restarted" || got.EndedAt.IsZero() {
		t.Fatalf("%+v", got)
	}
	b.Finish(id, TaskDone, "late", "") // settled once: no change
	if got, _ = b.Get(id); got.State != TaskLost {
		t.Fatalf("a lost order was finished: %s", got.State)
	}
	b.Ingest(HiveTask{ID: id, Worker: "w", From: "w", Dir: TaskPeer, State: TaskDone, Result: "it finished"})
	if got, _ = b.Get(id); got.State != TaskDone || got.Result != "it finished" {
		t.Fatalf("the worker's report did not replace lost: %+v", got)
	}
	if st, _ := s.Get(id); st.State != TaskDone {
		t.Fatalf("not written: %+v", st)
	}
}

func TestTrackVerdicts(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	every := time.Hour
	tr := Tracker{ID: "tr", Task: "abcd1234-0000", Every: every, State: TrackActive}
	run := func(last time.Duration) TrackSubject {
		return TrackSubject{Found: true, State: TaskRunning, Name: "the order", Last: now.Add(-last)}
	}
	tests := []struct {
		name    string
		tr      Tracker
		s       TrackSubject
		status  string
		over    bool
		redo    bool
		saying  bool
		contain string
	}{
		{"running and recent: quiet", tr, run(10 * time.Minute), StatusRunning, false, false, false, ""},
		{"quiet for more than two intervals: stuck", tr, run(3 * time.Hour), StatusStuck, false, false, true, "no progress for 3h"},
		{"stuck said once", func() Tracker { x := tr; x.Said = StatusStuck; return x }(), run(3 * time.Hour), StatusStuck, false, false, false, ""},
		{"a short interval still allows a quarter hour", func() Tracker { x := tr; x.Every = time.Minute; return x }(), run(10 * time.Minute), StatusRunning, false, false, false, ""},
		{"done ends it", tr, TrackSubject{Found: true, State: TaskDone, Name: "the order", Detail: "built"}, StatusDone, true, false, true, "finished. built"},
		{"failed ends it", tr, TrackSubject{Found: true, State: TaskFailed, Name: "the order", Detail: "boom"}, StatusFailed, true, false, true, "failed: boom"},
		{"lost is said", tr, TrackSubject{Found: true, State: TaskLost, Name: "the order", Detail: "worker gone"}, StatusLost, true, false, true, "lost: worker gone"},
		{"lost with resend asks for it", func() Tracker { x := tr; x.Redo = true; return x }(), TrackSubject{Found: true, State: TaskLost, Name: "the order", Detail: "gone"}, StatusRetrying, false, true, true, "attempt 1 of 3"},
		{"resend runs out", func() Tracker { x := tr; x.Redo = true; x.Redos = trackRedos; return x }(), TrackSubject{Found: true, State: TaskLost, Name: "the order", Detail: "gone"}, StatusLost, true, false, true, "lost"},
		{"deadline passed while running", func() Tracker { x := tr; x.Until = now.Add(-time.Minute); return x }(), run(time.Minute), StatusLate, true, false, true, "still not finished"},
		{"gone from the board", tr, TrackSubject{}, StatusGone, true, false, true, "no longer find"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := EvaluateTrack(tc.tr, tc.s, now)
			if v.Status != tc.status || v.Over != tc.over || v.Redo != tc.redo || (v.Say != "") != tc.saying || !strings.Contains(v.Say, tc.contain) {
				t.Fatalf("%+v", v)
			}
		})
	}
}

func TestTrackerSchedule(t *testing.T) {
	s, _ := openTestStore(t)
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	tr := NewTracker("task", TrackOrder, time.Hour, now.Add(150*time.Minute), false, "chat", now)
	if !tr.NextAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("first check %v", tr.NextAt)
	}
	if err := s.PutTracker(tr); err != nil {
		t.Fatal(err)
	}
	if n := len(s.DueTrackers(now.Add(59 * time.Minute))); n != 0 {
		t.Fatalf("due early: %d", n)
	}
	due := s.DueTrackers(now.Add(time.Hour))
	if len(due) != 1 || due[0].Session != "chat" || due[0].Every != time.Hour {
		t.Fatalf("due: %+v", due)
	}
	tr = due[0].Checked(now.Add(time.Hour), TrackVerdict{Status: StatusRunning})
	tr = tr.Checked(now.Add(2*time.Hour), TrackVerdict{Status: StatusRunning})
	// The next look would be past the deadline, so the deadline is the look.
	if !tr.NextAt.Equal(now.Add(150 * time.Minute)) {
		t.Fatalf("next %v, want the deadline", tr.NextAt)
	}
	tr = tr.Checked(tr.NextAt, TrackVerdict{Status: StatusLate, Over: true})
	if tr.State != TrackDone || tr.Checks != 3 {
		t.Fatalf("%+v", tr)
	}
	s.PutTracker(tr)
	if n := len(s.DueTrackers(now.Add(24 * time.Hour))); n != 0 {
		t.Fatalf("an ended tracker is due: %d", n)
	}
	if got, err := s.Tracker(tr.ID[:6]); err != nil || got.ID != tr.ID {
		t.Fatalf("by prefix: %v %v", got, err)
	}
}
