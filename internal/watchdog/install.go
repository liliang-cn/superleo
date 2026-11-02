package watchdog

import (
	"bytes"
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"text/template"

	"github.com/liliang-cn/superleo/deploy/macos"
)

// Label is the LaunchAgent's name.
const Label = "cn.superleo.watchdog"

// Installer writes and loads the LaunchAgent. Run is how launchctl is called
// (replaced in tests).
type Installer struct {
	Binary string // absolute path of superleo-watchdog
	Home   string // the user's home directory
	Dir    string // LaunchAgents directory
	LogDir string
	UID    int
	Run    func(name string, args ...string) error
}

// NewInstaller fills in the real paths.
func NewInstaller(binary string) (*Installer, error) {
	h, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return &Installer{Binary: binary, Home: h, Dir: filepath.Join(h, "Library", "LaunchAgents"),
		LogDir: filepath.Join(DataDir(), "logs"), UID: os.Getuid(),
		Run: func(n string, a ...string) error {
			out, err := exec.Command(n, a...).CombinedOutput()
			if err != nil {
				return fmt.Errorf("%s %v: %v: %s", n, a, err, bytes.TrimSpace(out))
			}
			return nil
		}}, nil
}

func (i *Installer) plistPath() string { return filepath.Join(i.Dir, Label+".plist") }
func (i *Installer) domain() string    { return "gui/" + strconv.Itoa(i.UID) }

// Render is the plist with the paths filled in (XML-escaped).
func (i *Installer) Render() (string, error) {
	t, err := template.New("plist").Parse(macos.WatchdogPlist)
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	err = t.Execute(&b, map[string]string{"Binary": html.EscapeString(i.Binary),
		"Home": html.EscapeString(i.Home), "LogDir": html.EscapeString(i.LogDir)})
	return b.String(), err
}

// Install writes the plist and loads it, replacing a loaded copy.
func (i *Installer) Install() error {
	body, err := i.Render()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(i.Dir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(i.LogDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(i.plistPath(), []byte(body), 0o644); err != nil {
		return err
	}
	_ = i.Run("launchctl", "bootout", i.domain()+"/"+Label) // not loaded is fine
	return i.Run("launchctl", "bootstrap", i.domain(), i.plistPath())
}

// Uninstall unloads and removes it.
func (i *Installer) Uninstall() error {
	_ = i.Run("launchctl", "bootout", i.domain()+"/"+Label)
	if err := os.Remove(i.plistPath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
