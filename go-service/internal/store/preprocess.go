package store

import (
	"encoding/json"
	"errors"
	"strings"

	"archivecenter2/goservice/internal/provider"
	"archivecenter2/goservice/internal/secret"
)

// Preprocessing modes (1.0 E01~E03, user 11-07 "유저 선택은 AI + JEV, AI,
// JEV, 사용안함으로 선택할 수 있게 해야해."): what chooses among the
// records Go ranked. "" is off (the Go basic delivery, the default).
const (
	PreprocessOff   = ""
	PreprocessAI    = "ai"     // AI: the AI removes unrelated lines of the basic delivery (AX)
	PreprocessJev   = "jev"    // JEV: the decision model over the wide list (GXW)
	PreprocessAIJev = "ai_jev" // AI + JEV: removed when both say unrelated (AND)
)

// PreprocessModes are the modes the settings screen offers, in its order.
var PreprocessModes = []string{PreprocessAIJev, PreprocessAI, PreprocessJev, PreprocessOff}

// ErrPreprocessMode is SetPreprocessSettings' answer to an unknown mode.
var ErrPreprocessMode = errors.New("unknown preprocessing mode")

// PreprocessSettings is the preprocessing choice and its AI connection
// (user 11-07 "따로 설정 + 평론가 빌려쓰기"): by default the AI is the
// critic's connection; AIOwn uses the one saved here. The key is sealed.
type PreprocessSettings struct {
	Mode  string
	AIOwn bool
	AI    provider.Config
}

var preprocessSettingNames = []string{"prep_mode", "prep_ai_own", "prep_ai_provider", "prep_ai_model", "prep_ai_url", "prep_ai_reasoning", "prep_ai_options", "prep_ai_key"}

// PreprocessSettings reads the saved choice; the key is opened with box.
func (d *DB) PreprocessSettings(box *secret.Box) (PreprocessSettings, error) {
	var s PreprocessSettings
	args := make([]any, len(preprocessSettingNames))
	for i, n := range preprocessSettingNames {
		args[i] = n
	}
	rows, err := d.sql.Query(`SELECT name, value FROM settings WHERE name IN (`+strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")+`)`, args...)
	if err != nil {
		return s, err
	}
	defer rows.Close()
	for rows.Next() {
		var name, value string
		if err := rows.Scan(&name, &value); err != nil {
			return s, err
		}
		switch name {
		case "prep_mode":
			s.Mode = value
		case "prep_ai_own":
			s.AIOwn = value == "1"
		case "prep_ai_provider":
			s.AI.Provider = value
		case "prep_ai_model":
			s.AI.Model = value
		case "prep_ai_url":
			s.AI.URL = value
		case "prep_ai_reasoning":
			s.AI.Reasoning = value
		case "prep_ai_options":
			if value != "" {
				if err := json.Unmarshal([]byte(value), &s.AI.Options); err != nil {
					return s, err
				}
			}
		case "prep_ai_key":
			if value != "" {
				if s.AI.Key, err = box.Open(value); err != nil {
					return s, err
				}
			}
		}
	}
	return s, rows.Err()
}

// SetPreprocessSettings saves the choice at once (all or none); the key is
// sealed.
func (d *DB) SetPreprocessSettings(box *secret.Box, s PreprocessSettings) error {
	known := false
	for _, m := range PreprocessModes {
		known = known || s.Mode == m
	}
	if !known {
		return ErrPreprocessMode
	}
	if err := s.AI.Options.Check(); err != nil {
		return err
	}
	key := ""
	if k := strings.TrimSpace(s.AI.Key); k != "" {
		var err error
		if key, err = box.Seal(k); err != nil {
			return err
		}
	}
	options, err := json.Marshal(s.AI.Options)
	if err != nil {
		return err
	}
	own := "0"
	if s.AIOwn {
		own = "1"
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for name, v := range map[string]string{"prep_mode": s.Mode, "prep_ai_own": own, "prep_ai_provider": s.AI.Provider, "prep_ai_model": strings.TrimSpace(s.AI.Model),
		"prep_ai_url": strings.TrimSpace(s.AI.URL), "prep_ai_reasoning": s.AI.Reasoning, "prep_ai_options": string(options), "prep_ai_key": key} {
		if _, err := tx.Exec(`INSERT INTO settings (name, value) VALUES (?, ?) ON DUPLICATE KEY UPDATE value=VALUES(value)`, name, v); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PreprocessRecord is what one delivery's preprocessing did, kept with the
// delivery: the mode chosen, what actually decided ("go" when it fell back to
// the basic delivery, with the reason), the calls and their cost, and how many
// records it left out or added.
type PreprocessRecord struct {
	Mode             string `json:"mode"`
	Ran              string `json:"ran"`
	Fallback         string `json:"fallback,omitempty"`
	Error            string `json:"error,omitempty"` // keys masked
	Asked            int    `json:"asked"`           // lines asked about
	DecisionCalls    int    `json:"decision_calls"`
	DecisionRejected int    `json:"decision_rejected"` // too long, asked again in halves
	InputTokens      int64  `json:"input_tokens"`
	AICalls          int    `json:"ai_calls"`
	Removed          int    `json:"removed"`
	Added            int    `json:"added"`
	MS               int64  `json:"ms"`
}
