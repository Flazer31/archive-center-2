package observe

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// tokenShaped has the access-token form so masking is tested; built from
// two parts so the source never holds a literal token-shaped value.
var tokenShaped = "ac2-" + "abcdef-ghijkl-mnopqr-stuvwx"

type reportProcessor struct{}

func (reportProcessor) Process([]Record) (bool, error) { return false, nil }
func (reportProcessor) Diagnostics() map[string]any {
	return map[string]any{"sessions": 3, "generation_runs": 2, "generation_last_error": "provider 503: private-memory-echo", "prompt": "private-prompt-marker", "chat_name": "private-chat-name", "generation_known_max": "private-type-abuse"}
}

// Error text (last error, failures, the plugin's
// recent errors) is shown as written with credentials masked; fields that are
// not errors (prompt, chat name, raw prompt, memory) are still never exported.
func TestDiagnosticReportProductionRouteAuthShowsErrorTextMasksCredentials(t *testing.T) {
	s := NewServer(&memSink{})
	s.Token = FixedToken("opaque-active-token")
	s.After = reportProcessor{}
	s.DataDirectory = "nonexistent-report-data"
	s.ReportSettings = func() ReportSettings {
		return ReportSettings{CriticProvider: "test", CriticModel: "sk-private-model-key", Retries: 4, MemoryChars: 2000, Transport: "text"}
	}
	s.ReportSecrets = func() []string { return []string{"opaque-key-value"} }
	s.ReportFailures = func() []ReportFailure {
		return []ReportFailure{{Kind: "failed", Time: time.Now(), Message: "provider 400: private-conversation-echo"}}
	}
	s.noteFailure(errors.New("private-sql-memory-echo"))
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		body := ""
		if method == http.MethodPost {
			body = `{"plugin":{"version":"opaque-active-token","host_type":"RisuAI","connection_hostname":"localhost","recent_errors":[{"message":"private-host-echo key=opaque-key-value ` + tokenShaped + `","time":"now"}],"raw_prompt":"private-plugin-prompt"},"memory":"private-plugin-memory"}`
		}
		if got := send(s.Public(), method, "/diagnostics/report", "", body); got.Code != 401 {
			t.Fatal("report bypassed existing auth")
		}
		got := send(s.Public(), method, "/diagnostics/report", s.Token.Value(), body)
		if got.Code != 200 || got.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("report status %d", got.Code)
		}
		text := got.Body.String()
		for _, secret := range []string{"opaque-active-token", "opaque-key-value", "sk-private-model-key", tokenShaped, "private-prompt-marker", "private-chat-name", "private-type-abuse", "private-plugin-prompt", "private-plugin-memory"} {
			if strings.Contains(text, secret) {
				t.Fatalf("report leaked %q", secret)
			}
		}
		// error text is shown whole, with the credential next to it masked
		for _, shown := range []string{"provider 503: private-memory-echo", "provider 400: private-conversation-echo", "private-sql-memory-echo", "private-host-echo key=[가림]"} {
			if method == http.MethodGet && strings.Contains(shown, "private-host-echo") {
				continue // the plugin errors arrive only in the POST body
			}
			if !strings.Contains(text, shown) {
				t.Fatalf("report dropped error text %q: %s", shown, text)
			}
		}
		var v map[string]any
		if err := json.Unmarshal(got.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		if v["backend_report_available"] != true || v["diagnostics"].(map[string]any)["lifecycle"].(map[string]any)["sessions"] != float64(3) {
			t.Fatal("safe scalar counts lost")
		}
	}
}

func TestDiagnosticReportPluginErrorLimitAndOversize(t *testing.T) {
	s := NewServer(&memSink{})
	errors := make([]PluginError, 45)
	for i := range errors {
		errors[i] = PluginError{Message: "synthetic failure", Time: "now"}
	}
	b, _ := json.Marshal(map[string]any{"plugin": PluginReport{RecentErrors: errors}})
	got := do(t, s.Handler(), "POST", "/diagnostics/report", string(b))
	var out struct {
		Plugin PluginReport `json:"plugin"`
	}
	if got.Code != 200 || json.Unmarshal(got.Body.Bytes(), &out) != nil || len(out.Plugin.RecentErrors) != 40 {
		t.Fatal("plugin error bound failed")
	}
	if got := do(t, s.Handler(), "POST", "/diagnostics/report", `{"plugin":{"version":"`+strings.Repeat("x", 4<<20)+`"}}`); got.Code != 400 {
		t.Fatal("oversize metadata accepted")
	}
	if got := do(t, s.Handler(), "DELETE", "/diagnostics/report", ""); got.Code != 405 {
		t.Fatal("method contract failed")
	}
}

func TestDiagnosticReportIncludesInitialZeroUptime(t *testing.T) {
	s := NewServer(&memSink{})
	// A future timestamp makes the bounded elapsed value deterministically 0.
	s.startedAt = time.Now().Add(time.Hour)
	got := do(t, s.Handler(), "GET", "/diagnostics/report", "")
	var out map[string]any
	if got.Code != http.StatusOK || json.Unmarshal(got.Body.Bytes(), &out) != nil {
		t.Fatal("report unavailable")
	}
	uptime, exists := out["uptime_seconds"]
	if !exists || uptime != float64(0) {
		t.Fatalf("initial uptime missing or wrong: %v", out)
	}
}
