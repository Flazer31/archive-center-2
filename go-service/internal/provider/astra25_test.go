package provider

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestA25HTTPErrorIsNotSuccessfulGeneration(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/api/chat" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, `{"detail":"upstream unavailable"}`)
	}))
	defer s.Close()
	out, err := (Ollama{URL: s.URL, Model: "mock-only", HTTP: s.Client()}).Chat(context.Background(), "synthetic")
	t.Logf("result=%+v err=%v", out, err)
	if err == nil {
		t.Fatal("HTTP 503 accepted as an empty successful generation")
	}
}
