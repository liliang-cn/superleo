package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/liliang-cn/superleo/internal/backend"
)

// fakeWorker answers HiveTaskDetail the way a worker's RPC does, from the
// orders it is given.
type fakeWorker struct {
	mu     sync.Mutex
	orders map[string]backend.HiveTask
	stops  []string
	srv    *httptest.Server
}

func newFakeWorker(t *testing.T) *fakeWorker {
	t.Helper()
	w := &fakeWorker{orders: map[string]backend.HiveTask{}}
	w.srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		var args []string
		_ = json.NewDecoder(r.Body).Decode(&args)
		w.mu.Lock()
		defer w.mu.Unlock()
		switch strings.TrimPrefix(r.URL.Path, "/api/rpc/") {
		case "HiveTaskDetail":
			if o, ok := w.orders[args[0]]; ok {
				json.NewEncoder(rw).Encode(map[string]any{"ok": true, "task": o})
				return
			}
			json.NewEncoder(rw).Encode(map[string]any{"ok": false, "error": "no such task"})
		case "StopHiveTask":
			w.stops = append(w.stops, args[0])
			json.NewEncoder(rw).Encode("ok")
		}
	}))
	t.Cleanup(w.srv.Close)
	return w
}

func (w *fakeWorker) set(o backend.HiveTask) {
	w.mu.Lock()
	w.orders[o.ID] = o
	w.mu.Unlock()
}

// restartedQueen is a queen whose board file already holds the given orders as
// the last process left them, started fresh on it.
func restartedQueen(t *testing.T, worker *fakeWorker, before func(old *App)) *App {
	t.Helper()
	t.Setenv("SUPERLEO_HOME", t.TempDir())
	t.Cleanup(func(o, g time.Duration) func() { return func() { orderPoll, orderGrace = o, g } }(orderPoll, orderGrace))
	orderPoll, orderGrace = 20*time.Millisecond, 300*time.Millisecond
	cfg := func() *backend.Settings {
		s := &backend.Settings{WorkspaceDir: t.TempDir()}
		if worker != nil {
			s.RemoteAgents = backend.RemoteAgents{Enabled: true, Agents: map[string]backend.RemoteAgent{"w1": {URL: worker.srv.URL}}}
		}
		return s
	}
	old := &App{settings: cfg()}
	old.openTaskBoard()
	before(old)
	old.stopTaskBoard()

	q := &App{settings: cfg()}
	q.openTaskBoard()
	t.Cleanup(q.stopTaskBoard)
	return q
}

func waitState(t *testing.T, a *App, id, want string) backend.HiveTask {
	t.Helper()
	var got backend.HiveTask
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		got, _ = a.tasks().Get(id)
		if got.State == want {
			return got
		}
	}
	t.Fatalf("order %s is %q, want %q (error %q)", id, got.State, want, got.Error)
	return got
}

// The queen goes away with orders out. When she is back each one is followed or
// called lost, and the ones that were over stay as they were.
func TestARestartedQueenFollowsOrReportsLostOrders(t *testing.T) {
	w := newFakeWorker(t)
	var still, finished, unknown, inbound, peer, over string
	q := restartedQueen(t, w, func(old *App) {
		b := old.tasks()
		still = b.Start("w1", backend.TaskOut, "still going")
		finished = b.Start("w1", backend.TaskOut, "ends while she is away")
		unknown = b.Start("w1", backend.TaskOut, "the worker never kept it")
		inbound = b.StartAs("", "w1", "", backend.TaskIn, "a turn this machine carried out")
		peer = b.StartAs("", "w2", "w1", backend.TaskPeer, "worker to worker")
		over = b.Start("w1", backend.TaskOut, "already over")
		b.Finish(over, backend.TaskDone, "result", "")
		w.set(backend.HiveTask{ID: still, State: backend.TaskRunning, Phase: backend.PhaseTool, Tool: "shell", Tools: 4})
		w.set(backend.HiveTask{ID: finished, State: backend.TaskDone, Result: "built it", EndedAt: time.Now()})
	})

	if got := waitState(t, q, finished, backend.TaskDone); got.Result != "built it" {
		t.Fatalf("result not taken from the worker: %+v", got)
	}
	if got := waitState(t, q, unknown, backend.TaskLost); !strings.Contains(got.Error, "no record") {
		t.Fatalf("why: %q", got.Error)
	}
	if got := waitState(t, q, inbound, backend.TaskLost); !strings.Contains(got.Error, "restarted") {
		t.Fatalf("why: %q", got.Error)
	}
	waitState(t, q, peer, backend.TaskLost)
	if got, _ := q.tasks().Get(over); got.State != backend.TaskDone || got.Result != "result" {
		t.Fatalf("a finished order changed: %+v", got)
	}

	// The one the worker still has is followed: its progress comes over, and
	// its end when it ends.
	for end := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if got, _ := q.tasks().Get(still); got.State == backend.TaskRunning && got.Tool == "shell" && got.Tools == 4 {
			break
		}
		if time.Now().After(end) {
			t.Fatalf("progress never came over: %+v", func() backend.HiveTask { x, _ := q.tasks().Get(still); return x }())
		}
	}
	w.set(backend.HiveTask{ID: still, State: backend.TaskDone, Result: "all done", EndedAt: time.Now()})
	waitState(t, q, still, backend.TaskDone)

	// Everything survives another restart as it ended.
	q.tasks().Flush()
	again := &App{settings: &backend.Settings{WorkspaceDir: t.TempDir()}}
	again.openTaskBoard()
	for id, want := range map[string]string{still: backend.TaskDone, unknown: backend.TaskLost, over: backend.TaskDone} {
		if got, ok := again.tasks().Get(id); !ok || got.State != want {
			t.Fatalf("after a second restart %s is %+v", id, got)
		}
	}
}

