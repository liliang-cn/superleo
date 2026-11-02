package backend

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func sttService(base, api string) *Service {
	return &Service{settings: &Settings{LLMBaseURL: base, LLMKey: "k1", STTModel: "m-stt", STTAPI: api}}
}

func TestRecognitionWithNoModelIsAStateNotAFailure(t *testing.T) {
	svc := &Service{settings: &Settings{LLMBaseURL: "https://gw.example/v1", LLMKey: "k"}}
	_, err := svc.Transcribe(context.Background(), []byte("RIFF"), "wav", "zh")
	if !errors.Is(err, ErrNoRecognition) {
		t.Fatalf("got %v", err)
	}
	if _, err := (*Service)(nil).Transcribe(context.Background(), []byte("x"), "wav", ""); !errors.Is(err, ErrNoRecognition) {
		t.Fatalf("nil service: %v", err)
	}
}

func TestRecognitionInheritsTheBrainsEndpoint(t *testing.T) {
	s := &Settings{LLMBaseURL: "https://gw.example/v1", LLMKey: "abc", STTModel: "m"}
	base, key, model, api, ok := s.Recognition()
	if !ok || base != "https://gw.example/v1" || key != "abc" || model != "m" || api != "transcriptions" {
		t.Fatalf("%q %q %q %q %v", base, key, model, api, ok)
	}
	s.STTAPI = "CHAT"
	if _, _, _, api, _ := s.Recognition(); api != "chat" {
		t.Fatalf("api %q", api)
	}
}

func TestTranscriptionsProtocolSendsTheFileModelLanguageAndHint(t *testing.T) {
	var gotAuth, gotModel, gotLang, gotPrompt, gotName string
	var gotBytes int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/transcriptions" {
			t.Errorf("path %s", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Error(err)
			return
		}
		gotModel, gotLang, gotPrompt = r.FormValue("model"), r.FormValue("language"), r.FormValue("prompt")
		f, h, err := r.FormFile("file")
		if err != nil {
			t.Error(err)
			return
		}
		b, _ := io.ReadAll(f)
		gotBytes, gotName = len(b), h.Filename
		_, _ = w.Write([]byte(`{"text":" 部署 Kubernetes 集群 "}`))
	}))
	defer srv.Close()
	text, err := sttService(srv.URL+"/v1", "").Transcribe(context.Background(), []byte("0123456789"), "wav", "")
	if err != nil {
		t.Fatal(err)
	}
	if text != "部署 Kubernetes 集群" {
		t.Errorf("text %q", text)
	}
	if gotAuth != "Bearer k1" || gotModel != "m-stt" || gotLang != "zh" || gotBytes != 10 || gotName != "speech.wav" {
		t.Errorf("auth %q model %q lang %q bytes %d name %q", gotAuth, gotModel, gotLang, gotBytes, gotName)
	}
	if !strings.Contains(gotPrompt, "Kubernetes") {
		t.Errorf("hint %q", gotPrompt)
	}
}

func TestChatProtocolPutsTheAudioInTheMessage(t *testing.T) {
	var body struct {
		Model    string `json:"model"`
		Messages []struct {
			Content []struct {
				Type       string `json:"type"`
				InputAudio struct {
					Data   string `json:"data"`
					Format string `json:"format"`
				} `json:"input_audio"`
			} `json:"content"`
		} `json:"messages"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path %s", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"今天天气不错。"}}]}`))
	}))
	defer srv.Close()
	text, err := sttService(srv.URL+"/v1", "chat").Transcribe(context.Background(), []byte("abc"), "wav", "zh")
	if err != nil || text != "今天天气不错。" {
		t.Fatalf("%q %v", text, err)
	}
	parts := body.Messages[0].Content
	if body.Model != "m-stt" || len(parts) != 2 || parts[1].Type != "input_audio" ||
		parts[1].InputAudio.Format != "wav" || parts[1].InputAudio.Data != "YWJj" {
		t.Fatalf("body %+v", body)
	}
}

func TestAProviderErrorCarriesItsOwnExplanation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"model not found"}`, http.StatusNotFound)
	}))
	defer srv.Close()
	_, err := sttService(srv.URL+"/v1", "").Transcribe(context.Background(), []byte("x"), "wav", "zh")
	if err == nil || !strings.Contains(err.Error(), "model not found") || errors.Is(err, ErrNoRecognition) {
		t.Fatalf("%v", err)
	}
}

func TestEmptyAudioIsRefused(t *testing.T) {
	if _, err := sttService("https://x/v1", "").Transcribe(context.Background(), nil, "wav", ""); err == nil {
		t.Fatal("empty audio accepted")
	}
}
