package watchdog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Client calls the app's POST /api/rpc/<Method> with a JSON array of args.
type Client struct {
	Base    string
	Token   string
	Timeout time.Duration
	HTTP    *http.Client
}

// ReadToken reads the "token" field of <home>/auth.json.
func ReadToken(home string) (string, error) {
	b, err := os.ReadFile(filepath.Join(home, "auth.json"))
	if err != nil {
		return "", err
	}
	var a struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(b, &a); err != nil {
		return "", fmt.Errorf("auth.json: not readable")
	}
	if a.Token == "" {
		return "", fmt.Errorf("auth.json: no token")
	}
	return a.Token, nil
}

// Call runs one method. out may be nil.
func (c *Client) Call(ctx context.Context, method string, out any, args ...any) error {
	if args == nil {
		args = []any{}
	}
	body, err := json.Marshal(args)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Base+"/api/rpc/"+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	h := c.HTTP
	if h == nil {
		h = http.DefaultClient
	}
	resp, err := h.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d %s", method, resp.StatusCode, strings.TrimSpace(string(data[:min(len(data), 200)])))
	}
	if out == nil || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	return json.Unmarshal(data, out)
}

// What the API returns, as far as the watchdog reads it.

type cliEvent struct {
	At     time.Time `json:"at"`
	Kind   string    `json:"kind"`
	Text   string    `json:"text"`
	Tool   string    `json:"tool"`
	Detail string    `json:"detail"`
	Failed bool      `json:"failed"`
}

type cliRun struct {
	ID      string     `json:"id"`
	Thread  string     `json:"thread"`
	Agent   string     `json:"agent"`
	Prompt  string     `json:"prompt"`
	Session string     `json:"session"`
	Remote  string     `json:"remote"`
	Paused  string     `json:"paused"`
	State   string     `json:"state"`
	Started time.Time  `json:"started"`
	Tools   int        `json:"tools"`
	Out     int        `json:"out"`
	Events  []cliEvent `json:"events"`
}

type hiveTaskEvent struct {
	At    time.Time `json:"at"`
	Phase string    `json:"phase"`
	Tool  string    `json:"tool"`
}

type hiveTask struct {
	ID        string          `json:"id"`
	Worker    string          `json:"worker"`
	Prompt    string          `json:"prompt"`
	State     string          `json:"state"`
	Phase     string          `json:"phase"`
	Tool      string          `json:"tool"`
	Tools     int             `json:"tools"`
	StartedAt time.Time       `json:"started_at"`
	Run       string          `json:"run"`
	Live      string          `json:"live"`
	Events    []hiveTaskEvent `json:"events"`
}

type longSummary struct {
	TaskID  string `json:"taskId"`
	Running bool   `json:"running"`
	State   string `json:"state"`
}

type longState struct {
	TaskID      string `json:"taskId"`
	Goal        string `json:"goal"`
	StartedAt   string `json:"startedAt"`
	Running     bool   `json:"running"`
	State       string `json:"state"`
	MaxSegments int    `json:"maxSegments"`
	ToolCalls   int    `json:"toolCalls"`
	TotalTokens int    `json:"totalTokens"`
	Rounds      []struct {
		Round int `json:"round"`
	} `json:"rounds"`
	Log []struct {
		At   string `json:"at"`
		Kind string `json:"kind"`
		Text string `json:"text"`
	} `json:"log"`
}

type schedule struct {
	ID       string     `json:"id"`
	Prompt   string     `json:"prompt"`
	Schedule string     `json:"schedule"`
	Note     string     `json:"note"`
	Enabled  bool       `json:"enabled"`
	NextRun  *time.Time `json:"next_run"`
	LastRun  *time.Time `json:"last_run"`
	Running  bool       `json:"running"`
}

type settings struct {
	LLMBaseURL string `json:"llm_base_url"`
	LLMModel   string `json:"llm_model"`
	LLMKey     string `json:"llm_key"`
}
