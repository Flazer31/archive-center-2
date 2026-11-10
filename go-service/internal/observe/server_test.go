package observe

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"archivecenter2/goservice/internal/serve"
)

type memSink struct {
	records []Record
	failErr error
}

func TestVersionIncludesListenerOwnedConnectionInfo(t *testing.T) {
	s := NewServer(&memSink{})
	s.Connection = serve.ConnectionInfo{LocalURL: "http://127.0.0.1:28190", RemoteURLs: []string{"http://192.0.2.9:28190"}, RemoteHint: "synthetic listener hint"}
	rec := do(t, s.Handler(), "GET", "/version", "")
	var value struct {
		Connection serve.ConnectionInfo `json:"connection"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &value) != nil || value.Connection.RemoteHint != s.Connection.RemoteHint || len(value.Connection.RemoteURLs) != len(s.Connection.RemoteURLs) || value.Connection.RemoteURLs[0] != s.Connection.RemoteURLs[0] {
		t.Fatal("version did not deliver listener-owned connection details")
	}
}

func (m *memSink) Write(r []Record) ([]Record, error) {
	if m.failErr != nil {
		return nil, m.failErr
	}
	m.records = append(m.records, r...)
	return r, nil
}
func (m *memSink) Ready() error { return m.failErr }

func do(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func code(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var v map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &v)
	s, _ := v["code"].(string)
	return s
}

func TestObserveStoresEventsVerbatimWithReceiveOrder(t *testing.T) {
	sink := &memSink{}
	h := NewServer(sink).Handler()
	body := `{"contract":"observation.v1","plugin_version":"p1","events":[{"hook":"beforeRequest","n":1},{"hook":"afterRequest","n":2}]}`
	rec := do(t, h, "POST", "/observe", body)
	if rec.Code != 200 {
		t.Fatalf("status %d %s", rec.Code, rec.Body.String())
	}
	if len(sink.records) != 2 {
		t.Fatalf("stored %d", len(sink.records))
	}
	if string(sink.records[0].Event) != `{"hook":"beforeRequest","n":1}` || sink.records[1].ReceiveSeq != 2 {
		t.Fatalf("records not verbatim/in order: %+v", sink.records)
	}
	if sink.records[0].PluginVersion != "p1" || sink.records[0].ReceivedAt == "" {
		t.Fatalf("missing receive metadata: %+v", sink.records[0])
	}
}

func TestObserveStableErrorCodes(t *testing.T) {
	sink := &memSink{}
	h := NewServer(sink).Handler()
	if rec := do(t, h, "POST", "/observe", `not json`); code(t, rec) != CodeBadRequest {
		t.Fatalf("bad json code %q", code(t, rec))
	}
	if rec := do(t, h, "POST", "/observe", `{"contract":"observation.v0","events":[]}`); code(t, rec) != CodeUnsupportedContract {
		t.Fatalf("contract code %q", code(t, rec))
	}
	if rec := do(t, h, "GET", "/observe", ``); code(t, rec) != CodeMethodNotAllowed {
		t.Fatalf("method code %q", code(t, rec))
	}
	sink.failErr = errors.New("disk full")
	rec := do(t, h, "POST", "/observe", `{"contract":"observation.v1","events":[{}]}`)
	if rec.Code != 500 || code(t, rec) != CodeStoreWriteFailed {
		t.Fatalf("write failure: %d %q", rec.Code, code(t, rec))
	}
	diag := do(t, h, "GET", "/diagnostics", "")
	if !strings.Contains(diag.Body.String(), "disk full") {
		t.Fatalf("diagnostics lost failure: %s", diag.Body.String())
	}
}

func TestReadyReflectsSinkAndVersionReportsContract(t *testing.T) {
	sink := &memSink{}
	h := NewServer(sink).Handler()
	if rec := do(t, h, "GET", "/ready", ""); rec.Code != 200 {
		t.Fatalf("ready %d", rec.Code)
	}
	sink.failErr = errNotReady
	if rec := do(t, h, "GET", "/ready", ""); rec.Code != 503 {
		t.Fatalf("not-ready %d", rec.Code)
	}
	rec := do(t, h, "GET", "/version", "")
	if !strings.Contains(rec.Body.String(), ContractVersion) {
		t.Fatalf("version body %s", rec.Body.String())
	}
}

func TestPreflightAllowsJSONAndPrivateNetwork(t *testing.T) {
	h := NewServer(&memSink{}).Handler()
	rec := do(t, h, "OPTIONS", "/observe", "")
	if rec.Code != 204 || !strings.Contains(rec.Header().Get("Access-Control-Allow-Headers"), "Content-Type") {
		t.Fatalf("preflight %d %v", rec.Code, rec.Header())
	}
	// a page calling a private address (Tailscale 100.x, LAN) is allowed
	if rec.Header().Get("Access-Control-Allow-Private-Network") != "true" {
		t.Fatalf("private network preflight not allowed: %v", rec.Header())
	}
}

// A plugin outbox overflow must leave a visible gap before the batch's events.
func TestObserveRecordsDroppedGapBeforeEvents(t *testing.T) {
	sink := &memSink{}
	h := NewServer(sink).Handler()
	body := `{"contract":"observation.v1","plugin_version":"p1","dropped_before":7,"events":[{"hook":"input","n":1}]}`
	if rec := do(t, h, "POST", "/observe", body); rec.Code != 200 {
		t.Fatalf("status %d %s", rec.Code, rec.Body.String())
	}
	if len(sink.records) != 2 || string(sink.records[0].Event) != `{"dropped_before":7,"hook":"gap"}` || string(sink.records[1].Event) != `{"hook":"input","n":1}` {
		t.Fatalf("gap not recorded first: %+v", sink.records)
	}
	sink.records = nil
	body = `{"contract":"observation.v1","plugin_version":"p1","dropped_before":0,"events":[{"hook":"input","n":2}]}`
	do(t, h, "POST", "/observe", body)
	if len(sink.records) != 1 {
		t.Fatalf("no gap expected: %+v", sink.records)
	}
}

type fakeProcessor struct {
	resend bool
	err    error
	got    int
}

func (f *fakeProcessor) Process(r []Record) (bool, error) {
	f.got += len(r)
	return f.resend, f.err
}
func (f *fakeProcessor) Diagnostics() map[string]any { return map[string]any{} }

// The plugin learns from the reply when the backend lacks texts it sent as
// fingerprints only.
func TestObserveTellsPluginToResendText(t *testing.T) {
	for _, resend := range []bool{false, true} {
		srv := NewServer(&memSink{})
		p := &fakeProcessor{resend: resend}
		srv.After = p
		rec := do(t, srv.Handler(), "POST", "/observe", `{"contract":"observation.v1","events":[{"hook":"input"}]}`)
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != 200 || p.got != 1 || (body["resend_text"] == true) != resend {
			t.Fatalf("resend=%v: status %d processed %d body %v", resend, rec.Code, p.got, body)
		}
	}
}

// A failed apply answers an error so the plugin keeps and resends the batch;
// the resent batch reaches the pipeline even though its raw rows exist.
func TestApplyFailureAnswersErrorAndResendIsProcessed(t *testing.T) {
	sink := &dedupSink{seen: map[string]bool{}}
	srv := NewServer(sink)
	p := &fakeProcessor{err: errors.New("save failed")}
	srv.After = p
	body := `{"contract":"observation.v1","events":[{"boot_id":"b","seq":1,"hook":"input"}]}`
	if rec := do(t, srv.Handler(), "POST", "/observe", body); rec.Code != 500 || code(t, rec) != CodeApplyFailed {
		t.Fatalf("status %d code %q", rec.Code, code(t, rec))
	}
	p.err = nil
	if rec := do(t, srv.Handler(), "POST", "/observe", body); rec.Code != 200 || p.got != 2 {
		t.Fatalf("resend: status %d processed %d", rec.Code, p.got)
	}
}

// dedupSink stores an event once, like the MariaDB sink.
type dedupSink struct{ seen map[string]bool }

func (d *dedupSink) Write(r []Record) ([]Record, error) {
	var out []Record
	for _, x := range r {
		if !d.seen[string(x.Event)] {
			d.seen[string(x.Event)] = true
			out = append(out, x)
		}
	}
	return out, nil
}
func (d *dedupSink) Ready() error { return nil }
