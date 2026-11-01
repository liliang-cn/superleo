package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// The hive's task board.
//
// Every order that crosses the hive is a task, and this is where its life is
// written down: sent, thinking, calling a tool, writing the answer, done or
// not. The queen records the orders it gives; a worker records the ones it is
// given. Both publish each change as it happens, which is what lets the Hive
// panel show work in motion instead of a list that was true a few seconds ago.
//
// It is a board and not a log on purpose. The last few tasks are held in
// memory; with a store attached (AttachStore) every change is also written to
// disk, so a restart finds the orders again and a month of them can be asked
// about. A hive's history still belongs to the sessions the workers ran, and
// a second copy of those here would be a second place for it to be wrong.

// HiveSessionPrefix marks a conversation a worker opened because the queen
// asked. It is how a worker knows an order came from the hive rather than from
// a person at its keyboard.
const HiveSessionPrefix = "hive:"

// HiveDirectSessionPrefix marks a conversation a worker opened because the
// person asked it something directly — its card on the phone, the desktop or
// the web — with the queen only carrying the words. It is a hive: session
// all the same (on the board, under the hive's tenant, kept out of the
// worker's own list), but the worker answers the person, not the queen.
const HiveDirectSessionPrefix = HiveSessionPrefix + "direct:"

// IsDirectAsk reports whether a worker's session is the person asking it
// directly rather than an order from the queen.
func IsDirectAsk(session string) bool {
	return strings.HasPrefix(session, HiveDirectSessionPrefix)
}

type directAskKey struct{}

// AskedDirectly marks ctx as the person asking an agent themselves, so a
// worker it reaches is told so (HiveDirectSessionPrefix) instead of being
// handed an order.
func AskedDirectly(ctx context.Context) context.Context {
	return context.WithValue(ctx, directAskKey{}, true)
}

func askedDirectly(ctx context.Context) bool {
	v, _ := ctx.Value(directAskKey{}).(bool)
	return v
}

// HiveTenant is the agent-go tenant every order from the hive runs under on a
// worker, so its runs can be told apart from a person's own turns — and, with
// CancelTenant, stopped together without touching those.
const HiveTenant = "hive"

const (
	TaskRunning   = "running"
	TaskDone      = "done"
	TaskFailed    = "failed"
	TaskCancelled = "cancelled"

	// Which way an order went, from this instance's side.
	TaskOut = "out" // this instance gave the order
	TaskIn  = "in"  // this instance received it
	// TaskPeer is an order one worker gave another. The queen sees these
	// because the worker reports them; neither end is the queen.
	TaskPeer = "peer"

	// What a running task is doing right now.
	PhaseThinking = "thinking"
	PhaseTool     = "tool"
	PhaseWriting  = "writing"
)

// boardKeep is how many finished tasks stay on the board.
const boardKeep = 60

// What a task keeps. Generous, because the detail page shows the whole order
// and the whole answer; still bounded, because the board is memory and an
// answer can be as long as a model can write.
const (
	promptKeep = 4000
	resultKeep = 20000
	errorKeep  = 2000
	eventsKeep = 200
	liveKeep   = 400
)

// liveEvery is the least time between two publishes of a task's Live text.
const liveEvery = 200 * time.Millisecond

// TaskEvent is one step in a task's life: when, and what it was doing.
type TaskEvent struct {
	At    time.Time `json:"at"`
	Phase string    `json:"phase"`
	Tool  string    `json:"tool,omitempty"`
}

