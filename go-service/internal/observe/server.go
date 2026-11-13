// Package observe is the step-1 backend: it receives RisuAI host observations
// from the AC2 plugin and records them as JSON lines. It makes no decisions.
package observe

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"archivecenter2/goservice/internal/serve"
)

const (
	ServiceName     = "ac2-go"
	ContractVersion = "observation.v1"
	maxBodyBytes    = 256 << 20 // the plugin batches up to 8 MiB; one large chat snapshot may be bigger
)

// Build metadata: release tags/manifests use MachineVersion; the UI uses the
// display name. Both may be supplied by the release build with -ldflags -X.
var ServiceVersion = "1.1.0"
var MachineVersion = "1.1.0"

// Stable error codes returned to the plugin.
const (
	CodeBadRequest          = "bad_request"
	CodeUnsupportedContract = "unsupported_contract"
	CodeStoreWriteFailed    = "store_write_failed"
	CodeApplyFailed         = "apply_failed"
	CodeMethodNotAllowed    = "method_not_allowed"
)

// Batch is what the plugin posts to /observe.
type Batch struct {
	Contract      string `json:"contract"`
	PluginVersion string `json:"plugin_version"`
	// DroppedBefore counts observations the plugin's outbox discarded
	// (overflow) before this batch; it is stored as a gap record.
	DroppedBefore int               `json:"dropped_before"`
	Events        []json.RawMessage `json:"events"`
}

// Record is one stored line: the raw event plus receive metadata.
type Record struct {
	ReceivedAt    string          `json:"received_at"`
	ReceiveSeq    uint64          `json:"receive_seq"`
	PluginVersion string          `json:"plugin_version"`
	Event         json.RawMessage `json:"event"`
}

// Sink stores records. The file sink is the production one; tests use a memory sink.
type Sink interface {
	// Write stores records and returns the ones that were new (a resent
	// event already stored is not returned).
	Write(records []Record) ([]Record, error)
	Ready() error
}

// Processor handles records after they are stored (lifecycle pipeline).
type Processor interface {
	// Process returns true when the plugin should resend texts it sent as
	// fingerprints only, and an error when applying failed (the plugin keeps
	// the batch and resends it).
	Process(records []Record) (bool, error)
	Diagnostics() map[string]any
}

type Server struct {
	applyMu sync.Mutex // raw write + lifecycle apply, shared with maintenance
	sink    Sink
	now     func() time.Time
	// After, when set, runs on records once they are stored.
	After Processor
	// Routes are more endpoints served with the same CORS (set before
	// Handler is called).
	Routes map[string]http.HandlerFunc
	// Token is checked on every request; without one the server answers
	// nothing.
	Token *Token
	// DatabaseReady is supplied by the full backend. A nil callback keeps
	// observation-only in-process users' existing readiness contract.
	DatabaseReady func() error
	// Connection is prepared by the listener owner, before Public is served.
	Connection serve.ConnectionInfo
	// Diagnostic projections are supplied by their existing runtime owners.
	DataDirectory      string
	DatabaseConfigured bool
	ReportSettings     func() ReportSettings
	ReportFailures     func() []ReportFailure
	ReportSecrets      func() []string
	startedAt          time.Time

	mu        sync.Mutex
	seq       uint64
	received  uint64
	failures  uint64
	lastError string
}

func NewServer(sink Sink) *Server {
	return &Server{sink: sink, now: time.Now, startedAt: time.Now()}
}

// Public is what the backend serves: every request must carry the access
// token (Token); a server without one answers nothing.
func (s *Server) Public() http.Handler {
	return cors(s.requireToken(s.routes()))
}

// Handler serves the same routes without the token check. Only tests and
// in-process callers use it; the listener serves Public.
func (s *Server) Handler() http.Handler {
	return cors(s.routes())
}

func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.health)
	mux.HandleFunc("/ready", s.ready)
	mux.HandleFunc("/version", s.version)
	mux.HandleFunc("/observe", s.observe)
	mux.HandleFunc("/diagnostics", s.diagnostics)
	mux.HandleFunc("/diagnostics/report", s.diagnosticReport)
	if s.Token != nil {
		mux.HandleFunc("/settings/token", s.tokenInfo)
		mux.HandleFunc("/settings/token/rotate", s.tokenRotate)
		mux.HandleFunc("/settings/token/check", s.tokenCheck)
		mux.HandleFunc("/settings/token/claim", s.tokenClaim)
	}
	for path, h := range s.Routes {
		mux.HandleFunc(path, h)
	}
	return mux
}

