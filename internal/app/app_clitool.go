package app

// The model's way to the run manager: hand a task to Claude Code or Codex,
// here or on a linked SuperLeo ("claude.mac"), and get its answer back. The run
// is an ordinary CLI run — on the Agents page, streaming, stoppable — which is
// what cli_agent_run's blocking call is not; and it reaches other machines,
// which is how a hive queen with no CLI of her own puts the Mac's Claude Code
// to work.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/liliang-cn/agent-go/v3/pkg/agent"
	"github.com/liliang-cn/superleo/internal/backend"
)

// registerCodingAgentTools runs inside the build, which holds a.mu: nothing
// here may take it. Which CLIs exist is asked at call time.
func (a *App) registerCodingAgentTools(svc *backend.Service, cfg *backend.Settings) {
	if svc == nil || cfg == nil {
		return
	}
	linked := false
	if cfg.RemoteAgents.Enabled {
		for _, r := range cfg.RemoteAgents.Agents {
			if strings.TrimSpace(r.URL) != "" {
				linked = true
			}
		}
	}
	if !cfg.ExternalAgents.Enabled && !linked {
		return
	}
	inner := svc.Agent()
	if inner == nil {
		return
	}

	inner.AddToolWithMetadata("coding_agent_list",
		"The coding agents (Claude Code, Codex, …) this app can hand work to: on this machine by name, on a linked SuperLeo as name.machine.",
		map[string]any{"type": "object", "properties": map[string]any{}},
		func(ctx context.Context, _ map[string]any) (any, error) {
			b, err := json.Marshal(a.codingAgents())
			return string(b), err
		},
		agent.ToolMetadata{ReadOnly: true, ConcurrencySafe: true})

	inner.AddToolWithMetadata("coding_agent_run",
		"Hand a task to a coding agent — Claude Code, Codex — and wait for its answer."+
			" On this machine by name (\"claude\"), on a linked SuperLeo as name.machine"+
			" (\"claude.mac\"); coding_agent_list says which exist."+
			"\n\nRight for real work in a code base or a shell on that machine: changes,"+
			" builds, investigations. The prompt must stand on its own; the agent sees none of"+
			" this conversation. Its tool calls may need the user's approval, so it can take a"+
			" while."+
			"\n\nPass continue_run with a previous answer's run id to continue that agent's"+
			" session instead of starting fresh.",
		map[string]any{
			"type": "object",
			"properties": map[string]any{
				"agent":        map[string]any{"type": "string", "description": "e.g. claude, codex, claude.mac"},
				"prompt":       map[string]any{"type": "string", "description": "The whole task, standing on its own."},
				"cwd":          map[string]any{"type": "string", "description": "Directory on that machine; blank for its workspace."},
				"continue_run": map[string]any{"type": "string", "description": "Run id from an earlier answer, to continue that session."},
			},
			"required": []string{"agent", "prompt"},
		},
		func(ctx context.Context, args map[string]any) (any, error) {
			id := a.tasks().Start(str(args["agent"]), backend.TaskOut, str(args["prompt"]))
			a.tasks().InSession(id, backend.ChatSessionFrom(ctx))
			ctx = withBoardTask(ctx, id)
			out := a.runCodingAgent(ctx, str(args["agent"]), str(args["prompt"]), str(args["cwd"]), str(args["continue_run"]))
			a.finishDelegated(id, out["ok"] == true, fmt.Sprint(orEmpty(out["answer"])), fmt.Sprint(orEmpty(out["error"])))
			return out, nil
		},
		agent.ToolMetadata{Destructive: true})
}

func (a *App) codingAgents() []map[string]string {
	out := []map[string]string{}
	for _, c := range a.localAgentNames() {
		out = append(out, map[string]string{"name": c.Name, "where": "this machine"})
	}
	for _, m := range a.remoteCLINames() {
		out = append(out, map[string]string{"name": m["name"], "where": m["about"]})
	}
	return out
}

