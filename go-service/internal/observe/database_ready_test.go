package observe

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestReadyReportsDatabaseDependencyAndRecovery(t *testing.T) {
	s := NewServer(&memSink{})
	err := errors.New("private connection detail must not be exposed")
	s.DatabaseReady = func() error { return err }
	h := s.Handler()
	r := do(t, h, http.MethodGet, "/ready", "")
	if r.Code != http.StatusServiceUnavailable || code(t, r) != "database_not_ready" || !strings.Contains(r.Body.String(), DatabaseHelp) {
		t.Fatalf("DB absent is not explained: %d %s", r.Code, r.Body.String())
	}
	if strings.Contains(r.Body.String(), err.Error()) {
		t.Fatal("connection detail leaked")
	}
	// Readiness is re-evaluated, without a new rejection around observations.
	if got := do(t, h, http.MethodGet, "/health", ""); got.Code != http.StatusOK {
		t.Fatal(got.Code)
	}
	err = nil
	if got := do(t, h, http.MethodGet, "/ready", ""); got.Code != http.StatusOK {
		t.Fatal(got.Body.String())
	}
}
