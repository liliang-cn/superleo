package backend

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// The task board on disk.
//
// The board (hive_tasks.go) is memory, and a queen that restarts used to come
// back knowing nothing of what she had sent out. The store is the same board
// written down as it changes, so an order is still there afterwards — running,
// finished, or lost — and a long-term task can be looked after across days.
//
// One row per order, the order itself as JSON beside the few columns the
// queries need. Finished orders are kept for taskKeepDays and at most
// taskKeepRows of them; a running order is never pruned.

const (
	taskKeepDays = 30
	taskKeepRows = 5000
)

// TaskLost is an order that was running when this instance went away and that
// nobody has been able to find since: the worker no longer has it, or could
// not be reached. Why is in its Error.
const TaskLost = "lost"

// TaskStore is the board's file: <data>/taskboard.db.
type TaskStore struct{ db *sql.DB }

// TaskStorePath is where the store lives.
func TaskStorePath() string { return filepath.Join(DataDir(), "taskboard.db") }

// OpenTaskStore opens (and makes, if need be) the store at path.
func OpenTaskStore(path string) (*TaskStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	// One writer at a time is all sqlite allows; a single connection makes the
	// store's own calls queue instead of failing on a locked database.
	db.SetMaxOpenConns(1)
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS hive_tasks (
			id TEXT PRIMARY KEY,
			worker TEXT NOT NULL,
			dir TEXT NOT NULL,
			state TEXT NOT NULL,
			started_at INTEGER NOT NULL,
			ended_at INTEGER NOT NULL DEFAULT 0,
			body TEXT NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS hive_tasks_started ON hive_tasks (started_at)`,
		`CREATE INDEX IF NOT EXISTS hive_tasks_state ON hive_tasks (state)`,
		`CREATE TABLE IF NOT EXISTS task_trackers (
			id TEXT PRIMARY KEY,
			task TEXT NOT NULL,
			kind TEXT NOT NULL,
			every_s INTEGER NOT NULL,
			until INTEGER NOT NULL DEFAULT 0,
			redo INTEGER NOT NULL DEFAULT 0,
			redos INTEGER NOT NULL DEFAULT 0,
			state TEXT NOT NULL,
			next_at INTEGER NOT NULL,
			last_at INTEGER NOT NULL DEFAULT 0,
			checks INTEGER NOT NULL DEFAULT 0,
			said TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL,
			session TEXT NOT NULL DEFAULT '')`,
	} {
		if _, err := db.Exec(q); err != nil {
			db.Close()
			return nil, err
		}
	}
	return &TaskStore{db: db}, nil
}

// Close closes the file.
func (s *TaskStore) Close() error {
	if s == nil {
		return nil
	}
	return s.db.Close()
}

func ms(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// Put writes an order, replacing what was there. Its Live text is not kept: it
// is what the order is doing this instant and is meaningless after a restart.
func (s *TaskStore) Put(t HiveTask) error {
	if s == nil {
		return nil
	}
	t.Live = ""
	body, err := json.Marshal(t)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO hive_tasks (id, worker, dir, state, started_at, ended_at, body)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET worker=excluded.worker, dir=excluded.dir, state=excluded.state,
			started_at=excluded.started_at, ended_at=excluded.ended_at, body=excluded.body`,
		t.ID, t.Worker, t.Dir, t.State, ms(t.StartedAt), ms(t.EndedAt), string(body))
	return err
}

func scanTasks(rows *sql.Rows) []HiveTask {
	defer rows.Close()
	var out []HiveTask
	for rows.Next() {
		var body string
		if rows.Scan(&body) != nil {
			continue
		}
		var t HiveTask
		if json.Unmarshal([]byte(body), &t) == nil {
			out = append(out, t)
		}
	}
	return out
}

