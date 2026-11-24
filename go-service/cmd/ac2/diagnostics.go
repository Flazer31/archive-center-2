package main

import (
	"archivecenter2/goservice/internal/diagnostics"
	"archivecenter2/goservice/internal/observe"
)

// diagnosticSettings builds a new allowlist; the credential-bearing models
// view and saved custom provider/prompt options never become report inputs.
func (m *models) diagnosticSettings() observe.ReportSettings {
	m.mu.RLock()
	saved := m.saved
	m.mu.RUnlock()
	critic, source := m.critic(saved)
	embed, _ := m.embedding(saved)
	retries := saved.Retries
	if source == "flags" {
		retries = m.flagRetries
	}
	v := observe.ReportSettings{CriticProvider: critic.Provider, CriticModel: critic.Model, EmbeddingProvider: embed.Provider, EmbeddingModel: embed.Model, Reasoning: critic.Reasoning, Retries: retries, KnownChars: saved.KnownChars}
	v.CriticKeyConfigured, v.EmbeddingKeyConfigured = critic.Key != "", embed.Key != ""
	var err error
	v.MemoryChars, v.SecretChars, err = m.db.MemoryBudgets()
	if err != nil {
		v.Error = diagnostics.ErrorSummary(err.Error())
		return v
	}
	v.Placement, _, err = m.db.MemoryPlacement()
	if err != nil {
		v.Error = diagnostics.ErrorSummary(err.Error())
		return v
	}
	v.Transport, err = m.db.MemoryTransport()
	if err != nil {
		v.Error = diagnostics.ErrorSummary(err.Error())
	}
	return v
}

func (m *models) diagnosticSecrets() []string {
	m.mu.RLock()
	keys := []string{m.saved.Key, m.saved.EmbedKey, m.flagCritic.Key, m.envKey}
	m.mu.RUnlock()
	if decision, err := m.db.DecisionSettings(m.box); err == nil {
		keys = append(keys, decision.Key)
	}
	if prep, err := m.db.PreprocessSettings(m.box); err == nil {
		keys = append(keys, prep.AI.Key)
	}
	if narrative, err := m.db.NarrativeSettings(m.box); err == nil {
		keys = append(keys, narrative.AI.Key)
	}
	return keys
}

func (s *status) diagnosticFailures() []observe.ReportFailure {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []observe.ReportFailure{}
	for _, e := range s.events {
		switch e.Kind {
		case "failed", "given_up", "index_failed", "query_failed":
			out = append(out, observe.ReportFailure{Kind: e.Kind, Time: e.At, Message: e.Detail})
		}
	}
	return out
}
