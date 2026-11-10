package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/store"
)

func c21Get(t *testing.T, st *status, chat string, id int64, cursor, limit int) store.DeliveryTracePage {
	t.Helper()
	a := callMem(t, st.traceHandler(), "GET", fmt.Sprintf("/injection/trace?chat_id=%s&delivery_id=%d&row_cursor=%d&row_limit=%d", chat, id, cursor, limit), "")
	if a.Code != http.StatusOK {
		t.Fatalf("page: %d %s", a.Code, a.Raw)
	}
	var body struct {
		Delivery store.DeliveryTracePage `json:"delivery"`
	}
	if err := json.Unmarshal([]byte(a.Raw), &body); err != nil {
		t.Fatal(err)
	}
	return body.Delivery
}

// Real Deliver -> persisted trace -> production HTTP projection. Vectors are
// synthetic stored inputs, not embedding calls; no provider exists in this test.
func TestC21SelectionRoundTrip(t *testing.T) {
	h := hudFixture(t, "", 0)
	scope, _ := injectionSeed(t, h, "A")
	raw := `{"entities":[{"id":"n1","name":"리나"},{"id":"n2","name":"세계 규칙"},{"id":"n3","name":"보호 대상 도윤"}],"memories":[
{"id":"m1","kind":"event","roles":{"actor":["n1"]},"body":"거대한 사건 ` + strings.Repeat("기록이 길다 ", 1000) + `"},
{"id":"m2","kind":"fact","about":"n2","at":"m1","body":"봉인은 금빛 열쇠로만 열린다"},
{"id":"m3","kind":"event","roles":{"actor":["n1"]},"body":"비밀인 만남 근거","hide_from":["n3"]},
{"id":"m4","kind":"view","owner":"n1","body":"근거에 대한 생각","basis":{"ref":"m3"}},
{"id":"m5","kind":"fact","about":"n1","slot":"옛 장소","body":"대비에서 제거할 기억"},
{"id":"m6","kind":"fact","about":"n1","slot":"현 장소","body":"계속 유지할 기억"}]}`
	ids := injectionSave(t, h.db, scope[0].Source, raw)
	hashes := map[string]string{}
	var hs []string
	var vectors [][]float32
	for _, i := range []int{2, 3, 4, 5} {
		id := ids[fmt.Sprintf("memories/%d", i)]
		hash := store.TextHash(id)
		hashes[id] = hash
		hs = append(hs, hash)
		v := []float32{1, 0}
		if i == 4 {
			v = []float32{0, 1}
		}
		vectors = append(vectors, v)
	}
	if err := h.db.SetDocHashes(hashes); err != nil {
		t.Fatal(err)
	}
	if err := h.db.SaveEmbeddings("c21-synthetic", hs, vectors); err != nil {
		t.Fatal(err)
	}
	req := store.DeliveryRequest{Input: "리나 거대한 사건 비밀인 만남 근거에 대한 생각", Chars: 2000, Secret: 4000, Model: "c21-synthetic", Query: []float32{1, 0}, Payload: []store.TextPrint{{Role: "user"}}}
	d, err := h.db.Deliver(scope, req)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{ids["memories/0"]: "over_ceiling", ids["memories/1"]: "parent_not_taken", ids["memories/2"]: "taken", ids["memories/3"]: "taken", ids["memories/4"]: "contrast_removed", ids["memories/5"]: "taken"}
	results := map[string]string{}
	for _, r := range d.Trace {
		results[r.ID] = r.Result
	}
	if !reflect.DeepEqual(results, want) {
		t.Fatalf("fixture did not exercise actual result branches: %+v", d.Trace)
	}
	if !d.SecretIDs[ids["memories/3"]] {
		t.Fatal("basis protection not carried")
	}
	snapshots, err := h.db.InjectedMemories("chat:A", d.IDs, scope, d.SecretIDs)
	if err != nil {
		t.Fatal(err)
	}
	sum := store.DeliverySummary{Input: req.Input, Candidates: d.Candidates, IDs: d.IDs, Memories: snapshots, Chars: d.Chars, SecretChars: d.SecretChars, CharsMax: req.Chars, SecretMax: req.Secret}
	id, err := h.db.SaveDeliveryTrace("chat:A", time.Now(), sum, d.Block, d.Trace)
	if err != nil {
		t.Fatal(err)
	}
	st := newStatus(h.db, h.st.mods, h.worker)
	page := c21Get(t, st, "A", id, 0, 100)
	if !reflect.DeepEqual(page.Trace, d.Trace) || page.Block != d.Block || page.RowTotal != len(d.Trace) || page.Summary.Candidates != d.Candidates || !reflect.DeepEqual(page.Summary.IDs, d.IDs) {
		t.Fatalf("stored decision changed: %+v", page)
	}
	// Candidates deliberately excludes the uncomputed completion row.
	if page.RowTotal == page.Summary.Candidates {
		t.Fatal("fixture must distinguish candidate populations")
	}
	for _, r := range page.Trace {
		if r.Result == "parent_not_taken" && (r.Size != 0 || r.Secret) {
			t.Fatalf("fabricated uncomputed fields: %+v", r)
		}
	}
	// History contains selected snapshots only. Later edits/deletions must not
	// substitute current records or hydrate dropped candidates into that snapshot.
	newBody := "later edited body"
	if err := h.db.EditMemory("chat:A", ids["memories/5"], &newBody, nil); err != nil {
		t.Fatal(err)
	}
	if err := h.db.DeleteMemory("chat:A", ids["memories/3"]); err != nil {
		t.Fatal(err)
	}
	if err := h.db.DeleteMemory("chat:A", ids["memories/4"]); err != nil {
		t.Fatal(err)
	}
	after := c21Get(t, st, "A", id, 0, 100)
	if !reflect.DeepEqual(after.Summary.Memories, snapshots) || after.Block != d.Block || strings.Contains(after.Block, newBody) {
		t.Fatal("history replaced by current records")
	}
	if len(after.Summary.Memories) != len(d.IDs) {
		t.Fatal("dropped candidate presented as historical body")
	}
	index := callMem(t, st.traceHandler(), "GET", "/injection/trace?chat_id=A&view=index", "")
	for _, private := range []string{"비밀인 만남 근거", "보호 대상 도윤", `"input"`, `"memories"`, `"block"`, `"trace"`} {
		if strings.Contains(index.Raw, private) {
			t.Fatalf("index leaked %q: %s", private, index.Raw)
		}
	}
	other := callMem(t, st.traceHandler(), "GET", fmt.Sprintf("/injection/trace?chat_id=B&delivery_id=%d", id), "")
	if other.Code != 404 || strings.Contains(other.Raw, "비밀") {
		t.Fatalf("other chat saw trace: %s", other.Raw)
	}
}

