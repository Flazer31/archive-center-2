package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"archivecenter2/goservice/internal/ingest"
	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/provider"
	"archivecenter2/goservice/internal/store"
)

func injectionSeed(t *testing.T, h *hud, chat string) ([]memory.ScopedTurn, []string) {
	t.Helper()
	rows := []string{}
	for i := 0; i < 3; i++ {
		rows = append(rows, memRow(2*i, "user", fmt.Sprintf("%s-u%d", chat, i), "", fmt.Sprintf("입력 %d", i)), memRow(2*i+1, "char", fmt.Sprintf("%s-a%d", chat, i), "g", fmt.Sprintf("응답 %d", i)))
	}
	rows = append(rows, memRow(6, "user", chat+"-current", "", "리나의 약속과 생각"))
	if _, err := h.pipe.Process([]observe.Record{memEvent(1, "beforeRequest", chat, rows...)}); err != nil {
		t.Fatal(err)
	}
	var scope []memory.ScopedTurn
	if err := h.pipe.WithSession("chat:"+chat, func(s *lifecycle.Session) { scope = memory.ScopeOf(s) }); err != nil {
		t.Fatal(err)
	}
	return scope, rows
}

func injectionSave(t *testing.T, db *store.DB, src memory.Source, raw string) map[string]string {
	t.Helper()
	j, err := db.JobFor(src)
	if err != nil {
		t.Fatal(err)
	}
	out, notes := memory.Parse(raw)
	if len(notes) > 0 {
		t.Fatal(notes)
	}
	if _, err := db.SaveGeneration(j, store.Generation{Request: "synthetic", ReadItems: "[]", Response: raw, Output: out}); err != nil {
		t.Fatal(err)
	}
	ids, err := db.ItemIDs(j.ID)
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

func injectionRequest(t *testing.T, srv *observe.Server, h *hud, chat string) memAnswer {
	t.Helper()
	prints := []store.TextPrint{}
	for _, text := range []string{"입력 2", "응답 2", "리나의 약속과 생각"} {
		n, hash := ingest.Fingerprint(text)
		prints = append(prints, store.TextPrint{Len: n, H: hash})
	}
	body, err := json.Marshal(map[string]any{"chat_id": chat, "input": "리나의 약속과 생각", "payload": prints})
	if err != nil {
		t.Fatal(err)
	}
	a := callMem(t, injectHandler(srv, h.pipe, h.db, func() *provider.Embedder { return nil }, h.st), "POST", "/memory/inject", string(body))
	if a.Code != 200 {
		t.Fatalf("inject: %d %s", a.Code, a.Raw)
	}
	return a
}

func injectionGet(t *testing.T, h *hud, chat string) memAnswer {
	t.Helper()
	a := callMem(t, h.st.injectionHandler(), "GET", "/injection?chat_id="+chat, "")
	if a.Code != 200 {
		t.Fatalf("injection view: %d %s", a.Code, a.Raw)
	}
	return a
}

// Real MariaDB, lifecycle, selector and both production HTTP handlers. No
// generation/embedding provider is called. Expected bodies/types come from
// the synthetic generation input, and order comes from the delivered IDs.
func TestInjectionSnapshot(t *testing.T) {
	h := hudFixture(t, "", 0)
	scope, _ := injectionSeed(t, h, "A")
	raw := `{"entities":[{"id":"n1","name":"리나"},{"id":"n2","name":"도윤"}],"memories":[{"id":"m1","kind":"event","body":"아무에게도 알리지 않은 만남","hide_from":["n2"]},{"id":"m2","kind":"fact","about":"n1","mode":"rule","slot":"약속","body":"약속은 해가 지기 전에 지킨다"},{"id":"m3","kind":"view","owner":"n1","about":"n2","aspect":"belief","body":"도윤은 만남을 모른다고 생각한다","basis":{"ref":"m1"}}],"scenes":[{"id":"s1","change":"open"}]}`
	want, notes := memory.Parse(raw)
	if len(notes) > 0 {
		t.Fatal(notes)
	}
	ids := injectionSave(t, h.db, scope[0].Source, raw)
	// The previous turn is complete, but its memory is already in payload.
	injectionSave(t, h.db, scope[2].Source, `{"memories":[{"id":"m1","kind":"fact","body":"최근 문맥에 이미 있는 내용"}]}`)
	srv := observe.NewServer(store.RawSink{DB: h.db})
	srv.After = h.pipe
	delivered := injectionRequest(t, srv, h, "A")
	a := injectionGet(t, h, "A")
	if a.V["none"] != false || a.V["block"] == "" || a.V["block"] != delivered.V["block"] || a.V["read_error"] != nil {
		t.Fatalf("not the delivered block: %s / %s", a.Raw, delivered.Raw)
	}
	for _, key := range []string{"chars", "secret_chars", "ranked", "warning", "total_ms"} {
		if a.V[key] != delivered.V[key] {
			t.Fatalf("%s mismatch: %v / %v", key, a.V[key], delivered.V[key])
		}
	}
	chars, secret, err := h.db.MemoryBudgets()
	if err != nil || a.V["chars_max"] != float64(chars) || a.V["secret_max"] != float64(secret) || a.V["at"] == nil {
		t.Fatalf("missing budget/time: %s (%v)", a.Raw, err)
	}
	mems := a.V["memories"].([]any)
	chosen := delivered.V["ids"].([]any)
	if len(mems) != len(want.Memories) || len(mems) != len(chosen) || a.V["items"] != float64(len(mems)) {
		t.Fatalf("missing/extra records: %s", a.Raw)
	}
	byID := map[string]memory.Memory{}
	for i, m := range want.Memories {
		byID[ids[fmt.Sprintf("memories/%d", i)]] = m
	}
	for i, row := range mems {
		m := row.(map[string]any)
		id := chosen[i].(string)
		original, ok := byID[id]
		if !ok || m["id"] != id || m["body"] != original.Body || m["kind"] != original.Kind || m["turn"] != float64(1) || m["inherited"] != false {
			t.Fatalf("record/order mismatch: %v / %+v", m, original)
		}
		if original.Mode != "" && m["mode"] != original.Mode || original.Aspect != "" && m["aspect"] != original.Aspect {
			t.Fatalf("subtype missing: %v", m)
		}
		// The view quotes a protected basis and must use the secret lane too.
		if m["secret"] != (original.Kind == "event" || original.Kind == "view") {
			t.Fatalf("secret marking differs from delivered text: %v", m)
		}
		if original.Kind == "view" && (m["owner"] != "리나" || m["about"] != "도윤") || original.Kind == "fact" && m["about"] != "리나" {
			t.Fatalf("unresolved name: %v", m)
		}
	}
	gen := a.V["generation"].(map[string]any)
	previous := hudStageView(t, hudTurnView(t, hudGet(t, h, "A"), scope[2].Source.Turn), "generation")
	for _, key := range []string{"state", "count", "detail", "at", "elapsed_ms"} {
		if gen[key] != previous[key] {
			t.Fatalf("not the HUD generation projection: %v / %v", gen, previous)
		}
	}
	if gen["state"] != "done" || gen["count"] != float64(1) || gen["turn"] != float64(scope[2].Source.Turn) {
		t.Fatalf("previous turn not done: %v", gen)
	}
	if other := injectionGet(t, h, "OTHER"); other.V["none"] != true || other.V["block"] != nil || other.V["memories"] != nil || other.V["generation"] != nil {
		t.Fatalf("cross-chat leak: %s", other.Raw)
	}
	for _, response := range []memAnswer{callMem(t, h.st.handler(), "GET", "/status?chat_id=A", ""), callMem(t, h.handler(), "GET", "/hud?chat_id=A", "")} {
		if response.Code != 200 || strings.Contains(response.Raw, want.Memories[0].Body) || strings.Contains(response.Raw, `"memories"`) || strings.Contains(response.Raw, `"block"`) {
			t.Fatalf("detailed memory escaped to status/HUD: %s", response.Raw)
		}
	}
	// Editing the database cannot rewrite a historical delivery snapshot.
	body := "수정 후 약속"
	if err := h.db.EditMemory("chat:A", ids["memories/1"], &body, nil); err != nil {
		t.Fatal(err)
	}
	if after := injectionGet(t, h, "A"); after.V["block"] != a.V["block"] || strings.Contains(after.Raw, body) {
		t.Fatalf("snapshot changed with the database: %s", after.Raw)
	}
	next := injectionRequest(t, srv, h, "A")
	after := injectionGet(t, h, "A")
	if after.V["block"] != next.V["block"] || !strings.Contains(after.Raw, body) || len(h.st.last) != 1 {
		t.Fatalf("last snapshot not replaced: %s", after.Raw)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, turn := range h.chats["chat:A"].turns {
		if turn.injection != nil && (turn.injection.Block != "" || turn.injection.Memories != nil) {
			t.Fatal("HUD retains detailed old snapshots")
		}
	}
}

func TestInjectionInherited(t *testing.T) {
	h := hudFixture(t, "", 0)
	scope, _ := injectionSeed(t, h, "PARENT")
	ids := injectionSave(t, h.db, scope[0].Source, `{"entities":[{"id":"n1","name":"리나"}],"memories":[{"id":"m1","kind":"fact","about":"n1","mode":"state","body":"부모에게 물려받은 사실","unaware":["n1"]}]}`)
	rows := []string{}
	for i := 0; i < 3; i++ {
		rows = append(rows, memRow(2*i, "user", fmt.Sprintf("PARENT-u%d", i), "", fmt.Sprintf("입력 %d", i)), memRow(2*i+1, "char", fmt.Sprintf("PARENT-a%d", i), "g", fmt.Sprintf("응답 %d", i)))
	}
	rows = append(rows, `{"i":6,"role":"char","chat_id":"marker","is_comment":true,"disabled":true,"data":{"text":"{{specialcomment::branchedfrom::PARENT::이름::PARENT-a2::}}"}}`, memRow(7, "user", "child-current", "", "리나의 약속과 생각"))
	ev := memEvent(2, "beforeRequest", "CHILD", rows...)
	if _, err := h.pipe.Process([]observe.Record{ev}); err != nil {
		t.Fatal(err)
	}
	srv := observe.NewServer(store.RawSink{DB: h.db})
	delivered := injectionRequest(t, srv, h, "CHILD")
	a := injectionGet(t, h, "CHILD")
	mems, ok := a.V["memories"].([]any)
	if !ok || len(mems) != 1 {
		t.Fatalf("inherited delivery absent: %s / %s", a.Raw, delivered.Raw)
	}
	m := mems[0].(map[string]any)
	if m["id"] != ids["memories/0"] || m["inherited"] != true || m["secret"] != true || m["about"] != "리나" || m["turn"] != float64(1) || m["mode"] != "state" {
		t.Fatalf("inherited metadata lost: %v", m)
	}
}

func TestInjectionDeliveryOrder(t *testing.T) {
	h := hudFixture(t, "", 0)
	scope, _ := injectionSeed(t, h, "ORDER")
	first := injectionSave(t, h.db, scope[0].Source, `{"entities":[{"id":"n1","name":"리나"}],"memories":[{"id":"m1","kind":"fact","about":"n1","slot":"직업","body":"리나는 약사다"}]}`)
	injectionSave(t, h.db, scope[1].Source, fmt.Sprintf(`{"memories":[{"id":"m1","kind":"fact","about":%q,"slot":"거처","body":"리나는 항구에 산다"}]}`, first["entities/0"]))
	srv := observe.NewServer(store.RawSink{DB: h.db})
	delivered := injectionRequest(t, srv, h, "ORDER")
	ids := delivered.V["ids"].([]any)
	if len(ids) != 2 || ids[0] == first["memories/0"] {
		t.Fatalf("fixture must deliver a newer record before the first stored one: %s", delivered.Raw)
	}
	a := injectionGet(t, h, "ORDER")
	mems := a.V["memories"].([]any)
	if len(mems) != len(ids) {
		t.Fatalf("records: %s", a.Raw)
	}
	for i, row := range mems {
		if row.(map[string]any)["id"] != ids[i] {
			t.Fatalf("storage order replaced delivery order: %s", a.Raw)
		}
	}
	if mems[0].(map[string]any)["turn"] != float64(2) || mems[1].(map[string]any)["turn"] != float64(1) {
		t.Fatalf("story turns lost: %s", a.Raw)
	}
	// After a restart the last delivery is read from the database as sent
	// (user 10-29: a delivery can be checked later).
	before := a
	restarted := newStatus(h.db, h.st.mods, h.worker)
	a = callMem(t, restarted.injectionHandler(), "GET", "/injection?chat_id=ORDER", "")
	if a.Code != 200 || a.V["none"] != false || a.V["block"] != before.V["block"] || a.V["items"] != before.V["items"] || a.V["chars"] != before.V["chars"] {
		t.Fatalf("delivery not kept over a restart: %s / %s", a.Raw, before.Raw)
	}
	after := a.V["memories"].([]any)
	for i, row := range mems {
		if after[i].(map[string]any)["id"] != row.(map[string]any)["id"] || after[i].(map[string]any)["body"] != row.(map[string]any)["body"] {
			t.Fatalf("kept records differ: %s", a.Raw)
		}
	}
	// the trace holds every candidate considered, the delivered ones taken
	tr := callMem(t, restarted.traceHandler(), "GET", "/injection/trace?chat_id=ORDER", "")
	if tr.Code != 200 {
		t.Fatal(tr.Raw)
	}
	d := tr.V["deliveries"].([]any)[0].(map[string]any)
	taken := []any{}
	for _, row := range d["trace"].([]any) {
		if r := row.(map[string]any); r["result"] == "taken" {
			taken = append(taken, r["id"])
		}
	}
	if fmt.Sprint(taken) != fmt.Sprint(ids) || d["summary"].(map[string]any)["input"] != "리나의 약속과 생각" {
		t.Fatalf("trace does not match the delivery: %s", tr.Raw)
	}
}

func TestInjectionDeleteAndReset(t *testing.T) {
	h := hudFixture(t, "", 0)
	srv := observe.NewServer(store.RawSink{DB: h.db})
	srv.After = h.pipe
	for _, chat := range []string{"A", "B"} {
		scope, _ := injectionSeed(t, h, chat)
		injectionSave(t, h.db, scope[0].Source, `{"memories":[{"id":"m1","kind":"fact","body":"삭제와 초기화 시험"}]}`)
		injectionRequest(t, srv, h, chat)
		if injectionGet(t, h, chat).V["none"] != false {
			t.Fatal("seed injection absent")
		}
	}
	a := callMem(t, chatOpsHandler(h.pipe, h.db, h.worker, h.st), "POST", "/chats/delete", `{"chat_id":"A"}`)
	if a.Code != 200 || injectionGet(t, h, "A").V["none"] != true || injectionGet(t, h, "B").V["none"] != false {
		t.Fatalf("delete not isolated: %s", a.Raw)
	}
	m := &maintenance{db: h.db, mods: h.st.mods, worker: h.worker, pipe: h.pipe, server: srv, status: h.st}
	a = callMem(t, m.reset, "POST", "/maintenance/reset", `{"phrase":"전체 DB 초기화"}`)
	if a.Code != 200 || injectionGet(t, h, "B").V["none"] != true || len(h.st.last) != 0 || len(h.chats) != 0 {
		t.Fatalf("reset retained injection: %s", a.Raw)
	}
}

func TestInjectionRetentionAndMethods(t *testing.T) {
	h := hudFixture(t, "", 0)
	srv := observe.NewServer(store.RawSink{DB: h.db})
	clock := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	h.st.now = func() time.Time { clock = clock.Add(time.Second); return clock }
	for i := 0; i <= injectionsKept; i++ {
		injectionRequest(t, srv, h, fmt.Sprintf("C%d", i))
	}
	// memory keeps the latest chats; an evicted chat's delivery is read
	// from the database
	if len(h.st.last) != injectionsKept || h.st.last["C0"] != nil || injectionGet(t, h, "C0").V["none"] != false || injectionGet(t, h, "C1").V["none"] != false {
		t.Fatal("oldest snapshot not evicted at the existing chat limit, or not kept in the database")
	}
	// Refreshing a chat replaces its old entry and its age.
	injectionRequest(t, srv, h, "C1")
	injectionRequest(t, srv, h, "NEW")
	if len(h.st.last) != injectionsKept || h.st.last["C2"] != nil || h.st.last["C1"] == nil || injectionGet(t, h, "C1").V["none"] != false {
		t.Fatal("refresh did not preserve the latest per-chat snapshot")
	}
	if a := callMem(t, h.st.injectionHandler(), "POST", "/injection?chat_id=C1", ""); a.Code != 405 {
		t.Fatalf("method: %s", a.Raw)
	}
	if a := injectionGet(t, h, "NEW"); a.V["none"] != false || a.V["block"] != "" || len(a.V["memories"].([]any)) != 0 {
		t.Fatalf("empty delivery confused with no request: %s", a.Raw)
	}
}

func TestInjectionGenerationStates(t *testing.T) {
	for _, outcome := range []string{"done", "failed", "retry_waiting"} {
		t.Run(outcome, func(t *testing.T) {
			entered, release := make(chan struct{}, 1), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Model    string `json:"model"`
					Messages []any  `json:"messages"`
				}
				if r.Method != "POST" || r.URL.Path != "/api/chat" || json.NewDecoder(r.Body).Decode(&req) != nil || req.Model != "fake" || len(req.Messages) == 0 {
					t.Error("unexpected synthetic provider request")
					http.Error(w, "unexpected", 400)
					return
				}
				entered <- struct{}{}
				<-release
				if outcome != "done" {
					http.Error(w, "synthetic generation failure", 503)
					return
				}
				json.NewEncoder(w).Encode(map[string]any{"message": map[string]string{"content": `{"memories":[{"id":"m1","kind":"fact","body":"직전 턴에 만든 기억"}]}`}})
			}))
			t.Cleanup(model.Close)
			t.Cleanup(unblock)
			retries := 0
			if outcome == "retry_waiting" {
				retries = 1
			}
			h := hudFixture(t, model.URL, retries)
			if _, err := h.pipe.Process([]observe.Record{memEvent(1, "beforeRequest", "GEN", memRow(0, "user", "u", "", "질문"), memRow(1, "char", "a", "g", "답"), memRow(2, "user", "u2", "", "다음"))}); err != nil {
				t.Fatal(err)
			}
			get := func() map[string]any { return injectionGet(t, h, "GEN").V["generation"].(map[string]any) }
			if gen := get(); gen["state"] != "waiting" || gen["count"] != float64(0) {
				t.Fatalf("waiting: %v", gen)
			}
			hudRun(t, h.worker)
			t.Cleanup(unblock) // release provider before worker cleanup even on failure
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("provider not entered")
			}
			if gen := get(); gen["state"] != "running" {
				t.Fatalf("running: %v", gen)
			}
			unblock()
			hudWait(t, func() bool { return get()["state"] == outcome })
			gen := get()
			if outcome == "done" && gen["count"] != float64(1) || outcome != "done" && gen["detail"] == nil || outcome == "retry_waiting" && gen["next_at"] == nil {
				t.Fatalf("generation result missing count/reason/retry time: %v", gen)
			}
		})
	}
}

