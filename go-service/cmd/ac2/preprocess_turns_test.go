package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"archivecenter2/goservice/internal/ingest"
	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/provider"
	"archivecenter2/goservice/internal/store"
)

// The decision model reads the last two turns of the request's window; the
// preprocessing AI reads the whole window, as before (1.1 C1). Five finished
// turns, the request holds turns 2~5 (window 4), the
// memories are from turn 1. JEV asks the basic delivery and the rest of the
// wide list in requests of their own (every request is checked); AI + JEV
// asks the AI too.
func TestDecisionReadsLastTwoTurns(t *testing.T) {
	for _, mode := range []string{store.PreprocessJev, store.PreprocessAIJev} {
		t.Run(mode, func(t *testing.T) { decisionTurnsCase(t, mode) })
	}
}

func decisionTurnsCase(t *testing.T, mode string) {
	h := hudFixture(t, "", 0)
	chat := "T"
	var rows []string
	for i := 0; i < 5; i++ {
		rows = append(rows, memRow(2*i, "user", fmt.Sprintf("%s-u%d", chat, i), "", fmt.Sprintf("입력 %d번째", i)), memRow(2*i+1, "char", fmt.Sprintf("%s-a%d", chat, i), "g", fmt.Sprintf("응답 %d번째", i)))
	}
	rows = append(rows, memRow(10, "user", chat+"-current", "", "리나의 약속과 생각"))
	if _, err := h.pipe.Process([]observe.Record{memEvent(1, "beforeRequest", chat, rows...)}); err != nil {
		t.Fatal(err)
	}
	var scope []memory.ScopedTurn
	if err := h.pipe.WithSession("chat:"+chat, func(s *lifecycle.Session) { scope = memory.ScopeOf(s) }); err != nil {
		t.Fatal(err)
	}
	injectionSave(t, h.db, scope[0].Source, `{"entities":[{"id":"n1","name":"리나"}],"memories":[{"id":"m1","kind":"fact","about":"n1","slot":"약속","mode":"상태","body":"리나는 해 지기 전에 돌아오겠다고 약속했다"},{"id":"m2","kind":"fact","about":"n1","slot":"일","mode":"상태","body":"리나는 마을 약국에서 약초를 말리며 일한다"},{"id":"m3","kind":"fact","about":"n1","slot":"취미","mode":"성향","body":"리나는 강가에서 낚시하는 것을 좋아한다"}]}`)
	srv := observe.NewServer(store.RawSink{DB: h.db})
	srv.After = h.pipe
	prints := []store.TextPrint{}
	for i := 1; i < 5; i++ {
		for _, text := range []string{fmt.Sprintf("입력 %d번째", i), fmt.Sprintf("응답 %d번째", i)} {
			n, hash := ingest.Fingerprint(text)
			prints = append(prints, store.TextPrint{Len: n, H: hash})
		}
	}
	n, hash := ingest.Fingerprint("리나의 약속과 생각")
	prints = append(prints, store.TextPrint{Len: n, H: hash})
	body, _ := json.Marshal(map[string]any{"chat_id": chat, "input": "리나의 약속과 생각", "payload": prints})
	inject := func() {
		t.Helper()
		a := callMem(t, injectHandler(srv, h.pipe, h.db, func() *provider.Embedder { return nil }, h.st), "POST", "/memory/inject", string(body))
		if a.Code != 200 {
			t.Fatalf("inject: %d %s", a.Code, a.Raw)
		}
	}
	// room for the largest record alone: the others are the rest of the
	// wide list (sizes from a delivery without preprocessing)
	inject()
	largest := 0
	for _, r := range lastTrace(t, h, chat).Trace {
		largest = max(largest, r.Size)
	}
	pm, marker, err := h.db.MemoryPlacement()
	if err != nil {
		t.Fatal(err)
	}
	if err := h.db.SetMemorySettings(largest, 4000, pm, marker); err != nil {
		t.Fatal(err)
	}
	f := newPrepFakes(t)
	prepSet(t, h, f, mode)
	inject()
	tr := lastTrace(t, h, chat).Summary
	if tr.Window != 4 {
		t.Fatalf("fixture: window %d, want 4", tr.Window)
	}
	f.mu.Lock()
	decs, ai, requests := append([]string(nil), f.decStates...), f.aiPrompt, len(f.decRequests)
	f.mu.Unlock()
	if mode == store.PreprocessJev && requests < 2 || len(decs) != requests {
		t.Fatalf("fixture: %d decision requests (%d states); JEV asks the basic lines and the rest apart", requests, len(decs))
	}
	for _, dec := range decs {
		for _, s := range []string{"리나의 약속과 생각", "입력 3번째", "응답 3번째", "입력 4번째", "응답 4번째", "직전 2턴"} {
			if !strings.Contains(dec, s) {
				t.Fatalf("decision state lacks %q: %q", s, dec)
			}
		}
		for _, s := range []string{"입력 1번째", "입력 2번째"} {
			if strings.Contains(dec, s) {
				t.Fatalf("decision state has %q beyond the last two turns: %q", s, dec)
			}
		}
	}
	if mode != store.PreprocessAIJev {
		return
	}
	if ai == "" {
		t.Fatal("fixture: no AI prompt")
	}
	for _, s := range []string{"입력 1번째", "입력 4번째", "직전 4턴"} {
		if !strings.Contains(ai, s) {
			t.Fatalf("ai prompt lacks %q (the whole window): %q", s, ai)
		}
	}
}