func TestC21RequestLocatorAndLatestID(t *testing.T) {
	// The child starts with no pipeline/status memory, reading only the
	// parent's already-persisted synthetic fixture through a new SQL pool.
	if raw := os.Getenv("AC2_C21_RESTART_DELIVERY_ID"); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		db, err := store.Open(os.Getenv("AC2_TEST_MARIADB_DSN") + "ac2_test_models?parseTime=true")
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		st := newStatus(db, nil, nil)
		page := c21Get(t, st, "A", id, 0, 1)
		locator, err := json.Marshal(page.Summary.Locator)
		if err != nil || string(locator) != os.Getenv("AC2_C21_RESTART_LOCATOR") {
			t.Fatalf("child locator: %s %v", locator, err)
		}
		a := callMem(t, st.injectionHandler(), "GET", "/injection?chat_id=A", "")
		if a.Code != 200 || a.V["delivery_id"] != float64(id) || a.V["block"] != page.Block {
			t.Fatalf("child latest: %s", a.Raw)
		}
		return
	}
	h := hudFixture(t, "", 0)
	scope, _ := injectionSeed(t, h, "A")
	injectionSave(t, h.db, scope[0].Source, `{"memories":[{"id":"m1","kind":"fact","body":"리나의 옛 기억"}]}`)
	srv := observe.NewServer(store.RawSink{DB: h.db})
	srv.After = h.pipe
	// Deleting a middle turn separates the logical ID from its display
	// position. Diagnostics must use the chat's own turn, not a source turn.
	rows := []string{memRow(0, "user", "A-u0", "", "입력 0"), memRow(1, "char", "A-a0", "g", "응답 0"),
		memRow(2, "user", "A-u2", "", "입력 2"), memRow(3, "char", "A-a2", "g", "응답 2"),
		memRow(4, "user", "A-current", "", "리나의 약속과 생각")}
	if _, err := h.pipe.Process([]observe.Record{memEvent(2, "beforeRequest", "A", rows...)}); err != nil {
		t.Fatal(err)
	}
	var expected store.DeliveryLocator
	if err := h.pipe.WithSession("chat:A", func(s *lifecycle.Session) {
		turns := s.ActiveTurns()
		last := turns[len(turns)-1]
		expected = store.DeliveryLocator{TurnID: last.ID, Turn: len(turns), SourceKey: memory.SourceOf(s.Key, last).Key()}
	}); err != nil {
		t.Fatal(err)
	}
	if expected.TurnID == expected.Turn {
		t.Fatal("fixture must distinguish logical ID and display position")
	}
	var requestIDs []int64
	for i := 0; i < 2; i++ {
		a := injectionRequest(t, srv, h, "A")
		id := int64(a.V["delivery_id"].(float64))
		requestIDs = append(requestIDs, id)
		page := c21Get(t, h.st, "A", id, 0, 1)
		if !reflect.DeepEqual(page.Summary.Locator, &expected) {
			t.Fatalf("request locator: %+v / %+v", page.Summary.Locator, expected)
		}
		latest := injectionGet(t, h, "A")
		if latest.V["delivery_id"] != float64(id) {
			t.Fatalf("latest ID: %s", latest.Raw)
		}
	}
	if requestIDs[0] == requestIDs[1] {
		t.Fatal("same-turn requests collapsed")
	}
	for _, view := range []string{"", "&view=index"} {
		a := callMem(t, h.st.traceHandler(), "GET", "/injection/trace?chat_id=A&limit=2"+view, "")
		if a.Code != 200 || len(a.V["deliveries"].([]any)) != len(requestIDs) {
			t.Fatalf("limit changed meaning: %s", a.Raw)
		}
	}
	// New HTTP owner and a freshly reopened DB pool read the same latest ID.
	db, err := store.Open(os.Getenv("AC2_TEST_MARIADB_DSN") + "ac2_test_models?parseTime=true")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	st := newStatus(db, h.st.mods, h.worker)
	a := callMem(t, st.injectionHandler(), "GET", "/injection?chat_id=A", "")
	if a.Code != 200 || a.V["delivery_id"] != float64(requestIDs[1]) {
		t.Fatalf("restart latest ID: %s", a.Raw)
	}
	locatorJSON, err := json.Marshal(expected)
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(os.Args[0], "-test.run=^TestC21RequestLocatorAndLatestID$", "-test.v")
	child.Env = append(os.Environ(), "AC2_C21_RESTART_DELIVERY_ID="+strconv.FormatInt(requestIDs[1], 10), "AC2_C21_RESTART_LOCATOR="+string(locatorJSON))
	if out, err := child.CombinedOutput(); err != nil {
		t.Fatalf("cold process read: %v\n%s", err, out)
	} else {
		t.Logf("cold process read passed: %s", out)
	}
	for _, url := range []string{"/injection/trace?chat_id=A&delivery_id=oops", "/injection/trace?chat_id=A&delivery_id=1&row_cursor=-1", "/injection/trace?chat_id=A&delivery_id=1&row_limit=0"} {
		if a := callMem(t, st.traceHandler(), "GET", url, ""); a.Code != 400 {
			t.Fatalf("invalid query: %s", a.Raw)
		}
	}
	page := c21Get(t, st, "A", requestIDs[1], 0, store.DeliveryTracePageMax+1)
	if page.RowLimit != store.DeliveryTracePageMax {
		t.Fatal("unbounded page")
	}
	// Empty delivery is a recorded request, including the unknown locator.
	empty := injectionRequest(t, srv, h, "EMPTY")
	id := int64(empty.V["delivery_id"].(float64))
	p := c21Get(t, h.st, "EMPTY", id, 0, 1)
	if p.Summary.Locator != nil || p.RowTotal != 0 || len(p.Trace) != 0 || p.NextCursor != nil {
		t.Fatalf("empty: %+v", p)
	}
}

