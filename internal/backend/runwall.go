package backend

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/liliang-cn/agent-go/v3/pkg/agent"
)

// The run wall.
//
// A long run is nearly invisible while it is going: its conversation reaches
// the store only when it ends and its events go to whoever called RunStream.
// agent-go's Observer is what narrates one — a callback per model turn, tool
// call, lint verdict, compaction, retry, error and segment boundary — and this
// is that narration kept as state instead of written to a log file, so a
// window can draw it.
//
// One RunWall watches every task the agent runs. State is per task id, which
// is the one identity that spans a segmented run's many sessions.

// RoundStat is one model turn.
type RoundStat struct {
	Segment   int    `json:"segment"`
	Round     int    `json:"round"`
	Tokens    int    `json:"tokens"`
	Cached    int    `json:"cached"`
	Tools     int    `json:"tools"`
	Text      int    `json:"text"`
	DurMs     int64  `json:"durMs"`
	Compacted bool   `json:"compacted,omitempty"`
	Lint      string `json:"lint,omitempty"`
	Retried   bool   `json:"retried,omitempty"`
	Failed    bool   `json:"failed,omitempty"`
}

// SegmentStat is one run of a segmented task.
type SegmentStat struct {
	Index      int    `json:"index"`
	SessionID  string `json:"sessionId"`
	StartedAt  string `json:"startedAt"`
	EndedAt    string `json:"endedAt,omitempty"`
	StopReason string `json:"stopReason,omitempty"`
	Productive bool   `json:"productive"`
	Err        string `json:"err,omitempty"`
	Rounds     int    `json:"rounds"`
}

// LogLine is one line of the activity narration.
type LogLine struct {
	At   string `json:"at"`
	Kind string `json:"kind"`
	Text string `json:"text"`
}

// TaskState is everything the wall knows about one task.
type TaskState struct {
	TaskID    string `json:"taskId"`
	Goal      string `json:"goal"`
	Model     string `json:"model"`
	StartedAt string `json:"startedAt"`
	EndedAt   string `json:"endedAt,omitempty"`
	Done      bool   `json:"done"`
	Running   bool   `json:"running"`
	// State is running, done, failed, stopped or interrupted. Interrupted is
	// a task cut off by SuperLeo going away that is not (or not yet) being
	// carried on.
	State string `json:"state,omitempty"`
	Stop  string `json:"stop,omitempty"`
	Final string `json:"final,omitempty"`

	MaxSegments int `json:"maxSegments"`

	Segments []SegmentStat    `json:"segments"`
	Rounds   []RoundStat      `json:"rounds"`
	Plan     []agent.PlanItem `json:"plan"`

	ToolCounts  map[string]int `json:"toolCounts"`
	ToolCalls   int            `json:"toolCalls"`
	ToolErrors  int            `json:"toolErrors"`
	Lints       map[string]int `json:"lints"`
	LintRetries int            `json:"lintRetries"`
	LintBlocks  int            `json:"lintBlocks"`
	Compactions int            `json:"compactions"`
	Retries     int            `json:"retries"`
	Checkpoints int            `json:"checkpoints"`
	Errors      int            `json:"errors"`

	TotalTokens int `json:"totalTokens"`
	TotalCached int `json:"totalCached"`

	Log []LogLine `json:"log"`
}

// runWallMaxLog bounds the narration kept per task. What a window shows is
// the tail; the whole thing is in the checkpoint store.
const runWallMaxLog = 240

// RunWall implements agent.Observer and keeps per-task state.
type RunWall struct {
	agent.BaseObserver

	mu    sync.Mutex
	tasks map[string]*TaskState
	// order remembers first-seen order so a list is stable across ticks.
	order []string
	// current round being narrated, per task: OnModelStart opens it,
	// OnModelEnd fills it, the callbacks between attach to it.
	open map[string]*RoundStat
	// segment index per task, from the last OnSegment start.
	seg map[string]int
	// plan reads the current plan for a task, so a snapshot can carry it.
	plan func(ctx context.Context, taskID string) []agentPlanItem
	// tick is told a task changed. Optional.
	tick func(taskID, kind string)
}

