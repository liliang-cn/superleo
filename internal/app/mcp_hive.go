package app

// The hive tools /mcp adds to the ones in mcp.go: delegation, the task board
// and its stop button, and the approvals waiting for the person.
//
// Delegation is asynchronous. A piece can take minutes or hours (it has no
// limit unless one is set, and a failure moves it to another worker or the
// queen), and an MCP
// client such as Claude Code gives up on a call long before that. So
// superleo_hive_delegate starts the delegation, waits a little in case it is
// quick, and otherwise answers with its id; superleo_hive_tasks with that id
// is the rest of it. The delegation runs on its own context here, so the
// caller going away does not stop it; superleo_hive_task_cancel does.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/liliang-cn/superleo/internal/backend"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	// delegationsKeep is how many finished delegations are remembered, in
	// memory, like the task board.
	delegationsKeep = 30
	// How long superleo_hive_delegate waits before answering with the id.
	// Under a minute: past that, clients start timing the call out.
	delegateWaitDefault = 25 * time.Second
	delegateWaitMax     = 55 * time.Second
	// What a delegation keeps of each report and of the tool calls made.
	delegationTextKeep  = 20000
	delegationStepsKeep = 200
	delegationArgsKeep  = 4096
)

// delegationPrefix starts a delegation's id, so the cancel tool and a reader
// tell it from a task board id at a glance.
const delegationPrefix = "dlg-"

// delegationJob is one superleo_hive_delegate call and how it is going.
type delegationJob struct {
	id        string
	caller    string
	tasks     []backend.DelegateTask
	startedAt time.Time
	endedAt   time.Time
	state     string // running, done, failed, cancelled
	// assigned is who has each piece now, as OnAssign said.
	assigned  []string
	fallback  []bool
	results   []backend.DelegateResult
	steps     []backend.WorkerToolEvent
	err       string
	cancel    context.CancelFunc
	cancelled bool
	done      chan struct{}
}

// delegationStore holds the recent delegations. The zero value is ready.
type delegationStore struct {
	mu   sync.Mutex
	jobs []*delegationJob
}

func (s *delegationStore) add(j *delegationJob) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs = append(s.jobs, j)
	if len(s.jobs) <= delegationsKeep {
		return
	}
	over := len(s.jobs) - delegationsKeep
	kept := s.jobs[:0]
	for _, x := range s.jobs {
		if over > 0 && x.state != backend.TaskRunning {
			over--
			continue
		}
		kept = append(kept, x)
	}
	s.jobs = kept
}

func (s *delegationStore) find(id string) *delegationJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs {
		if j.id == id {
			return j
		}
	}
	return nil
}

// startDelegation starts one delegation in the background and returns it.
func (a *App) startDelegation(tasks []backend.DelegateTask, caller string) (*delegationJob, error) {
	a.mu.Lock()
	queen := a.hive != nil
	a.mu.Unlock()
	if !queen {
		return nil, errors.New("this instance is not a queen; it has no workers to delegate to")
	}
	if err := checkDelegateTasks(tasks); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	j := &delegationJob{
		id: delegationPrefix + uuid.NewString(), caller: caller, tasks: tasks, startedAt: time.Now(),
		state: backend.TaskRunning, assigned: make([]string, len(tasks)), fallback: make([]bool, len(tasks)),
		cancel: cancel, done: make(chan struct{}),
	}
	st := &a.delegations
	ctx = backend.WithWorkerTrace(ctx, func(ev backend.WorkerToolEvent) {
		if ev.Kind == "text" {
			return // words being written are not a step
		}
		if b, err := json.Marshal(ev.Args); err != nil || len(b) > delegationArgsKeep {
			ev.Args = nil
		}
		st.mu.Lock()
		if len(j.steps) < delegationStepsKeep {
			j.steps = append(j.steps, ev)
		}
		st.mu.Unlock()
	})
	st.add(j)
	go func() {
		defer close(j.done)
		defer cancel()
		results, err := a.delegate(ctx, tasks, func(i int, worker string, fallback bool) {
			st.mu.Lock()
			j.assigned[i], j.fallback[i] = worker, fallback
			st.mu.Unlock()
		})
		st.mu.Lock()
		defer st.mu.Unlock()
		j.endedAt, j.results = time.Now(), results
		failed := 0
		for _, r := range results {
			if r.Failed {
				failed++
			}
		}
		switch {
		case j.cancelled:
			j.state = backend.TaskCancelled
		case err != nil:
			j.state, j.err = backend.TaskFailed, err.Error()
		case failed == len(results):
			j.state = backend.TaskFailed
		default:
			j.state = backend.TaskDone
		}
	}()
	return j, nil
}

