package watchdog

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Watchdog runs the four checks once per Tick.
type Watchdog struct {
	Cfg    Config
	API    *Client
	Host   Host
	Judge  Judge
	Log    *Log
	Now    func() time.Time
	Sleep  func(time.Duration)
	State  *State
	apiOK  bool
	statep string
}

// New builds a Watchdog with the real host and judge.
func New(cfg Config) (*Watchdog, error) {
	tok, err := ReadToken(cfg.Home)
	log := NewLog(cfg.Home, cfg.LogMaxMB)
	if err != nil {
		log.Line("setup", "warn", "error", err.Error())
	}
	log.Hide(tok)
	log.Hide(cfg.JudgeKey)
	w := &Watchdog{
		Cfg:  cfg,
		API:  &Client{Base: cfg.APIURL, Token: tok, Timeout: cfg.APITimeout()},
		Host: MacHost{AppPath: cfg.AppPath},
		Log:  log, Now: time.Now, Sleep: time.Sleep,
	}
	w.Judge = &ChatJudge{Cfg: cfg, Settings: w.settings}
	w.init()
	return w, nil
}

func (w *Watchdog) init() {
	w.statep = filepath.Join(w.Cfg.Home, "watchdog-state.json")
	w.State = loadState(w.statep)
	if w.Now == nil {
		w.Now = time.Now
	}
	if w.Sleep == nil {
		w.Sleep = time.Sleep
	}
}

// Setup finishes a Watchdog built by hand (tests): state path and defaults.
func (w *Watchdog) Setup() { w.init() }

func (w *Watchdog) settings(ctx context.Context) (settings, error) {
	var s settings
	err := w.API.Call(ctx, "GetSettings", &s)
	if s.LLMKey != "" {
		w.Log.Hide(s.LLMKey)
	}
	return s, err
}

// Run ticks until ctx ends.
func (w *Watchdog) Run(ctx context.Context) {
	w.Log.Line("watchdog", "start", "tick", w.Cfg.Tick().String(), "stall_after", w.Cfg.StallAfter().String())
	for {
		w.Tick(ctx)
		select {
		case <-ctx.Done():
			w.Log.Line("watchdog", "stop")
			return
		case <-time.After(w.Cfg.Tick()):
		}
	}
}

// Tick runs every check once.
func (w *Watchdog) Tick(ctx context.Context) {
	w.apiOK = false
	w.checkAlive(ctx)
	if w.apiOK {
		w.checkRuns(ctx)
		w.checkSchedules(ctx)
	}
	w.State.save(w.statep)
}

// ---- 1. alive ----

func (w *Watchdog) checkAlive(ctx context.Context) {
	now := w.Now()
	var st map[string]any
	err := w.API.Call(ctx, "GetStatus", &st)
	if err == nil {
		w.apiOK = true
		w.State.HangSince = time.Time{}
		w.Log.Line("alive", "ok")
		w.clear("app_down", "app_hung", "app_relaunched")
		return
	}
	reason := w.Log.Scrub(err.Error())
	if w.Host.Running() {
		if w.State.HangSince.IsZero() {
			w.State.HangSince = now
		}
		d := now.Sub(w.State.HangSince)
		w.Log.Line("alive", "hung", "for", d.Round(time.Second).String(), "error", reason)
		if d > w.Cfg.HangAfter() {
			w.raise("app_hung", "app", "SuperLeo 已无响应 "+mins(d)+" 分钟")
		}
		return
	}
	w.State.HangSince = time.Time{}
	if _, perr := os.Stat(filepath.Join(w.Cfg.Home, "watchdog.pause")); perr == nil {
		w.Log.Line("alive", "down_paused")
		return
	}
	// Forget relaunches older than the window.
	keep := w.State.Relaunches[:0]
	for _, t := range w.State.Relaunches {
		if now.Sub(t) < w.Cfg.RelaunchWindow() {
			keep = append(keep, t)
		}
	}
	w.State.Relaunches = keep
	if len(keep) >= w.Cfg.RelaunchMax {
		w.Log.Line("alive", "down_backoff", "relaunches", fmt.Sprint(len(keep)))
		w.raise("app_down", "app", fmt.Sprintf("SuperLeo 已退出，%d 分钟内重启 %d 次仍不行，不再自动重启", int(w.Cfg.RelaunchWindow().Minutes()), len(keep)))
		return
	}
	if n := len(keep); n > 0 && now.Sub(keep[n-1]) < w.Cfg.RelaunchGap() {
		w.Log.Line("alive", "down_waiting_start")
		return
	}
	if err := w.Host.Launch(); err != nil {
		w.Log.Line("alive", "relaunch_failed", "error", err.Error())
		w.raise("app_down", "app", "SuperLeo 已退出，重启失败")
		return
	}
	w.State.Relaunches = append(w.State.Relaunches, now)
	w.Log.Line("alive", "relaunched", "count", fmt.Sprint(len(w.State.Relaunches)))
	w.raise("app_relaunched", "app", "SuperLeo 已退出，已重新打开")
}

