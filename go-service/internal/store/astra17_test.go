package store

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/pipeline"
)

// Recheck counterexamples through the real HTTP handler, RawSink and
// MariaDB state store, kept as regressions.
type fail17 struct {
	*DB
	fail bool
}

func (f *fail17) SaveSession(s *lifecycle.Session, e []lifecycle.Event, r []lifecycle.Rename) error {
	if f.fail {
		f.fail = false
		return errors.New("injected one-time lifecycle save failure")
	}
	return f.DB.SaveSession(s, e, r)
}
func TestAstra17RawCommitThenFailedApply(t *testing.T) {
	db := testDB(t)
	st := &fail17{DB: db, fail: true}
	p := pipeline.New(st)
	srv := observe.NewServer(RawSink{DB: db})
	srv.After = p
	body := `{"contract":"observation.v1","events":[{"boot_id":"b17","seq":1,"hook":"output","event_chat":{"chat_id":"A","rows":[{"i":0,"role":"user","chat_id":"u1","data":{"text":"q"}},{"i":1,"role":"char","chat_id":"a1","data":{"text":"r"}}]}}]}`
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/observe", strings.NewReader(body)))
		t.Logf("attempt %d HTTP %d %s", i, w.Code, w.Body.String())
	}
	// Restart and replay the exact persisted raw event through the server too.
	srv.After = pipeline.New(st)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/observe", strings.NewReader(body)))
	raw, _ := db.RawAfter(0, 10)
	_, found, err := db.LoadSession("chat:A")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatalf("raw=%d state missing after retry+restart; diag=%v", len(raw), p.Diagnostics())
	}
}
func TestAstra17RenameAfterRestart(t *testing.T) {
	db := testDB(t)
	s := lifecycle.NewSession("chat:A")
	s.Apply(lsnap(lifecycle.SourceLoad, lifecycle.Row{Role: "user", Time: 7, Data: "q"}, lifecycle.Row{Role: "char", Data: "r"}))
	if err := db.SaveSession(s, s.TakeEvents(), s.TakeRenames()); err != nil {
		t.Fatal(err)
	}
	s, _, _ = db.LoadSession("chat:A")
	s.Apply(lsnap(lifecycle.SourceBeforeRequest, lifecycle.Row{Role: "user", ChatID: "u1", Time: 7, Data: "q"}, lifecycle.Row{Role: "char", ChatID: "a1", Data: "r"}))
	if err := db.SaveSession(s, s.TakeEvents(), s.TakeRenames()); err != nil {
		t.Fatal(err)
	}
	got, _, err := db.LoadSession("chat:A")
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(s.Turns)
	b, _ := json.Marshal(got.Turns)
	if string(a) != string(b) {
		t.Fatalf("rename restart roundtrip %s != %s", a, b)
	}
}

type pause17 struct {
	p                *pipeline.Pipeline
	once             sync.Once
	entered, release chan struct{}
}

func (p *pause17) Process(r []observe.Record) (bool, error) {
	p.once.Do(func() { close(p.entered); <-p.release })
	return p.p.Process(r)
}
func (p *pause17) Diagnostics() map[string]any { return p.p.Diagnostics() }
func TestAstra17RawOrderVersusProcessOrder(t *testing.T) {
	db := testDB(t)
	p := pipeline.New(db)
	// Pause only the first dispatch after its raw transaction committed.
	entered, release := make(chan struct{}), make(chan struct{})
	srv := observe.NewServer(RawSink{DB: db})
	srv.After = &pause17{p: p, entered: entered, release: release}
	first := `{"contract":"observation.v1","events":[{"boot_id":"b17","seq":1,"hook":"output","event_chat":{"chat_id":"A","rows":[{"i":0,"role":"user","chat_id":"u1","data":{"text":"q"}},{"i":1,"role":"char","chat_id":"a1","data":{"text":"r"}}]}}]}`
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/observe", strings.NewReader(first)))
	}()
	<-entered
	// A second handler is ready while the first is descheduled. Use the same
	// pipeline directly as After to avoid adding any lock of our own.
	other := observe.NewServer(RawSink{DB: db})
	other.After = p
	other.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/observe", strings.NewReader(`{"contract":"observation.v1","events":[{"boot_id":"b17","seq":2,"hook":"input"}]}`)))
	close(release)
	<-done
	raw, err := db.RawAfter(0, 10)
	if err != nil {
		t.Fatal(err)
	}
	_, found, err := db.LoadSession("chat:A")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatalf("stored raw order=%s then %s but output dropped: %v", raw[0].Event, raw[1].Event, p.Diagnostics())
	}
}
