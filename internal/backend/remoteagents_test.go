package backend

import (
	"strings"
	"testing"
	"time"
)

// The prompt is a person's question and it ends up inside a shell command on
// another machine. This is the one place in the feature where getting it wrong
// is not a bug but a remote code execution, so it is tested against the
// characters that would do it rather than against a happy path.
func TestAQuestionCannotBecomeACommand(t *testing.T) {
	agent := RemoteAgent{Command: []string{"/bin/ask", "-p", "{prompt}"}}
	for _, nasty := range []string{
		"; rm -rf /",
		"$(touch /tmp/pwned)",
		"`id`",
		"'; id; '",
		"a' && curl evil.example '",
		"$HOME",
		"line one\nline two",
		"quote\" and 'quote'",
	} {
		script, err := remoteScript(agent, nasty)
		if err != nil {
			t.Fatalf("%q: %v", nasty, err)
		}
		// Everything after the argv's fixed head must be inside one quoted
		// word. The only way that is false is if a quote closed early.
		body := strings.TrimPrefix(script, "'/bin/ask' '-p' ")
		if body == script {
			t.Fatalf("%q produced an unexpected shape: %s", nasty, script)
		}
		if !strings.HasPrefix(body, "'") || !strings.HasSuffix(body, "'") {
			t.Errorf("%q escaped its quoting: %s", nasty, script)
		}
		// A single quote may appear only as the '\'' idiom that closes,
		// escapes and reopens. Anything else is an early close.
		inner := body[1 : len(body)-1]
		if strings.Contains(strings.ReplaceAll(inner, `'\''`, ""), "'") {
			t.Errorf("%q left a bare quote inside the word: %s", nasty, script)
		}
	}
}

// A quoted word has to survive round-tripping, or the other agent is answering
// a different question from the one that was asked.
func TestQuotingKeepsTheQuestionIntact(t *testing.T) {
	for _, s := range []string{"", "plain", "it's", `"double"`, "$(x)", "a\nb", "中文 · 标点，符号"} {
		q := shellQuote(s)
		// Undo the transformation the way sh would.
		got := strings.ReplaceAll(strings.TrimSuffix(strings.TrimPrefix(q, "'"), "'"), `'\''`, "'")
		if got != s {
			t.Errorf("%q round-tripped to %q", s, got)
		}
	}
}

// An agent whose command has nowhere to put the question would silently ask
// nothing at all.
func TestACommandWithNoPlaceholderIsRefused(t *testing.T) {
	_, err := remoteScript(RemoteAgent{Command: []string{"/bin/ask", "--interactive"}}, "hello")
	if err == nil {
		t.Fatal("a command with no {prompt} was accepted")
	}
	if !strings.Contains(err.Error(), "{prompt}") {
		t.Errorf("the error does not say what is missing: %v", err)
	}
}

// The two things the cluster taught us the hard way have to be in the rendered
// script, because neither failure names itself: a wrong working directory
// reports a permission error about a file nobody mentioned, and a missing HOME
// silently sends the agent to a config that does not follow the volume.
func TestTheScriptCarriesTheDirAndTheEnvironment(t *testing.T) {
	script, err := remoteScript(RemoteAgent{
		User:    "openclaw",
		Dir:     "/tmp",
		Env:     map[string]string{"HOME": "/var/lib/openclaw-home"},
		Command: []string{"/var/lib/openclaw/hermes/hermes", "--oneshot", "{prompt}"},
	}, "hello")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"cd '/tmp'", "sudo -n -u 'openclaw'", "'HOME=/var/lib/openclaw-home'", "--oneshot"} {
		if !strings.Contains(script, want) {
			t.Errorf("the script is missing %q: %s", want, script)
		}
	}
	// cd first, then sudo: the other order runs cd as the login user and hands
	// sudo a directory it may not be able to enter.
	if strings.Index(script, "cd '/tmp'") > strings.Index(script, "sudo") {
		t.Errorf("sudo comes before the cd: %s", script)
	}
}

// A host entry is an ssh argument list, which is what lets a Lima VM — whose
// port is assigned at boot — be named by its generated config file.
func TestAHostEntryCanCarrySSHOptions(t *testing.T) {
	if got := hostArgs("sds@192.168.123.212"); len(got) != 1 || got[0] != "sds@192.168.123.212" {
		t.Errorf("a plain target was mangled: %v", got)
	}
	got := hostArgs("-F ~/.lima/sds-a/ssh.config lima-sds-a")
	if len(got) != 3 || got[0] != "-F" || got[2] != "lima-sds-a" {
		t.Fatalf("options were not split out: %v", got)
	}
	if strings.HasPrefix(got[1], "~") {
		t.Errorf("~ was left for a shell that is not there: %q", got[1])
	}
	// What the person sees is the machine, not the plumbing in front of it.
	if hostLabel("-F ~/.lima/sds-a/ssh.config lima-sds-a") != "lima-sds-a" {
		t.Error("the label is not the target")
	}
}