// ---- 2 + 3. runs: progress and drift ----

// item is one thing that runs: a coding run, a long run, a hive task.
type item struct {
	Key, Kind, ID string
	Goal          string
	Started       time.Time
	APILast       time.Time // newest timestamp the API shows
	Sig           string    // changes whenever the run does something
	Steps         []string
	run           *cliRun
	long          *longState
}

func (w *Watchdog) checkRuns(ctx context.Context) {
	now := w.Now()
	items, ok := w.gather(ctx)
	if !ok {
		return
	}
	live := map[string]bool{}
	stalled := map[string]bool{}
	if len(items) == 0 {
		w.Log.Line("progress", "ok", "runs", "0")
	}
	for _, it := range items {
		live[it.Key] = true
		last := w.lastProgress(it, now)
		idle := now.Sub(last)
		if idle < w.Cfg.StallAfter() {
			w.Log.Line("progress", "ok", "run", it.Key, "idle", idle.Round(time.Second).String())
			w.drift(ctx, it, now)
			continue
		}
		stalled[it.Key] = true
		w.Log.Line("progress", "stalled", "run", it.Key, "idle", idle.Round(time.Second).String())
		w.stalled(ctx, it, idle, now)
	}
	w.clearExcept("stalled", stalled)
	w.clearExcept("stall_restarted", stalled)
	// Forget runs that are over.
	for k := range w.State.Progress {
		if !live[k] {
			delete(w.State.Progress, k)
		}
	}
	for k, t := range w.State.Intervened {
		if !live[k] && now.Sub(t) > 5*time.Minute {
			delete(w.State.Intervened, k)
		}
	}
	for k := range w.State.Judged {
		if !live[k] {
			delete(w.State.Judged, k)
		}
	}
	w.clearExcept("off_track", live)
}

// lastProgress is the newest of what the API timestamps and when this
// watchdog last saw the run's signature change.
func (w *Watchdog) lastProgress(it item, now time.Time) time.Time {
	last := it.APILast
	if last.IsZero() {
		last = it.Started
	}
	p, seen := w.State.Progress[it.Key]
	switch {
	case !seen:
		w.State.Progress[it.Key] = progress{Sig: it.Sig, At: last}
	case p.Sig != it.Sig:
		w.State.Progress[it.Key] = progress{Sig: it.Sig, At: now}
		p.At = now
	}
	if p.At.After(last) {
		last = p.At
	}
	return last
}

