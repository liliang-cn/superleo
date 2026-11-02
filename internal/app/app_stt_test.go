package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/liliang-cn/superleo/internal/backend"
)

func TestTranscribeRouteSaysUnavailableWhenNothingIsConfigured(t *testing.T) {
	a := &App{svc: &backend.Service{}}
	rec := httptest.NewRecorder()
	a.handleTranscribe(rec, httptest.NewRequest(http.MethodPost, "/api/stt", strings.NewReader("RIFFxxxx")))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
}

func TestTranscribeRouteIsPostOnlyAndWaitsForTheBackend(t *testing.T) {
	a := &App{}
	rec := httptest.NewRecorder()
	a.handleTranscribe(rec, httptest.NewRequest(http.MethodGet, "/api/stt", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	a.handleTranscribe(rec, httptest.NewRequest(http.MethodPost, "/api/stt", strings.NewReader("x")))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST %d", rec.Code)
	}
}

func TestFormatOfContentType(t *testing.T) {
	for in, want := range map[string]string{"audio/wav": "wav", "audio/mp4": "m4a", "audio/mpeg; x=1": "mp3", "": "wav"} {
		if got := formatOf(in); got != want {
			t.Errorf("%q = %q, want %q", in, got, want)
		}
	}
}
