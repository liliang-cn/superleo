//go:build unix

package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/liliang-cn/agentexec"
	"github.com/liliang-cn/superleo/internal/backend"
	"golang.org/x/sys/unix"
)

// gatedClaude is a claude that names its session, says it is working, waits
// for the gate file, and answers. Resumed (--resume), it answers at once.
// Every start appends its arguments to the args file.
func gatedClaude(t *testing.T) (bin, gate, args string) {
	t.Helper()
	dir := t.TempDir()
	gate, args = filepath.Join(dir, "gate"), filepath.Join(dir, "args")
	bin = fakeCLI(t, "claude", fmt.Sprintf(`
echo "$*" >> %q
for a in "$@"; do
  if [ "$a" = "--resume" ]; then
    echo '{"type":"system","subtype":"init","session_id":"sess-1"}'
    echo '{"type":"assistant","message":{"content":[{"type":"text","text":"picking it up"}]},"session_id":"sess-1"}'
    echo '{"type":"result","subtype":"success","result":"finished after the resume","session_id":"sess-1","is_error":false}'
    exit 0
  fi
done
echo '{"type":"system","subtype":"init","session_id":"sess-1"}'
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"working on it"}]},"session_id":"sess-1"}'
while [ ! -f %q ]; do sleep 0.05; done
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"almost there"}]},"session_id":"sess-1"}'
echo '{"type":"result","subtype":"success","result":"all done","session_id":"sess-1","is_error":false}'
`, args, gate))
	return bin, gate, args
}

// liveApp is a SuperLeo on a data directory, as a test restarts it: the same
// settings and home each time.
func liveApp(t *testing.T, home, ws, bin string) *App {
	t.Helper()
	t.Setenv("SUPERLEO_HOME", home)
	noDiscovery(t, agentexec.Installed{Name: "claude", Binary: bin})
	return &App{settings: &backend.Settings{WorkspaceDir: ws, ExternalAgents: backend.ExternalAgents{Enabled: true}}}
}

func readLive(t *testing.T, id string) (cliLive, bool) {
	t.Helper()
	raw, err := os.ReadFile(cliLivePath(id))
	if err != nil {
		return cliLive{}, false
	}
	var l cliLive
	if err := json.Unmarshal(raw, &l); err != nil {
		return cliLive{}, false
	}
	return l, true
}

func runState(a *App, id string) CLIRun {
	r, _ := a.CLIRunDetail(id)
	return r
}

func texts(r CLIRun, kind string) []string {
	var out []string
	for _, e := range r.Events {
		if e.Kind == kind {
			out = append(out, e.Text)
		}
	}
	return out
}

func stopGroup(pid int) {
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}