// Get is one order by its full id.
func (s *TaskStore) Get(id string) (HiveTask, bool) {
	if s == nil || id == "" {
		return HiveTask{}, false
	}
	rows, err := s.db.Query(`SELECT body FROM hive_tasks WHERE id = ?`, id)
	if err != nil {
		return HiveTask{}, false
	}
	if ts := scanTasks(rows); len(ts) == 1 {
		return ts[0], true
	}
	return HiveTask{}, false
}

// Prefixed is the ids that start with prefix, up to limit.
func (s *TaskStore) Prefixed(prefix string, limit int) []string {
	if s == nil || prefix == "" {
		return nil
	}
	// LIKE with the wildcards of the prefix itself escaped: an id is a UUID,
	// but what was typed is whatever the model wrote.
	esc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(prefix)
	rows, err := s.db.Query(`SELECT id FROM hive_tasks WHERE id LIKE ? ESCAPE '\' ORDER BY started_at DESC LIMIT ?`, esc+"%", limit)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			out = append(out, id)
		}
	}
	return out
}

// List is the orders newest first; runningOnly keeps just the ones in motion.
func (s *TaskStore) List(limit int, runningOnly bool) []HiveTask {
	if s == nil {
		return nil
	}
	q := `SELECT body FROM hive_tasks`
	if runningOnly {
		q += ` WHERE state = '` + TaskRunning + `'`
	}
	rows, err := s.db.Query(q+` ORDER BY started_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil
	}
	return scanTasks(rows)
}

// Active is what the board is started with: every order still running and the
// last keep that are not, oldest first.
func (s *TaskStore) Active(keep int) []HiveTask {
	if s == nil {
		return nil
	}
	rows, err := s.db.Query(`SELECT body FROM hive_tasks WHERE state = ?
		OR id IN (SELECT id FROM hive_tasks WHERE state != ? ORDER BY started_at DESC LIMIT ?)
		ORDER BY started_at ASC`, TaskRunning, TaskRunning, keep)
	if err != nil {
		return nil
	}
	return scanTasks(rows)
}

// Prune drops finished orders older than taskKeepDays, then the oldest
// finished ones beyond taskKeepRows. It returns how many went.
func (s *TaskStore) Prune(now time.Time) int {
	if s == nil {
		return 0
	}
	cut := now.AddDate(0, 0, -taskKeepDays).UnixMilli()
	n := 0
	// An order is as old as its end; one with no end (never settled) as its start.
	if r, err := s.db.Exec(`DELETE FROM hive_tasks WHERE state != ? AND MAX(ended_at, started_at) < ?`, TaskRunning, cut); err == nil {
		k, _ := r.RowsAffected()
		n += int(k)
	}
	if r, err := s.db.Exec(`DELETE FROM hive_tasks WHERE state != ? AND id NOT IN
		(SELECT id FROM hive_tasks WHERE state != ? ORDER BY started_at DESC LIMIT ?)`,
		TaskRunning, TaskRunning, taskKeepRows); err == nil {
		k, _ := r.RowsAffected()
		n += int(k)
	}
	return n
}

// Count is how many orders are stored.
func (s *TaskStore) Count() int {
	if s == nil {
		return 0
	}
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM hive_tasks`).Scan(&n)
	return n
}

// ---- trackers ----

const (
	TrackActive  = "active"
	TrackDone    = "done"
	TrackStopped = "stopped"

	// What a tracker looks at.
	TrackOrder = "order" // an order on the task board
	TrackLong  = "long"  // a long task
)