func (w *Watchdog) gather(ctx context.Context) ([]item, bool) {
	var items []item
	var runs []cliRun
	if err := w.API.Call(ctx, "CLIRuns", &runs); err != nil {
		w.Log.Line("progress", "api_error", "call", "CLIRuns", "error", w.Log.Scrub(err.Error()))
		return nil, false
	}
	running := map[string]bool{}
	// A follow-up's own prompt is "continue"; the goal is what its thread's
	// first run was asked, and the work is as old as that run.
	first := map[string]cliRun{}
	for _, r := range runs {
		if f, ok := first[r.Thread]; r.Thread != "" && (!ok || r.Started.Before(f.Started)) {
			first[r.Thread] = r
		}
	}
	for _, r := range runs {
		if r.State != "running" || r.Paused != "" {
			continue
		}
		running[r.ID] = true
		var d cliRun
		if err := w.API.Call(ctx, "CLIRunDetail", &d, r.ID); err != nil {
			w.Log.Line("progress", "api_error", "call", "CLIRunDetail", "run", r.ID, "error", w.Log.Scrub(err.Error()))
			d = r
		}
		g := first[r.Thread]
		if r.Thread == "" {
			g = r
		}
		it := item{Kind: "cli", ID: r.ID, Key: "cli:" + firstNonEmpty(r.Thread, r.ID), Goal: firstNonEmpty(g.Prompt, r.Prompt),
			Started: g.Started, Sig: fmt.Sprintf("%d/%d/%d", d.Tools, d.Out, len(d.Events)), run: &r}
		it.run.Session = d.Session
		for _, e := range d.Events {
			if e.At.After(it.APILast) {
				it.APILast = e.At
			}
		}
		from := max(0, len(d.Events)-8)
		for _, e := range d.Events[from:] {
			it.Steps = append(it.Steps, strings.TrimSpace(fmt.Sprintf("%s %s %s", e.Kind, e.Tool, trunc(firstNonEmpty(e.Detail, e.Text), 200))))
		}
		items = append(items, it)
	}
	var hs struct {
		Tasks []hiveTask `json:"tasks"`
	}
	if err := w.API.Call(ctx, "HiveStatus", &hs); err == nil {
		for _, t := range hs.Tasks {
			if t.State != "running" || (t.Run != "" && running[t.Run]) {
				continue
			}
			it := item{Kind: "hive", ID: t.ID, Key: "hive:" + t.ID, Goal: t.Prompt, Started: t.StartedAt,
				Sig: fmt.Sprintf("%d/%s/%s/%d", t.Tools, t.Phase, t.Tool, len(t.Events))}
			for _, e := range t.Events {
				if e.At.After(it.APILast) {
					it.APILast = e.At
				}
			}
			from := max(0, len(t.Events)-8)
			for _, e := range t.Events[from:] {
				it.Steps = append(it.Steps, strings.TrimSpace(e.Phase+" "+e.Tool))
			}
			if t.Live != "" {
				it.Steps = append(it.Steps, "live "+trunc(t.Live, 200))
			}
			items = append(items, it)
		}
	} else {
		w.Log.Line("progress", "api_error", "call", "HiveStatus", "error", w.Log.Scrub(err.Error()))
	}
	var longs []longSummary
	if err := w.API.Call(ctx, "LongRunList", &longs); err == nil {
		for _, l := range longs {
			if !l.Running {
				continue
			}
			var s longState
			if err := w.API.Call(ctx, "LongRunState", &s, l.TaskID); err != nil || s.TaskID == "" {
				continue
			}
			it := item{Kind: "long", ID: l.TaskID, Key: "long:" + l.TaskID, Goal: s.Goal, long: &s,
				Sig: fmt.Sprintf("%d/%d/%d/%d", len(s.Rounds), s.ToolCalls, s.TotalTokens, len(s.Log))}
			it.Started, _ = time.Parse(time.RFC3339Nano, s.StartedAt)
			for _, l := range s.Log {
				if t, err := time.Parse(time.RFC3339Nano, l.At); err == nil && t.After(it.APILast) {
					it.APILast = t
				}
			}
			from := max(0, len(s.Log)-8)
			for _, l := range s.Log[from:] {
				it.Steps = append(it.Steps, strings.TrimSpace(l.Kind+" "+trunc(l.Text, 200)))
			}
			items = append(items, it)
		}
	} else {
		w.Log.Line("progress", "api_error", "call", "LongRunList", "error", w.Log.Scrub(err.Error()))
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Key < items[j].Key })
	return items, true
}