// HiveTask is one order and where it stands.
//
// The ID is a UUID made once, by whoever gave the order, and kept everywhere the
// order goes: the queen's board, the worker's board, the worker's session
// ("hive:<id>") and the address of the detail page. One order, one name.
type HiveTask struct {
	ID     string `json:"id"`
	Worker string `json:"worker"`
	// From is who gave the order when it was not the queen: the asking worker,
	// for a peer task.
	From string `json:"from,omitempty"`
	Dir  string `json:"dir"`
	// Session is the conversation the order was given in, so the orders one
	// request fanned out into can be shown together.
	Session string `json:"session,omitempty"`
	Prompt  string `json:"prompt"`
	State   string `json:"state"`
	Phase   string `json:"phase,omitempty"`
	// Tool is the tool being called while Phase is "tool"; Tools counts how many
	// have been called so far.
	Tool      string      `json:"tool,omitempty"`
	Tools     int         `json:"tools"`
	StartedAt time.Time   `json:"started_at"`
	EndedAt   time.Time   `json:"ended_at,omitempty"`
	Result    string      `json:"result,omitempty"`
	Error     string      `json:"error,omitempty"`
	Events    []TaskEvent `json:"events,omitempty"`
	// Run is the coding run carrying the order out, when it went to a coding
	// agent: what lets a restarted queen find the run again.
	Run string `json:"run,omitempty"`
	// Live is the tail of what the task is doing right now — the tool being
	// called with its arguments, how a call came out, the words being written —
	// for the hologram over the worker. Only while running, only the last
	// liveKeep characters, and published at most every liveEvery.
	Live string `json:"live,omitempty"`

	liveAt    time.Time
	liveTimer *time.Timer
}

// TaskBoard holds the recent tasks and reports each change.
type TaskBoard struct {
	mu      sync.Mutex
	tasks   []*HiveTask
	emit    func(HiveTask)
	pulseFn func(HivePulse)
	// stops are how the running tasks this instance can stop are stopped, by
	// id: an order it gave (the run's context) or one it is carrying out (the
	// turn). Dropped when the task settles.
	stops map[string]func()
	// store, when attached, has every change written to it. Progress goes out
	// on a short debounce (dirty, flushT); a start and an end go at once.
	store   *TaskStore
	dirty   map[string]bool
	flushT  *time.Timer
	pruneAt time.Time
}

// saveEvery is how long a progress change waits to be written, so a worker
// calling tools quickly is a write a second and not a write a call.
var saveEvery = time.Second

// ErrTaskStopped is the cause a task's context carries when it was stopped by
// request, which is how a delegation tells "stop this" from "this worker
// failed" and does not hand the piece to someone else.
var ErrTaskStopped = errors.New("the order was stopped on request")

// NewTaskBoard builds a board; emit is called with a copy after every change,
// outside the lock.
func NewTaskBoard(emit func(HiveTask)) *TaskBoard { return &TaskBoard{emit: emit} }

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// Start records a new running task and returns its id. Safe on a nil board, so
// callers do not have to know whether anyone is watching.
func (b *TaskBoard) Start(worker, dir, prompt string) string {
	return b.StartFrom(worker, "", dir, prompt)
}

// NewTaskID makes the UUID an order will be known by.
func NewTaskID() string { return uuid.NewString() }

type taskIDKey struct{}

// WithTaskID has the order sent under ctx go out under id instead of a new
// one, for a caller that has to know the id before the order is answered (a
// tracker sending a lost order again).
func WithTaskID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, taskIDKey{}, id)
}

// TaskIDFrom is the id WithTaskID set, or "".
func TaskIDFrom(ctx context.Context) string {
	s, _ := ctx.Value(taskIDKey{}).(string)
	return s
}

// TaskIDFromSession recovers the order's id from a worker's session name, and
// says whether there was one. A session that merely starts with the prefix but
// carries something else is treated as having none, so a stray "hive:foo"
// cannot become a task's identity.
func TaskIDFromSession(session string) (string, bool) {
	id, ok := strings.CutPrefix(session, HiveDirectSessionPrefix)
	if !ok {
		id, ok = strings.CutPrefix(session, HiveSessionPrefix)
	}
	if !ok {
		return "", false
	}
	if _, err := uuid.Parse(id); err != nil {
		return "", false
	}
	return id, true
}

// StartFrom is Start for an order that came from somewhere other than the
// queen.
func (b *TaskBoard) StartFrom(worker, from, dir, prompt string) string {
	return b.StartAs("", worker, from, dir, prompt)
}