// cancelDelegation stops a running delegation: every piece still out is
// stopped on its worker, and nothing is handed on.
func (a *App) cancelDelegation(j *delegationJob) error {
	st := &a.delegations
	st.mu.Lock()
	if j.state != backend.TaskRunning {
		state := j.state
		st.mu.Unlock()
		return fmt.Errorf("the delegation already ended: %s", state)
	}
	j.cancelled = true
	st.mu.Unlock()
	j.cancel()
	return nil
}

type delegationPiece struct {
	Index    int      `json:"index"`
	Task     string   `json:"task"`
	Needs    []string `json:"needs,omitempty"`
	State    string   `json:"state"`
	Worker   string   `json:"worker,omitempty"`
	Fallback bool     `json:"fallback,omitempty"`
	Why      string   `json:"why,omitempty"`
	Tried    []string `json:"tried,omitempty"`
	Tools    []string `json:"tools,omitempty"`
	// Text is the worker's report, which ends with the raw data its tools
	// returned when it quoted them.
	Text   string `json:"text,omitempty"`
	Reason string `json:"reason,omitempty"`
	TaskID string `json:"task_id,omitempty"`
	MS     int64  `json:"ms,omitempty"`
}

type delegationView struct {
	ID        string                    `json:"id"`
	State     string                    `json:"state"`
	By        string                    `json:"by,omitempty"`
	StartedAt time.Time                 `json:"started_at"`
	EndedAt   time.Time                 `json:"ended_at,omitzero"`
	MS        int64                     `json:"ms"`
	Pieces    []delegationPiece         `json:"pieces"`
	Steps     []backend.WorkerToolEvent `json:"steps,omitempty"`
	Error     string                    `json:"error,omitempty"`
	Next      string                    `json:"next,omitempty"`
}

// view is the delegation as it stands; full adds every tool call and the
// whole of each report.
func (a *App) delegationView(j *delegationJob, full bool) delegationView {
	st := &a.delegations
	st.mu.Lock()
	defer st.mu.Unlock()
	v := delegationView{ID: j.id, State: j.state, By: j.caller, StartedAt: j.startedAt, EndedAt: j.endedAt, Error: j.err}
	end := j.endedAt
	if end.IsZero() {
		end = time.Now()
	}
	v.MS = end.Sub(j.startedAt).Milliseconds()
	for i, t := range j.tasks {
		p := delegationPiece{Index: i, Task: t.Task, Needs: t.Needs, State: backend.TaskRunning, Worker: j.assigned[i], Fallback: j.fallback[i]}
		if i < len(j.results) {
			r := j.results[i]
			p.State = backend.TaskDone
			if r.Failed {
				p.State = backend.TaskFailed
			}
			p.Worker, p.Fallback, p.Why, p.Tried, p.Tools = r.Worker, r.Fallback, r.Why, r.Tried, r.Tools
			p.Reason, p.TaskID, p.MS = r.Reason, r.TaskID, r.MS
			keep := delegationTextKeep
			if !full {
				keep = 600
			}
			p.Text = clipText(r.Text, keep)
		} else if j.state == backend.TaskCancelled {
			p.State = backend.TaskCancelled
		}
		if !full {
			p.Task = clipText(p.Task, 200)
		}
		v.Pieces = append(v.Pieces, p)
	}
	if full {
		v.Steps = append([]backend.WorkerToolEvent(nil), j.steps...)
	}
	if j.state == backend.TaskRunning {
		v.Next = fmt.Sprintf("still running: call superleo_hive_tasks with id %q to follow it, superleo_hive_task_cancel to stop it", j.id)
	}
	return v
}

