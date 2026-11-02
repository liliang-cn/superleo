package watchdog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Verdict values.
const (
	OnTrack  = "on_track"
	OffTrack = "off_track"
	Unclear  = "unclear"
)

// Judge decides whether a run's last steps still serve its goal.
type Judge interface {
	Verdict(ctx context.Context, goal string, steps []string) (verdict, reason string, err error)
}

// ChatJudge asks an OpenAI-compatible /chat/completions endpoint.
type ChatJudge struct {
	// Settings reads the app's llm_* settings when the config names none.
	Settings func(ctx context.Context) (settings, error)
	Cfg      Config
	HTTP     *http.Client
}

const judgePrompt = `你是任务监督员。下面是一个任务的目标和它最近的几步。判断这些步骤是否仍在朝目标推进。
只回答一行 JSON：{"verdict":"on_track|off_track|unclear","reason":"一句话原因"}。
on_track=在推进目标；off_track=明显偏离、在做无关的事或原地打转；unclear=信息不足。`

func (j *ChatJudge) Verdict(ctx context.Context, goal string, steps []string) (string, string, error) {
	base, model, key := j.Cfg.JudgeBaseURL, j.Cfg.JudgeModel, j.Cfg.JudgeKey
	if base == "" || model == "" || key == "" {
		s, err := j.Settings(ctx)
		if err != nil {
			return "", "", fmt.Errorf("settings: %w", err)
		}
		base, model, key = firstNonEmpty(base, s.LLMBaseURL), firstNonEmpty(model, s.LLMModel), firstNonEmpty(key, s.LLMKey)
	}
	if base == "" || model == "" {
		return "", "", errors.New("no judge model configured")
	}
	user := "目标：\n" + trunc(goal, 1500) + "\n\n最近的步骤：\n" + strings.Join(steps, "\n")
	body, _ := json.Marshal(map[string]any{
		"model": model, "temperature": 0, "max_tokens": 800,
		"messages": []map[string]string{{"role": "system", "content": judgePrompt}, {"role": "user", "content": user}},
	})
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", "", errors.New("bad judge url")
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	h := j.HTTP
	if h == nil {
		h = http.DefaultClient
	}
	resp, err := h.Do(req)
	if err != nil {
		return "", "", errors.New("judge request failed")
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("judge HTTP %d", resp.StatusCode)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(data, &out) != nil || len(out.Choices) == 0 {
		return "", "", errors.New("judge answer not understood")
	}
	v, r := parseVerdict(out.Choices[0].Message.Content)
	return v, r, nil
}

// parseVerdict finds the JSON object in a model's answer, falling back to the
// first verdict word in it.
func parseVerdict(s string) (verdict, reason string) {
	if i, k := strings.Index(s, "{"), strings.LastIndex(s, "}"); i >= 0 && k > i {
		var o struct{ Verdict, Reason string }
		if json.Unmarshal([]byte(s[i:k+1]), &o) == nil {
			if v := normVerdict(o.Verdict); v != "" {
				return v, strings.TrimSpace(o.Reason)
			}
		}
	}
	low := strings.ToLower(s)
	for _, v := range []string{OffTrack, OnTrack, Unclear} {
		if strings.Contains(low, v) {
			return v, ""
		}
	}
	return Unclear, ""
}

func normVerdict(v string) string {
	v = strings.ToLower(strings.TrimSpace(strings.ReplaceAll(v, " ", "_")))
	v = strings.ReplaceAll(v, "-", "_")
	switch v {
	case OnTrack, OffTrack, Unclear:
		return v
	}
	return ""
}

func firstNonEmpty(a ...string) string {
	for _, s := range a {
		if s != "" {
			return s
		}
	}
	return ""
}

func trunc(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) > n {
		return string(r[:n]) + "..."
	}
	return string(r)
}