// A worker that never comes back is given its time, then the order is lost
// and says so.
func TestAWorkerThatNeverComesBackLosesTheOrder(t *testing.T) {
	var id string
	q := restartedQueen(t, nil, func(old *App) { id = old.tasks().Start("ghost", backend.TaskOut, "to nobody") })
	got := waitState(t, q, id, backend.TaskLost)
	if !strings.Contains(got.Error, "could not be reached") {
		t.Fatalf("why: %q", got.Error)
	}
}

// Stopping an order the queen only follows stops it on the worker too.
func TestStoppingAFollowedOrderStopsItOnTheWorker(t *testing.T) {
	w := newFakeWorker(t)
	var id string
	q := restartedQueen(t, w, func(old *App) {
		id = old.tasks().Start("w1", backend.TaskOut, "long")
		w.set(backend.HiveTask{ID: id, State: backend.TaskRunning})
	})
	for end := time.Now().Add(5 * time.Second); !q.tasks().Stoppable(id); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("never stoppable")
		}
	}
	tl := tools{}
	q.registerQueenOrderTools(tl)
	if _, err := tl["hive_task_cancel"](context.Background(), map[string]any{"id": id[:8]}); err != nil {
		t.Fatal(err)
	}
	waitState(t, q, id, backend.TaskCancelled)
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.stops) != 1 || w.stops[0] != id {
		t.Fatalf("worker was told to stop %v", w.stops)
	}
}

// An order handed to a coding agent is taken back to its run; a run that is
// gone makes the order lost.
func TestARestartedQueenFindsTheCodingRunOfAnOrder(t *testing.T) {
	var gone, ended string
	q := restartedQueen(t, nil, func(old *App) {
		gone = old.tasks().Start("codex", backend.TaskOut, "x")
		old.tasks().SetRun(gone, "no-such-run")
		ended = old.tasks().Start("codex", backend.TaskOut, "y")
		old.tasks().SetRun(ended, "run-finished")
	})
	// The run, as the coding runs left it: finished while the queen was away.
	_ = q
	t.Run("missing run", func(t *testing.T) {
		if got := waitState(t, q, gone, backend.TaskLost); !strings.Contains(got.Error, "no-such-run") {
			t.Fatalf("why: %q", got.Error)
		}
	})
	t.Run("run that is there and finished", func(t *testing.T) {
		// Put the run in, then follow the order again as a restart would.
		s := q.cliStore()
		s.mu.Lock()
		s.runs["run-done"] = &CLIRun{ID: "run-done", State: "done", Summary: "wrote the file\nmore"}
		s.mu.Unlock()
		id := q.tasks().Start("codex", backend.TaskOut, "z")
		q.tasks().SetRun(id, "run-done")
		task, _ := q.tasks().Get(id)
		q.followCLIOrder(task)
		if got := waitState(t, q, id, backend.TaskDone); got.Result != "wrote the file" {
			t.Fatalf("%+v", got)
		}
	})
	_ = ended
}