func clipText(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + fmt.Sprintf(" […%d more characters]", len(r)-n)
}

// taskLine is one task board entry, short.
type taskLine struct {
	ID        string    `json:"id"`
	Worker    string    `json:"worker"`
	From      string    `json:"from,omitempty"`
	Dir       string    `json:"dir"`
	State     string    `json:"state"`
	Phase     string    `json:"phase,omitempty"`
	Tool      string    `json:"tool,omitempty"`
	Tools     int       `json:"tools"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at,omitzero"`
	MS        int64     `json:"ms"`
	Prompt    string    `json:"prompt"`
	Result    string    `json:"result,omitempty"`
	Error     string    `json:"error,omitempty"`
	// Stoppable is whether superleo_hive_task_cancel can stop it from here.
	Stoppable bool `json:"stoppable,omitempty"`
}

type delegationLine struct {
	ID        string    `json:"id"`
	State     string    `json:"state"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at,omitzero"`
	MS        int64     `json:"ms"`
	Pieces    int       `json:"pieces"`
	Done      int       `json:"done"`
	Failed    int       `json:"failed"`
	Workers   []string  `json:"workers,omitempty"`
	First     string    `json:"first_task"`
}

type hiveTasksIn struct {
	ID          string `json:"id,omitempty" jsonschema:"a delegation id (dlg-…) or a task id: the whole record of that one. Omit to list"`
	Limit       int    `json:"limit,omitempty" jsonschema:"how many of each, newest first; default 20, at most 60"`
	RunningOnly bool   `json:"running_only,omitempty" jsonschema:"list only what is still running"`
}

type remoteAgentSendIn struct {
	Agent   string `json:"agent" jsonschema:"who: a named agent (pi-mac, openclaw) or a coding agent on a machine (codex.mac); not a hive worker"`
	Message string `json:"message" jsonschema:"the whole message, standing on its own"`
}

type hiveTaskCancelIn struct {
	ID string `json:"id" jsonschema:"a delegation id (dlg-…) from superleo_hive_delegate, or a running task's id from superleo_hive_tasks"`
}

type hiveDelegateTask struct {
	Task   string   `json:"task" jsonschema:"one self-contained instruction: what to do, absolute dates and times, ids, what to report"`
	Needs  []string `json:"needs,omitempty" jsonschema:"MCP servers the task's tools come from, e.g. [\"noted\"], so it goes to a worker that has them"`
	Worker string   `json:"worker,omitempty" jsonschema:"only to pin the task to one worker; usually omit"`
}

type hiveDelegateIn struct {
	Tasks       []hiveDelegateTask `json:"tasks" jsonschema:"one entry per independent piece of work, at most 20"`
	WaitSeconds *int               `json:"wait_seconds,omitempty" jsonschema:"how long to wait for the result before answering with the id; default 25, at most 55, 0 answers at once"`
}

type approvalLine struct {
	ID      string `json:"id"`
	Tool    string `json:"tool"`
	Command string `json:"command,omitempty"`
	// Summary is the arguments in one line, secrets redacted.
	Summary          string    `json:"summary,omitempty"`
	By               string    `json:"by,omitempty"`
	Conversation     string    `json:"conversation,omitempty"`
	Machine          string    `json:"machine,omitempty"`
	Origin           string    `json:"origin,omitempty"`
	Project          string    `json:"project,omitempty"`
	AskedAt          time.Time `json:"asked_at"`
	ExpiresAt        time.Time `json:"expires_at"`
	ExpiresInSeconds int       `json:"expires_in_seconds"`
}

