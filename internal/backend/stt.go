package backend

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"time"
)

// Turning a recording into text, for the phone's 「服务器」 recognition mode.
//
// The phone has already shown live text from the device while the user spoke;
// this is the better pass that replaces it when they stop. So it is bounded
// and quiet when it fails: the phone keeps its own text on any error.

// ErrNoRecognition is returned when no recognition model is configured; like
// ErrNoSpeech it is a state, not a fault.
var ErrNoRecognition = errors.New("no speech recognition model configured")

const (
	recognitionTimeout = 60 * time.Second
	// sttMaxAudio bounds one upload: minutes of 16 kHz mono WAV.
	sttMaxAudio = 20 << 20
)

// sttPrompt is the hint that goes with every recording: Mandarin, with the
// English words of a technical conversation left in English.
const sttPrompt = "普通话，夹杂英文技术词汇（如 Kubernetes、Docker、API、GitHub、Claude、SuperLeo）。英文词保持英文原样，使用简体中文和标准标点。"

// Transcribe returns the text of a recording. format is the audio container's
// short name ("wav", "mp3", "m4a"); language an ISO code such as "zh".
func (s *Service) Transcribe(ctx context.Context, audio []byte, format, language string) (string, error) {
	if len(audio) == 0 {
		return "", errors.New("no audio")
	}
	if len(audio) > sttMaxAudio {
		return "", fmt.Errorf("recording too large: %d bytes, limit %d", len(audio), sttMaxAudio)
	}
	if s == nil || s.settings == nil {
		return "", ErrNoRecognition
	}
	baseURL, key, model, api, ok := s.settings.Recognition()
	if !ok {
		return "", ErrNoRecognition
	}
	format = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(format), "."))
	if format == "" {
		format = "wav"
	}
	if language == "" {
		language = "zh"
	}
	hint := firstNonEmpty(s.settings.STTPrompt, sttPrompt)

	ctx, cancel := context.WithTimeout(ctx, recognitionTimeout)
	defer cancel()

	var req *http.Request
	var err error
	base := strings.TrimRight(baseURL, "/")
	if api == "chat" {
		req, err = chatTranscriptionRequest(ctx, base, model, hint, audio, format)
	} else {
		req, err = multipartTranscriptionRequest(ctx, base, model, hint, language, audio, format)
	}
	if err != nil {
		return "", err
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(raw))
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return "", fmt.Errorf("recognition: %s: %s", resp.Status, msg)
	}
	text, err := transcriptFrom(raw, api)
	if err != nil {
		return "", err
	}
	return text, nil
}

func multipartTranscriptionRequest(ctx context.Context, base, model, hint, language string, audio []byte, format string) (*http.Request, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("model", model)
	_ = w.WriteField("language", language)
	_ = w.WriteField("prompt", hint)
	_ = w.WriteField("response_format", "json")
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="speech.%s"`, format))
	h.Set("Content-Type", audioMIME(format))
	part, err := w.CreatePart(h)
	if err != nil {
		return nil, err
	}
	if _, err := part.Write(audio); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/audio/transcriptions", &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	return req, nil
}

func chatTranscriptionRequest(ctx context.Context, base, model, hint string, audio []byte, format string) (*http.Request, error) {
	payload := map[string]any{
		"model": model,
		"messages": []any{map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "text", "text": "逐字转写这段音频，只输出转写出的文字，不要解释，不要加引号。" + hint},
				map[string]any{"type": "input_audio", "input_audio": map[string]any{
					"data": base64.StdEncoding.EncodeToString(audio), "format": format,
				}},
			},
		}},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}

// transcriptFrom reads the text out of either protocol's reply.
func transcriptFrom(raw []byte, api string) (string, error) {
	if api == "chat" {
		var r struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(raw, &r); err != nil || len(r.Choices) == 0 {
			return "", errors.New("recognition: unreadable reply")
		}
		return strings.TrimSpace(r.Choices[0].Message.Content), nil
	}
	var r struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		// Some servers answer response_format=json with bare text.
		if t := strings.TrimSpace(string(raw)); t != "" && !strings.HasPrefix(t, "{") {
			return t, nil
		}
		return "", errors.New("recognition: unreadable reply")
	}
	return strings.TrimSpace(r.Text), nil
}

func audioMIME(format string) string {
	switch format {
	case "mp3":
		return "audio/mpeg"
	case "m4a", "aac", "mp4":
		return "audio/mp4"
	case "ogg":
		return "audio/ogg"
	case "flac":
		return "audio/flac"
	}
	return "audio/wav"
}
