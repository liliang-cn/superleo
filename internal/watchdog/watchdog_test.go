package watchdog

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

type fakeAPI struct {
	mu       sync.Mutex
	handlers map[string]func(args []json.RawMessage) (any, int)
	calls    []string
	srv      *httptest.Server
	hang     chan struct{}
}

func newFakeAPI(t *testing.T) *fakeAPI {
	f := &fakeAPI{handlers: map[string]func([]json.RawMessage) (any, int){}, hang: make(chan struct{})}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok-secret-1" {
			http.Error(w, "no", 401)
			return
		}
		m := strings.TrimPrefix(r.URL.Path, "/api/rpc/")
		var args []json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&args)
		f.mu.Lock()
		f.calls = append(f.calls, m)
		h := f.handlers[m]
		f.mu.Unlock()
		if h == nil {
			http.Error(w, "no such method", 404)
			return
		}
		v, code := h(args)
		if code == -1 { // hang
			select {
			case <-f.hang:
			case <-r.Context().Done():
			}
			return
		}
		if code != 0 {
			http.Error(w, "boom", code)
			return
		}
		_ = json.NewEncoder(w).Encode(v)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) on(m string, v any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers[m] = func([]json.RawMessage) (any, int) { return v, 0 }
}
func (f *fakeAPI) onFn(m string, fn func([]json.RawMessage) (any, int)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers[m] = fn
}
func (f *fakeAPI) count(m string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == m {
			n++
		}
	}
	return n
}

type fakeHost struct {
	running  bool
	launches int
	notes    []string
}

func (h *fakeHost) Running() bool { return h.running }
func (h *fakeHost) Launch() error { h.launches++; return nil }
func (h *fakeHost) Notify(title, text string) error {
	h.notes = append(h.notes, text)
	return nil
}

type fakeJudge struct {
	verdict, reason string
	asked           int
}

func (j *fakeJudge) Verdict(context.Context, string, []string) (string, string, error) {
	j.asked++
	return j.verdict, j.reason, nil
}

type rig struct {
	w    *Watchdog
	api  *fakeAPI
	host *fakeHost
	j    *fakeJudge
	now  time.Time
	home string
}

func newRig(t *testing.T) *rig {
	api := newFakeAPI(t)
	home := t.TempDir()
	r := &rig{api: api, host: &fakeHost{running: true}, j: &fakeJudge{verdict: OnTrack}, now: t0, home: home}
	cfg := Config{Home: home, APIURL: api.srv.URL, APITimeoutSeconds: 1}
	cfg.defaults()
	f := false
	_ = f
	r.w = &Watchdog{Cfg: cfg, API: &Client{Base: api.srv.URL, Token: "tok-secret-1", Timeout: cfg.APITimeout()},
		Host: r.host, Judge: r.j, Log: NewLog(home, 1), Now: func() time.Time { return r.now }, Sleep: func(time.Duration) {}}
	r.w.Log.now = r.w.Now
	r.w.Log.Hide("tok-secret-1")
	r.w.Setup()
	api.on("GetStatus", map[string]any{"ready": true})
	api.on("CLIRuns", []cliRun{})
	api.on("HiveStatus", map[string]any{"tasks": []hiveTask{}})
	api.on("LongRunList", []longSummary{})
	api.on("ScheduledPrompts", []schedule{})
	api.on("PostNotification", map[string]any{"ok": true})
	return r
}

