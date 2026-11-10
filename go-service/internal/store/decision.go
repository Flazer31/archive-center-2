package store

import (
	"strings"

	"archivecenter2/goservice/internal/secret"
)

// DecisionSettings is the decision-model connection (1.0 E02: Clef or Jev
// over System One), set on the settings screen; the key is sealed.
type DecisionSettings struct {
	Provider string // "" = none set
	URL      string // "" = the provider's default
	Model    string // "" = the provider's default
	Key      string
}

var decisionSettingNames = []string{"decision_provider", "decision_url", "decision_model", "decision_key"}

// DecisionSettings reads the saved connection; the key is opened with box.
func (d *DB) DecisionSettings(box *secret.Box) (DecisionSettings, error) {
	var s DecisionSettings
	rows, err := d.sql.Query(`SELECT name, value FROM settings WHERE name IN (?,?,?,?)`, decisionSettingNames[0], decisionSettingNames[1], decisionSettingNames[2], decisionSettingNames[3])
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
		case "decision_provider":
			s.Provider = value
		case "decision_url":
			s.URL = value
		case "decision_model":
			s.Model = value
		case "decision_key":
			if value != "" {
				if s.Key, err = box.Open(value); err != nil {
					return s, err
				}
			}
		}
	}
	return s, rows.Err()
}

// SetDecisionSettings saves the connection at once; the key is sealed.
func (d *DB) SetDecisionSettings(box *secret.Box, s DecisionSettings) error {
	key := ""
	if k := strings.TrimSpace(s.Key); k != "" {
		var err error
		if key, err = box.Seal(k); err != nil {
			return err
		}
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for name, v := range map[string]string{"decision_provider": s.Provider, "decision_url": strings.TrimSpace(s.URL), "decision_model": strings.TrimSpace(s.Model), "decision_key": key} {
		if _, err := tx.Exec(`INSERT INTO settings (name, value) VALUES (?, ?) ON DUPLICATE KEY UPDATE value=VALUES(value)`, name, v); err != nil {
			return err
		}
	}
	return tx.Commit()
}
