package main

import (
	"fmt"
	"testing"

	"archivecenter2/goservice/internal/provider"
	"archivecenter2/goservice/internal/store"
)

// The settings views say what the saved choice lacks, as a turn would find
// it (1.1): a preprocessing mode without its connection, a publisher on its
// own setting without a provider; nothing when off or set.
func TestSettingsViewProblem(t *testing.T) {
	h := hudFixture(t, "", 0)
	m := h.st.mods
	problem := func(path string) string {
		t.Helper()
		var a memAnswer
		if path == "/settings/preprocess" {
			a = callMem(t, preprocessHandler(m), "GET", path, "")
		} else {
			a = callMem(t, narrativeHandler(m), "GET", path, "")
		}
		if a.Code != 200 {
			t.Fatalf("%s: %d %s", path, a.Code, a.Raw)
		}
		p, ok := a.V["problem"].(string)
		if !ok {
			t.Fatalf("%s: no problem field: %s", path, a.Raw)
		}
		return p
	}
	if p := problem("/settings/preprocess"); p != "" {
		t.Fatalf("off: %q", p)
	}
	if err := h.db.SetPreprocessSettings(m.box, store.PreprocessSettings{Mode: store.PreprocessJev}); err != nil {
		t.Fatal(err)
	}
	if p := problem("/settings/preprocess"); p != "decision_off" {
		t.Fatalf("jev without a decision model: %q", p)
	}
	if err := h.db.SetPreprocessSettings(m.box, store.PreprocessSettings{Mode: store.PreprocessAI, AIOwn: true}); err != nil {
		t.Fatal(err)
	}
	if p := problem("/settings/preprocess"); p != "ai_off" {
		t.Fatalf("own AI without a provider: %q", p)
	}
	// AI + decision model lacking both: both are named, the decision model first
	if err := h.db.SetPreprocessSettings(m.box, store.PreprocessSettings{Mode: store.PreprocessAIJev, AIOwn: true}); err != nil {
		t.Fatal(err)
	}
	if a := callMem(t, preprocessHandler(m), "GET", "/settings/preprocess", ""); fmt.Sprint(a.V["problems"]) != "[decision_off ai_off]" || a.V["problem"] != "decision_off" {
		t.Fatalf("both lacking: %s", a.Raw)
	}
	if p := problem("/settings/narrative"); p != "" {
		t.Fatalf("publisher off: %q", p)
	}
	if err := h.db.SetNarrativeSettings(m.box, store.NarrativeSettings{Level: store.GuideWeak, Chars: store.DefaultGuideChars, AIOwn: true}); err != nil {
		t.Fatal(err)
	}
	if p := problem("/settings/narrative"); p != "ai_off" {
		t.Fatalf("own publisher without a provider: %q", p)
	}
	// borrowing the critic: a warning while the critic is unset, none once set
	if err := h.db.SetPreprocessSettings(m.box, store.PreprocessSettings{Mode: store.PreprocessAI}); err != nil {
		t.Fatal(err)
	}
	if err := h.db.SetNarrativeSettings(m.box, store.NarrativeSettings{Level: store.GuideWeak, Chars: store.DefaultGuideChars}); err != nil {
		t.Fatal(err)
	}
	if p, q := problem("/settings/preprocess"), problem("/settings/narrative"); p != "ai_off" || q != "ai_off" {
		t.Fatalf("borrowing an unset critic: %q %q", p, q)
	}
	m.flagCritic = provider.Config{Provider: "ollama", Model: "fake", URL: "http://127.0.0.1:1"}
	if p, q := problem("/settings/preprocess"), problem("/settings/narrative"); p != "" || q != "" {
		t.Fatalf("set: %q %q", p, q)
	}
	// an unknown provider is named by the backend's own code
	if err := h.db.SetPreprocessSettings(m.box, store.PreprocessSettings{Mode: store.PreprocessAI, AIOwn: true, AI: provider.Config{Provider: "nope", Model: "x"}}); err == nil {
		if p := problem("/settings/preprocess"); p == "" {
			t.Fatal("unknown provider: no problem")
		}
	}
}

// AI + JEV with one side not set runs the other and records why the first
// did not run as not set ("ai_off"), not as a failed call (1.1).
func TestPreprocessHalfUnsetIsNotFailure(t *testing.T) {
	h := hudFixture(t, "", 0)
	srv := narrSeed(t, h, "H")
	f := newPrepFakes(t)
	if err := h.db.SetDecisionSettings(h.st.mods.box, store.DecisionSettings{Provider: "ollama", URL: f.dec.URL}); err != nil {
		t.Fatal(err)
	}
	if err := h.db.SetPreprocessSettings(h.st.mods.box, store.PreprocessSettings{Mode: store.PreprocessAIJev, AIOwn: true}); err != nil {
		t.Fatal(err)
	}
	injectionRequest(t, srv, h, "H")
	p := lastTrace(t, h, "H").Summary.Preprocess
	if p == nil || p.Ran != store.PreprocessJev || p.Fallback != "ai_off" {
		t.Fatalf("record: %+v", p)
	}
}
