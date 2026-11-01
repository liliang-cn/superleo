package app

// The task board across a restart, and the checks left on long orders.
//
// The board (backend/hive_tasks.go) is written to <data>/taskboard.db as it
// changes. What this file adds is what happens when SuperLeo starts and finds
// orders that were running when it stopped:
//
//   - one handed to a coding agent: the run is looked for in the coding runs
//     (cli_live.go brings the local ones back) and the order follows it again;
//   - one handed to a worker: the worker still has its own record of the order
//     under the same id, so the queen asks it, for as long as it answers, and
//     takes its progress and its end from it;
//   - anything else — a turn this worker was carrying out, an order given to an
//     agent whose answer was coming back over a connection that is gone — is
//     marked lost, with why.
//
// A tracker is a check left on an order for a long job: every so often look at
// where it stands, and say so when it finished, failed, was lost or went quiet.

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/liliang-cn/agent-go/v3/pkg/agent"
	"github.com/liliang-cn/superleo/internal/backend"
)

// How a restarted queen follows an order a worker still has: how often it asks,
// and how long a worker may stay unreachable before the order is called lost.
// Workers announce themselves to a queen that has just started, so the first
// minutes are normally a worker not being on the roster yet.
var (
	orderPoll  = 3 * time.Second
	orderGrace = 2 * time.Minute
)

// trackTick is how often the trackers are looked at to see which are due.
var trackTick = 30 * time.Second

type taskBoardState struct {
	mu   sync.Mutex
	quit chan struct{} // ends the followers
	// stopTrack ends the loop that looks at the checks.
	stopTrack chan struct{}
	// say, when set, takes what a tracker has to tell the person in place of
	// the notice; for tests.
	say func(level backend.Level, msg, session, key string)
}

func (a *App) boardQuit() chan struct{} {
	a.taskBoard.mu.Lock()
	defer a.taskBoard.mu.Unlock()
	if a.taskBoard.quit == nil {
		a.taskBoard.quit = make(chan struct{})
	}
	return a.taskBoard.quit
}

// openTaskBoard attaches the board's file and settles the orders it finds
// still running. A file that cannot be opened leaves the board in memory, as
// it was.
func (a *App) openTaskBoard() {
	board := a.tasks()
	if board.Store() != nil {
		return
	}
	st, err := backend.OpenTaskStore(backend.TaskStorePath())
	if err != nil {
		slog.Warn("task board: not kept across restarts", "error", err)
		return
	}
	running := board.AttachStore(st)
	a.reconcileOrders(running)
}

// stopTaskBoard is SuperLeo stopping: what waits on the debounce is written,
// and the followers and the trackers stand down.
func (a *App) stopTaskBoard() {
	a.stopTrackers()
	a.taskBoard.mu.Lock()
	if a.taskBoard.quit != nil {
		close(a.taskBoard.quit)
		a.taskBoard.quit = nil
	}
	a.taskBoard.mu.Unlock()
	if board := a.tasks(); board.Store() != nil {
		board.Flush()
	}
}

// reconcileOrders decides, for each order that was running when SuperLeo
// stopped, whether it can still be followed.
func (a *App) reconcileOrders(running []backend.HiveTask) {
	board := a.tasks()
	for _, t := range running {
		switch {
		case t.Dir == backend.TaskIn:
			board.MarkLost(t.ID, "this machine restarted while it was carrying the order out, and the turn did not survive")
		case t.Dir == backend.TaskPeer:
			board.MarkLost(t.ID, "SuperLeo restarted before the end of this order was reported; if the worker reports it later, that replaces this")
		case t.Run != "":
			a.followCLIOrder(t)
		default:
			go a.followWorkerOrder(t)
		}
	}
}

// followCLIOrder takes an order that went to a coding agent back to its run.
func (a *App) followCLIOrder(t backend.HiveTask) {
	board := a.tasks()
	s := a.cliStore()
	s.mu.Lock()
	run := s.runs[t.Run]
	var state, errMsg, summary, remote string
	if run != nil {
		state, errMsg, summary, remote = run.State, run.Error, run.Summary, run.Remote
		if state == "running" {
			s.watch[run.ID] = a.cliTaskFeed(t.ID)
			s.done[run.ID] = func(r CLIRun) { a.finishFromRun(t.ID, r.State, r.Summary, r.Error) }
		}
	}
	s.mu.Unlock()
	switch {
	case run == nil:
		board.MarkLost(t.ID, fmt.Sprintf("the coding run %s is no longer in this machine's records", t.Run))
	case state == "running":
		board.OnStop(t.ID, func() { a.CancelCLIRun(t.Run) })
		slog.Info("task board: following an order's coding run again", "task", t.ID, "run", t.Run)
	case remote != "":
		board.MarkLost(t.ID, fmt.Sprintf("the run was on %s and followed from here; the line to it ended with this restart (%s). It may still be going there: run %s", remote, errMsg, t.Run))
	case state == "cancelled" && strings.HasPrefix(errMsg, "SuperLeo stopped"):
		board.MarkLost(t.ID, "SuperLeo stopped while the coding run was going, and it could not be picked up")
	default:
		a.finishFromRun(t.ID, state, summary, errMsg)
	}
}

