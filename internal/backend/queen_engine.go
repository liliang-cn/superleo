package backend

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"
)

// QueenEngineServers are this instance's own MCP servers that an agent CLI
// thinking for the queen is handed too, when they are configured here: the
// person's data (noted) and the shared brain (cortexdb). The rest stay
// SuperLeo's.
var QueenEngineServers = []string{NotedServer, "cortexdb"}

// EngineMCPServer is one of this instance's MCP servers as an agent CLI is
// handed it, in the shape Claude's --mcp-config reads. The entry keeps its
// ${NAME} references, which Claude fills in itself, and env carries their
// values: a secret then travels in the CLI's environment, never on its
// command line where any process listing shows it.
//
// ok is false when it is not configured here, needs a variable that is not
// set, or — with remote, for a CLI on another machine — is a command (it
// would run there, not here) or a URL only this machine can reach.
func EngineMCPServer(name string, remote bool) (entry map[string]any, env map[string]string, ok bool) {
	entry, env, _, ok = EngineMCPServerWhy(name, remote)
	return entry, env, ok
}

// EngineNotConfigured is the reason EngineMCPServerWhy gives for a server
// this instance simply does not have.
const EngineNotConfigured = "not configured on this machine"

// EngineMCPServerWhy is EngineMCPServer that also says why a server was left
// out. The reason names variables, never their values.
//
// A ${NAME} is read from the entry's own env first — mcp-remote's documented
// form is "--header", "Authorization:${AUTH}" with "env": {"AUTH": "…"} —
// and then from this process's environment.
func EngineMCPServerWhy(name string, remote bool) (entry map[string]any, env map[string]string, reason string, ok bool) {
	raw, found := rawMCPServer(MCPConfigSources(), name)
	if !found {
		return nil, nil, EngineNotConfigured, false
	}
	entry = make(map[string]any, len(raw))
	for k, v := range raw {
		entry[k] = v
	}
	own, _ := entry["env"].(map[string]any)
	lookup := func(k string) (string, bool) {
		if v, ok := own[k].(string); ok && v != "" {
			return v, true
		}
		return os.LookupEnv(k)
	}
	u, _ := entry["url"].(string)
	switch t, _ := entry["type"].(string); {
	case u != "":
		// agent-go also reads streamable-http; Claude knows it as http.
		if t == "" || strings.Contains(t, "streamable") {
			entry["type"] = "http"
		}
		if resolved, _ := expandMCPEnv(u, lookup); remote && loopbackURL(resolved) {
			return nil, nil, "its URL only this machine can reach, and the engine is on another machine", false
		}
	case remote:
		return nil, nil, "it is a command, which would run on the engine's machine, not here", false
	}
	body, _ := json.Marshal(entry)
	env = map[string]string{}
	for _, m := range mcpEnvRef.FindAllStringSubmatch(string(body), -1) {
		v, set := lookup(m[1])
		if !set || v == "" {
			return nil, nil, fmt.Sprintf("${%s} is set neither in its own env nor in the environment", m[1]), false
		}
		// The other machine's own HOME or PATH is the one its CLI needs.
		if remote && slices.Contains([]string{"HOME", "PATH", "USER", "SHELL", "TMPDIR", "PWD"}, m[1]) {
			return nil, nil, fmt.Sprintf("${%s} is this machine's own, not the engine machine's", m[1]), false
		}
		env[m[1]] = v
	}
	return entry, env, "", true
}

// rawMCPServer is the first entry called name in sources, as written.
func rawMCPServer(sources []string, name string) (map[string]any, bool) {
	for _, src := range sources {
		b, err := os.ReadFile(src)
		if err != nil {
			continue
		}
		var f struct {
			MCPServers map[string]map[string]any `json:"mcpServers"`
		}
		if json.Unmarshal(b, &f) != nil {
			continue
		}
		if e, ok := f.MCPServers[name]; ok {
			return e, true
		}
	}
	return nil, false
}

func loopbackURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return true
	}
	h := strings.ToLower(u.Hostname())
	return h == "localhost" || h == "::1" || strings.HasPrefix(h, "127.")
}

// MCPEnvRefs are the variables a piece of MCP config names as ${NAME}.
func MCPEnvRefs(s string) []string {
	var out []string
	for _, m := range mcpEnvRef.FindAllStringSubmatch(s, -1) {
		out = append(out, m[1])
	}
	return out
}