const resumePrompt = "你卡住了一段时间。从上次停下的地方继续，如果卡在某一步就换个办法。"

func (w *Watchdog) stalled(ctx context.Context, it item, idle time.Duration, now time.Time) {
	name := label(it)
	if _, done := w.State.Intervened[it.Key]; done || it.Kind == "hive" {
		note := "no_resume_api"
		if done {
			note = "already_restarted"
		}
		w.Log.Line("progress", "escalate", "run", it.Key, "why", note)
		w.raise("stalled", it.Key, fmt.Sprintf("%s 已 %s 分钟没有进展", name, mins(idle)))
		return
	}
	w.State.Intervened[it.Key] = now
	var err error
	switch it.Kind {
	case "cli":
		err = w.restartCLI(ctx, it)
	case "long":
		err = w.restartLong(ctx, it)
	}
	if err != nil {
		w.Log.Line("progress", "restart_failed", "run", it.Key, "error", w.Log.Scrub(err.Error()))
		w.raise("stalled", it.Key, fmt.Sprintf("%s 已 %s 分钟没有进展，续跑失败", name, mins(idle)))
		return
	}
	// The resumed run gets a fresh grace period of its own.
	w.State.Progress[it.Key] = progress{Sig: "restarted", At: now}
	w.Log.Line("progress", "restarted", "run", it.Key)
	w.raise("stall_restarted", it.Key, fmt.Sprintf("%s 卡了 %s 分钟，已停止并续跑", name, mins(idle)))
}

func (w *Watchdog) restartCLI(ctx context.Context, it item) error {
	var msg string
	if err := w.API.Call(ctx, "CancelCLIRun", &msg, it.ID); err != nil {
		return err
	}
	w.Log.Line("progress", "stopped", "run", it.Key, "answer", msg)
	if it.run.Session == "" {
		return fmt.Errorf("the run has no session to continue")
	}
	for i := 0; i < 20; i++ { // wait for the process to end
		var d cliRun
		if err := w.API.Call(ctx, "CLIRunDetail", &d, it.ID); err != nil || d.State != "running" {
			break
		}
		w.Sleep(time.Second)
	}
	var next cliRun
	return w.API.Call(ctx, "FollowUpCLIRun", &next, it.ID, resumePrompt)
}

func (w *Watchdog) restartLong(ctx context.Context, it item) error {
	var stopped bool
	if err := w.API.Call(ctx, "LongRunStop", &stopped, it.ID); err != nil {
		return err
	}
	w.Log.Line("progress", "stopped", "run", it.Key)
	for i := 0; i < 20; i++ {
		var s longState
		if err := w.API.Call(ctx, "LongRunState", &s, it.ID); err != nil || !s.Running {
			break
		}
		w.Sleep(time.Second)
	}
	var id string
	return w.API.Call(ctx, "LongRunStart", &id, it.Goal, it.long.MaxSegments, 0, 0, 0, it.ID, false)
}

func (w *Watchdog) drift(ctx context.Context, it item, now time.Time) {
	if w.Judge == nil || it.Goal == "" || len(it.Steps) == 0 {
		return
	}
	if !it.Started.IsZero() && now.Sub(it.Started) < w.Cfg.DriftAfter() {
		return
	}
	if t, ok := w.State.Judged[it.Key]; ok && now.Sub(t) < w.Cfg.DriftEvery() {
		return
	}
	w.State.Judged[it.Key] = now
	v, reason, err := w.Judge.Verdict(ctx, it.Goal, it.Steps)
	if err != nil {
		w.Log.Line("drift", "judge_error", "run", it.Key, "error", w.Log.Scrub(err.Error()))
		return
	}
	w.Log.Line("drift", v, "run", it.Key, "reason", reason)
	switch v {
	case OffTrack:
		if reason == "" {
			reason = "偏离目标"
		}
		// There is no way to put a note into a running coding run (a
		// follow-up is refused while it runs), so this only tells the person.
		w.raise("off_track", it.Key, fmt.Sprintf("%s 偏离目标：%s", label(it), reason))
	case OnTrack:
		w.clear("off_track|" + it.Key)
	}
}