// finishFromRun settles an order from how its coding run ended.
func (a *App) finishFromRun(id, state, summary, errMsg string) {
	switch state {
	case "done":
		a.tasks().Finish(id, backend.TaskDone, firstLineOf(summary, 400), "")
	case "cancelled":
		a.tasks().Finish(id, backend.TaskCancelled, firstLineOf(summary, 400), "stopped")
	default:
		a.tasks().Finish(id, backend.TaskFailed, firstLineOf(summary, 400), firstNonEmpty(errMsg, state))
	}
}

// followWorkerOrder asks the worker how an order stands until it is over. The
// worker keeps its own record of it under the order's id, and that record is
// the truth: this side only lost its connection to it.
func (a *App) followWorkerOrder(t backend.HiveTask) {
	board := a.tasks()
	quit := a.boardQuit()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stopped atomic.Bool
	board.OnStop(t.ID, func() { stopped.Store(true); cancel() })
	go func() {
		select {
		case <-quit:
			cancel()
		case <-ctx.Done():
		}
	}()

	deadline := time.Now().Add(orderGrace)
	var lastErr error
	for {
		var out struct {
			OK    bool             `json:"ok"`
			Error string           `json:"error"`
			Task  backend.HiveTask `json:"task"`
		}
		err := a.remoteRunner().CallWorker(ctx, t.Worker, "HiveTaskDetail", []any{t.ID}, &out)
		switch {
		case ctx.Err() != nil:
			if stopped.Load() {
				bg, c := context.WithTimeout(context.Background(), 5*time.Second)
				_ = a.remoteRunner().CallWorker(bg, t.Worker, "StopHiveTask", []any{t.ID}, nil)
				c()
				board.Finish(t.ID, backend.TaskCancelled, "", "stopped")
			}
			// SuperLeo stopping leaves the order running for the next start.
			return
		case err != nil:
			lastErr = err
			if time.Now().After(deadline) {
				board.MarkLost(t.ID, fmt.Sprintf("%s could not be reached for %s after SuperLeo restarted (%v)", t.Worker, orderGrace, lastErr))
				return
			}
		case !out.OK:
			board.MarkLost(t.ID, fmt.Sprintf("%s has no record of the order: it was restarted as well, or never took it", t.Worker))
			return
		default:
			deadline = time.Now().Add(orderGrace)
			board.Mirror(t.ID, out.Task)
			if out.Task.State != backend.TaskRunning {
				return
			}
		}
		select {
		case <-time.After(orderPoll):
		case <-ctx.Done():
		}
	}
}

// StopHiveTask stops an order this instance is carrying out, by its id: how a
// queen that restarted stops an order she no longer holds the connection of.
func (a *App) StopHiveTask(id string) string {
	if err := a.tasks().Stop(strings.TrimSpace(id)); err != nil {
		return err.Error()
	}
	return "ok"
}

// ---- trackers ----

const (
	trackMin  = time.Minute
	trackMax  = 50 // active trackers
	trackList = 40
)

// TaskTrack leaves a check on an order or a long task: every everySeconds,
// look at it, until it is over or until untilHours from now (0 for no limit).
// With redo, a lost order is sent again to the same worker.
func (a *App) TaskTrack(id string, everySeconds int, untilHours float64, redo bool) map[string]any {
	return a.trackTask(id, time.Duration(everySeconds)*time.Second, untilHours, redo, "")
}

func (a *App) trackTask(id string, every time.Duration, untilHours float64, redo bool, session string) map[string]any {
	st := a.tasks().Store()
	if st == nil {
		return failWith("the task board is not kept on disk here, so a check could not outlive this run")
	}
	if every < trackMin {
		return failWith("the interval must be at least a minute")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return failWith("id is required: an order's task id, or a long task's id")
	}
	kind, full := backend.TrackOrder, ""
	if a.delegations.find(id) != nil {
		return failWith("that is a delegation; track one of its pieces by the task id it names")
	}
	if r, err := a.resolveTaskID(id); err == nil {
		full = r
	} else if ls := a.LongRunState(id); ls != nil {
		kind, full = backend.TrackLong, ls.TaskID
	} else {
		return failWith(err.Error())
	}
	if redo {
		t, _ := a.tasks().Get(full)
		if kind != backend.TrackOrder || t.Dir != backend.TaskOut || t.Run != "" {
			return failWith("only an order given to a hive worker can be sent again")
		}
	}
	if len(st.Trackers(true)) >= trackMax {
		return failWith("there are already too many checks running; remove some with task_untrack")
	}
	now := time.Now()
	var until time.Time
	if untilHours > 0 {
		until = now.Add(time.Duration(untilHours * float64(time.Hour)))
	}
	tr := backend.NewTracker(full, kind, every, until, redo, session, now)
	if err := st.PutTracker(tr); err != nil {
		return failWith(err.Error())
	}
	return okData(trackView(tr))
}