// Tracker is a standing check on a long-term order: look at it every Every
// until it is over, or until Until, and say so when it is stuck or lost.
type Tracker struct {
	ID    string        `json:"id"`
	Task  string        `json:"task"`
	Kind  string        `json:"kind"`
	Every time.Duration `json:"every"`
	// Until is when to stop looking; zero looks until the order is over.
	Until time.Time `json:"until,omitzero"`
	// Redo sends the order again, to the same worker, when it is lost (at most
	// trackRedos times); the tracker then follows the new order.
	Redo   bool      `json:"redo,omitempty"`
	Redos  int       `json:"redos,omitempty"`
	State  string    `json:"state"`
	NextAt time.Time `json:"next_at"`
	LastAt time.Time `json:"last_at,omitzero"`
	Checks int       `json:"checks"`
	// Said is what the last check told the person, so a stuck order is said once
	// and not at every look.
	Said      string    `json:"said,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	// Session is the conversation the check was asked for in, opened by the
	// notice that reports it.
	Session string `json:"session,omitempty"`
}

// trackRedos is how many times a tracker re-sends a lost order.
const trackRedos = 3

const trackCols = `id, task, kind, every_s, until, redo, redos, state, next_at, last_at, checks, said, created_at, session`

func (s *TaskStore) PutTracker(t Tracker) error {
	if s == nil {
		return errors.New("the task board is not stored here")
	}
	_, err := s.db.Exec(`INSERT INTO task_trackers (`+trackCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET task=excluded.task, kind=excluded.kind, every_s=excluded.every_s,
			until=excluded.until, redo=excluded.redo, redos=excluded.redos, state=excluded.state,
			next_at=excluded.next_at, last_at=excluded.last_at, checks=excluded.checks, said=excluded.said,
			session=excluded.session`,
		t.ID, t.Task, t.Kind, int64(t.Every/time.Second), ms(t.Until), boolInt(t.Redo), t.Redos, t.State,
		ms(t.NextAt), ms(t.LastAt), t.Checks, t.Said, ms(t.CreatedAt), t.Session)
	return err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func fromMS(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.UnixMilli(v)
}

func (s *TaskStore) trackers(where string, args ...any) []Tracker {
	if s == nil {
		return nil
	}
	rows, err := s.db.Query(`SELECT `+trackCols+` FROM task_trackers `+where, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []Tracker
	for rows.Next() {
		var t Tracker
		var every, until, next, last, created int64
		var redo int
		if rows.Scan(&t.ID, &t.Task, &t.Kind, &every, &until, &redo, &t.Redos, &t.State, &next, &last,
			&t.Checks, &t.Said, &created, &t.Session) != nil {
			continue
		}
		t.Every, t.Until, t.Redo = time.Duration(every)*time.Second, fromMS(until), redo != 0
		t.NextAt, t.LastAt, t.CreatedAt = fromMS(next), fromMS(last), fromMS(created)
		out = append(out, t)
	}
	return out
}

// Trackers lists them newest first; activeOnly leaves out the ones that are over.
func (s *TaskStore) Trackers(activeOnly bool) []Tracker {
	if activeOnly {
		return s.trackers(`WHERE state = ? ORDER BY created_at DESC`, TrackActive)
	}
	return s.trackers(`ORDER BY created_at DESC`)
}

// Tracker is one by id, or by the start of an id.
func (s *TaskStore) Tracker(id string) (Tracker, error) {
	all := s.trackers(`WHERE id LIKE ? ESCAPE '\'`, strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(id)+"%")
	switch len(all) {
	case 0:
		return Tracker{}, fmt.Errorf("no tracker %q", id)
	case 1:
		return all[0], nil
	}
	return Tracker{}, fmt.Errorf("%q is the start of %d trackers; give more of it", id, len(all))
}

// DueTrackers are the active ones whose time has come.
func (s *TaskStore) DueTrackers(now time.Time) []Tracker {
	return s.trackers(`WHERE state = ? AND next_at <= ? ORDER BY next_at`, TrackActive, ms(now))
}

// PruneTrackers drops the ones that ended more than taskKeepDays ago.
func (s *TaskStore) PruneTrackers(now time.Time) {
	if s == nil {
		return
	}
	_, _ = s.db.Exec(`DELETE FROM task_trackers WHERE state != ? AND MAX(last_at, created_at) < ?`,
		TrackActive, now.AddDate(0, 0, -taskKeepDays).UnixMilli())
}