// A check left on an order fires at its interval, says nothing while the order
// is well, says so when it goes quiet or is lost, and stops once it is over.
func TestATrackerFiresAndStopsWhenTheOrderIsOver(t *testing.T) {
	t.Setenv("SUPERLEO_HOME", t.TempDir())
	a := &App{settings: &backend.Settings{WorkspaceDir: t.TempDir()}}
	a.openTaskBoard()
	t.Cleanup(a.stopTaskBoard)

	type said struct {
		level backend.Level
		msg   string
	}
	var mu sync.Mutex
	var heard []said
	a.taskBoard.say = func(l backend.Level, msg, session, key string) {
		mu.Lock()
		heard = append(heard, said{l, msg})
		mu.Unlock()
	}
	count := func() int { mu.Lock(); defer mu.Unlock(); return len(heard) }

	id := a.tasks().Start("w1", backend.TaskOut, "a big job")
	res := a.TaskTrack(id[:8], 3600, 0, false)
	if res["ok"] != true {
		t.Fatalf("%v", res)
	}
	start := time.Now()

	if n := a.checkTrackers(start.Add(30 * time.Minute)); n != 0 {
		t.Fatalf("checked early: %d", n)
	}
	// An hour on, the order is well: checked, nothing said.
	if n := a.checkTrackers(start.Add(61 * time.Minute)); n != 1 || count() != 0 {
		t.Fatalf("n=%d said=%v", n, heard)
	}
	// Three hours on and silent: stuck, said once.
	a.checkTrackers(start.Add(4 * time.Hour))
	a.checkTrackers(start.Add(5 * time.Hour))
	if count() != 1 || !strings.Contains(heard[0].msg, "no progress") || heard[0].level != backend.LevelWarn {
		t.Fatalf("heard %+v", heard)
	}
	tracks := a.TaskTracks()["data"].([]map[string]any)
	if len(tracks) != 1 || tracks[0]["state"] != backend.TrackActive || tracks[0]["checks"] != 3 {
		t.Fatalf("%+v", tracks)
	}

	// It finishes: the next look says so and the tracker is over.
	a.tasks().Finish(id, backend.TaskDone, "shipped", "")
	a.checkTrackers(start.Add(6 * time.Hour))
	if count() != 2 || !strings.Contains(heard[1].msg, "finished") || heard[1].level != backend.LevelSuccess {
		t.Fatalf("heard %+v", heard)
	}
	if n := a.checkTrackers(start.Add(48 * time.Hour)); n != 0 {
		t.Fatalf("an over tracker was checked again: %d", n)
	}
	if tr := a.TaskTracks()["data"].([]map[string]any); tr[0]["state"] != backend.TrackDone {
		t.Fatalf("%+v", tr)
	}
}

func TestTrackingRefusals(t *testing.T) {
	t.Setenv("SUPERLEO_HOME", t.TempDir())
	a := &App{settings: &backend.Settings{WorkspaceDir: t.TempDir()}}
	a.openTaskBoard()
	t.Cleanup(a.stopTaskBoard)
	id := a.tasks().Start("w1", backend.TaskOut, "job")
	run := a.tasks().Start("codex", backend.TaskOut, "job")
	a.tasks().SetRun(run, "r1")
	in := a.tasks().StartAs("", "me", "", backend.TaskIn, "job")

	tests := []struct {
		name  string
		id    string
		every int
		redo  bool
		err   string
	}{
		{"fine", id, 60, false, ""},
		{"too often", id, 10, false, "at least a minute"},
		{"unknown", "nope", 60, false, "no delegation or task"},
		{"empty", "", 60, false, "id is required"},
		{"resend for a coding run", run, 60, true, "only an order given to a hive worker"},
		{"resend for an inbound turn", in, 60, true, "only an order given to a hive worker"},
		{"resend for a worker order", id, 60, true, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := a.TaskTrack(tc.id, tc.every, 0, tc.redo)
			if tc.err == "" && res["ok"] != true {
				t.Fatalf("%v", res)
			}
			if tc.err != "" && !strings.Contains(res["error"].(string), tc.err) {
				t.Fatalf("%v", res)
			}
		})
	}
	// The queen's tools go through the same path and can be listed and ended.
	tl := tools{}
	a.registerQueenTrackTools(tl)
	out, _ := tl["task_tracks"](context.Background(), nil)
	list := out.(map[string]any)["data"].([]map[string]any)
	if len(list) != 2 {
		t.Fatalf("%d trackers", len(list))
	}
	got, _ := tl["task_untrack"](context.Background(), map[string]any{"id": list[0]["id"].(string)[:8]})
	if got.(map[string]any)["data"].(map[string]any)["state"] != backend.TrackStopped {
		t.Fatalf("%v", got)
	}
	// hive_tasks reads the persisted board, by id start too.
	got, err := tl["hive_tasks"](context.Background(), map[string]any{"id": id[:8]})
	if err != nil || got.(map[string]any)["task"].(backend.HiveTask).ID != id {
		t.Fatalf("%v %v", got, err)
	}
}