// StartAs is StartFrom with the id already decided: the one the order was given
// under, when this board is recording an order made elsewhere. An empty id makes
// a new one.
func (b *TaskBoard) StartAs(id, worker, from, dir, prompt string) string {
	if b == nil {
		return ""
	}
	if id == "" {
		id = uuid.NewString()
	}
	now := time.Now()
	t := &HiveTask{
		ID: id, Worker: worker, From: from, Dir: dir, Prompt: clip(prompt, promptKeep),
		State: TaskRunning, Phase: PhaseThinking, StartedAt: now,
		Events: []TaskEvent{{At: now, Phase: "sent"}, {At: now, Phase: PhaseThinking}},
	}
	b.mu.Lock()
	b.tasks = append(b.tasks, t)
	b.trimLocked()
	c := t.copy()
	b.mu.Unlock()
	b.save(c, true)
	b.publish(c)
	return t.ID
}

// Progress notes what a running task is doing. A tool call also counts it.
// InSession records the conversation an order was given in. "" is a no-op.
func (b *TaskBoard) InSession(id, session string) {
	if session == "" {
		return
	}
	b.update(id, func(t *HiveTask) bool {
		if t.Session == session {
			return false
		}
		t.Session = session
		return true
	})
}

type chatSessionKey struct{}

// WithChatSession marks a run's context with the conversation it serves;
// ChatSessionFrom reads it back in a tool that runs within it.
func WithChatSession(ctx context.Context, session string) context.Context {
	return context.WithValue(ctx, chatSessionKey{}, session)
}

// ChatSessionFrom is the conversation a tool call is part of, or "".
func ChatSessionFrom(ctx context.Context) string {
	s, _ := ctx.Value(chatSessionKey{}).(string)
	return s
}

func (b *TaskBoard) Progress(id, phase, tool string) {
	b.update(id, func(t *HiveTask) bool {
		if t.State != TaskRunning {
			return false
		}
		if phase == PhaseTool {
			t.Tools++
		}
		if t.Phase == phase && t.Tool == tool && phase != PhaseTool {
			return false // nothing new to say
		}
		t.Phase, t.Tool = phase, tool
		t.note(phase, tool)
		return true
	})
}

// Live appends to what a running task shows as it works, keeping the last few
// hundred characters. It publishes at most every liveEvery; a change inside
// that window goes out when the window ends.
func (b *TaskBoard) Live(id, s string) {
	if b == nil || id == "" || s == "" {
		return
	}
	b.mu.Lock()
	var t *HiveTask
	for _, x := range b.tasks {
		if x.ID == id {
			t = x
			break
		}
	}
	if t == nil || t.State != TaskRunning {
		b.mu.Unlock()
		return
	}
	r := []rune(t.Live + s)
	if len(r) > liveKeep {
		r = r[len(r)-liveKeep:]
	}
	t.Live = string(r)
	if wait := liveEvery - time.Since(t.liveAt); wait > 0 {
		if t.liveTimer == nil {
			t.liveTimer = time.AfterFunc(wait, func() { b.flushLive(id) })
		}
		b.mu.Unlock()
		return
	}
	t.liveAt = time.Now()
	c := t.copy()
	b.mu.Unlock()
	b.publish(c)
}

func (b *TaskBoard) flushLive(id string) {
	b.mu.Lock()
	var c *HiveTask
	for _, t := range b.tasks {
		if t.ID == id {
			t.liveTimer = nil
			if t.State == TaskRunning {
				t.liveAt = time.Now()
				cp := t.copy()
				c = &cp
			}
			break
		}
	}
	b.mu.Unlock()
	if c != nil {
		b.publish(*c)
	}
}

// Finish settles a task. Only the first call counts: a task that was cancelled
// and then reports done a moment later stays cancelled.
func (b *TaskBoard) Finish(id, state, result, errMsg string) {
	b.update(id, func(t *HiveTask) bool {
		delete(b.stops, id)
		if t.State != TaskRunning {
			return false
		}
		t.State, t.Phase, t.Tool, t.Live = state, "", "", ""
		if t.liveTimer != nil {
			t.liveTimer.Stop()
			t.liveTimer = nil
		}
		t.EndedAt = time.Now()
		t.Result, t.Error = clip(result, resultKeep), clip(errMsg, errorKeep)
		t.note(state, "")
		return true
	})
}

