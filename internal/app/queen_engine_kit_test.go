package app

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/liliang-cn/superleo/internal/backend"
)

// A noted that takes its token from its own env (mcp-remote's form) reaches
// the engine: the kit carries the server and the token by name.
func TestQueenKitHandsNotedWhoseTokenIsInItsOwnEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SUPERLEO_HOME", home)
	t.Setenv("NOTED_AUTH", "")
	cfg := `{"mcpServers":{"noted":{"type":"stdio","command":"/opt/bin/mcp-remote",
	  "args":["https://noted.example/mcp","--header","Authorization:${NOTED_AUTH}"],
	  "env":{"NOTED_AUTH":"Bearer fake-token"}}}}`
	if err := os.WriteFile(filepath.Join(home, "mcpServers.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &App{serveURL: "http://127.0.0.1:43999"}
	kit, err := a.queenKitFor("claude", &backend.Settings{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := kit.Servers["noted"]; !ok {
		t.Fatalf("noted is not in the kit: %v", kit.Servers)
	}
	if kit.Env["NOTED_AUTH"] != "Bearer fake-token" {
		t.Fatal("NOTED_AUTH is not in the kit's env")
	}
}
