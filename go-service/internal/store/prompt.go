package store

import (
	"database/sql"
	"errors"
)

// CriticPrompt returns only the instruction override, never KNOWN/turn data.
// An absent row means the code's current default.
func (d *DB) CriticPrompt() (string, error) { return d.promptText("critic_prompt") }

func (d *DB) SetCriticPrompt(text string) error { return d.setPromptText("critic_prompt", text) }

// PublisherPrompt is the narrative guide's instruction override (user 11-09,
// like AC1's editable publisher prompt): the rules above each level's item
// list, never the items or the output shape. "" means the code's default.
func (d *DB) PublisherPrompt() (string, error) { return d.promptText("publisher_prompt") }

func (d *DB) SetPublisherPrompt(text string) error { return d.setPromptText("publisher_prompt", text) }

func (d *DB) promptText(name string) (string, error) {
	var text string
	err := d.sql.QueryRow(`SELECT value FROM settings WHERE name=?`, name).Scan(&text)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return text, err
}

func (d *DB) setPromptText(name, text string) error {
	if text == "" {
		_, err := d.sql.Exec(`DELETE FROM settings WHERE name=?`, name)
		return err
	}
	_, err := d.sql.Exec(`INSERT INTO settings (name,value) VALUES (?,?) ON DUPLICATE KEY UPDATE value=VALUES(value)`, name, text)
	return err
}
