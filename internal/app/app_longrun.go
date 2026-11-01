package app

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/liliang-cn/agent-go/v3/pkg/agent"
	"github.com/liliang-cn/superleo/internal/backend"
)

// The Runs page: a window onto a task that takes hours.
//
// A long run has a backend (StreamLong) and, until this, no surface. These
// are the four calls the page makes, and the wall they read from.

// runWall returns the app's one RunWall, creating it on first use. One per
// app rather than per service, so a settings save does not wipe the wall.
func (a *App) runWall() *backend.RunWall {
	a.wallOnce.Do(func() {
		a.wall = backend.NewRunWall(
			func(ctx context.Context, taskID string) []agent.PlanItem {
				a.mu.Lock()
				svc := a.svc
				a.mu.Unlock()
				if svc == nil {
					return nil
				}
				return svc.Plan(ctx, taskID)
			},
			func(taskID, kind string) {
				a.longJournalTouch(taskID, a.runWall().Tokens(taskID), kind == "finish")
				a.emit("longrun:tick", map[string]any{"taskId": taskID, "kind": kind})
			},
		)
	})
	return a.wall
}

// LongRunStart begins a segmented task and returns its id. Everything the
// run does reaches the page as longrun:tick events; LongRunState reads the
// wall back. maxSegments / roundsPerSegment / maxMinutes at zero
// take agent-go's defaults; maxTokens at zero takes the settings'
// long_task_max_tokens, and none there is no budget. taskID non-empty resumes that task. unattended
// lets its tool calls through the approval gate without asking (audited),
// which a task that runs for hours needs and a page nobody has open cannot
// give.
func (a *App) LongRunStart(goal string, maxSegments, roundsPerSegment, maxMinutes, maxTokens int, taskID string, unattended bool) string {
	goal = strings.TrimSpace(goal)
	if goal == "" {
		return ""
	}
	a.mu.Lock()
	svc := a.svc
	settings := a.settings
	buildErr := a.buildErr
	a.mu.Unlock()

	if taskID == "" {
		taskID = uuid.NewString()
	}
	model := ""
	if settings != nil {
		model = settings.LLMModel
	}
	// A budget the caller did not give falls back to the settings', and
	// none there means none at all.
	if maxTokens <= 0 && settings != nil {
		maxTokens = settings.LongTaskMaxTokens
	}
	rec := &longRecord{
		TaskID: taskID, Goal: goal, Model: model,
		MaxSegments: maxSegments, RoundsPerSegment: roundsPerSegment,
		MaxMinutes: maxMinutes, MaxTokens: maxTokens, Unattended: unattended,
	}
	if old := readLongRecord(taskID); old != nil {
		rec.StartedAt = old.StartedAt
	}
	if svc == nil {
		wall := a.runWall()
		wall.Begin(taskID, goal, model, maxSegments)
		wall.Finish(taskID, false, "unavailable", "", errBackendNotReady(buildErr))
		return taskID
	}
	a.startLongRun(rec, maxMinutes, maxTokens, false)
	return taskID
}

// startLongRun runs a task in the background under its record. maxMinutes and
// maxTokens are the limits this run is given, which on a resume are what is
// left of the record's. resumed says it is a task picked up after a restart.
func (a *App) startLongRun(rec *longRecord, maxMinutes, maxTokens int, resumed bool) {
	a.mu.Lock()
	svc := a.svc
	buildErr := a.buildErr
	a.mu.Unlock()
	taskID := rec.TaskID
	wall := a.runWall()
	wall.Begin(taskID, rec.Goal, rec.Model, rec.MaxSegments)
	if svc == nil {
		wall.Finish(taskID, false, "unavailable", "", errBackendNotReady(buildErr))
		return
	}
	a.longJournalBegin(rec)
	if resumed {
		wall.Resumed(taskID, rec.Resumes)
	}

	ctx, cancel := context.WithCancel(context.Background())
	a.longMu.Lock()
	if a.longRuns == nil {
		a.longRuns = map[string]context.CancelFunc{}
	}
	a.longRuns[taskID] = cancel
	a.longMu.Unlock()

	go func() {
		defer func() {
			cancel()
			a.longMu.Lock()
			delete(a.longRuns, taskID)
			delete(a.longStopped, taskID)
			a.longMu.Unlock()
		}()
		opts := backend.LongRunOptions{
			MaxSegments:      rec.MaxSegments,
			RoundsPerSegment: rec.RoundsPerSegment,
			MaxTotalTokens:   maxTokens,
			TaskID:           taskID,
			Unattended:       rec.Unattended,
		}
		if maxMinutes > 0 {
			opts.MaxDuration = time.Duration(maxMinutes) * time.Minute
		}
		report, err := svc.StreamLong(ctx, rec.Goal, opts, nil)
		tokens := wall.Tokens(taskID)

		a.longMu.Lock()
		byPerson := a.longStopped[taskID]
		a.longMu.Unlock()
		// The process going away ends the run too, and that is not the task
		// ending: it stays running in the journal, which is what the next
		// start looks for.
		leaving := !byPerson && a.quitting.Load()

		if err != nil {
			stop := "error"
			if ctx.Err() != nil {
				stop = "cancelled"
			}
			wall.Finish(taskID, false, stop, "", err)
			switch {
			case byPerson:
				wall.SetState(taskID, longStopped)
				a.longJournalEnd(taskID, longStopped, stop, tokens)
			case !leaving:
				a.longJournalEnd(taskID, longFailed, err.Error(), tokens)
			}
			return
		}
		wall.Finish(taskID, report.Done, report.Stop, report.Text, nil)
		switch {
		case byPerson:
			wall.SetState(taskID, longStopped)
			a.longJournalEnd(taskID, longStopped, report.Stop, tokens)
		case report.Done:
			a.longJournalEnd(taskID, longDone, report.Stop, tokens)
		case !leaving:
			a.longJournalEnd(taskID, longFailed, report.Stop, tokens)
		}
	}()
}

// LongRunStop cancels a task in flight. A task not in flight is not an error.
func (a *App) LongRunStop(taskID string) bool {
	a.longMu.Lock()
	cancel, ok := a.longRuns[taskID]
	if ok {
		// Said before the cancel: a run ending in a cancel is told apart
		// from the process ending by this, and only this one is final.
		if a.longStopped == nil {
			a.longStopped = map[string]bool{}
		}
		a.longStopped[taskID] = true
	}
	a.longMu.Unlock()
	if !ok {
		return false
	}
	cancel()
	return true
}

// LongRunState returns everything the wall knows about a task, or nil.
func (a *App) LongRunState(taskID string) *backend.TaskState {
	return a.runWall().Snapshot(context.Background(), taskID)
}

// LongRunList returns every task the wall has seen, newest first.
func (a *App) LongRunList() []backend.TaskSummary {
	return a.runWall().List()
}

type backendNotReady string

func (e backendNotReady) Error() string { return "backend not ready: " + string(e) }

func errBackendNotReady(buildErr string) error { return backendNotReady(buildErr) }
