package provider

import (
	"context"
	"os"
	"testing"
)

// A real call through the local Ollama signed in to ollama.com (clef:cloud),
// run only with AC2_LIVE_OLLAMA=1: one request, two questions.
func TestDecisionLiveOllama(t *testing.T) {
	if os.Getenv("AC2_LIVE_OLLAMA") != "1" {
		t.Skip("AC2_LIVE_OLLAMA not set")
	}
	d, err := NewDecider(DecisionConfig{Provider: "ollama"})
	if err != nil {
		t.Fatal(err)
	}
	a, err := d.Ask(context.Background(), "리나가 약국 문을 열고 들어왔다. 도윤이 계산대 뒤에서 약을 정리하고 있었다.", []Question{
		{ID: "here", Text: "도윤이 지금 장면에 있는가?", Yes: "장면에 있다", No: "장면에 없다"},
		{ID: "sea", Text: "지금 장면이 바다 위 배 안인가?", Yes: "배 안이다", No: "배 안이 아니다"},
	})
	if err != nil || a.Calls != 1 || len(a.Yes) != 2 {
		t.Fatalf("live: %+v %v", a, err)
	}
	t.Logf("model %s tokens %d here=%.3f sea=%.3f", a.Model, a.InputTokens, a.Yes["here"], a.Yes["sea"])
	if a.Yes["here"] <= a.Yes["sea"] {
		t.Fatalf("the present person should be likelier than the wrong place: %v", a.Yes)
	}
}