// runCodingAgent is told which board entry it works for through ctx
// (withBoardTask); the run's events then feed that entry as they come.
func (a *App) runCodingAgent(ctx context.Context, name, prompt, cwd, cont string) map[string]any {
	fail := func(msg string) map[string]any { return map[string]any{"ok": false, "error": msg} }
	if !a.isDrivenCLI(name) {
		names := []string{}
		for _, c := range a.codingAgents() {
			names = append(names, c["name"])
		}
		return fail(fmt.Sprintf("no coding agent called %q; available: %s", name, strings.Join(names, ", ")))
	}
	a.mu.Lock()
	unattended := a.settings != nil && a.settings.ExternalAgents.Unattended
	a.mu.Unlock()

	// The coding agent's run is a span of the flow that asked for it: under
	// the tool call that handed it out, with how it came out.
	_, span := backend.StartSpan(ctx, "coding."+name, prompt)
	done := make(chan CLIRun, 1)
	origin := "a turn"
	if chat := backend.ChatSessionFrom(ctx); chat != "" {
		origin = "a turn in conversation " + chat
	}
	o := cliStart{Agent: name, Prompt: prompt, Cwd: cwd, Ask: !unattended, Origin: origin,
		Watch: a.cliTaskFeed(boardTaskFrom(ctx)),
		Done:  func(r CLIRun) { done <- r }}
	if cont != "" {
		s := a.cliStore()
		s.mu.Lock()
		if p := s.runs[cont]; p != nil {
			c := *p
			c.Events = nil
			o.Prev, o.Thread, o.Session, o.Cwd, o.Model = &c, c.Thread, c.Session, c.Cwd, c.Model
		}
		s.mu.Unlock()
	}
	run, err := a.startCLIRun(o)
	if err != nil {
		span.End("failed", err)
		return fail(err.Error())
	}
	// Which run carries the order, so a restart can find it again.
	a.tasks().SetRun(boardTaskFrom(ctx), run.ID)
	var r CLIRun
	select {
	case r = <-done:
	case <-ctx.Done():
		a.CancelCLIRun(run.ID)
		r = <-done
	}
	switch {
	case r.State == "done":
		span.End("completed", nil)
	case r.State == "cancelled" || ctx.Err() != nil:
		span.End("cancelled", nil)
	default:
		span.End("failed", errors.New(firstNonEmpty(r.Error, r.State)))
	}
	out := map[string]any{
		"ok": r.State == "done", "agent": name, "run": r.ID, "state": r.State,
		"answer": r.Summary, "tool_calls": r.Tools, "cost_usd": r.CostUSD,
	}
	if r.Error != "" {
		out["error"] = r.Error
	}
	return out
}

// finishDelegated closes the board entry for work handed to an agent outside
// the hive — a coding agent on a linked machine, a named agent — so the hive's
// live views show it beside the workers' orders, from start to answer.
func (a *App) finishDelegated(id string, ok bool, answer, errMsg string) {
	state := backend.TaskDone
	if !ok {
		state = backend.TaskFailed
	}
	a.tasks().Finish(id, state, firstLineOf(answer, 400), errMsg)
}

// delegateOutside hands an order to an agent outside the hive: a coding agent
// on a linked machine ("codex.mac") or a named agent ("openclaw"). handled is
// false when the name is neither. It is what hive_command does with such a
// name, so a job's parts for workers and for agents go out in one call and run
// at the same time.
func (a *App) delegateOutside(ctx context.Context, name, prompt string) (text string, failed bool, handled bool) {
	text, failed, handled, _ = a.delegateOutsideID(ctx, name, prompt)
	return text, failed, handled
}

// delegateOutsideID is delegateOutside, with the id of the board entry the
// work went out under. hive_task_cancel on that id stops the work where it
// runs: the coding run, on whichever machine, or the named agent's ask.
func (a *App) delegateOutsideID(ctx context.Context, name, prompt string) (text string, failed, handled bool, id string) {
	switch {
	case a.isDrivenCLI(name):
		id = a.tasks().Start(name, backend.TaskOut, prompt)
		a.tasks().InSession(id, backend.ChatSessionFrom(ctx))
		ctx, release := a.stoppableTask(ctx, id)
		defer release()
		out := a.runCodingAgent(withBoardTask(ctx, id), name, prompt, "", "")
		ok := out["ok"] == true
		answer, errMsg := fmt.Sprint(orEmpty(out["answer"])), fmt.Sprint(orEmpty(out["error"]))
		a.finishOutside(ctx, id, ok, answer, errMsg)
		if !ok {
			return firstNonEmpty(errMsg, answer), true, true, id
		}
		return answer, false, true, id
	case a.addressable(name):
		id = a.tasks().Start(name, backend.TaskOut, prompt)
		a.tasks().InSession(id, backend.ChatSessionFrom(ctx))
		ctx, release := a.stoppableTask(ctx, id)
		defer release()
		res := a.askAgent(ctx, name, prompt)
		a.finishOutside(ctx, id, !res.Failed, res.Text, res.Reason)
		if res.Failed {
			return firstNonEmpty(res.Reason, res.Text), true, true, id
		}
		return res.Text, false, true, id
	}
	return "", false, false, ""
}