func (b *TaskBoard) update(id string, f func(*HiveTask) bool) {
	if b == nil || id == "" {
		return
	}
	b.mu.Lock()
	var c *HiveTask
	for _, t := range b.tasks {
		if t.ID == id {
			if f(t) {
				cp := t.copy()
				c = &cp
			}
			break
		}
	}
	b.mu.Unlock()
	if c != nil {
		b.save(*c, c.State != TaskRunning)
		b.publish(*c)
	}
}

func (b *TaskBoard) publish(t HiveTask) {
	if b.emit != nil {
		b.emit(t)
	}
}

// trimLocked drops the oldest finished tasks past the limit. A running task is
// never dropped, however old.
func (b *TaskBoard) trimLocked() {
	if len(b.tasks) <= boardKeep {
		return
	}
	kept := b.tasks[:0]
	over := len(b.tasks) - boardKeep
	for _, t := range b.tasks {
		if over > 0 && t.State != TaskRunning {
			over--
			continue
		}
		kept = append(kept, t)
	}
	b.tasks = kept
}

// Recent is the board, oldest first, for a panel that has just opened and has
// missed the events.
func (b *TaskBoard) Recent() []HiveTask {
	if b == nil {
		return []HiveTask{}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]HiveTask, len(b.tasks))
	for i, t := range b.tasks {
		out[i] = t.copy()
	}
	return out
}

// Get finds one task by its id.
func (b *TaskBoard) Get(id string) (HiveTask, bool) {
	if b == nil {
		return HiveTask{}, false
	}
	b.mu.Lock()
	for _, t := range b.tasks {
		if t.ID == id {
			c := t.copy()
			b.mu.Unlock()
			return c, true
		}
	}
	st := b.store
	b.mu.Unlock()
	// Older than the board holds: the store has it, if one is attached.
	return st.Get(id)
}

// OnStop says how a running task is stopped. Only the instance running the
// order (or giving it) can stop it, so a task reported from elsewhere has none.
func (b *TaskBoard) OnStop(id string, stop func()) {
	if b == nil || id == "" || stop == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, t := range b.tasks {
		if t.ID == id && t.State == TaskRunning {
			if b.stops == nil {
				b.stops = map[string]func(){}
			}
			b.stops[id] = stop
			return
		}
	}
}

// Stoppable says whether Stop would stop the task.
func (b *TaskBoard) Stoppable(id string) bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.stops[id] != nil
}

// Stop stops a running task, or says why it cannot. The task settles as
// cancelled through its own Finish, once whatever ran it has stopped.
func (b *TaskBoard) Stop(id string) error {
	if b == nil {
		return errors.New("there is no task board here")
	}
	b.mu.Lock()
	var found *HiveTask
	for _, t := range b.tasks {
		if t.ID == id {
			found = t
			break
		}
	}
	stop := b.stops[id]
	state := ""
	if found != nil {
		state = found.State
	}
	st := b.store
	b.mu.Unlock()
	if found == nil {
		if old, ok := st.Get(id); ok {
			found, state = &old, old.State
		}
	}
	switch {
	case found == nil:
		return errors.New("no such task here: it is not on the board, and the board keeps a month")
	case state != TaskRunning:
		return fmt.Errorf("the task already ended: %s", state)
	case stop == nil:
		return errors.New("this instance neither gave nor is carrying out that order, so it cannot stop it")
	}
	stop()
	return nil
}

// note appends a step to the timeline, keeping the newest when it is full.
func (t *HiveTask) note(phase, tool string) {
	t.Events = append(t.Events, TaskEvent{At: time.Now(), Phase: phase, Tool: tool})
	if len(t.Events) > eventsKeep {
		t.Events = append([]TaskEvent(nil), t.Events[len(t.Events)-eventsKeep:]...)
	}
}

