package app

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/liliang-cn/superleo/internal/backend"
)

// The recognition route, next to /api/tts and gated the same way: it spends
// someone's money per call. POST the recording as the body (Content-Type
// audio/wav, audio/mp4, audio/mpeg); ?language=zh optional. Answers
// {"text": "…"}; 501 when this install has no recognition model, which the
// phone reads as "keep the text you already have".

func (a *App) handleTranscribe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	audio, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 20<<20))
	if err != nil {
		writeJSONStatus(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "recording too large"})
		return
	}
	a.mu.Lock()
	svc := a.svc
	a.mu.Unlock()
	if svc == nil {
		writeJSONStatus(w, http.StatusServiceUnavailable, map[string]any{"error": "backend not ready"})
		return
	}
	format := r.URL.Query().Get("format")
	if format == "" {
		format = formatOf(r.Header.Get("Content-Type"))
	}
	text, err := svc.Transcribe(r.Context(), audio, format, r.URL.Query().Get("language"))
	if err != nil {
		if errors.Is(err, backend.ErrNoRecognition) {
			writeJSONStatus(w, http.StatusNotImplemented, map[string]any{"error": err.Error()})
			return
		}
		writeJSONStatus(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSONStatus(w, http.StatusOK, map[string]any{"text": text})
}

func formatOf(contentType string) string {
	ct := strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))
	switch ct {
	case "audio/mpeg", "audio/mp3":
		return "mp3"
	case "audio/mp4", "audio/m4a", "audio/x-m4a", "audio/aac":
		return "m4a"
	case "audio/ogg":
		return "ogg"
	case "audio/flac":
		return "flac"
	}
	return "wav"
}
