package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"archivecenter2/goservice/internal/provider"
	"archivecenter2/goservice/internal/store"
)

// The critic's connection (memory generation, and the AI preprocessing
// borrows it) does not follow a redirect: the key and the story go only to
// the address set (user 11-08 "평론가도 막기"). Both the saved connection
// and the flags.
func TestCriticDoesNotFollowRedirect(t *testing.T) {
	calls := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/v1/chat/completions", http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	m := &models{flagCritic: provider.Config{Provider: "custom", Model: "fake", URL: redirect.URL + "/v1", Key: "synthetic-critic-key"}}
	for _, tc := range []struct {
		name string
		s    store.ModelSettings
	}{
		{"flags", store.ModelSettings{}},
		{"saved", store.ModelSettings{CriticSet: true, Provider: "custom", Model: "fake", URL: redirect.URL + "/v1", Key: "synthetic-critic-key"}},
	} {
		cfg, _ := m.critic(tc.s)
		c, err := provider.New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		calls = 0
		_, err = c.Chat(context.Background(), "기억을 만들어라")
		if err == nil || calls != 0 || strings.Contains(err.Error(), "synthetic-critic-key") {
			t.Fatalf("%s: redirect followed (calls %d, err %v)", tc.name, calls, err)
		}
	}
	// a connection with no redirect still answers
	direct := &models{flagCritic: provider.Config{Provider: "custom", Model: "fake", URL: target.URL + "/v1"}}
	cfg, _ := direct.critic(store.ModelSettings{})
	c, _ := provider.New(cfg)
	if r, err := c.Chat(context.Background(), "기억을 만들어라"); err != nil || r.Content != "ok" {
		t.Fatalf("direct: %v %v", r, err)
	}
}