// pendingApprovals is what is waiting for the person, oldest first. It reads;
// nothing here answers.
func (a *App) pendingApprovals() []approvalLine {
	a.approvalMu.Lock()
	reqs := make([]backend.ApprovalRequest, 0, len(a.approvals))
	for _, p := range a.approvals {
		reqs = append(reqs, p.req)
	}
	a.approvalMu.Unlock()
	sort.Slice(reqs, func(i, j int) bool { return reqs[i].AskedAt.Before(reqs[j].AskedAt) })
	now := time.Now()
	out := make([]approvalLine, 0, len(reqs))
	for _, r := range reqs {
		by := r.By
		if by == "" {
			by = "SuperLeo"
		}
		out = append(out, approvalLine{
			ID: r.ID, Tool: r.Tool, Command: r.Command, Summary: backend.SummarizeArgs(r.Tool, r.Args), By: by,
			Conversation: r.SessionID, Machine: r.Machine, Origin: r.Origin, Project: r.Project,
			AskedAt: r.AskedAt, ExpiresAt: r.ExpiresAt, ExpiresInSeconds: max(0, int(r.ExpiresAt.Sub(now).Seconds())),
		})
	}
	return out
}

// hiveTaskList is superleo_hive_tasks without an id.
func (a *App) hiveTaskList(limit int, runningOnly bool) map[string]any {
	if limit <= 0 {
		limit = 20
	}
	limit = min(limit, 60)
	board := a.tasks()
	tasks := []taskLine{}
	for _, t := range board.List(limit, runningOnly) {
		end := t.EndedAt
		if end.IsZero() {
			end = time.Now()
		}
		tasks = append(tasks, taskLine{
			ID: t.ID, Worker: t.Worker, From: t.From, Dir: t.Dir, State: t.State, Phase: t.Phase, Tool: t.Tool,
			Tools: t.Tools, StartedAt: t.StartedAt, EndedAt: t.EndedAt, MS: end.Sub(t.StartedAt).Milliseconds(),
			Prompt: clipText(t.Prompt, 200), Result: clipText(t.Result, 400), Error: clipText(t.Error, 300),
			Stoppable: t.State == backend.TaskRunning && board.Stoppable(t.ID),
		})
	}

	st := &a.delegations
	st.mu.Lock()
	jobs := append([]*delegationJob(nil), st.jobs...)
	st.mu.Unlock()
	dels := []delegationLine{}
	for i := len(jobs) - 1; i >= 0 && len(dels) < limit; i-- {
		v := a.delegationView(jobs[i], false)
		if runningOnly && v.State != backend.TaskRunning {
			continue
		}
		l := delegationLine{ID: v.ID, State: v.State, StartedAt: v.StartedAt, EndedAt: v.EndedAt, MS: v.MS, Pieces: len(v.Pieces)}
		seen := map[string]bool{}
		for _, p := range v.Pieces {
			switch p.State {
			case backend.TaskDone:
				l.Done++
			case backend.TaskFailed:
				l.Failed++
			}
			if p.Worker != "" && !seen[p.Worker] {
				seen[p.Worker] = true
				l.Workers = append(l.Workers, p.Worker)
			}
		}
		if len(v.Pieces) > 0 {
			l.First = v.Pieces[0].Task
		}
		dels = append(dels, l)
	}
	return map[string]any{"delegations": dels, "tasks": tasks}
}

// hiveTaskOne is superleo_hive_tasks with an id.
func (a *App) hiveTaskOne(id string) (any, error) {
	id, err := a.resolveTaskID(id)
	if err != nil {
		return nil, err
	}
	if j := a.delegations.find(id); j != nil {
		return map[string]any{"delegation": a.delegationView(j, true)}, nil
	}
	t, _ := a.tasks().Get(id)
	return map[string]any{"task": t, "stoppable": t.State == backend.TaskRunning && a.tasks().Stoppable(id)}, nil
}

