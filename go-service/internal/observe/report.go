package observe

import (
	"encoding/json"
	"net/http"
	"time"

	"archivecenter2/goservice/internal/diagnostics"
)

// ReportSettings contains only safe settings. Credentials, URLs, custom prompt
// and provider extra-body/header strings have no fields in this projection.
type ReportSettings struct {
	CriticProvider         string `json:"critic_provider"`
	CriticModel            string `json:"critic_model"`
	EmbeddingProvider      string `json:"embedding_provider"`
	EmbeddingModel         string `json:"embedding_model"`
	CriticKeyConfigured    bool   `json:"critic_key_configured"`
	EmbeddingKeyConfigured bool   `json:"embedding_key_configured"`
	Reasoning              string `json:"reasoning"`
	Retries                int    `json:"retries"`
	KnownChars             int    `json:"known_chars"`
	MemoryChars            int    `json:"memory_chars"`
	SecretChars            int    `json:"secret_chars"`
	Placement              string `json:"placement"`
	Transport              string `json:"transport"`
	Error                  string `json:"error,omitempty"`
}

type ReportFailure struct {
	Kind    string    `json:"kind"`
	Time    time.Time `json:"time"`
	Message string    `json:"message"`
}

type PluginError struct {
	Message string `json:"message"`
	Time    string `json:"time"`
}

type PluginReport struct {
	Version            string        `json:"version"`
	HostType           string        `json:"host_type,omitempty"`
	HostVersion        string        `json:"host_version,omitempty"`
	ConnectionHostname string        `json:"connection_hostname"`
	RecentErrors       []PluginError `json:"recent_errors"`
}

func (s *Server) diagnosticReport(w http.ResponseWriter, r *http.Request) {
	var plugin *PluginReport
	switch r.Method {
	case http.MethodGet:
	case http.MethodPost:
		var input struct {
			Plugin *PluginReport `json:"plugin"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&input); err != nil {
			writeError(w, http.StatusBadRequest, CodeBadRequest, "invalid diagnostic metadata")
			return
		}
		plugin = input.Plugin
		if plugin != nil && len(plugin.RecentErrors) > 40 {
			plugin.RecentErrors = plugin.RecentErrors[len(plugin.RecentErrors)-40:]
		}
		if plugin != nil {
			for i := range plugin.RecentErrors {
				plugin.RecentErrors[i].Message = diagnostics.ErrorSummary(plugin.RecentErrors[i].Message)
			}
		}
	default:
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "GET or POST only")
		return
	}
	base := diagnostics.NewReport(s.DataDirectory, ServiceVersion, s.startedAt)
	// Convert typed projections before the final recursive scrub. This makes
	// all strings, including nested files and observed metadata, use one owner.
	out := map[string]any{}
	encoded, _ := json.Marshal(base)
	_ = json.Unmarshal(encoded, &out)
	// A newly started online process still has a measured uptime of zero.
	out["uptime_seconds"] = base.UptimeSeconds
	out["backend_report_available"] = true
	out["machine_version"] = MachineVersion
	out["database_configured"] = s.DatabaseConfigured
	out["token_configured"] = s.Token != nil
	out["diagnostics"] = s.reportCounts()
	if s.ReportSettings != nil {
		out["settings"] = s.ReportSettings()
	}
	if s.ReportFailures != nil {
		failures := s.ReportFailures()
		for i := range failures {
			failures[i].Message = diagnostics.ErrorSummary(failures[i].Message)
		}
		out["recent_failures"] = failures
	}
	if plugin != nil {
		out["plugin"] = plugin
	}
	secrets := []string{}
	if s.Token != nil {
		secrets = append(secrets, s.Token.Value())
	}
	if s.ReportSecrets != nil {
		secrets = append(secrets, s.ReportSecrets()...)
	}
	// Struct/slice fields also become JSON-shaped values for Clean.
	encoded, _ = json.Marshal(out)
	_ = json.Unmarshal(encoded, &out)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, diagnostics.Clean(out, secrets...))
}

func (s *Server) reportCounts() map[string]any {
	s.mu.Lock()
	out := map[string]any{"received_events": s.received, "write_failures": s.failures, "last_error": diagnostics.ErrorSummary(s.lastError)}
	s.mu.Unlock()
	if s.After != nil {
		all := s.After.Diagnostics()
		lifecycle := map[string]any{}
		for _, key := range []string{"snapshots_applied", "resent_skipped", "process_failures", "sessions", "generation_runs", "generation_failures", "generation_waiting_chats", "index_failures", "index_backlog_chats", "generation_retry_waiting_chats", "generation_given_up_turns", "generation_retries", "generation_known_max", "generation_running", "generation_running_chats", "index_running_chats", "index_retry_waiting_chats"} {
			// Counts/flags only, even if another Processor grows text fields.
			switch value := all[key].(type) {
			case int, int64, uint64, float64, bool:
				lifecycle[key] = value
			}
		}
		for _, key := range []string{"process_last_error", "generation_last_error"} {
			if value, ok := all[key].(string); ok {
				lifecycle[key] = diagnostics.ErrorSummary(value)
			}
		}
		out["lifecycle"] = lifecycle
	}
	return out
}