// NewRunWall returns an empty wall. plan may be nil.
func NewRunWall(plan func(ctx context.Context, taskID string) []agentPlanItem, tick func(taskID, kind string)) *RunWall {
	return &RunWall{
		tasks: map[string]*TaskState{},
		open:  map[string]*RoundStat{},
		seg:   map[string]int{},
		plan:  plan,
		tick:  tick,
	}
}

func nowStamp() string { return time.Now().Format(time.RFC3339Nano) }

// Begin registers a task the wall should expect. Called by whoever starts
// the run, so the goal and model are known before the first callback.
func (w *RunWall) Begin(taskID, goal, model string, maxSegments int) {
	w.mu.Lock()
	t := w.get(taskID)
	t.Goal = goal
	t.Model = model
	t.MaxSegments = maxSegments
	t.Running = true
	t.State = "running"
	t.Done, t.EndedAt, t.Stop = false, "", ""
	if t.StartedAt == "" {
		t.StartedAt = nowStamp()
	}
	w.mu.Unlock()
	w.notify(taskID, "begin")
}

// Finish records how the task ended.
func (w *RunWall) Finish(taskID string, done bool, stop, final string, err error) {
	w.mu.Lock()
	t := w.get(taskID)
	t.Running = false
	t.Done = done
	switch {
	case done:
		t.State = "done"
	case stop == "cancelled":
		t.State = "stopped"
	default:
		t.State = "failed"
	}
	t.Stop = stop
	t.Final = final
	t.EndedAt = nowStamp()
	if err != nil {
		t.Errors++
		w.log(t, "error", "run: "+err.Error())
	}
	w.mu.Unlock()
	w.notify(taskID, "finish")
}

// Seed puts a task on the wall that a previous process was running, so the
// page shows it while it is carried on, or shows why it is not. Its segments
// and rounds are gone with that process; the plan is read from the store.
func (w *RunWall) Seed(taskID, goal, model, state, stop string, started time.Time) {
	w.mu.Lock()
	t := w.get(taskID)
	t.Goal, t.Model, t.State, t.Stop = goal, model, state, stop
	t.Running, t.Done = false, false
	if !started.IsZero() {
		t.StartedAt = started.Format(time.RFC3339Nano)
	}
	w.mu.Unlock()
	w.notify(taskID, state)
}

// Resumed narrates that a task was picked up again after a restart, and tells
// the page so with a kind of its own.
func (w *RunWall) Resumed(taskID string, resumes int) {
	w.mu.Lock()
	t := w.get(taskID)
	w.log(t, "resume", fmt.Sprintf("picked back up after a restart (#%d), from its checkpoint", resumes))
	w.mu.Unlock()
	w.notify(taskID, "resumed")
}

// SetState records a state the run's own result does not say, such as a stop
// a person asked for.
func (w *RunWall) SetState(taskID, state string) {
	w.mu.Lock()
	if t, ok := w.tasks[taskID]; ok {
		t.State = state
	}
	w.mu.Unlock()
}

// Tokens is what the task has used since the wall began watching it.
func (w *RunWall) Tokens(taskID string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	if t, ok := w.tasks[taskID]; ok {
		return t.TotalTokens
	}
	return 0
}

// get returns the state for a task, creating it. Caller holds w.mu.
func (w *RunWall) get(taskID string) *TaskState {
	t, ok := w.tasks[taskID]
	if !ok {
		t = &TaskState{
			TaskID:     taskID,
			ToolCounts: map[string]int{},
			Lints:      map[string]int{},
		}
		w.tasks[taskID] = t
		w.order = append(w.order, taskID)
	}
	return t
}

// watching reports whether a task is one the wall was told about.
//
// The observer sees every run the service makes — every chat turn included —
// and those are not the wall's to narrate. Only a task that went through
// Begin is: without this, one chat message put a stray task on the wall and
// pushed longrun:tick events at a page that never asked. Caller holds w.mu.
func (w *RunWall) watching(taskID string) bool {
	_, ok := w.tasks[taskID]
	return ok
}

// log appends a narration line. Caller holds w.mu.
func (w *RunWall) log(t *TaskState, kind, text string) {
	t.Log = append(t.Log, LogLine{At: nowStamp(), Kind: kind, Text: text})
	if len(t.Log) > runWallMaxLog {
		t.Log = t.Log[len(t.Log)-runWallMaxLog:]
	}
}