// resolveTaskID turns what was typed into a full delegation or task id: the
// id itself, or the start of exactly one (the 8 characters hive_task shows).
// A start shared by several is refused with the ids it could be.
func (a *App) resolveTaskID(id string) (string, error) {
	if a.delegations.find(id) != nil {
		return id, nil
	}
	if _, ok := a.tasks().Get(id); ok {
		return id, nil
	}
	var matches []string
	seen := map[string]bool{}
	add := func(full string) {
		if strings.HasPrefix(full, id) && !seen[full] {
			seen[full] = true
			matches = append(matches, full)
		}
	}
	st := &a.delegations
	st.mu.Lock()
	for _, j := range st.jobs {
		add(j.id)
	}
	st.mu.Unlock()
	for _, full := range a.tasks().Resolve(id) {
		add(full)
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no delegation or task %q here: orders are kept for a month, delegations only while this runs", id)
	case 1:
		return matches[0], nil
	}
	sort.Strings(matches)
	return "", fmt.Errorf("%q is the start of %d ids; give more of it: %s", id, len(matches), strings.Join(matches, ", "))
}

// stopTarget finds what an id names and whether it can be stopped, so the
// person is only asked about a stop that would do something. id is a full
// one, from resolveTaskID.
func (a *App) stopTarget(id string) (stop func() error, what string, err error) {
	if j := a.delegations.find(id); j != nil {
		st := &a.delegations
		st.mu.Lock()
		state := j.state
		st.mu.Unlock()
		if state != backend.TaskRunning {
			return nil, "", fmt.Errorf("the delegation already ended: %s", state)
		}
		return func() error { return a.cancelDelegation(j) }, "delegation", nil
	}
	board := a.tasks()
	t, ok := board.Get(id)
	switch {
	case !ok:
		return nil, "", fmt.Errorf("no delegation or task %q here: orders are kept for a month, delegations only while this runs", id)
	case t.State != backend.TaskRunning:
		return nil, "", fmt.Errorf("the task already ended: %s", t.State)
	case !board.Stoppable(id):
		return nil, "", errors.New("this instance neither gave nor is carrying out that order, so it cannot stop it")
	}
	return func() error { return board.Stop(id) }, "task", nil
}

