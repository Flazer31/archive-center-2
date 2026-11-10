package main

import (
	"archivecenter2/goservice/internal/datapath"
	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/secret"
	"os"
	"path/filepath"
	"testing"
)

// Exercises the existing file owners through the selected directory. This is
// file persistence evidence, not a running MariaDB or package-update test.
func TestSharedDataReopensCredentialsAcrossPackageRoots(t *testing.T) {
	shared := t.TempDir()
	t.Setenv("AC2_DATA_DIR", shared)
	var tokenValue, sealed string
	var original installConfig
	var keyBytes string
	for i := 0; i < 2; i++ {
		data, err := datapath.Resolve("", t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		token, made, err := observe.LoadToken(filepath.Join(data, "token.txt"))
		if err != nil {
			t.Fatal(err)
		}
		box, err := secret.LoadBox(filepath.Join(data, "secret.key"))
		if err != nil {
			t.Fatal(err)
		}
		config, err := installSettings(data, 3123)
		if err != nil {
			t.Fatal(err)
		}
		key, err := os.ReadFile(filepath.Join(data, "secret.key"))
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if !made {
				t.Fatal("first token was not created")
			}
			tokenValue = token.Value()
			original = config
			keyBytes = string(key)
			sealed, err = box.Seal("synthetic-api-key")
			if err != nil {
				t.Fatal(err)
			}
		} else {
			if made || token.Value() != tokenValue || config != original || string(key) != keyBytes {
				t.Fatal("reopen replaced persisted file data")
			}
			plain, err := box.Open(sealed)
			if err != nil || plain != "synthetic-api-key" {
				t.Fatal("persisted key cannot open the original sealed value", err)
			}
		}
	}
}