func (w *RunWall) notify(taskID, kind string) {
	if w.tick != nil {
		w.tick(taskID, kind)
	}
}

// --- Observer ---

func (w *RunWall) OnSegment(_ context.Context, info agent.SegmentInfo) {
	w.mu.Lock()
	if !w.watching(info.TaskID) {
		w.mu.Unlock()
		return
	}
	t := w.get(info.TaskID)
	if !info.Ending {
		w.seg[info.TaskID] = info.Index
		t.Segments = append(t.Segments, SegmentStat{
			Index: info.Index, SessionID: info.SessionID, StartedAt: nowStamp(),
		})
		if info.Total > 0 {
			t.MaxSegments = info.Total
		}
		w.log(t, "segment", fmt.Sprintf("segment %d/%d start", info.Index+1, info.Total))
	} else {
		for i := range t.Segments {
			s := &t.Segments[i]
			if s.Index == info.Index && s.EndedAt == "" {
				s.EndedAt = nowStamp()
				s.StopReason = string(info.StopReason)
				s.Productive = info.Productive
				s.Err = info.Err
			}
		}
		status := string(info.StopReason)
		if info.Err != "" {
			status = "FAILED: " + oneLine(info.Err, 80)
		}
		w.log(t, "segment", fmt.Sprintf("segment %d/%d end  %s  %s", info.Index+1, info.Total, status, info.Duration.Round(time.Second)))
	}
	w.mu.Unlock()
	w.notify(info.TaskID, "segment")
}

func (w *RunWall) OnModelStart(_ context.Context, info agent.ModelInfo) {
	w.mu.Lock()
	if !w.watching(info.TaskID) {
		w.mu.Unlock()
		return
	}
	t := w.get(info.TaskID)
	r := &RoundStat{Segment: w.seg[info.TaskID], Round: info.Round}
	w.open[info.TaskID] = r
	_ = t
	w.mu.Unlock()
}

func (w *RunWall) OnModelEnd(_ context.Context, info agent.ModelInfo, res *agent.ModelResult, err error) {
	w.mu.Lock()
	if !w.watching(info.TaskID) {
		w.mu.Unlock()
		return
	}
	t := w.get(info.TaskID)
	r := w.open[info.TaskID]
	if r == nil {
		r = &RoundStat{Segment: w.seg[info.TaskID], Round: info.Round}
	}
	if res != nil {
		r.Tokens = res.TokensUsed
		r.Cached = res.CachedTokens
		r.Tools = res.ToolCalls
		r.Text = len(res.Content)
		r.DurMs = res.DurationMs
		t.TotalTokens += res.TokensUsed
		t.TotalCached += res.CachedTokens
	}
	if err != nil {
		r.Failed = true
		t.Errors++
	}
	t.Rounds = append(t.Rounds, *r)
	for i := range t.Segments {
		if t.Segments[i].Index == r.Segment {
			t.Segments[i].Rounds++
		}
	}
	delete(w.open, info.TaskID)
	cached := ""
	if r.Cached > 0 {
		cached = fmt.Sprintf(" cached=%d", r.Cached)
	}
	w.log(t, "model", fmt.Sprintf("r%-3d %s calls=%d text=%d tokens=%d%s",
		r.Round, shortDur(r.DurMs), r.Tools, r.Text, r.Tokens, cached))
	w.mu.Unlock()
	w.notify(info.TaskID, "round")
}

func (w *RunWall) OnToolStart(_ context.Context, info agent.ToolInfo) {
	w.mu.Lock()
	if !w.watching(info.TaskID) {
		w.mu.Unlock()
		return
	}
	t := w.get(info.TaskID)
	t.ToolCalls++
	t.ToolCounts[info.Tool]++
	w.log(t, "tool", "     "+info.Tool+" "+formatArgs(info.Args))
	w.mu.Unlock()
	w.notify(info.TaskID, "tool")
}

func (w *RunWall) OnToolEnd(_ context.Context, info agent.ToolInfo, _ any, err error) {
	if err == nil {
		return
	}
	w.mu.Lock()
	if !w.watching(info.TaskID) {
		w.mu.Unlock()
		return
	}
	t := w.get(info.TaskID)
	t.ToolErrors++
	w.log(t, "error", "     "+info.Tool+" failed: "+oneLine(err.Error(), 160))
	w.mu.Unlock()
	w.notify(info.TaskID, "tool")
}