// As AC1, only the main model's request gets memory. The same request shape
// sent as an auxiliary type gets no block and leaves the chat's last
// injection as the main request left it.
func TestInjectionOnlyMainModelRequest(t *testing.T) {
	h := hudFixture(t, "", 0)
	scope, _ := injectionSeed(t, h, "A")
	injectionSave(t, h.db, scope[0].Source, `{"entities":[{"id":"n1","name":"리나"}],"memories":[{"id":"m1","kind":"fact","about":"n1","mode":"rule","slot":"약속","body":"약속은 해가 지기 전에 지킨다"}]}`)
	srv := observe.NewServer(store.RawSink{DB: h.db})
	srv.After = h.pipe
	send := func(typ string) memAnswer {
		prints := []store.TextPrint{}
		for _, text := range []string{"입력 2", "응답 2", "리나의 약속과 생각"} {
			n, hash := ingest.Fingerprint(text)
			prints = append(prints, store.TextPrint{Len: n, H: hash})
		}
		body, _ := json.Marshal(map[string]any{"chat_id": "A", "type": typ, "input": "리나의 약속", "payload": prints})
		a := callMem(t, injectHandler(srv, h.pipe, h.db, func() *provider.Embedder { return nil }, h.st), "POST", "/memory/inject", string(body))
		if a.Code != 200 {
			t.Fatalf("inject %q: %d %s", typ, a.Code, a.Raw)
		}
		return a
	}
	main := send("model")
	if b, _ := main.V["block"].(string); !strings.Contains(b, "약속은 해가 지기 전에 지킨다") {
		t.Fatalf("main request not delivered: %s", main.Raw)
	}
	before := injectionGet(t, h, "A")
	for _, typ := range []string{"otherAx", "submodel", "translate"} {
		aux := send(typ)
		if aux.V["block"] != nil || aux.V["skipped"] != "request_type" {
			t.Fatalf("%s request got memory: %s", typ, aux.Raw)
		}
	}
	if after := injectionGet(t, h, "A"); after.V["block"] != before.V["block"] || after.V["at"] != before.V["at"] {
		t.Fatalf("auxiliary request replaced the main injection: %s", after.Raw)
	}
	// a host that sends no type is treated as the main request (AC1: !type)
	if none := send(""); none.V["block"] != main.V["block"] {
		t.Fatalf("untyped request not delivered: %s", none.Raw)
	}
}

