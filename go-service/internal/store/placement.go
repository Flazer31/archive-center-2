package store

import (
	"database/sql"
	"errors"
	"strconv"
	"strings"

	"archivecenter2/goservice/internal/memory"
)

func (d *DB) MemoryPlacement() (mode, marker string, err error) {
	mode = "auto"
	for name, v := range map[string]*string{"memory_placement": &mode, "memory_anchor": &marker} {
		err = d.sql.QueryRow(`SELECT value FROM settings WHERE name=?`, name).Scan(v)
		if errors.Is(err, sql.ErrNoRows) {
			err = nil
		}
		if err != nil {
			return
		}
	}
	mode = memory.PlacementMode(mode)
	return
}

func (d *DB) SetMemorySettings(chars, secret int, mode, marker string, transport ...string) error {
	if chars < 0 || secret < 0 {
		return ErrNegativeBudget
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	values := map[string]string{"memory_chars": strconv.Itoa(chars), "secret_chars": strconv.Itoa(secret), "memory_placement": memory.PlacementMode(mode), "memory_anchor": strings.TrimSpace(marker)}
	if len(transport) > 0 {
		values["memory_transport_mode"] = MemoryTransportMode(transport[0])
	}
	for name, value := range values {
		if _, err := tx.Exec(`INSERT INTO settings (name,value) VALUES (?,?) ON DUPLICATE KEY UPDATE value=VALUES(value)`, name, value); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// MemoryTransportMode follows placement settings: an unsupported choice uses the default.
func MemoryTransportMode(mode string) string {
	if mode == "google_pdf" || mode == "llm_gateway_pdf" || mode == "provider_manager_pdf" {
		return mode
	}
	return "text"
}

func (d *DB) MemoryTransport() (string, error) {
	var mode string
	err := d.sql.QueryRow(`SELECT value FROM settings WHERE name=?`, "memory_transport_mode").Scan(&mode)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return MemoryTransportMode(mode), err
}
