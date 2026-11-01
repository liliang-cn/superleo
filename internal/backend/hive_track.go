package backend

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Looking after a long-term order.
//
// A tracker is a check the queen leaves on an order: every so often, look at
// where it has got, and say so when it has finished, failed, got lost or gone
// quiet. It is deterministic on purpose — reading a record costs no model
// turn, so "every hour for three days" is cheap — and the queen is only
// brought in when there is something to decide.

// TrackSubject is what a check found: the order (or long task) as it stands.
type TrackSubject struct {
	// Found is false when nothing by that id is known any more.
	Found bool
	// State is running, done, failed, cancelled or lost.
	State string
	// Name is what to call it to the person.
	Name string
	// Detail is the result, the error or why it is lost, whichever there is.
	Detail string
	// Last is the last sign of life while it runs.
	Last time.Time
}

// SubjectOf reads a board order as a tracker sees it.
func SubjectOf(t HiveTask) TrackSubject {
	last := t.StartedAt
	if n := len(t.Events); n > 0 && t.Events[n-1].At.After(last) {
		last = t.Events[n-1].At
	}
	detail := t.Result
	if t.Error != "" {
		detail = t.Error
	}
	return TrackSubject{Found: true, State: t.State, Name: fmt.Sprintf("the order to %s (%s)", t.Worker, shortID(t.ID)),
		Detail: clip(detail, 300), Last: last}
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// Check-by-check outcomes. A status is what the last check found, so the same
// thing is not said again at every look.
const (
	StatusRunning   = "running"
	StatusStuck     = "stuck"
	StatusLate      = "late"
	StatusRetrying  = "retrying"
	StatusGone      = "gone"
	StatusLost      = TaskLost
	StatusDone      = TaskDone
	StatusFailed    = TaskFailed
	StatusCancelled = TaskCancelled
)

// TrackVerdict is what one check came to.
type TrackVerdict struct {
	Status string
	// Say is the message for the person; empty when there is nothing new.
	Say   string
	Level Level
	// Over ends the tracker.
	Over bool
	// Redo asks for the order to be sent again.
	Redo bool
}

// stuckAfter is how long an order may show no sign of life before it is called
// stuck: two intervals, and at least a quarter of an hour, because a worker in
// one long tool call is quiet and well.
func stuckAfter(every time.Duration) time.Duration {
	return max(2*every, 15*time.Minute)
}

// EvaluateTrack decides what a check at now makes of the subject.
func EvaluateTrack(tr Tracker, s TrackSubject, now time.Time) TrackVerdict {
	say := func(v TrackVerdict) TrackVerdict {
		// Said once: the same finding as last time is not repeated.
		if !v.Over && v.Status == tr.Said {
			v.Say = ""
		}
		return v
	}
	if !s.Found {
		return TrackVerdict{Status: StatusGone, Level: LevelWarn, Over: true,
			Say: "I can no longer find the order I was keeping an eye on (" + tr.Task + "). It is not on the board."}
	}
	switch s.State {
	case TaskDone:
		return TrackVerdict{Status: StatusDone, Level: LevelSuccess, Over: true, Say: s.Name + " is finished. " + s.Detail}
	case TaskFailed:
		return TrackVerdict{Status: StatusFailed, Level: LevelError, Over: true, Say: s.Name + " failed: " + s.Detail}
	case TaskCancelled:
		return TrackVerdict{Status: StatusCancelled, Level: LevelInfo, Over: true, Say: s.Name + " was cancelled."}
	case TaskLost:
		if tr.Redo && tr.Redos < trackRedos {
			return TrackVerdict{Status: StatusRetrying, Level: LevelWarn, Redo: true,
				Say: fmt.Sprintf("%s was lost (%s). Sending it again, attempt %d of %d.", s.Name, s.Detail, tr.Redos+1, trackRedos)}
		}
		return TrackVerdict{Status: StatusLost, Level: LevelWarn, Over: true, Say: s.Name + " was lost: " + s.Detail}
	}
	if !tr.Until.IsZero() && !now.Before(tr.Until) {
		return TrackVerdict{Status: StatusLate, Level: LevelWarn, Over: true,
			Say: s.Name + " is still not finished at the time you gave (" + tr.Until.Local().Format("Mon 15:04") + ")."}
	}
	if q := now.Sub(s.Last); q > stuckAfter(tr.Every) {
		return say(TrackVerdict{Status: StatusStuck, Level: LevelWarn,
			Say: fmt.Sprintf("%s has shown no progress for %s.", s.Name, q.Round(time.Minute))})
	}
	return TrackVerdict{Status: StatusRunning}
}

// NewTracker makes a tracker on an order, first checking one interval from now.
func NewTracker(task, kind string, every time.Duration, until time.Time, redo bool, session string, now time.Time) Tracker {
	t := Tracker{ID: uuid.NewString(), Task: task, Kind: kind, Every: every, Until: until, Redo: redo,
		State: TrackActive, CreatedAt: now, Session: session}
	t.NextAt = t.next(now)
	return t
}

// next is when the check after now is due: an interval on, or the deadline if
// that comes first, so the deadline is itself a look.
func (t Tracker) next(now time.Time) time.Time {
	n := now.Add(t.Every)
	if !t.Until.IsZero() && n.After(t.Until) && now.Before(t.Until) {
		return t.Until
	}
	return n
}

// Checked is the tracker after a check at now came to v.
func (t Tracker) Checked(now time.Time, v TrackVerdict) Tracker {
	t.Checks++
	t.LastAt = now
	t.Said = v.Status
	if v.Redo {
		t.Redos++
		t.Said = "" // the new order starts a fresh account
	}
	t.NextAt = t.next(now)
	if v.Over {
		t.State = TrackDone
	}
	return t
}