func (w *RunWall) OnLint(_ context.Context, info agent.LintInfo) {
	w.mu.Lock()
	if !w.watching(info.TaskID) {
		w.mu.Unlock()
		return
	}
	t := w.get(info.TaskID)
	t.Lints[info.Lint]++
	if info.Retrying {
		t.LintRetries++
	} else {
		t.LintBlocks++
	}
	if r := w.open[info.TaskID]; r != nil {
		r.Lint = info.Lint
	} else if n := len(t.Rounds); n > 0 {
		t.Rounds[n-1].Lint = info.Lint
	}
	verdict := "BLOCKED by"
	if info.Retrying {
		verdict = "retry after"
	}
	w.log(t, "lint", fmt.Sprintf("r%-3d %s %s: %s", info.Round, verdict, info.Lint, oneLine(info.Reason, 120)))
	w.mu.Unlock()
	w.notify(info.TaskID, "lint")
}

func (w *RunWall) OnCompaction(_ context.Context, info agent.CompactionInfo) {
	w.mu.Lock()
	if !w.watching(info.TaskID) {
		w.mu.Unlock()
		return
	}
	t := w.get(info.TaskID)
	t.Compactions++
	if r := w.open[info.TaskID]; r != nil {
		r.Compacted = true
	} else if n := len(t.Rounds); n > 0 {
		t.Rounds[n-1].Compacted = true
	}
	w.log(t, "compact", fmt.Sprintf("r%-3d compact %s msgs %d -> %d (~%d tokens)",
		info.Round, info.Trigger, info.MessagesBefore, info.MessagesAfter, info.ContextTokens))
	w.mu.Unlock()
	w.notify(info.TaskID, "compact")
}

func (w *RunWall) OnModelRetry(_ context.Context, info agent.ModelRetryInfo) {
	w.mu.Lock()
	if !w.watching(info.TaskID) {
		w.mu.Unlock()
		return
	}
	t := w.get(info.TaskID)
	t.Retries++
	if r := w.open[info.TaskID]; r != nil {
		r.Retried = true
	}
	switch info.Kind {
	case "max_tokens_truncation":
		w.log(t, "retry", fmt.Sprintf("r%-3d truncated, max_tokens %d -> %d", info.Round, info.MaxTokensFrom, info.MaxTokensTo))
	default:
		w.log(t, "retry", fmt.Sprintf("r%-3d %s attempt=%d: %s", info.Round, info.Kind, info.Attempt, oneLine(info.Reason, 120)))
	}
	w.mu.Unlock()
	w.notify(info.TaskID, "retry")
}

func (w *RunWall) OnError(_ context.Context, info agent.ErrorInfo) {
	w.mu.Lock()
	if !w.watching(info.TaskID) {
		w.mu.Unlock()
		return
	}
	t := w.get(info.TaskID)
	t.Errors++
	marker := info.Marker
	if marker == "" {
		marker = "error"
	}
	w.log(t, "error", marker+": "+oneLine(info.Message, 200))
	w.mu.Unlock()
	w.notify(info.TaskID, "error")
}

func (w *RunWall) OnCheckpoint(_ context.Context, info agent.CheckpointInfo) {
	w.mu.Lock()
	if !w.watching(info.TaskID) {
		w.mu.Unlock()
		return
	}
	t := w.get(info.TaskID)
	t.Checkpoints++
	w.mu.Unlock()
	w.notify(info.TaskID, "checkpoint")
}

// --- Snapshots ---

// Snapshot returns a copy of one task's state, with the plan read fresh.
func (w *RunWall) Snapshot(ctx context.Context, taskID string) *TaskState {
	w.mu.Lock()
	t, ok := w.tasks[taskID]
	if !ok {
		w.mu.Unlock()
		return nil
	}
	cp := *t
	cp.Segments = append([]SegmentStat(nil), t.Segments...)
	cp.Rounds = append([]RoundStat(nil), t.Rounds...)
	cp.Log = append([]LogLine(nil), t.Log...)
	cp.ToolCounts = map[string]int{}
	for k, v := range t.ToolCounts {
		cp.ToolCounts[k] = v
	}
	cp.Lints = map[string]int{}
	for k, v := range t.Lints {
		cp.Lints[k] = v
	}
	w.mu.Unlock()
	if w.plan != nil {
		cp.Plan = w.plan(ctx, taskID)
	}
	if cp.Plan == nil {
		cp.Plan = []agent.PlanItem{}
	}
	return &cp
}