// ---- 4. schedules ----

func (w *Watchdog) checkSchedules(ctx context.Context) {
	now := w.Now()
	var list []schedule
	if err := w.API.Call(ctx, "ScheduledPrompts", &list); err != nil {
		w.Log.Line("schedule", "api_error", "error", w.Log.Scrub(err.Error()))
		return
	}
	missed := map[string]bool{}
	for _, s := range list {
		var last time.Time
		if s.LastRun != nil {
			last = *s.LastRun
		}
		prev, seen := w.State.SchedLast[s.ID]
		w.State.SchedLast[s.ID] = last
		if !s.Enabled || s.NextRun == nil || s.Running {
			continue
		}
		late := now.Sub(*s.NextRun)
		advanced := seen && !last.Equal(prev)
		if late > w.Cfg.ScheduleGrace() && !last.After(*s.NextRun) && !advanced {
			missed[s.ID] = true
			w.Log.Line("schedule", "missed", "id", s.ID, "late", late.Round(time.Second).String())
			w.raise("schedule_missed", s.ID, fmt.Sprintf("定时任务没有按时运行：%s（晚了 %s 分钟）", trunc(firstNonEmpty(s.Note, s.Prompt), 30), mins(late)))
		}
	}
	if len(missed) == 0 {
		w.Log.Line("schedule", "ok", "count", fmt.Sprint(len(list)))
	}
	w.clearExcept("schedule_missed", missed)
}

// ---- alerts ----

func alertKey(kind, target string) string { return kind + "|" + target }

// raise escalates once per (kind, target) until it is cleared.
func (w *Watchdog) raise(kind, target, text string) {
	k := alertKey(kind, target)
	if _, on := w.State.Alerts[k]; on {
		return
	}
	w.State.Alerts[k] = w.Now()
	w.Log.Line("alert", "raised", "kind", kind, "target", target, "text", text)
	w.escalate(text)
}

func (w *Watchdog) clear(keysOrKinds ...string) {
	for _, kk := range keysOrKinds {
		for k := range w.State.Alerts {
			if k == kk || strings.HasPrefix(k, kk+"|") {
				delete(w.State.Alerts, k)
				w.Log.Line("alert", "cleared", "key", k)
			}
		}
	}
}

// clearExcept drops the alerts of a kind whose target is not in active.
func (w *Watchdog) clearExcept(kind string, active map[string]bool) {
	for k := range w.State.Alerts {
		kd, target, _ := strings.Cut(k, "|")
		if kd == kind && !active[target] {
			delete(w.State.Alerts, k)
			w.Log.Line("alert", "cleared", "key", k)
		}
	}
}

// escalate puts the text where the person sees it: SuperLeo's own
// notification (centre + phone push) when it answers, and the Mac's.
func (w *Watchdog) escalate(text string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*w.Cfg.APITimeout())
	defer cancel()
	if w.apiOK {
		var out map[string]any
		err := w.API.Call(ctx, "PostNotification", &out, map[string]any{
			"title": "看门狗", "body": text, "source": "watchdog", "kind": "alert"})
		if err != nil {
			w.Log.Line("notify", "app_failed", "error", w.Log.Scrub(err.Error()))
		} else {
			w.Log.Line("notify", "app_ok")
		}
	}
	if w.Cfg.macNotify() {
		if err := w.Host.Notify("SuperLeo 看门狗", text); err != nil {
			w.Log.Line("notify", "mac_failed", "error", err.Error())
		} else {
			w.Log.Line("notify", "mac_ok")
		}
	}
}

func label(it item) string {
	switch it.Kind {
	case "cli":
		return "编码任务「" + trunc(it.Goal, 24) + "」"
	case "long":
		return "长任务「" + trunc(it.Goal, 24) + "」"
	}
	return "任务「" + trunc(it.Goal, 24) + "」"
}

func mins(d time.Duration) string { return fmt.Sprint(int(d.Minutes())) }