func TestC21TraceSaveFailureStillDelivers(t *testing.T) {
	h := hudFixture(t, "", 0)
	scope, _ := injectionSeed(t, h, "A")
	injectionSave(t, h.db, scope[0].Source, `{"entities":[{"id":"n1","name":"리나"}],"memories":[{"id":"m1","kind":"fact","about":"n1","body":"실패해도 전달할 기억"}]}`)
	srv := observe.NewServer(store.RawSink{DB: h.db})
	srv.After = h.pipe
	before := injectionRequest(t, srv, h, "A")
	conn, err := sql.Open("mysql", os.Getenv("AC2_TEST_MARIADB_DSN")+"ac2_test_models")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Exec(`CREATE TRIGGER c21_fail_trace BEFORE INSERT ON delivery_traces FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='synthetic trace failure'`); err != nil {
		t.Fatal(err)
	}
	defer conn.Exec("DROP TRIGGER c21_fail_trace")
	after := injectionRequest(t, srv, h, "A")
	if after.V["block"] != before.V["block"] || !reflect.DeepEqual(after.V["ids"], before.V["ids"]) || after.V["delivery_id"] != nil {
		t.Fatalf("diagnostic write changed delivery: %s / %s", before.Raw, after.Raw)
	}
	if got := injectionGet(t, h, "A"); got.V["block"] != after.V["block"] || got.V["delivery_id"] != nil {
		t.Fatalf("failed trace linked old request: %s", got.Raw)
	}
}