// TaskTracks lists the checks, the live ones first.
func (a *App) TaskTracks() map[string]any {
	st := a.tasks().Store()
	if st == nil {
		return okData([]any{})
	}
	all := st.Trackers(false)
	out := []map[string]any{}
	for pass := 0; pass < 2; pass++ {
		for _, t := range all {
			if (t.State == backend.TrackActive) == (pass == 0) && len(out) < trackList {
				out = append(out, trackView(t))
			}
		}
	}
	return okData(out)
}

// TaskUntrack ends a check.
func (a *App) TaskUntrack(id string) map[string]any {
	st := a.tasks().Store()
	tr, err := st.Tracker(strings.TrimSpace(id))
	if err != nil {
		return failWith(err.Error())
	}
	if tr.State == backend.TrackActive {
		tr.State = backend.TrackStopped
		if err := st.PutTracker(tr); err != nil {
			return failWith(err.Error())
		}
	}
	return okData(trackView(tr))
}

func trackView(t backend.Tracker) map[string]any {
	v := map[string]any{
		"id": t.ID, "task": t.Task, "kind": t.Kind, "state": t.State,
		"every": t.Every.String(), "checks": t.Checks, "next_check": t.NextAt, "last_found": t.Said,
	}
	if !t.Until.IsZero() {
		v["until"] = t.Until
	}
	if !t.LastAt.IsZero() {
		v["last_check"] = t.LastAt
	}
	if t.Redo {
		v["resend_if_lost"] = true
		v["resent"] = t.Redos
	}
	return v
}

// startTrackers runs the loop that looks at the checks that are due. Only in
// the process that fires schedules, so two on one machine do not say
// everything twice.
func (a *App) startTrackers() {
	a.stopTrackers()
	a.mu.Lock()
	lock := a.scheduleLock
	a.mu.Unlock()
	if lock == nil || a.tasks().Store() == nil {
		return
	}
	stop := make(chan struct{})
	a.taskBoard.mu.Lock()
	a.taskBoard.stopTrack = stop
	a.taskBoard.mu.Unlock()
	go func() {
		tick := time.NewTicker(trackTick)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case now := <-tick.C:
				a.checkTrackers(now)
			}
		}
	}()
}

func (a *App) stopTrackers() {
	a.taskBoard.mu.Lock()
	stop := a.taskBoard.stopTrack
	a.taskBoard.stopTrack = nil
	a.taskBoard.mu.Unlock()
	if stop != nil {
		close(stop)
	}
}

// checkTrackers runs every check that is due at now and returns how many.
func (a *App) checkTrackers(now time.Time) int {
	st := a.tasks().Store()
	due := st.DueTrackers(now)
	for _, tr := range due {
		a.checkTracker(st, tr, now)
	}
	return len(due)
}

func (a *App) checkTracker(st *backend.TaskStore, tr backend.Tracker, now time.Time) {
	subj := backend.TrackSubject{}
	var order backend.HiveTask
	switch tr.Kind {
	case backend.TrackLong:
		if ls := a.LongRunState(tr.Task); ls != nil {
			subj = backend.TrackSubject{Found: true, Name: "the long task " + shortTaskID(ls.TaskID), Last: now}
			switch {
			case ls.Running:
				subj.State = backend.TaskRunning
			case ls.Done:
				subj.State, subj.Detail = backend.TaskDone, firstLineOf(ls.Final, 300)
			default:
				subj.State, subj.Detail = backend.TaskLost, firstNonEmpty(ls.Stop, "it stopped without finishing")
			}
		}
	default:
		if t, ok := a.tasks().Get(tr.Task); ok {
			order, subj = t, backend.SubjectOf(t)
		}
	}
	v := backend.EvaluateTrack(tr, subj, now)
	if v.Redo {
		tr.Task = a.resendOrder(order, tr)
	}
	tr = tr.Checked(now, v)
	if err := st.PutTracker(tr); err != nil {
		slog.Warn("task board: could not write a check", "tracker", tr.ID, "error", err)
	}
	slog.Info("task board: checked an order", "tracker", tr.ID, "task", tr.Task, "found", v.Status, "over", v.Over)
	if v.Say != "" {
		a.trackSay(v.Level, v.Say, tr.Session, "track-"+tr.ID)
	}
}