// copy is a value copy that does not share the timeline, so a task handed out
// is not changed by the next step.
func (t *HiveTask) copy() HiveTask {
	c := *t
	c.liveTimer = nil
	c.Events = append([]TaskEvent(nil), t.Events...)
	return c
}

// Ingest takes a task another instance reports, keyed by its own id, and
// publishes it. A worker's dealings with a peer reach the queen this way, so
// the queen's board — and the panel drawn from it — shows the whole hive and
// not only the orders the queen gave.
//
// A finished task is not reopened by a late report of it running.
func (b *TaskBoard) Ingest(in HiveTask) {
	if b == nil || in.ID == "" {
		return
	}
	if r := []rune(in.Live); len(r) > liveKeep {
		in.Live = string(r[len(r)-liveKeep:])
	}
	in.Prompt = clip(in.Prompt, promptKeep)
	in.Result, in.Error = clip(in.Result, resultKeep), clip(in.Error, errorKeep)
	b.mu.Lock()
	for i, t := range b.tasks {
		if t.ID != in.ID {
			continue
		}
		// A lost order is one nobody could find; the worker's own word on it
		// is better than that and replaces it.
		if t.State != TaskRunning && t.State != TaskLost {
			b.mu.Unlock()
			return
		}
		*b.tasks[i] = in
		b.mu.Unlock()
		b.save(in, in.State != TaskRunning)
		b.publish(in)
		return
	}
	cp := in
	b.tasks = append(b.tasks, &cp)
	b.trimLocked()
	b.mu.Unlock()
	b.save(in, in.State != TaskRunning)
	b.publish(in)
}

// HivePulse is one thing that happened while an order was being carried out: a
// tool called, a result back, the model thinking, some text written. It is not
// stored — the board keeps the story of a task, and this is the flicker on top
// of it — but it is published as it happens, and that is what the hive panel
// draws as light travelling between the two ends of the order.
type HivePulse struct {
	Task   string `json:"task"`
	Worker string `json:"worker"`
	From   string `json:"from,omitempty"`
	Dir    string `json:"dir"`
	// Kind is "thinking", "tool", "result" or "text".
	Kind  string `json:"kind"`
	Tool  string `json:"tool,omitempty"`
	Bytes int    `json:"bytes,omitempty"`
}

// SetPulse says where pulses go. Called once, before use.
func (b *TaskBoard) SetPulse(f func(HivePulse)) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.pulseFn = f
	b.mu.Unlock()
}

// Pulse publishes one pulse for a task, filled in with who and where from the
// task itself. A task that is not on the board (or has finished) pulses nothing.
func (b *TaskBoard) Pulse(id, kind, tool string, bytes int) {
	if b == nil || id == "" {
		return
	}
	b.mu.Lock()
	fn := b.pulseFn
	var p HivePulse
	found := false
	for _, t := range b.tasks {
		if t.ID == id && t.State == TaskRunning {
			p = HivePulse{Task: id, Worker: t.Worker, From: t.From, Dir: t.Dir, Kind: kind, Tool: tool, Bytes: bytes}
			found = true
			break
		}
	}
	b.mu.Unlock()
	if found && fn != nil {
		fn(p)
	}
}

// Pulser turns the raw stream of an agent's events into pulses worth drawing.
//
// A model streams text a few characters at a time, many times a second, and a
// panel that drew one beam per chunk would be a blur that says nothing. So a
// tool call and its result are each a pulse, thinking is one at most every
// quarter second, and text is gathered into one pulse every tenth of a second
// that carries how much of it there was.
type Pulser struct {
	fn        func(kind, tool string, bytes int)
	mu        sync.Mutex
	lastThink time.Time
	lastText  time.Time
	pending   int
}

// NewPulser builds one that reports to fn.
func NewPulser(fn func(kind, tool string, bytes int)) *Pulser { return &Pulser{fn: fn} }

