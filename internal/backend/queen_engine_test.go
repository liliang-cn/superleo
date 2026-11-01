package backend

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAnOutsideQueenReadsTheQueensOwnPrompt(t *testing.T) {
	s := &Settings{QueenName: "小蜂", Hive: HiveSettings{Role: HiveRoleQueen, Name: "q"}}
	got := ExternalQueenPrompt(s)
	for _, part := range []string{queenSection(s.Hive), queenNameSection(s), answerLanguageRule, "superleo_hive_x"} {
		if part == "" || !strings.Contains(got, part) {
			t.Fatalf("missing %q in\n%s", part, got)
		}
	}
	// The queen's own agent reads the very same section.
	if hiveSection(s.Hive) != queenSection(s.Hive) {
		t.Fatal("the queen's two prompts differ")
	}
	alone := ExternalQueenPrompt(&Settings{})
	if strings.Contains(alone, "hive_x") || strings.Contains(alone, "In the hive") {
		t.Fatalf("a standalone install was told about a hive:\n%s", alone)
	}
}

func TestAnEngineIsHandedTheServerWithItsSecretOutOfTheEntry(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SUPERLEO_HOME", home)
	t.Setenv("NOTED_TOKEN", "n-secret")
	cfg := `{"mcpServers":{
	  "noted":{"type":"streamable-http","url":"https://noted.example/mcp","headers":{"Authorization":"Bearer ${NOTED_TOKEN}"}},
	  "cortexdb":{"type":"stdio","command":"/usr/local/bin/cortexdb-mcp"},
	  "local":{"url":"http://127.0.0.1:4000/mcp"}}}`
	if err := os.WriteFile(filepath.Join(home, "mcpServers.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	entry, env, ok := EngineMCPServer("noted", true)
	if !ok || entry["type"] != "http" || env["NOTED_TOKEN"] != "n-secret" {
		t.Fatalf("%v %v %v", entry, env, ok)
	}
	if h := entry["headers"].(map[string]any); h["Authorization"] != "Bearer ${NOTED_TOKEN}" {
		t.Fatalf("the secret went into the entry: %v", h)
	}
	if _, _, ok := EngineMCPServer("cortexdb", false); !ok {
		t.Fatal("a command was not handed to a CLI on this machine")
	}
	if _, _, ok := EngineMCPServer("cortexdb", true); ok {
		t.Fatal("a command was handed to another machine")
	}
	if _, _, ok := EngineMCPServer("local", true); ok {
		t.Fatal("a loopback URL was handed to another machine")
	}
	t.Setenv("NOTED_TOKEN", "")
	if _, _, ok := EngineMCPServer("noted", false); ok {
		t.Fatal("a server was handed over without its secret")
	}
}

func TestEngineMCPServerResolvesReferences(t *testing.T) {
	cfg := `{"mcpServers":{
	  "own":{"type":"stdio","command":"/bin/mcp-remote","args":["--header","Authorization:${OWN_AUTH}"],"env":{"OWN_AUTH":"Bearer fake-own"}},
	  "proc":{"type":"stdio","command":"/bin/mcp-remote","args":["--header","Authorization:${PROC_AUTH}"]},
	  "nowhere":{"type":"stdio","command":"/bin/mcp-remote","args":["--header","Authorization:${NOWHERE_AUTH}"]},
	  "loop":{"url":"http://localhost:4000/mcp"}}}`
	cases := []struct {
		name, server string
		remote       bool
		wantOK       bool
		wantEnv      map[string]string
		wantReason   string
	}{
		{"ref in the entry's own env", "own", false, true, map[string]string{"OWN_AUTH": "Bearer fake-own"}, ""},
		{"ref in the process env", "proc", false, true, map[string]string{"PROC_AUTH": "Bearer fake-proc"}, ""},
		{"ref nowhere", "nowhere", false, false, nil, "${NOWHERE_AUTH}"},
		{"loopback URL to another machine", "loop", true, false, nil, "another machine"},
		{"not configured", "absent", false, false, nil, EngineNotConfigured},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("SUPERLEO_HOME", home)
			t.Setenv("PROC_AUTH", "Bearer fake-proc")
			t.Setenv("NOWHERE_AUTH", "")
			if err := os.WriteFile(filepath.Join(home, "mcpServers.json"), []byte(cfg), 0o600); err != nil {
				t.Fatal(err)
			}
			_, env, why, ok := EngineMCPServerWhy(c.server, c.remote)
			if ok != c.wantOK {
				t.Fatalf("ok = %v (%s)", ok, why)
			}
			if !ok {
				if !strings.Contains(why, c.wantReason) {
					t.Fatalf("reason %q lacks %q", why, c.wantReason)
				}
				return
			}
			if len(env) != len(c.wantEnv) {
				t.Fatalf("env = %v", env)
			}
			for k, v := range c.wantEnv {
				if env[k] != v {
					t.Fatalf("env[%s] = %q", k, env[k])
				}
			}
		})
	}
}