// Off is off. The zero value is what a settings file written before this
// existed unmarshals to, and an upgrade must not start running commands on
// other machines.
func TestRemoteAgentsAreOffUntilTurnedOn(t *testing.T) {
	var s Settings
	s.RemoteAgents.backfill()
	if s.RemoteAgents.Enabled {
		t.Error("backfilling the catalogue switched the feature on")
	}
	if len(s.RemoteAgents.Agents) == 0 {
		t.Fatal("the catalogue is empty, so @pi would never resolve")
	}
	if s.RemoteAgents.Has("pi") {
		t.Error("a name resolves while the feature is off")
	}
	s.RemoteAgents.Enabled = true
	if !s.RemoteAgents.Has("pi") || !s.RemoteAgents.Has("openclaw") {
		t.Errorf("the cluster agents are missing: %v", s.RemoteAgents.Names())
	}

	// And a run refuses rather than reaching for ssh.
	res, err := NewRemoteRunner(RemoteAgents{}).Run(t.Context(), "pi", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Failed || !strings.Contains(res.Reason, "switched off") {
		t.Errorf("a run with the feature off got %+v", res)
	}
}

// Something a person configured by hand outranks what ships, or an upgrade
// would quietly undo their fix.
func TestAConfiguredAgentIsNotOverwritten(t *testing.T) {
	s := Settings{RemoteAgents: RemoteAgents{
		Agents: map[string]RemoteAgent{
			"pi": {Hosts: []string{"me@elsewhere"}, Command: []string{"pi", "{prompt}"}},
		},
	}}
	s.RemoteAgents.backfill()
	if got := s.RemoteAgents.Agents["pi"].Hosts[0]; got != "me@elsewhere" {
		t.Errorf("the built-in catalogue overwrote a configured agent: %q", got)
	}
	if _, ok := s.RemoteAgents.Agents["openclaw"]; !ok {
		t.Error("backfilling stopped at the first name that was taken")
	}
}

// An entry that cannot be called must not be offered: a name in the menu that
// always fails is worse than a name that is not there.
func TestAnUncallableAgentIsDropped(t *testing.T) {
	withSSH(t, true)
	r := RemoteAgents{Agents: map[string]RemoteAgent{
		"nohost": {Command: []string{"x", "{prompt}"}},
		"noargv": {Hosts: []string{"a@b"}},
		"blank":  {Hosts: []string{"  ", ""}, Command: []string{"x", "{prompt}"}},
		"fine":   {Hosts: []string{"a@b"}, Command: []string{"x", "{prompt}"}},
	}}
	r.normalize()
	if got := r.Names(); len(got) != 1 || got[0] != "fine" {
		t.Errorf("normalize kept agents that cannot run: %v", got)
	}
}

// A hive pod has no ssh client, so the cluster agents it would reach over SSH
// are not offered there; what runs locally and the workers reached by URL are.
func TestWithoutSSHOnlyLocalAndURLAgentsAreOffered(t *testing.T) {
	withSSH(t, false)
	var r RemoteAgents
	r.backfill()
	r.Agents["pi-here"] = RemoteAgent{Hosts: []string{LocalHost}, Command: []string{"pi", "{prompt}"}}
	r.Agents["worker"] = RemoteAgent{URL: "http://worker:43117"}
	r.normalize()
	if got := r.Names(); len(got) != 2 || got[0] != "pi-here" || got[1] != "worker" {
		t.Errorf("without ssh the agents offered are %v", got)
	}
}

func withSSH(t *testing.T, found bool) {
	t.Helper()
	was := sshFound
	sshFound = func() bool { return found }
	t.Cleanup(func() { sshFound = was })
}

// The runner reports a missing name rather than erroring, because the caller
// is often a model and the sentence is what it needs to correct itself.
func TestAnUnknownAgentComesBackAsAReadableRefusal(t *testing.T) {
	r := RemoteAgents{Enabled: true, Agents: map[string]RemoteAgent{
		"pi": {Hosts: []string{"a@b"}, Command: []string{"x", "{prompt}"}},
	}}
	res, err := NewRemoteRunner(r).Run(t.Context(), "gpt", "hi")
	if err != nil {
		t.Fatalf("a bad name should not be an error: %v", err)
	}
	if !res.Failed || !strings.Contains(res.Reason, "pi") {
		t.Errorf("the refusal does not say what is available: %+v", res)
	}
}

// The model must not be able to switch this on for itself. It is the same rule
// external_agents follows, and here it is about another machine.
func TestTheModelCannotTurnRemoteAgentsOn(t *testing.T) {
	for _, key := range []string{"remote_agents", "remote_agents_enabled", "remote_agents.enabled"} {
		if _, ok := settingsWritable[key]; ok {
			t.Errorf("%q is writable by the agent", key)
		}
	}
}

// The exact 429 a delegated pi produced. It arrived as one enormous line of
// JSON and was shown to the user in full — eight lines of quota metrics and
// documentation URLs in the middle of a conversation.
const gemini429 = `exit status 1: 429: {"code":429,"message":"You exceeded your current quota, please check your plan and billing details. For more information on this error, head to: https://ai.google.dev/gemini-api/docs/rate-limits. \n* Quota exceeded for metric: generativelanguage.googleapis.com/generate_content_free_tier_requests, limit: 20, model: gemini-3.8-flash\nPlease retry in 15.081681758s.","status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.Help","links":[{"description":"Learn more about Gemini API quotas","url":"https://ai.google.dev/gemini-api/docs/rate-limits"}]},{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"15s"}]}`

func TestAFailureIsCondensedToWhatIsWorthReading(t *testing.T) {
	got := CondenseAgentFailure(gemini429)
	if n := len([]rune(got)); n > 300 {
		t.Errorf("a %d-rune failure went into the transcript:\n%s", n, got)
	}
	// What to do about it comes first.
	if !strings.HasPrefix(got, "rate-limited or out of quota") {
		t.Errorf("the reader is not told what kind of failure this is: %q", got)
	}
	// And the API's own sentence survives, because it names the account, the
	// model and the quota — a category alone would be less useful.
	if !strings.Contains(got, "You exceeded your current quota") {
		t.Errorf("the agent's own words were dropped: %q", got)
	}
	// The machinery around it does not.
	for _, noise := range []string{"@type", "RetryInfo", "quotaDimensions", "google.rpc"} {
		if strings.Contains(got, noise) {
			t.Errorf("%q survived into the message: %s", noise, got)
		}
	}
	if strings.Contains(got, "\n") {
		t.Errorf("the message spans lines: %q", got)
	}
}

func TestFailuresAreClassifiedByWhatToDoAboutThem(t *testing.T) {
	cases := []struct{ raw, want string }{
		{gemini429, "rate-limited"},
		{`cursor-agent failed: Error: Authentication required. Please run 'agent login' first, or set CURSOR_API_KEY`, "not signed in"},
		{`gemini failed: Error authenticating: IneligibleTierError: This client is no longer supported`, "not eligible"},
	}
	for _, c := range cases {
		if got := CondenseAgentFailure(c.raw); !strings.Contains(got, c.want) {
			t.Errorf("%.40q\n  got  %q\n  want it to contain %q", c.raw, got, c.want)
		}
	}
	// Something with no recognisable shape is passed through, tidied but not
	// relabelled: inventing a category for an unknown failure is worse than
	// showing it.
	plain := "the launcher could not find its venv"
	if got := CondenseAgentFailure(plain); got != plain {
		t.Errorf("an unclassifiable failure was rewritten: %q", got)
	}
	// What openclaw said when it could not use its working directory. The word
	// quota in it is about a disk, and must not become a billing problem.
	disk := "Reason: spawn /usr/local/bin/node EACCES; snapshot staging root /var/lib/openclaw-home/.cache/openclaw: free disk space/quota or set XDG_CACHE_HOME to a writable filesystem"
	if got := CondenseAgentFailure(disk); strings.Contains(got, "rate-limited") {
		t.Errorf("a filesystem error was read as a rate limit: %q", got)
	}
	for _, q := range []string{`{"error":{"code":"insufficient_quota"}}`, "You exceeded your current quota"} {
		if got := CondenseAgentFailure(q); !strings.Contains(got, "rate-limited") {
			t.Errorf("a real quota error was not recognised: %q", got)
		}
	}
}

// The API says when to try again; that is worth acting on, but only for a wait
// short enough to hold somebody's turn open.
func TestRetryAfterOnlyReportsAWaitWorthHolding(t *testing.T) {
	if got := RetryAfter(gemini429); got < 15*time.Second || got > 16*time.Second {
		t.Errorf("RetryAfter = %v, want the 15s the error asked for", got)
	}
	if got := RetryAfter(`{"retryDelay":"3600s"}`); got != 0 {
		t.Errorf("an hour-long wait was reported as retryable: %v", got)
	}
	if got := RetryAfter("just a plain failure"); got != 0 {
		t.Errorf("a delay was invented for a failure that named none: %v", got)
	}
}

// A single line of JSON is still a wall. The cap has to be on length, not just
// on how many newlines it happens to contain — at both stages.
func TestOneEnormousLineIsStillTrimmed(t *testing.T) {
	got := firstLines(strings.Repeat("x", 40000), 3)
	if n := len([]rune(got)); n > rawFailureRunes+4 {
		t.Errorf("a 40,000-character single line came back as %d runes", n)
	}
	if !strings.HasSuffix(got, "…") {
		t.Error("nothing said it had been cut")
	}
	// And what is shown is far shorter than what is carried.
	if n := len([]rune(CondenseAgentFailure(got))); n > failureDetailRunes+4 {
		t.Errorf("the displayed form is %d runes", n)
	}
}

// The trim must not happen before the condense: the sentence worth quoting and
// the retry delay worth obeying both sit near the end of a Gemini 429, and a
// first pass at display length threw away both — which is how a failure that
// said "retry in 15s" came back in 5 seconds having never retried.
func TestTheRetryHintSurvivesTheWayOut(t *testing.T) {
	carried := firstLines(gemini429, 40)
	if RetryAfter(carried) == 0 {
		t.Error("the retry delay did not survive being carried out of the run")
	}
	if !strings.Contains(CondenseAgentFailure(carried), "You exceeded your current quota") {
		t.Errorf("the API's own sentence did not survive: %q", CondenseAgentFailure(carried))
	}
}