// Event takes one agent event: its type, the tool it names, and how many bytes
// of content and of tool result it carried.
func (p *Pulser) Event(typ, tool string, content, result int) {
	if p == nil || p.fn == nil {
		return
	}
	now := time.Now()
	switch typ {
	case "tool_call":
		p.fn("tool", tool, 0)
	case "tool_result":
		p.fn("result", tool, result)
	case "thinking":
		p.mu.Lock()
		ok := now.Sub(p.lastThink) >= 250*time.Millisecond
		if ok {
			p.lastThink = now
		}
		p.mu.Unlock()
		if ok {
			p.fn("thinking", "", 0)
		}
	case "partial":
		p.mu.Lock()
		p.pending += content
		n := 0
		if now.Sub(p.lastText) >= 120*time.Millisecond && p.pending > 0 {
			n, p.pending, p.lastText = p.pending, 0, now
		}
		p.mu.Unlock()
		if n > 0 {
			p.fn("text", "", n)
		}
	}
}

// LiveTool is the line a tool call adds to a task's Live text.
func LiveTool(tool string, args any) string {
	a := ""
	if args != nil {
		if b, err := json.Marshal(args); err == nil && string(b) != "null" && string(b) != "{}" {
			a = " " + string(b)
		}
	}
	return "\n▸ " + tool + clip(a, 120) + "\n"
}

// LiveResult is the line a tool result adds: how it came out, in a few words.
func LiveResult(result any) string {
	if ToolResultFailed(result) {
		return "◂ ✗ " + clip(toolResultError(result), 100) + "\n"
	}
	b, _ := json.Marshal(result)
	return "◂ " + clip(string(b), 100) + "\n"
}

// ---- on disk ----

// AttachStore makes the board persistent: what the store holds is loaded back
// (every running order and the most recent finished ones), and from here on
// every change is written to it. It returns the orders that were running when
// the store was last written — the ones whose fate is not known any more, for
// the caller to follow or mark lost. Called once, before use.
func (b *TaskBoard) AttachStore(s *TaskStore) []HiveTask {
	if b == nil || s == nil {
		return nil
	}
	loaded := s.Active(boardKeep)
	s.Prune(time.Now())
	s.PruneTrackers(time.Now())
	b.mu.Lock()
	b.store = s
	b.pruneAt = time.Now()
	have := map[string]bool{}
	var before []HiveTask
	for _, t := range b.tasks {
		have[t.ID] = true
		before = append(before, t.copy())
	}
	var running []HiveTask
	for i := range loaded {
		t := loaded[i]
		if have[t.ID] {
			continue
		}
		b.tasks = append(b.tasks, &t)
		if t.State == TaskRunning {
			running = append(running, t.copy())
		}
	}
	sort.SliceStable(b.tasks, func(i, j int) bool { return b.tasks[i].StartedAt.Before(b.tasks[j].StartedAt) })
	b.mu.Unlock()
	// What was on the board before the store came is written down too.
	for _, t := range before {
		_ = s.Put(t)
	}
	return running
}

// Store is the attached store, or nil.
func (b *TaskBoard) Store() *TaskStore {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.store
}

// save writes a task down: at once, or on the debounce when it is only
// progress. Not called with the lock held.
func (b *TaskBoard) save(t HiveTask, now bool) {
	b.mu.Lock()
	st := b.store
	if st == nil {
		b.mu.Unlock()
		return
	}
	if !now {
		if b.dirty == nil {
			b.dirty = map[string]bool{}
		}
		b.dirty[t.ID] = true
		if b.flushT == nil {
			b.flushT = time.AfterFunc(saveEvery, b.Flush)
		}
		b.mu.Unlock()
		return
	}
	delete(b.dirty, t.ID)
	prune := time.Since(b.pruneAt) > 6*time.Hour
	if prune {
		b.pruneAt = time.Now()
	}
	b.mu.Unlock()
	if err := st.Put(t); err != nil {
		slog.Warn("task board: could not write a task", "task", t.ID, "error", err)
	}
	if prune {
		st.Prune(time.Now())
		st.PruneTrackers(time.Now())
	}
}