func TestC21TraceAuthAndPrivacy(t *testing.T) {
	h := hudFixture(t, "", 0)
	scope, _ := injectionSeed(t, h, "A")
	privateBody, privateName := "비밀 본문 감시 표식", "보호 대상 이름 감시 표식"
	raw := fmt.Sprintf(`{"entities":[{"id":"n1","name":"리나"},{"id":"n2","name":%q}],"memories":[{"id":"m1","kind":"fact","about":"n1","body":%q,"hide_from":["n2"]}]}`, privateName, privateBody)
	injectionSave(t, h.db, scope[0].Source, raw)
	srv := observe.NewServer(store.RawSink{DB: h.db})
	srv.After = h.pipe
	delivered := injectionRequest(t, srv, h, "A")
	id := int64(delivered.V["delivery_id"].(float64))
	srv.ReportSettings = h.st.mods.diagnosticSettings
	srv.ReportFailures = h.st.diagnosticFailures
	srv.Routes = map[string]http.HandlerFunc{"/injection/trace": h.st.traceHandler(), "/status": h.st.handler(), "/hud": h.handler()}
	var err error
	// Only newly generated synthetic credentials, never user token/key files.
	srv.Token, _, err = observe.LoadToken(filepath.Join(t.TempDir(), "synthetic-token"))
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := srv.Token.Rotate()
	if err != nil {
		t.Fatal(err)
	}
	url := fmt.Sprintf("/injection/trace?chat_id=A&delivery_id=%d", id)
	for _, value := range []string{"", "wrong"} {
		r := httptest.NewRequest("GET", url, nil)
		r.Header.Set(observe.TokenHeader, value)
		w := httptest.NewRecorder()
		srv.Public().ServeHTTP(w, r)
		if w.Code != 401 || strings.Contains(w.Body.String(), privateBody) {
			t.Fatalf("unauthorized detail: %d", w.Code)
		}
	}
	for _, url := range []string{url, "/status?chat_id=A", "/hud?chat_id=A", "/diagnostics/report"} {
		r := httptest.NewRequest("GET", url, nil)
		r.Header.Set(observe.TokenHeader, token)
		w := httptest.NewRecorder()
		srv.Public().ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("route %s: %d %s", url, w.Code, w.Body)
		}
		if strings.HasPrefix(url, "/injection/trace") {
			if !strings.Contains(w.Body.String(), privateBody) {
				t.Fatal("authorized detail lost snapshot")
			}
			continue
		}
		for _, private := range []string{privateBody, privateName, "리나의 약속과 생각", `"source_key"`, `"delivery_id"`} {
			if strings.Contains(w.Body.String(), private) {
				t.Fatalf("management detail escaped to %s: %s", url, w.Body)
			}
		}
	}
}