// A run going on this machine is written down as it goes: who does it,
// where, for whom, its CLI session and its process — which is in a session
// of its own, not SuperLeo's.
func TestARunningCodingRunIsJournalledWithItsProcessOutsideSuperLeo(t *testing.T) {
	bin, gate, _ := gatedClaude(t)
	a := liveApp(t, t.TempDir(), t.TempDir(), bin)
	done := make(chan CLIRun, 1)
	run, err := a.startCLIRun(cliStart{Agent: "claude", Prompt: "fix the bug", Origin: "hive task t-1",
		Done: func(r CLIRun) { done <- r }})
	if err != nil {
		t.Fatal(err)
	}
	var live cliLive
	waitFor(t, "the journal with the session", func() bool {
		l, ok := readLive(t, run.ID)
		live = l
		return ok && l.Run.Session == "sess-1"
	})
	t.Cleanup(func() { stopGroup(live.PID) })
	ws, _ := filepath.EvalSymlinks(a.settings.WorkspaceDir)
	if live.Run.Agent != "claude" || live.Run.Cwd != ws || live.Run.Origin != "hive task t-1" || live.Req.Prompt != "fix the bug" {
		t.Fatalf("journal %+v", live.Run)
	}
	if live.PID <= 0 || !procAlive(live.PID) {
		t.Fatalf("no live process in the journal: %d", live.PID)
	}
	if sid, err := unix.Getsid(live.PID); err != nil || sid != live.PID {
		t.Fatalf("the CLI is not in a session of its own: sid %d, pid %d, %v", sid, live.PID, err)
	}
	if sid, _ := unix.Getsid(0); sid == live.PID {
		t.Fatal("the CLI shares SuperLeo's session")
	}
	waitFor(t, "the event journalled", func() bool {
		evs, off := readCLILiveEvents(live.eventsPath(), 0)
		return len(evs) > 0 && off > 0
	})

	_ = os.WriteFile(gate, nil, 0o600)
	select {
	case r := <-done:
		if r.State != "done" || r.Summary != "all done" || r.Origin != "hive task t-1" {
			t.Fatalf("ended %s %q (%s)", r.State, r.Summary, r.Error)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the run did not end")
	}
	if cliLiveExists(run.ID) {
		t.Fatal("the journal of a finished run is still there")
	}
	files, _ := filepath.Glob(filepath.Join(cliLiveDir(), run.ID+".*"))
	if len(files) != 0 {
		t.Fatalf("left behind: %v", files)
	}
}

// SuperLeo stops while a run goes on; the CLI does not stop with it. The
// next SuperLeo finds it still working, reads on from where the last one
// left off — nothing twice, nothing missed — and finishes it.
func TestARunStillGoingIsReattachedAfterARestart(t *testing.T) {
	bin, gate, args := gatedClaude(t)
	home, ws := t.TempDir(), t.TempDir()
	a1 := liveApp(t, home, ws, bin)
	run, err := a1.startCLIRun(cliStart{Agent: "claude", Prompt: "fix the bug", Origin: "a page"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the first answer", func() bool { return len(texts(runState(a1, run.ID), "text")) == 1 })
	live, _ := readLive(t, run.ID)
	t.Cleanup(func() { stopGroup(live.PID) })

	a1.detachCLIRuns() // SuperLeo stopping
	if got := a1.CancelCLIRun(run.ID); got == "ok" {
		t.Fatal("a stopping SuperLeo stopped the run it was leaving")
	}
	time.Sleep(3 * cliPoll)
	if !procAlive(live.PID) {
		t.Fatal("the CLI died with SuperLeo")
	}

	a2 := liveApp(t, home, ws, bin)
	a2.recoverCLIRuns()
	r := runState(a2, run.ID)
	if r.State != "running" || r.Session != "sess-1" || len(texts(r, "text")) != 1 {
		t.Fatalf("picked up as %s, session %q, events %+v", r.State, r.Session, r.Events)
	}
	notes := texts(r, "note")
	if len(notes) != 1 || !strings.Contains(notes[0], "still working") {
		t.Fatalf("notes %q", notes)
	}

	_ = os.WriteFile(gate, nil, 0o600)
	waitFor(t, "the run to finish", func() bool { return runState(a2, run.ID).State != "running" })
	r = runState(a2, run.ID)
	if r.State != "done" || r.Summary != "all done" {
		t.Fatalf("ended %s %q (%s)", r.State, r.Summary, r.Error)
	}
	if got := texts(r, "text"); strings.Join(got, "|") != "working on it|almost there" {
		t.Fatalf("texts %q", got)
	}
	for i := 1; i < len(r.Events); i++ {
		if r.Events[i].Seq != r.Events[i-1].Seq+1 {
			t.Fatalf("events out of sequence: %+v", r.Events)
		}
	}
	if raw, _ := os.ReadFile(args); strings.Count(string(raw), "\n") != 1 {
		t.Fatalf("the CLI was started again: %s", raw)
	}
	// Saved as finished for the next start.
	a3 := liveApp(t, home, ws, bin)
	if got := runState(a3, run.ID); got.State != "done" {
		t.Fatalf("saved as %s", got.State)
	}
}

// The machine went down with the run: the next SuperLeo finds the CLI gone
// without an exit code and resumes its session, the run keeping its id.
func TestARunWhoseCLIDiedIsResumedOnItsSessionAfterARestart(t *testing.T) {
	bin, _, args := gatedClaude(t)
	home, ws := t.TempDir(), t.TempDir()
	a1 := liveApp(t, home, ws, bin)
	run, err := a1.startCLIRun(cliStart{Agent: "claude", Prompt: "fix the bug", Origin: "hive task t-2"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the session", func() bool {
		l, ok := readLive(t, run.ID)
		return ok && l.Run.Session == "sess-1" && len(texts(runState(a1, run.ID), "text")) == 1
	})
	live, _ := readLive(t, run.ID)
	a1.detachCLIRuns()
	stopGroup(live.PID) // the power going
	waitFor(t, "the CLI to be gone", func() bool { return !procAlive(live.PID) })

	a2 := liveApp(t, home, ws, bin)
	a2.recoverCLIRuns()
	waitFor(t, "the run to finish", func() bool { return runState(a2, run.ID).State != "running" })
	r := runState(a2, run.ID)
	if r.State != "done" || r.Summary != "finished after the resume" || r.Resumed != 1 || r.Origin != "hive task t-2" {
		t.Fatalf("ended %s %q resumed %d origin %q (%s)", r.State, r.Summary, r.Resumed, r.Origin, r.Error)
	}
	raw, _ := os.ReadFile(args)
	starts := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(starts) != 2 || !strings.Contains(starts[1], "--resume sess-1") || !strings.Contains(starts[1], "Carry on with the same task") {
		t.Fatalf("starts:\n%s", raw)
	}
	notes := strings.Join(texts(r, "note"), "|")
	if !strings.Contains(notes, "had stopped") || !strings.Contains(notes, "resuming its session sess-1") {
		t.Fatalf("notes %q", notes)
	}
	if got := texts(r, "text"); strings.Join(got, "|") != "working on it|picking it up" {
		t.Fatalf("texts %q", got)
	}
}

// A run that finished while SuperLeo was away is finished from what it
// left: its output and its exit code.
func TestARunThatFinishedWhileSuperLeoWasAwayIsFinishedFromItsOutput(t *testing.T) {
	bin, gate, args := gatedClaude(t)
	home, ws := t.TempDir(), t.TempDir()
	a1 := liveApp(t, home, ws, bin)
	run, err := a1.startCLIRun(cliStart{Agent: "claude", Prompt: "fix the bug"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the first answer", func() bool { return len(texts(runState(a1, run.ID), "text")) == 1 })
	live, _ := readLive(t, run.ID)
	a1.detachCLIRuns()
	time.Sleep(3 * cliPoll) // the old SuperLeo gone before the CLI finishes
	_ = os.WriteFile(gate, nil, 0o600)
	waitFor(t, "the exit code", func() bool { _, ok := readExitCode(live.exitPath()); return ok })

	a2 := liveApp(t, home, ws, bin)
	a2.recoverCLIRuns()
	waitFor(t, "the run to finish", func() bool { return runState(a2, run.ID).State != "running" })
	r := runState(a2, run.ID)
	if r.State != "done" || r.Summary != "all done" || r.Resumed != 0 {
		t.Fatalf("ended %s %q resumed %d (%s)", r.State, r.Summary, r.Resumed, r.Error)
	}
	if raw, _ := os.ReadFile(args); strings.Count(string(raw), "\n") != 1 {
		t.Fatalf("the CLI was started again: %s", raw)
	}
}

// Stop still stops: the whole process group, and the run reads cancelled.
func TestStoppingADetachedRunEndsItsProcess(t *testing.T) {
	bin, _, _ := gatedClaude(t)
	a := liveApp(t, t.TempDir(), t.TempDir(), bin)
	run, err := a.startCLIRun(cliStart{Agent: "claude", Prompt: "fix the bug"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the first answer", func() bool { return len(texts(runState(a, run.ID), "text")) == 1 })
	live, _ := readLive(t, run.ID)
	if got := a.CancelCLIRun(run.ID); got != "ok" {
		t.Fatal(got)
	}
	waitFor(t, "the run to end", func() bool { return runState(a, run.ID).State != "running" })
	if r := runState(a, run.ID); r.State != "cancelled" {
		t.Fatalf("ended %s (%s)", r.State, r.Error)
	}
	waitFor(t, "the process to go", func() bool { return !procAlive(live.PID) })
}

// fakeRemote is another SuperLeo's /api as the mirror reads it: an event
// stream the test writes to and can cut, and the run's detail.
type fakeRemote struct {
	mu      sync.Mutex
	streams chan chan string // each new event stream, for the test to write to
	detail  CLIRun
	down    bool
}

func (f *fakeRemote) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	down := f.down
	f.mu.Unlock()
	if down {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}
	switch {
	case r.URL.Path == "/api/events":
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		lines := make(chan string, 16)
		f.streams <- lines
		for {
			select {
			case l, ok := <-lines:
				if !ok {
					return // the line dropping
				}
				fmt.Fprintf(w, "data: %s\n\n", l)
				w.(http.Flusher).Flush()
			case <-r.Context().Done():
				return
			}
		}
	case r.URL.Path == "/api/rpc/StartCLIRun":
		_ = json.NewEncoder(w).Encode(CLIRun{ID: "there-1", Thread: "there-1", Agent: "claude", State: "running"})
	case r.URL.Path == "/api/rpc/CLIRunDetail":
		f.mu.Lock()
		d := f.detail
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(d)
	default:
		http.NotFound(w, r)
	}
}

func cliEventLine(seq int, text string) string {
	raw, _ := json.Marshal(map[string]any{"name": "cli:event", "payload": map[string]any{"run": "there-1",
		"event": CLIRunEvent{Seq: seq, Kind: "text", Text: text}}})
	return string(raw)
}

// The machine a run is on restarts: here, where it was handed out, the run
// pauses instead of failing, catches up on what it did meanwhile when the
// line is back, and ends with its answer, for whoever handed it out.
func TestARemoteRunPausesWhileItsMachineIsAwayAndFinishesWhenItIsBack(t *testing.T) {
	was, wasBack := cliRemoteRetry, cliRemoteBack
	cliRemoteRetry, cliRemoteBack = 20*time.Millisecond, 10*time.Second
	t.Cleanup(func() { cliRemoteRetry, cliRemoteBack = was, wasBack })

	f := &fakeRemote{streams: make(chan chan string, 4)}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	t.Setenv("SUPERLEO_HOME", t.TempDir())
	a := &App{settings: &backend.Settings{WorkspaceDir: t.TempDir(), RemoteAgents: backend.RemoteAgents{Enabled: true,
		Agents: map[string]backend.RemoteAgent{"mac": {URL: srv.URL, Token: "x"}}}}}
	a.remoteCLI.names = map[string][]string{"mac": {"claude"}}
	a.remoteCLI.until = time.Now().Add(time.Hour)

	done := make(chan CLIRun, 1)
	run, err := a.startCLIRun(cliStart{Agent: "claude.mac", Prompt: "fix the bug", Origin: "hive task t-3",
		Done: func(r CLIRun) { done <- r }})
	if err != nil {
		t.Fatal(err)
	}
	first := <-f.streams
	first <- cliEventLine(1, "working on it")
	waitFor(t, "the first event", func() bool { return len(texts(runState(a, run.ID), "text")) == 1 })

	// The machine goes away.
	f.mu.Lock()
	f.down = true
	f.detail = CLIRun{ID: "there-1", State: "running", Session: "sess-1", Events: []CLIRunEvent{
		{Seq: 1, Kind: "text", Text: "working on it"},
		{Seq: 2, Kind: "note", Text: "SuperLeo restarted; back on claude (pid 1), still working"},
		{Seq: 3, Kind: "text", Text: "while you were away"},
	}}
	f.mu.Unlock()
	close(first)
	waitFor(t, "the pause", func() bool { return runState(a, run.ID).Paused != "" })
	if r := runState(a, run.ID); r.State != "running" {
		t.Fatalf("the run reads %s while its machine is away", r.State)
	}

	// It is back.
	f.mu.Lock()
	f.down = false
	f.mu.Unlock()
	second := <-f.streams
	waitFor(t, "the catch-up", func() bool { return runState(a, run.ID).Paused == "" })
	second <- cliEventLine(3, "while you were away") // carried by both: once only
	second <- cliEventLine(4, "almost there")
	end, _ := json.Marshal(map[string]any{"name": "cli:run", "payload": CLIRun{ID: "there-1", State: "done", Summary: "all done", Session: "sess-1"}})
	second <- string(end)

	select {
	case r := <-done:
		if r.State != "done" || r.Summary != "all done" || r.Origin != "hive task t-3" || r.Paused != "" {
			t.Fatalf("ended %s %q origin %q paused %q (%s)", r.State, r.Summary, r.Origin, r.Paused, r.Error)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the run did not end")
	}
	r := runState(a, run.ID)
	if got := strings.Join(texts(r, "text"), "|"); got != "working on it|while you were away|almost there" {
		t.Fatalf("texts %q", got)
	}
	notes := strings.Join(texts(r, "note"), "|")
	if !strings.Contains(notes, "lost the connection to mac") || !strings.Contains(notes, "back in touch with mac") || !strings.Contains(notes, "still working") {
		t.Fatalf("notes %q", notes)
	}
}
