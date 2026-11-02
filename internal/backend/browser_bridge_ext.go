package backend

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/liliang-cn/superleo/extension"
)

// ChromeExtensionDir is where SuperLeo unpacks its Chrome extension: a fixed
// path, so "Load unpacked" in chrome://extensions is pointed at it once and
// every later SuperLeo update lands in the same place.
func ChromeExtensionDir() string { return filepath.Join(DataDir(), "chrome-extension") }

// InstallChromeExtension writes the embedded extension to ChromeExtensionDir,
// touching only the files that differ.
func InstallChromeExtension() error {
	dst := ChromeExtensionDir()
	return fs.WalkDir(extension.Chrome, "chrome", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel("chrome", p)
		out := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(out, 0o755)
		}
		data, err := extension.Chrome.ReadFile(p)
		if err != nil {
			return err
		}
		if cur, err := os.ReadFile(out); err == nil && bytes.Equal(cur, data) {
			return nil
		}
		return os.WriteFile(out, data, 0o644)
	})
}

// WriteChromeExtensionPair leaves the bridge's port and token in the unpacked
// extension as pair.json, which the extension reads on its own, so the person
// never copies a code between SuperLeo and Chrome. Only this user can read it.
func WriteChromeExtensionPair(port int, token string) error {
	data, err := json.Marshal(map[string]any{"port": port, "token": token})
	if err != nil {
		return err
	}
	out := filepath.Join(ChromeExtensionDir(), "pair.json")
	if cur, err := os.ReadFile(out); err == nil && bytes.Equal(cur, data) {
		return nil
	}
	return os.WriteFile(out, data, 0o600)
}