// Flush writes every task whose progress is waiting on the debounce. Called
// by the timer, and on the way out.
func (b *TaskBoard) Flush() {
	if b == nil {
		return
	}
	b.mu.Lock()
	st := b.store
	var out []HiveTask
	for _, t := range b.tasks {
		if b.dirty[t.ID] {
			out = append(out, t.copy())
		}
	}
	b.dirty = nil
	if b.flushT != nil {
		b.flushT.Stop()
		b.flushT = nil
	}
	b.mu.Unlock()
	for _, t := range out {
		if err := st.Put(t); err != nil {
			slog.Warn("task board: could not write a task", "task", t.ID, "error", err)
		}
	}
}

// List is the orders newest first, from the store when there is one and from
// memory otherwise; a task in memory is the fresher copy of itself.
func (b *TaskBoard) List(limit int, runningOnly bool) []HiveTask {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	st := b.store
	mem := make(map[string]HiveTask, len(b.tasks))
	for _, t := range b.tasks {
		mem[t.ID] = t.copy()
	}
	b.mu.Unlock()
	var out []HiveTask
	seen := map[string]bool{}
	for _, t := range st.List(limit+len(mem), runningOnly) {
		if m, ok := mem[t.ID]; ok {
			t = m
		}
		if runningOnly && t.State != TaskRunning {
			continue
		}
		seen[t.ID] = true
		out = append(out, t)
	}
	for id, t := range mem {
		if !seen[id] && (!runningOnly || t.State == TaskRunning) {
			out = append(out, t)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// Resolve is the ids that start with prefix, on the board or in the store.
func (b *TaskBoard) Resolve(prefix string) []string {
	if b == nil || prefix == "" {
		return nil
	}
	b.mu.Lock()
	st := b.store
	seen := map[string]bool{}
	var out []string
	for _, t := range b.tasks {
		if strings.HasPrefix(t.ID, prefix) {
			seen[t.ID] = true
			out = append(out, t.ID)
		}
	}
	b.mu.Unlock()
	for _, id := range st.Prefixed(prefix, 20) {
		if !seen[id] {
			out = append(out, id)
		}
	}
	return out
}

// MarkLost settles a running order nobody can find, with why. Like Finish, only
// a running task changes.
func (b *TaskBoard) MarkLost(id, why string) {
	b.update(id, func(t *HiveTask) bool {
		delete(b.stops, id)
		if t.State != TaskRunning {
			return false
		}
		t.State, t.Phase, t.Tool, t.Live = TaskLost, "", "", ""
		t.EndedAt = time.Now()
		t.Error = clip(why, errorKeep)
		t.note(TaskLost, "")
		return true
	})
}

// SetRun notes the coding run an order went to.
func (b *TaskBoard) SetRun(id, run string) {
	if run == "" {
		return
	}
	b.update(id, func(t *HiveTask) bool {
		if t.Run == run {
			return false
		}
		t.Run = run
		return true
	})
}

// Mirror takes a worker's own record of an order a restart left this side
// unsure of: how far it has got while it runs, how it ended once it has. The
// order keeps its start and its prompt, which are this side's.
func (b *TaskBoard) Mirror(id string, w HiveTask) {
	b.update(id, func(t *HiveTask) bool {
		if t.State != TaskRunning {
			return false
		}
		if w.State != "" && w.State != TaskRunning {
			delete(b.stops, id)
			t.State, t.Phase, t.Tool, t.Live = w.State, "", "", ""
			t.EndedAt = w.EndedAt
			if t.EndedAt.IsZero() {
				t.EndedAt = time.Now()
			}
			t.Result, t.Error = clip(w.Result, resultKeep), clip(w.Error, errorKeep)
			t.note(w.State, "")
			return true
		}
		if t.Phase == w.Phase && t.Tool == w.Tool && t.Tools == w.Tools {
			return false
		}
		t.Phase, t.Tool, t.Tools = w.Phase, w.Tool, w.Tools
		t.note(w.Phase, w.Tool)
		return true
	})
}