// cors lets the sandboxed plugin call the service. Every request carries
// the access token; where the
// service listens (loopback by default, a Tailscale IP, or all
// addresses) is set separately.
func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, "+TokenHeader)
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		// a public page calling a private address (Tailscale 100.x, LAN)
		// asks this in the preflight (Private Network Access)
		w.Header().Set("Access-Control-Allow-Private-Network", "true")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

const DatabaseHelp = "백엔드가 데이터베이스에 연결되지 않았습니다. 패키지의 시작 파일로 다시 실행해 주세요. 시작 창의 MariaDB 오류와 디스크 여유 공간을 확인한 뒤 다시 시도해 주세요."

func DatabaseUnavailable(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{
		"status": "not_ready", "code": "database_not_ready", "message": DatabaseHelp,
		"components": map[string]any{"database": "not_connected"},
	})
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	if s.DatabaseReady != nil && s.DatabaseReady() != nil {
		DatabaseUnavailable(w, r)
		return
	}
	if err := s.sink.Ready(); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status":     "not_ready",
			"components": map[string]any{"observation_log": err.Error()},
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":     "ready",
		"components": map[string]any{"observation_log": "ok", "database": s.DatabaseReady != nil},
	})
}

func (s *Server) version(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"service":         ServiceName,
		"version":         ServiceVersion,
		"machine_version": MachineVersion,
		"contract":        ContractVersion,
		"connection":      s.Connection,
	})
}

func (s *Server) observe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "POST only")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil || len(body) > maxBodyBytes {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "body unreadable or too large")
		return
	}
	var batch Batch
	if err := json.Unmarshal(body, &batch); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "invalid json")
		return
	}
	if batch.Contract != ContractVersion {
		writeError(w, http.StatusBadRequest, CodeUnsupportedContract, "expected "+ContractVersion)
		return
	}
	stored, resend, code, err := s.Apply(batch)
	if err != nil {
		writeError(w, http.StatusInternalServerError, code, err.Error())
		return
	}
	out := map[string]any{"status": "ok", "stored": stored}
	if resend {
		out["resend_text"] = true
	}
	writeJSON(w, http.StatusOK, out)
}

// Apply stores a batch and applies it, as /observe does: it returns how
// many records were new, whether the plugin should resend texts, and on
// failure the stable error code. A batch already stored is applied again
// (resending is safe).
func (s *Server) Apply(batch Batch) (int, bool, string, error) {
	s.applyMu.Lock()
	defer s.applyMu.Unlock()
	records := s.stamp(batch)
	stored, err := s.sink.Write(records)
	if err != nil {
		s.noteFailure(err)
		return 0, false, CodeStoreWriteFailed, err
	}
	resend := false
	if s.After != nil {
		// every record, also ones already stored: a resent batch may carry
		// observations that were stored but not applied
		if resend, err = s.After.Process(records); err != nil {
			s.noteFailure(err)
			return len(stored), false, CodeApplyFailed, err
		}
	}
	return len(stored), resend, "", nil
}

func (s *Server) diagnostics(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	out := map[string]any{
		"received_events": s.received,
		"write_failures":  s.failures,
		"last_error":      s.lastError,
	}
	s.mu.Unlock()
	if s.After != nil {
		out["lifecycle"] = s.After.Diagnostics()
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) stamp(batch Batch) []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	at := s.now().UTC().Format(time.RFC3339Nano)
	events := batch.Events
	if batch.DroppedBefore > 0 {
		gap, _ := json.Marshal(map[string]any{"hook": "gap", "dropped_before": batch.DroppedBefore})
		events = append([]json.RawMessage{gap}, events...)
	}
	out := make([]Record, 0, len(events))
	for _, ev := range events {
		s.seq++
		out = append(out, Record{ReceivedAt: at, ReceiveSeq: s.seq, PluginVersion: batch.PluginVersion, Event: ev})
	}
	s.received += uint64(len(out))
	return out
}

func (s *Server) noteFailure(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures++
	s.lastError = err.Error()
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, detail string) {
	writeJSON(w, status, map[string]any{"status": "error", "code": code, "detail": detail})
}

var errNotReady = errors.New("observation log not writable")