// The query is the host's own row of this turn, not the request's last user
// message: a preset may put more user-role prompt parts after it.
func TestInjectionQueryIsHostInput(t *testing.T) {
	h := hudFixture(t, "", 0)
	injectionSeed(t, h, "A")
	var mu sync.Mutex
	var asked [][]string
	voyage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Input []string `json:"input"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		asked = append(asked, body.Input)
		mu.Unlock()
		http.Error(w, "down", 503)
	}))
	defer voyage.Close()
	qe, err := provider.NewEmbedder(provider.EmbedConfig{Provider: "voyageai", Model: "voyage-4-large", URL: voyage.URL, Key: "k"})
	if err != nil {
		t.Fatal(err)
	}
	srv := observe.NewServer(store.RawSink{DB: h.db})
	srv.After = h.pipe
	prints := []store.TextPrint{}
	for _, m := range [][2]string{{"user", "입력 2"}, {"assistant", "응답 2"}, {"user", "리나의 약속과 생각"}, {"user", "프리셋 지시문"}} {
		n, hash := ingest.Fingerprint(m[1])
		prints = append(prints, store.TextPrint{Len: n, H: hash, Role: m[0]})
	}
	body, _ := json.Marshal(map[string]any{"chat_id": "A", "type": "model", "input": "프리셋 지시문", "payload": prints})
	a := callMem(t, injectHandler(srv, h.pipe, h.db, func() *provider.Embedder { return qe }, h.st), "POST", "/memory/inject", string(body))
	if a.Code != 200 {
		t.Fatalf("inject: %d %s", a.Code, a.Raw)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(asked) == 0 {
		t.Fatalf("no query sent: %s", a.Raw)
	}
	want := [][]string{{"리나의 약속과 생각", "USER: 입력 2\nREPLY: 응답 2\n"}, {"리나의 약속과 생각"}}
	if !reflect.DeepEqual(asked, want) {
		t.Fatalf("queries must use the host input and separate previous turn, with input-only recovery: %q", asked)
	}
}

// The restored baseline quotes history as stored, including a promise
// whose closing event is also delivered. It does not rewrite old statuses.
func TestInjectionBaselineObligationHistory(t *testing.T) {
	h := hudFixture(t, "", 0)
	scope, _ := injectionSeed(t, h, "A")
	first := injectionSave(t, h.db, scope[0].Source, `{"entities":[{"id":"n1","name":"리나"},{"id":"n2","name":"도윤"}],"memories":[{"id":"m1","kind":"fact","about":"n1","mode":"의무","slot":"의무","to":"n2","status":"active","body":"리나는 도윤에게 책을 돌려주기로 약속했다"},{"id":"m2","kind":"fact","about":"n1","mode":"의무","slot":"의무","to":"n2","status":"active","body":"리나는 도윤을 다시 만나기로 약속했다"}],"scenes":[{"id":"s1","change":"open"}]}`)
	injectionSave(t, h.db, scope[1].Source, `{"memories":[{"id":"m1","kind":"event","body":"리나가 도윤에게 책을 돌려주었다","effects":[{"about":"`+first["memories/0"]+`","status":"fulfilled"}]}]}`)
	srv := observe.NewServer(store.RawSink{DB: h.db})
	srv.After = h.pipe
	a := injectionRequest(t, srv, h, "A")
	block, _ := a.V["block"].(string)
	if !strings.Contains(block, "책을 돌려주기로 약속했다") {
		t.Fatalf("baseline promise history missing: %s", block)
	}
	if !strings.Contains(block, "리나가 도윤에게 책을 돌려주었다") || !strings.Contains(block, "다시 만나기로 약속했다") {
		t.Fatalf("closing event or open obligation missing: %s", block)
	}
}