func shortTaskID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// resendOrder sends a lost order again to the same worker, under an id made
// now so the check can follow it at once, and returns that id.
func (a *App) resendOrder(t backend.HiveTask, tr backend.Tracker) string {
	id := backend.NewTaskID()
	go func() {
		ctx := backend.WithChatSession(backend.WithTaskID(context.Background(), id), tr.Session)
		_, _ = a.remoteRunner().Run(ctx, t.Worker, t.Prompt)
	}()
	return id
}

// trackSay tells the person what a check found, the way a schedule does.
func (a *App) trackSay(level backend.Level, msg, session, key string) {
	if f := a.taskBoard.say; f != nil {
		f(level, msg, session, key)
		return
	}
	a.mu.Lock()
	svc := a.svc
	a.mu.Unlock()
	n := backend.Notice{Level: level, Title: "Order check", Message: msg, Source: "task_track", Session: session, Key: key, Push: true}
	if svc == nil {
		a.emit(backend.NoticeEvent, map[string]any{"level": level, "title": n.Title, "message": msg})
		return
	}
	svc.Notices().Raise(context.Background(), n)
}

// registerQueenTrackTools gives the queen the board's history and the checks.
func (a *App) registerQueenTrackTools(inner toolAdder) {
	inner.AddToolWithMetadata("hive_tasks",
		"The orders you have given, newest first, kept for a month and across restarts: worker, state (running, done,"+
			" failed, cancelled, or lost when a restart left it unfound), the start of the order and of the answer."+
			" With an id, or its first 8 characters, the whole record.",
		map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id":      map[string]any{"type": "string", "description": "One order's task id, or the start of it."},
				"running": map[string]any{"type": "boolean", "description": "Only the ones still running."},
				"limit":   map[string]any{"type": "integer", "description": "How many; default 20."},
			},
		},
		func(ctx context.Context, args map[string]any) (any, error) {
			if id := strings.TrimSpace(str(args["id"])); id != "" {
				return a.hiveTaskOne(id)
			}
			limit, _ := args["limit"].(float64)
			running, _ := args["running"].(bool)
			return a.hiveTaskList(int(limit), running), nil
		},
		agent.ToolMetadata{ReadOnly: true, ConcurrencySafe: true, OutputLimit: -1})

	inner.AddToolWithMetadata("task_track",
		"Keep an eye on a long order: look at it every so often and tell the person when it finished, failed, was lost or"+
			" has shown no progress. Works for an order's task id and for a long task. Say how often (hours) and, if"+
			" there is a deadline, until when. resend_if_lost sends a lost order to the same worker again.",
		map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id":             map[string]any{"type": "string", "description": "The order's task id, or a long task's id."},
				"every_hours":    map[string]any{"type": "number", "description": "How often to look; at least 0.02 (a minute). Default 1."},
				"until_hours":    map[string]any{"type": "number", "description": "Stop looking after this many hours from now; omit to look until it is over."},
				"resend_if_lost": map[string]any{"type": "boolean"},
			},
			"required": []string{"id"},
		},
		func(ctx context.Context, args map[string]any) (any, error) {
			hours, _ := args["every_hours"].(float64)
			if hours <= 0 {
				hours = 1
			}
			until, _ := args["until_hours"].(float64)
			redo, _ := args["resend_if_lost"].(bool)
			return a.trackTask(str(args["id"]), time.Duration(hours*float64(time.Hour)), until, redo, backend.ChatSessionFrom(ctx)), nil
		},
		agent.ToolMetadata{Destructive: true})

	inner.AddToolWithMetadata("task_tracks",
		"The checks left on orders: what each watches, how often, when it looks next and what it last found.",
		map[string]any{"type": "object", "properties": map[string]any{}},
		func(ctx context.Context, args map[string]any) (any, error) { return a.TaskTracks(), nil },
		agent.ToolMetadata{ReadOnly: true, ConcurrencySafe: true})

	inner.AddToolWithMetadata("task_untrack",
		"Stop a check left with task_track, by its id or the start of it.",
		map[string]any{
			"type":       "object",
			"properties": map[string]any{"id": map[string]any{"type": "string"}},
			"required":   []string{"id"},
		},
		func(ctx context.Context, args map[string]any) (any, error) {
			return a.TaskUntrack(str(args["id"])), nil
		},
		agent.ToolMetadata{Destructive: true})
}
