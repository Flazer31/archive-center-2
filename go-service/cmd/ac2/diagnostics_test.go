package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"archivecenter2/goservice/internal/provider"
	"archivecenter2/goservice/internal/store"
)

func TestDiagnosticSettingsProductionAllowlist(t *testing.T) {
	db := modelsDB(t)
	if err := db.SetCriticPrompt("private-prompt-original"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetMemorySettings(1234, 4567, "auto", "private-anchor-text", "google_pdf"); err != nil {
		t.Fatal(err)
	}
	m := &models{db: db, saved: store.ModelSettings{CriticSet: true, Provider: "openai", Model: "test-critic", URL: "https://private-url", Key: "private-critic-key", EmbedProvider: "voyageai", EmbedModel: "test-embed", EmbedKey: "private-embed-key", EmbedURL: "private-embed-url", Retries: 5, KnownChars: 9876, Options: provider.Options{ExtraHeadersJSON: `{"Authorization":"private-extra-key"}`, ExtraBodyJSON: `{"messages":["private-conversation-text"]}`}}}
	v := m.diagnosticSettings()
	if v.MemoryChars != 1234 || v.SecretChars != 4567 || v.Transport != "google_pdf" || v.Retries != 5 || v.KnownChars != 9876 || !v.CriticKeyConfigured || !v.EmbeddingKeyConfigured {
		t.Fatalf("safe settings not preserved %+v", v)
	}
	encoded, _ := json.Marshal(v)
	for _, value := range []string{"private-prompt-original", "private-anchor-text", "private-url", "private-critic-key", "private-embed-key", "private-embed-url", "private-extra-key", "private-conversation-text", "key_last4", "extra_body_json", "extra_headers_json"} {
		if strings.Contains(string(encoded), value) {
			t.Fatalf("forbidden setting in diagnostic projection %q", value)
		}
	}
}

func TestDiagnosticFailuresExcludeChatAndInjectionContents(t *testing.T) {
	s := &status{events: []event{{At: time.Now(), Kind: "failed", Chat: "private-chat-label", Turn: 7, Detail: "provider 500: private-upstream-echo"}, {At: time.Now(), Kind: "injected", Chat: "private-chat-label", Detail: "private-memory-content"}}, last: map[string]*injection{"private-chat-label": {Block: "private-injection-block", ReadError: "private-read-items"}}}
	failures := s.diagnosticFailures()
	if len(failures) != 1 || failures[0].Kind != "failed" {
		t.Fatal("failure projection lost known failure")
	}
	encoded, _ := json.Marshal(failures)
	for _, marker := range []string{"private-chat-label", "private-memory-content", "private-injection-block", "private-read-items"} {
		if strings.Contains(string(encoded), marker) {
			t.Fatalf("failure projection leaked %q", marker)
		}
	}
}