// addHiveMCPTools puts delegation, the task board and the pending approvals
// on the MCP server.
func addHiveMCPTools(s *mcp.Server, app *App, caller string) {
	mcp.AddTool(s, &mcp.Tool{
		Name:  "superleo_hive_delegate",
		Title: "Delegate work to the hive",
		Description: "Hand concrete pieces of tool work to the hive's workers, the way the queen does: each piece goes to the " +
			"least busy live worker that has the MCP servers in needs; a worker that fails or times out is replaced by another, " +
			"and when nobody can do it the queen does it herself. Several independent pieces run at the same time on different " +
			"workers. Each piece stands alone (the worker sees nothing else), so put everything needed in it. " +
			"Asynchronous: it waits up to wait_seconds (default 25) and returns the delegation, with id and state. If state is " +
			"still running, call superleo_hive_tasks with that id until it is done, failed or cancelled. When finished, each " +
			"piece has who did it, the tools it called, its report (which carries the raw data its tools returned) and its task " +
			"id; the full record adds every tool call with its arguments. Only on a queen; at most 20 pieces per call.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in hiveDelegateIn) (*mcp.CallToolResult, any, error) {
		tasks := make([]backend.DelegateTask, 0, len(in.Tasks))
		for _, t := range in.Tasks {
			dt := backend.DelegateTask{Task: strings.TrimSpace(t.Task), Worker: strings.TrimSpace(t.Worker)}
			for _, n := range t.Needs {
				if n = strings.TrimSpace(n); n != "" {
					dt.Needs = append(dt.Needs, n)
				}
			}
			if dt.Task != "" {
				tasks = append(tasks, dt)
			}
		}
		j, err := app.startDelegation(tasks, caller)
		if err != nil {
			return toolErr(err.Error())
		}
		wait := delegateWaitDefault
		if in.WaitSeconds != nil {
			wait = min(max(time.Duration(*in.WaitSeconds)*time.Second, 0), delegateWaitMax)
		}
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-j.done:
		case <-timer.C:
		case <-ctx.Done():
		}
		return toolOK(app.delegationView(j, true))
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:  "superleo_hive_tasks",
		Title: "Hive tasks and delegations",
		Description: "What the hive is doing and has done. Without id: the recent delegations (superleo_hive_delegate calls: " +
			"state, pieces done and failed, workers) and the recent task board orders (each order one worker carries out: " +
			"worker, direction, state running/done/failed/cancelled/lost, current phase and tool, timings, the start of the prompt " +
			"and result, and whether it can be stopped from here), newest first. With id: the whole record of that " +
			"delegation (every piece's report and every tool call) or task (full prompt, full result, every step). Orders " +
			"are kept for a month and survive a restart (an order a restart lost track of is state lost, with why); " +
			"delegations only while SuperLeo runs.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in hiveTasksIn) (*mcp.CallToolResult, any, error) {
		if id := strings.TrimSpace(in.ID); id != "" {
			out, err := app.hiveTaskOne(id)
			if err != nil {
				return toolErr(err.Error())
			}
			return toolOK(out)
		}
		return toolOK(app.hiveTaskList(in.Limit, in.RunningOnly))
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:  "superleo_hive_task_cancel",
		Title: "Stop a task or delegation",
		Description: "Stop a running delegation (every piece still out is stopped on its worker and nothing is handed on) or " +
			"one running order on the task board (one this instance gave or is carrying out). Passes SuperLeo's approval gate: " +
			"the person is asked on the phone and the call waits for the answer, denied if nobody answers in time. Refused " +
			"at once, without asking, for an id that is unknown or already finished.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(true)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in hiveTaskCancelIn) (*mcp.CallToolResult, any, error) {
		id := strings.TrimSpace(in.ID)
		if id == "" {
			return toolErr("id is required: a delegation id or a task id from superleo_hive_tasks")
		}
		id, err := app.resolveTaskID(id)
		if err != nil {
			return toolErr(err.Error())
		}
		stop, what, err := app.stopTarget(id)
		if err != nil {
			return toolErr(err.Error())
		}
		if refused := app.mcpGate(ctx, caller, "superleo_hive_task_cancel", map[string]any{"id": id}); refused != nil {
			return refused, nil, nil
		}
		if err := stop(); err != nil {
			return toolErr(err.Error())
		}
		return toolOK(map[string]any{"stopping": id, "kind": what,
			"note": "stopped; it settles as cancelled once its worker has stopped, which superleo_hive_tasks shows"})
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:  "superleo_remote_agent_send",
		Title: "Message an agent outside the hive",
		Description: "Send a message to an agent that is not a hive worker — a named agent on a linked machine (pi-mac, " +
			"openclaw) or a coding agent on a machine (codex.mac) — and wait for its answer. It has no memory of earlier " +
			"messages, so the message must stand alone. The result names the task id, which superleo_hive_task_cancel stops. " +
			"Passes SuperLeo's approval gate.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(true)},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in remoteAgentSendIn) (*mcp.CallToolResult, any, error) {
		name, msg := strings.TrimSpace(in.Agent), strings.TrimSpace(in.Message)
		if name == "" || msg == "" {
			return toolErr("agent and message are both required")
		}
		if _, onRoster := app.remoteRunner().Workers()[name]; onRoster {
			return toolErr(name + " is a hive worker: use superleo_hive_command or superleo_hive_send")
		}
		if refused := app.mcpGate(ctx, caller, "superleo_remote_agent_send", map[string]any{"agent": name, "message": msg}); refused != nil {
			return refused, nil, nil
		}
		text, failed, handled, id := app.delegateOutsideID(ctx, name, msg)
		if !handled {
			return toolErr(fmt.Sprintf("no agent called %q here; superleo_linked_agents says who there is", name))
		}
		return toolOK(map[string]any{"agent": name, "task": id, "ok": !failed, "answer": text})
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:  "superleo_approvals_pending",
		Title: "Pending approvals",
		Description: "The tool calls waiting for the person to allow or deny them, oldest first: what (tool, the command or " +
			"a one-line summary of the arguments, secrets redacted), who asked, in which conversation or on which machine, " +
			"when, and when it expires (denied if nobody answers). Read only: approvals are answered by the person on the " +
			"phone, never from here; tell them what is waiting.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		return toolOK(map[string]any{"pending": app.pendingApprovals()})
	})
}