func (r *rig) tick(adv time.Duration) {
	r.now = r.now.Add(adv)
	r.w.Tick(context.Background())
}
func (r *rig) logText(t *testing.T) string {
	b, err := os.ReadFile(r.w.Log.Path())
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestAliveDownHangAndBackoff(t *testing.T) {
	r := newRig(t)
	r.api.onFn("GetStatus", func([]json.RawMessage) (any, int) { return nil, 500 })

	// process gone -> relaunch once, then wait for it to start
	r.host.running = false
	r.tick(0)
	if r.host.launches != 1 {
		t.Fatalf("launches = %d", r.host.launches)
	}
	r.tick(30 * time.Second)
	if r.host.launches != 1 {
		t.Fatal("relaunched inside the gap")
	}
	// keeps dying: 3 in the window, then escalate only
	r.tick(3 * time.Minute)
	r.tick(3 * time.Minute)
	if r.host.launches != 3 {
		t.Fatalf("launches = %d, want 3", r.host.launches)
	}
	r.tick(3 * time.Minute)
	if r.host.launches != 3 {
		t.Fatal("relaunch storm")
	}
	if _, ok := r.w.State.Alerts["app_down|app"]; !ok {
		t.Fatalf("no app_down alert: %v", r.w.State.Alerts)
	}
	// outside the 30 minute window it may try again
	r.tick(31 * time.Minute)
	if r.host.launches != 4 {
		t.Fatalf("launches = %d after window", r.host.launches)
	}
	// back up: alerts clear
	r.api.on("GetStatus", map[string]any{})
	r.tick(time.Minute)
	if len(r.w.State.Alerts) != 0 {
		t.Fatalf("alerts not cleared: %v", r.w.State.Alerts)
	}
}

func TestPauseFile(t *testing.T) {
	r := newRig(t)
	r.api.onFn("GetStatus", func([]json.RawMessage) (any, int) { return nil, 500 })
	r.host.running = false
	if err := os.WriteFile(filepath.Join(r.home, "watchdog.pause"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	r.tick(0)
	if r.host.launches != 0 || !strings.Contains(r.logText(t), "result=down_paused") {
		t.Fatalf("paused but launches=%d", r.host.launches)
	}
}

func TestHungAPIEscalatesNeverKills(t *testing.T) {
	r := newRig(t)
	r.api.onFn("GetStatus", func([]json.RawMessage) (any, int) { return nil, -1 })
	r.host.running = true
	r.tick(0)
	if len(r.host.notes) != 0 {
		t.Fatal("alerted too early")
	}
	r.tick(11 * time.Minute)
	if len(r.host.notes) != 1 || !strings.Contains(r.host.notes[0], "无响应") {
		t.Fatalf("notes = %v", r.host.notes)
	}
	r.tick(time.Minute)
	if len(r.host.notes) != 1 || r.host.launches != 0 {
		t.Fatal("deduped alert repeated or app touched")
	}
}

func cliRunning(id string, started time.Time, events ...cliEvent) cliRun {
	return cliRun{ID: id, Thread: "th-" + id, Agent: "claude", Prompt: "fix the build", Session: "s1", State: "running", Started: started, Events: events}
}

func TestStalledRunStopsResumesOnceThenEscalates(t *testing.T) {
	r := newRig(t)
	ev := cliEvent{At: t0, Kind: "tool", Tool: "bash"}
	run := cliRunning("r1", t0, ev)
	var mu sync.Mutex
	stopped := false
	cur := func() cliRun {
		mu.Lock()
		defer mu.Unlock()
		c := run
		if stopped {
			c.State = "cancelled"
		}
		return c
	}
	r.api.onFn("CLIRuns", func([]json.RawMessage) (any, int) { return []cliRun{cur()}, 0 })
	r.api.onFn("CLIRunDetail", func([]json.RawMessage) (any, int) { return cur(), 0 })
	r.api.onFn("CancelCLIRun", func([]json.RawMessage) (any, int) { mu.Lock(); stopped = true; mu.Unlock(); return "ok", 0 })
	r.api.on("FollowUpCLIRun", cliRun{ID: "r2"})

	r.tick(5 * time.Minute) // healthy
	if r.api.count("CancelCLIRun") != 0 {
		t.Fatal("stopped a healthy run")
	}
	r.tick(20 * time.Minute) // 25 min with no event
	if r.api.count("CancelCLIRun") != 1 || r.api.count("FollowUpCLIRun") != 1 {
		t.Fatalf("stop=%d follow=%d", r.api.count("CancelCLIRun"), r.api.count("FollowUpCLIRun"))
	}
	if len(r.host.notes) != 1 || !strings.Contains(r.host.notes[0], "已停止并续跑") {
		t.Fatalf("notes=%v", r.host.notes)
	}
	// the resumed run (same thread) is stalled again -> escalate, no second restart
	mu.Lock()
	stopped = false
	run = cliRunning("r2", t0.Add(25*time.Minute))
	run.Thread = "th-r1"
	mu.Unlock()
	r.api.onFn("CLIRuns", func([]json.RawMessage) (any, int) { return []cliRun{cur()}, 0 })
	r.tick(time.Minute)
	if r.api.count("CancelCLIRun") != 1 {
		t.Fatal("restarted twice")
	}
	r.tick(25 * time.Minute)
	if r.api.count("CancelCLIRun") != 1 || r.api.count("FollowUpCLIRun") != 1 {
		t.Fatal("restarted twice")
	}
	last := r.host.notes[len(r.host.notes)-1]
	if !strings.Contains(last, "没有进展") {
		t.Fatalf("notes=%v", r.host.notes)
	}
	n := len(r.host.notes)
	r.tick(time.Minute) // same alert, deduped
	if len(r.host.notes) != n {
		t.Fatal("alert repeated")
	}
	if r.api.count("PostNotification") != n {
		t.Fatalf("app notifications %d vs mac %d", r.api.count("PostNotification"), n)
	}
}

func TestStalledNoSessionEscalates(t *testing.T) {
	r := newRig(t)
	run := cliRunning("r1", t0, cliEvent{At: t0, Kind: "tool"})
	run.Session = ""
	r.api.on("CLIRuns", []cliRun{run})
	r.api.on("CLIRunDetail", run)
	r.api.on("CancelCLIRun", "ok")
	r.tick(30 * time.Minute)
	if r.api.count("FollowUpCLIRun") != 0 || len(r.host.notes) != 1 || !strings.Contains(r.host.notes[0], "续跑失败") {
		t.Fatalf("follow=%d notes=%v", r.api.count("FollowUpCLIRun"), r.host.notes)
	}
}

func TestProgressSignatureChangeCountsAsProgress(t *testing.T) {
	r := newRig(t)
	tools := 0
	r.api.onFn("CLIRuns", func([]json.RawMessage) (any, int) {
		return []cliRun{cliRunning("r1", t0)}, 0
	})
	r.api.onFn("CLIRunDetail", func([]json.RawMessage) (any, int) {
		run := cliRunning("r1", t0)
		run.Tools = tools
		return run, 0
	})
	for i := 0; i < 5; i++ { // a tool call every 10 minutes: never 20 idle
		tools++
		r.tick(10 * time.Minute)
	}
	if len(r.host.notes) != 0 || r.api.count("CancelCLIRun") != 0 {
		t.Fatalf("false stall: %v", r.host.notes)
	}
}

func TestHiveTaskStallEscalatesOnly(t *testing.T) {
	r := newRig(t)
	r.api.on("HiveStatus", map[string]any{"tasks": []hiveTask{{ID: "h1", Worker: "pi", Prompt: "p", State: "running", StartedAt: t0,
		Events: []hiveTaskEvent{{At: t0, Phase: "tool"}}}, {ID: "h2", State: "done", StartedAt: t0}}})
	r.tick(25 * time.Minute)
	if len(r.host.notes) != 1 || r.api.count("StopHiveTask") != 0 {
		t.Fatalf("notes=%v", r.host.notes)
	}
}

func TestLongRunStallStopsAndResumes(t *testing.T) {
	r := newRig(t)
	st := longState{TaskID: "L1", Goal: "migrate", Running: true, StartedAt: t0.Format(time.RFC3339Nano), MaxSegments: 6}
	st.Log = append(st.Log, struct {
		At   string `json:"at"`
		Kind string `json:"kind"`
		Text string `json:"text"`
	}{t0.Format(time.RFC3339Nano), "round", "x"})
	r.api.on("LongRunList", []longSummary{{TaskID: "L1", Running: true}})
	r.api.onFn("LongRunState", func([]json.RawMessage) (any, int) { return st, 0 })
	r.api.onFn("LongRunStop", func([]json.RawMessage) (any, int) { st.Running = false; return true, 0 })
	r.api.on("LongRunStart", "L1")
	r.tick(25 * time.Minute)
	if r.api.count("LongRunStop") != 1 || r.api.count("LongRunStart") != 1 {
		t.Fatalf("stop=%d start=%d", r.api.count("LongRunStop"), r.api.count("LongRunStart"))
	}
}

func TestOffTrackVerdict(t *testing.T) {
	r := newRig(t)
	run := cliRunning("r1", t0, cliEvent{At: t0, Kind: "tool", Tool: "bash"})
	r.api.onFn("CLIRuns", func([]json.RawMessage) (any, int) { return []cliRun{run}, 0 })
	r.api.onFn("CLIRunDetail", func([]json.RawMessage) (any, int) {
		run.Events = append(run.Events, cliEvent{At: r.now, Kind: "tool", Tool: "bash"})
		return run, 0
	})
	r.j.verdict, r.j.reason = OffTrack, "在改无关的文档"

	r.tick(5 * time.Minute) // younger than 15 min: not asked
	if r.j.asked != 0 {
		t.Fatal("asked too early")
	}
	r.tick(11 * time.Minute) // 16 min
	if r.j.asked != 1 || len(r.host.notes) != 1 || !strings.Contains(r.host.notes[0], "在改无关的文档") {
		t.Fatalf("asked=%d notes=%v", r.j.asked, r.host.notes)
	}
	r.tick(5 * time.Minute) // within 10 min of the last question
	if r.j.asked != 1 {
		t.Fatal("asked again inside 10 minutes")
	}
	r.tick(6 * time.Minute)
	if r.j.asked != 2 || len(r.host.notes) != 1 {
		t.Fatalf("asked=%d notes=%d (dedupe)", r.j.asked, len(r.host.notes))
	}
	// back on track clears; off again alerts again
	r.j.verdict = OnTrack
	r.tick(11 * time.Minute)
	if len(r.w.State.Alerts) != 0 {
		t.Fatalf("alerts %v", r.w.State.Alerts)
	}
	r.j.verdict = OffTrack
	r.tick(11 * time.Minute)
	if len(r.host.notes) != 2 {
		t.Fatalf("notes=%v", r.host.notes)
	}
}

func TestUnclearVerdictSilent(t *testing.T) {
	r := newRig(t)
	run := cliRunning("r1", t0.Add(-time.Hour), cliEvent{At: t0, Kind: "tool"})
	r.api.on("CLIRuns", []cliRun{run})
	r.api.on("CLIRunDetail", run)
	r.j.verdict = Unclear
	r.tick(time.Minute)
	if r.j.asked != 1 || len(r.host.notes) != 0 {
		t.Fatalf("asked=%d notes=%v", r.j.asked, r.host.notes)
	}
}

func TestMissedSchedule(t *testing.T) {
	r := newRig(t)
	next := t0.Add(-10 * time.Minute)
	last := t0.Add(-24 * time.Hour)
	okNext := t0.Add(-2 * time.Minute)
	r.api.on("ScheduledPrompts", []schedule{
		{ID: "late", Prompt: "daily brief", Enabled: true, NextRun: &next, LastRun: &last},
		{ID: "recent", Prompt: "x", Enabled: true, NextRun: &okNext, LastRun: &last},
		{ID: "off", Prompt: "x", Enabled: false, NextRun: &next, LastRun: &last},
		{ID: "run", Prompt: "x", Enabled: true, NextRun: &next, LastRun: &last, Running: true},
	})
	r.tick(0)
	if len(r.host.notes) != 1 || !strings.Contains(r.host.notes[0], "daily brief") {
		t.Fatalf("notes=%v", r.host.notes)
	}
	r.tick(time.Minute)
	if len(r.host.notes) != 1 {
		t.Fatal("not deduped")
	}
	// it fires: next moves on, last advances -> cleared
	n2, l2 := t0.Add(24*time.Hour), t0.Add(time.Minute)
	r.api.on("ScheduledPrompts", []schedule{{ID: "late", Prompt: "daily brief", Enabled: true, NextRun: &n2, LastRun: &l2}})
	r.tick(time.Minute)
	if len(r.w.State.Alerts) != 0 {
		t.Fatalf("alerts %v", r.w.State.Alerts)
	}
}

func TestStatePersistsAcrossWatchdogRestart(t *testing.T) {
	r := newRig(t)
	r.api.on("ScheduledPrompts", func() []schedule {
		n, l := t0.Add(-time.Hour), t0.Add(-48*time.Hour)
		return []schedule{{ID: "s", Prompt: "p", Enabled: true, NextRun: &n, LastRun: &l}}
	}())
	r.tick(0)
	w2 := *r.w
	w2.State = nil
	w2.Setup()
	if _, ok := w2.State.Alerts["schedule_missed|s"]; !ok {
		t.Fatal("alert not persisted")
	}
}

func TestLogNeverHoldsTheToken(t *testing.T) {
	r := newRig(t)
	r.api.onFn("GetStatus", func([]json.RawMessage) (any, int) { return nil, 500 })
	r.w.Log.Line("x", "y", "error", "Bearer tok-secret-1 leaked")
	if strings.Contains(r.logText(t), "tok-secret-1") {
		t.Fatal("token in log")
	}
}

func TestLogRotates(t *testing.T) {
	home := t.TempDir()
	l := NewLog(home, 1)
	l.max = 200
	for i := 0; i < 10; i++ {
		l.Line("a", "b", "k", strings.Repeat("x", 50))
	}
	if _, err := os.Stat(l.Path() + ".1"); err != nil {
		t.Fatal("not rotated")
	}
}

func TestParseVerdict(t *testing.T) {
	for _, c := range []struct{ in, v, r string }{
		{`{"verdict":"off_track","reason":"x"}`, OffTrack, "x"},
		{"```json\n{\"verdict\": \"On-Track\", \"reason\": \"ok\"}\n```", OnTrack, "ok"},
		{"it is off_track", OffTrack, ""},
		{"???", Unclear, ""},
	} {
		if v, r := parseVerdict(c.in); v != c.v || r != c.r {
			t.Errorf("%q -> %s %s", c.in, v, r)
		}
	}
}

func TestChatJudgeUsesSettingsAndHidesKey(t *testing.T) {
	var gotAuth, gotModel string
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		var b struct{ Model string }
		_ = json.NewDecoder(r.Body).Decode(&b)
		gotModel = b.Model
		if r.URL.Path != "/v1/chat/completions" {
			http.Error(w, "x", 404)
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"verdict\":\"off_track\",\"reason\":\"绕圈\"}"}}]}`))
	}))
	defer llm.Close()
	j := &ChatJudge{Settings: func(context.Context) (settings, error) {
		return settings{LLMBaseURL: llm.URL + "/v1", LLMModel: "m1", LLMKey: "sk-llm-key"}, nil
	}}
	v, reason, err := j.Verdict(context.Background(), "goal", []string{"a"})
	if err != nil || v != OffTrack || reason != "绕圈" || gotAuth != "Bearer sk-llm-key" || gotModel != "m1" {
		t.Fatalf("%v %s %s %s %s", err, v, reason, gotAuth, gotModel)
	}
	j.Cfg.JudgeBaseURL = llm.URL + "/nope"
	if _, _, err := j.Verdict(context.Background(), "g", nil); err == nil || strings.Contains(err.Error(), "sk-llm-key") {
		t.Fatalf("err = %v", err)
	}
}

func TestLaunchEnvDropsAnthropicKey(t *testing.T) {
	got := launchEnv([]string{"A=1", "ANTHROPIC_API_KEY=sk-x", "ANTHROPIC_API_KEY_2=ok", "B=2"})
	if strings.Join(got, ",") != "A=1,ANTHROPIC_API_KEY_2=ok,B=2" {
		t.Fatalf("%v", got)
	}
}

func TestAsStringEscapes(t *testing.T) {
	if got := asString("a\"b\\c\nd"); got != `"a\"b\\c d"` {
		t.Fatal(got)
	}
}

func TestInstallUninstall(t *testing.T) {
	dir := t.TempDir()
	var calls []string
	in := &Installer{Binary: "/opt/a&b/superleo-watchdog", Home: "/Users/x", Dir: filepath.Join(dir, "LaunchAgents"),
		LogDir: filepath.Join(dir, "logs"), UID: 501,
		Run: func(n string, a ...string) error { calls = append(calls, n+" "+strings.Join(a, " ")); return nil }}
	if err := in.Install(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(in.Dir, Label+".plist"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, "/opt/a&amp;b/superleo-watchdog") || strings.Contains(s, "{{") || !strings.Contains(s, "<string>run</string>") {
		t.Fatalf("plist:\n%s", s)
	}
	want := []string{"launchctl bootout gui/501/" + Label, "launchctl bootstrap gui/501 " + filepath.Join(in.Dir, Label+".plist")}
	if strings.Join(calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls %v", calls)
	}
	if err := in.Uninstall(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(in.Dir, Label+".plist")); !os.IsNotExist(err) {
		t.Fatal("plist left")
	}
}

func TestConfigDefaultsAndFile(t *testing.T) {
	home := t.TempDir()
	c, err := LoadConfig(home)
	if err != nil || c.TickSeconds != 60 || c.StallAfterMinutes != 20 || c.APIURL != "http://127.0.0.1:43119" || c.RelaunchMax != 3 {
		t.Fatalf("%+v %v", c, err)
	}
	_ = os.WriteFile(filepath.Join(home, "watchdog.json"), []byte(`{"stall_after_minutes":5,"api_url":"http://x:1/"}`), 0o600)
	c, _ = LoadConfig(home)
	if c.StallAfterMinutes != 5 || c.APIURL != "http://x:1" || c.TickSeconds != 60 {
		t.Fatalf("%+v", c)
	}
}

func TestReadToken(t *testing.T) {
	home := t.TempDir()
	_ = os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"user":"u","token":"abc","password_hash":"h"}`), 0o600)
	if tok, err := ReadToken(home); err != nil || tok != "abc" {
		t.Fatal(tok, err)
	}
}

func TestFollowUpRunJudgedByThreadsFirstPrompt(t *testing.T) {
	r := newRig(t)
	old := cliRunning("r1", t0.Add(-time.Hour))
	old.State = "cancelled"
	cur := cliRunning("r2", t0, cliEvent{At: t0, Kind: "tool"})
	cur.Thread, cur.Prompt = old.Thread, "continue"
	r.api.on("CLIRuns", []cliRun{cur, old})
	r.api.on("CLIRunDetail", cur)
	var goal string
	r.w.Judge = judgeFn(func(g string) { goal = g })
	r.tick(time.Minute)
	if goal != "fix the build" {
		t.Fatalf("goal = %q", goal)
	}
}

type judgeFn func(goal string)

func (f judgeFn) Verdict(_ context.Context, goal string, _ []string) (string, string, error) {
	f(goal)
	return OnTrack, "", nil
}
