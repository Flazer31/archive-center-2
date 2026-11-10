package store

import (
	"database/sql"
	"errors"
	"strconv"
)

// Memory budgets: ceilings of the memory a request gets. The user changes
// them in the settings screen (step 9); the backend owns them.
const (
	DefaultMemoryChars = 2000
	DefaultSecretChars = 4000
)

var settingsSchema = []string{
	`CREATE TABLE IF NOT EXISTS settings (
		name VARCHAR(64) PRIMARY KEY,
		value TEXT NOT NULL
	) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`,
}

func init() { schema = append(schema, settingsSchema...) }

// MemoryBudgets reads the memory ceilings (defaults when not set).
func (d *DB) MemoryBudgets() (chars, secret int, err error) {
	chars, secret = DefaultMemoryChars, DefaultSecretChars
	for name, v := range map[string]*int{"memory_chars": &chars, "secret_chars": &secret} {
		var s string
		switch err := d.sql.QueryRow(`SELECT value FROM settings WHERE name=?`, name).Scan(&s); {
		case errors.Is(err, sql.ErrNoRows):
			continue
		case err != nil:
			return 0, 0, err
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			return 0, 0, err
		}
		*v = n
	}
	return chars, secret, nil
}

// ErrNegativeBudget is SetMemoryBudgets' answer to a negative ceiling.
var ErrNegativeBudget = errors.New("a memory ceiling cannot be negative")

// SetMemoryBudgets stores the memory ceilings.
func (d *DB) SetMemoryBudgets(chars, secret int) error {
	if chars < 0 || secret < 0 {
		return ErrNegativeBudget
	}
	for name, v := range map[string]int{"memory_chars": chars, "secret_chars": secret} {
		if _, err := d.sql.Exec(`INSERT INTO settings (name, value) VALUES (?, ?) ON DUPLICATE KEY UPDATE value=VALUES(value)`, name, strconv.Itoa(v)); err != nil {
			return err
		}
	}
	return nil
}
