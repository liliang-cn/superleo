package watchdog

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Log writes one greppable line per result or action, rotating at a size.
type Log struct {
	mu   sync.Mutex
	path string
	max  int64
	now  func() time.Time
	// Secrets are scrubbed from every line.
	secrets []string
}

func NewLog(home string, maxMB int) *Log {
	if maxMB <= 0 {
		maxMB = 4
	}
	return &Log{path: filepath.Join(home, "logs", "watchdog.log"), max: int64(maxMB) << 20, now: time.Now}
}

func (l *Log) Path() string { return l.path }

// Hide registers a value that must never reach the file.
func (l *Log) Hide(s string) {
	if len(s) >= 6 {
		l.mu.Lock()
		l.secrets = append(l.secrets, s)
		l.mu.Unlock()
	}
}

// Scrub removes registered secrets from s.
func (l *Log) Scrub(s string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, k := range l.secrets {
		s = strings.ReplaceAll(s, k, "***")
	}
	return s
}

// Line writes "<time> check=<check> result=<result> k=v ..." (kv are pairs).
func (l *Log) Line(check, result string, kv ...string) {
	var b strings.Builder
	fmt.Fprintf(&b, "%s check=%s result=%s", l.now().Format(time.RFC3339), check, result)
	for i := 0; i+1 < len(kv); i += 2 {
		v := strings.Join(strings.Fields(kv[i+1]), " ")
		if len(v) > 300 {
			v = v[:300] + "..."
		}
		if strings.ContainsAny(v, " \"=") {
			v = fmt.Sprintf("%q", v)
		}
		fmt.Fprintf(&b, " %s=%s", kv[i], v)
	}
	line := l.Scrub(b.String()) + "\n"
	l.mu.Lock()
	defer l.mu.Unlock()
	_ = os.MkdirAll(filepath.Dir(l.path), 0o755)
	if st, err := os.Stat(l.path); err == nil && st.Size() >= l.max {
		_ = os.Rename(l.path, l.path+".1")
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprint(os.Stderr, line)
		return
	}
	defer f.Close()
	_, _ = f.WriteString(line)
}
