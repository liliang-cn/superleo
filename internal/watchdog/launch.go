package watchdog

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Host is what the watchdog needs from the machine around the app.
type Host interface {
	// Running reports whether the app's process exists.
	Running() bool
	// Launch opens the app, without the API key that breaks the queen.
	Launch() error
	// Notify shows a macOS notification.
	Notify(title, text string) error
}

// MacHost is the real thing.
type MacHost struct{ AppPath string }

func (m MacHost) Running() bool {
	// The executable inside the bundle; pgrep -f matches the full command line.
	err := exec.Command("pgrep", "-f", filepath.Join(m.AppPath, "Contents", "MacOS")+"/").Run()
	return err == nil
}

// launchEnv is env without ANTHROPIC_API_KEY: that key has no credit, and a
// SuperLeo that finds it hands it to the queen's engine.
func launchEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, e := range env {
		if strings.HasPrefix(e, "ANTHROPIC_API_KEY=") {
			continue
		}
		out = append(out, e)
	}
	return out
}

func (m MacHost) Launch() error {
	if _, err := os.Stat(m.AppPath); err != nil {
		return fmt.Errorf("app not found at %s", m.AppPath)
	}
	// `open` starts the app through launchd, which does not pass our
	// environment on, so the user session's copy is dropped as well.
	_ = exec.Command("launchctl", "unsetenv", "ANTHROPIC_API_KEY").Run()
	cmd := exec.Command("open", m.AppPath)
	cmd.Env = launchEnv(os.Environ())
	return cmd.Run()
}

func (m MacHost) Notify(title, text string) error {
	script := `display notification ` + asString(text) + ` with title ` + asString(title)
	return exec.Command("osascript", "-e", script).Run()
}

// asString quotes s as an AppleScript string literal.
func asString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.NewReplacer("\n", " ", "\r", " ").Replace(s)
	return `"` + s + `"`
}