// TaskSummary is one card on the fleet: enough to see how every task is
// doing at once without opening any of them.
type TaskSummary struct {
	TaskID      string `json:"taskId"`
	Goal        string `json:"goal"`
	Model       string `json:"model"`
	StartedAt   string `json:"startedAt"`
	EndedAt     string `json:"endedAt,omitempty"`
	Running     bool   `json:"running"`
	Done        bool   `json:"done"`
	State       string `json:"state,omitempty"`
	Stop        string `json:"stop,omitempty"`
	Segments    int    `json:"segments"`
	MaxSegments int    `json:"maxSegments"`
	SegmentOpen bool   `json:"segmentOpen"`
	Rounds      int    `json:"rounds"`
	LastTokens  int    `json:"lastTokens"`
	LastTools   int    `json:"lastTools"`
	TotalTokens int    `json:"totalTokens"`
	TotalCached int    `json:"totalCached"`
	Rejected    int    `json:"rejected"`
	Errors      int    `json:"errors"`
	PlanDone    int    `json:"planDone"`
	PlanTotal   int    `json:"planTotal"`
	// Spark is tokens per turn for the last few turns, for a card sparkline.
	Spark []int `json:"spark"`
}

// runWallSparkLen is how many recent turns a card's sparkline shows.
const runWallSparkLen = 24

// List returns every task the wall has seen, newest first, each with the
// figures a fleet card draws. Plans are read outside the lock.
func (w *RunWall) List() []TaskSummary {
	w.mu.Lock()
	out := make([]TaskSummary, 0, len(w.order))
	for _, id := range w.order {
		t := w.tasks[id]
		s := TaskSummary{
			TaskID: t.TaskID, Goal: t.Goal, Model: t.Model,
			StartedAt: t.StartedAt, EndedAt: t.EndedAt,
			Running: t.Running, Done: t.Done, State: t.State, Stop: t.Stop,
			Segments: len(t.Segments), MaxSegments: t.MaxSegments,
			Rounds:      len(t.Rounds),
			TotalTokens: t.TotalTokens, TotalCached: t.TotalCached,
			Rejected: t.LintRetries + t.LintBlocks, Errors: t.Errors,
		}
		if n := len(t.Segments); n > 0 && t.Segments[n-1].EndedAt == "" {
			s.SegmentOpen = true
		}
		if n := len(t.Rounds); n > 0 {
			s.LastTokens = t.Rounds[n-1].Tokens
			s.LastTools = t.Rounds[n-1].Tools
			from := n - runWallSparkLen
			if from < 0 {
				from = 0
			}
			for _, r := range t.Rounds[from:] {
				s.Spark = append(s.Spark, r.Tokens)
			}
		}
		if s.Spark == nil {
			s.Spark = []int{}
		}
		out = append(out, s)
	}
	w.mu.Unlock()

	if w.plan != nil {
		ctx := context.Background()
		for i := range out {
			for _, p := range w.plan(ctx, out[i].TaskID) {
				out[i].PlanTotal++
				if p.Done {
					out[i].PlanDone++
				}
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].StartedAt > out[j].StartedAt })
	return out
}

// --- formatting ---

// oneLine squeezes s onto one line of at most max runes — runes, so a cut
// never lands inside a character.
func oneLine(s string, max int) string {
	r := []rune(strings.ReplaceAll(strings.TrimSpace(s), "\n", " "))
	if len(r) > max {
		return string(r[:max-1]) + "…"
	}
	return string(r)
}

func shortDur(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", ms)
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	default:
		return d.Round(time.Second).String()
	}
}

// formatArgs renders tool arguments in a stable order and bounded length —
// sorted, because a map iterates randomly and a log whose lines differ only by
// field order cannot be read.
func formatArgs(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		v := fmt.Sprint(args[k])
		parts = append(parts, k+"="+oneLine(v, 70))
	}
	return strings.Join(parts, " ")
}

// agentPlanItem is agent.PlanItem under a local name, so tests can build a
// plan reader without importing agent-go themselves.
type agentPlanItem = agent.PlanItem
