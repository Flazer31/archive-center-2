package store

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"archivecenter2/goservice/internal/provider"
	"archivecenter2/goservice/internal/secret"
)

// ModelSettings are the critic's (memory generation) and the embedding's
// connections as the settings screen saves them. Keys are sealed in the
// database and plain only in memory.
type ModelSettings struct {
	provider.Options
	// CriticSet: the settings screen saved a critic choice ("사용 안 함"
	// included); until then the -gen-* flags serve
	CriticSet bool
	Provider  string // "" = none: generation waits
	Model     string
	URL       string
	Reasoning string
	Retries   int // more calls after a failed one (user setting, default 3)
	// KnownChars is the ceiling on a generation call's whole request, in
	// chars (0 = none; user setting, default DefaultKnownChars); the stored
	// memory it reads gets what the rest leaves (generate.turn)
	KnownChars int
	Key        string

	EmbedProvider string // "" = not set (Voyage, with the -embed-model default)
	EmbedModel    string // "" = not set (the provider's default model)
	EmbedURL      string // "" = the provider's default address
	EmbedKey      string
}

// DefaultRetries is the retry count before the user changes it (10-23).
const DefaultRetries = 3

// DefaultKnownChars is the ceiling on the whole generation request (chars)
// before the user changes it. With the ceiling on
// the whole request, 50,000 kept all 21 stored scenes under 35,000 actual
// tokens with 34 of the critic's 41 references still shown (the earlier
// 40,000 on stored memory alone: 7 scenes over, 35 shown). A value saved
// earlier is kept.
const DefaultKnownChars = 50000

var modelSettingNames = []string{"critic_options", "critic_provider", "critic_model", "critic_url", "critic_reasoning", "critic_retries", "critic_known_chars", "critic_key", "embed_provider", "embed_model", "embed_url", "embed_key"}

// ModelSettings reads the saved connections; keys are opened with box.
func (d *DB) ModelSettings(box *secret.Box) (ModelSettings, error) {
	m := ModelSettings{Retries: DefaultRetries, KnownChars: DefaultKnownChars}
	args := make([]any, len(modelSettingNames))
	for i, n := range modelSettingNames {
		args[i] = n
	}
	rows, err := d.sql.Query(`SELECT name, value FROM settings WHERE name IN (`+strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")+`)`, args...)
	if err != nil {
		return m, err
	}
	defer rows.Close()
	for rows.Next() {
		var name, value string
		if err := rows.Scan(&name, &value); err != nil {
			return m, err
		}
		switch name {
		case "critic_options":
			if err := json.Unmarshal([]byte(value), &m.Options); err != nil {
				return m, err
			}
		case "critic_provider":
			m.Provider, m.CriticSet = value, true
		case "critic_model":
			m.Model = value
		case "critic_url":
			m.URL = value
		case "critic_reasoning":
			m.Reasoning = value
		case "critic_retries":
			if n, err := strconv.Atoi(value); err == nil {
				m.Retries = n
			}
		case "critic_known_chars":
			if n, err := strconv.Atoi(value); err == nil {
				m.KnownChars = n
			}
		case "critic_key", "embed_key":
			if value == "" {
				continue
			}
			plain, err := box.Open(value)
			if err != nil {
				return m, err
			}
			if name == "critic_key" {
				m.Key = plain
			} else {
				m.EmbedKey = plain
			}
		case "embed_provider":
			m.EmbedProvider = value
		case "embed_model":
			m.EmbedModel = value
		case "embed_url":
			m.EmbedURL = value
		}
	}
	return m, rows.Err()
}

// ErrBadRetries is SetModelSettings' answer to a retry count outside 0-10.
var ErrBadRetries = errors.New("retries must be 0 to 10")

// ErrBadKnownChars is SetModelSettings' answer to a negative reading ceiling.
var ErrBadKnownChars = errors.New("known chars must not be negative")

// SetModelSettings saves the connections at once (all or none); keys are
// sealed with box.
func (d *DB) SetModelSettings(box *secret.Box, m ModelSettings) error {
	if err := m.Options.Check(); err != nil {
		return err
	}
	if m.Retries < 0 || m.Retries > 10 {
		return ErrBadRetries
	}
	if m.KnownChars < 0 {
		return ErrBadKnownChars
	}
	seal := func(k string) (string, error) {
		if strings.TrimSpace(k) == "" {
			return "", nil
		}
		return box.Seal(k)
	}
	ck, err := seal(m.Key)
	if err != nil {
		return err
	}
	ek, err := seal(m.EmbedKey)
	if err != nil {
		return err
	}
	options, err := json.Marshal(m.Options)
	if err != nil {
		return err
	}
	values := map[string]string{
		"critic_options":  string(options),
		"critic_provider": m.Provider, "critic_model": strings.TrimSpace(m.Model), "critic_url": strings.TrimSpace(m.URL),
		"critic_reasoning": m.Reasoning, "critic_retries": strconv.Itoa(m.Retries), "critic_known_chars": strconv.Itoa(m.KnownChars), "critic_key": ck,
		"embed_provider": m.EmbedProvider, "embed_model": m.EmbedModel, "embed_url": strings.TrimSpace(m.EmbedURL), "embed_key": ek,
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if !m.CriticSet {
		delete(values, "critic_provider")
		if _, err := tx.Exec(`DELETE FROM settings WHERE name='critic_provider'`); err != nil {
			return err
		}
	}
	for name, v := range values {
		if _, err := tx.Exec(`INSERT INTO settings (name, value) VALUES (?, ?) ON DUPLICATE KEY UPDATE value=VALUES(value)`, name, v); err != nil {
			return err
		}
	}
	return tx.Commit()
}
