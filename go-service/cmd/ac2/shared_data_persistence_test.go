package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"archivecenter2/goservice/internal/datapath"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/secret"
	"archivecenter2/goservice/internal/store"
)

// Reopens the production stores from another package root. This deliberately
// does not claim process restart, managed-database restart, or old-data migration.
func TestSharedDataSettingsAndMemoryReopen(t *testing.T) {
	for _, mode := range []string{"explicit", "common-default"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			common := filepath.Join(root, "common")
			getenv := func(k string) string {
				if k == "LOCALAPPDATA" {
					return common
				}
				return ""
			}
			explicit := ""
			if mode == "explicit" {
				explicit = filepath.Join(common, "chosen")
			}
			first, err := datapath.ResolveFor(explicit, filepath.Join(root, "package-a"), "windows", getenv)
			if err != nil {
				t.Fatal(err)
			}
			second, err := datapath.ResolveFor(explicit, filepath.Join(root, "package-b"), "windows", getenv)
			if err != nil || first != second {
				t.Fatalf("package roots changed data path: %q %q %v", first, second, err)
			}
			db := modelsDB(t)
			box, err := secret.LoadBox(filepath.Join(first, "secret.key"))
			if err != nil {
				t.Fatal(err)
			}
			token, created, err := observe.LoadToken(filepath.Join(first, "token.txt"))
			if err != nil || !created {
				t.Fatalf("first token: created=%v err=%v", created, err)
			}
			keyBefore, err := os.ReadFile(filepath.Join(first, "secret.key"))
			if err != nil {
				t.Fatal(err)
			}
			mods := &models{db: db, box: box}
			const syntheticKey = "synthetic-shared-settings-key-1234"
			if code, _, raw := callModels(t, mods.handler(), "POST", `{"critic":{"provider":"openai","model":"gpt-5-mini","reasoning":"low","retries":4,"known_chars":12345,"key":"`+syntheticKey+`"}}`); code != 200 {
				t.Fatalf("save: %d %s", code, raw)
			}
			saved, err := db.ModelSettings(box)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.SetMemoryBudgets(1357, 2468); err != nil {
				t.Fatal(err)
			}
			job, err := db.JobFor(memory.Source{Session: "synthetic-shared-data", Turn: 1})
			if err != nil {
				t.Fatal(err)
			}
			const body = "A synthetic traveler left a blue notebook at the station."
			if _, err := db.SaveGeneration(job, store.Generation{Model: "synthetic-no-provider", Output: memory.Output{Memories: []memory.Memory{{ID: "m1", Kind: "event", Body: body}}}}); err != nil {
				t.Fatal(err)
			}
			before, err := db.ItemsOf([]int64{job.ID})
			if err != nil || len(before.Memories) != 1 {
				t.Fatalf("memory save: %+v %v", before, err)
			}
			var sealed string
			if err := dbSQL(t, db).QueryRow("SELECT value FROM settings WHERE name='critic_key'").Scan(&sealed); err != nil {
				t.Fatal(err)
			}
			if sealed == syntheticKey || !bytes.HasPrefix([]byte(sealed), []byte("v1:")) {
				t.Fatal("settings key was not sealed")
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := store.Open(os.Getenv("AC2_TEST_MARIADB_DSN") + "ac2_test_models?parseTime=true")
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			box2, err := secret.LoadBox(filepath.Join(second, "secret.key"))
			if err != nil {
				t.Fatal(err)
			}
			after, err := reopened.ModelSettings(box2)
			if err != nil || !reflect.DeepEqual(saved, after) || after.Key != syntheticKey {
				t.Fatalf("settings changed on reopen: %v", err)
			}
			c, s, err := reopened.MemoryBudgets()
			if err != nil || c != 1357 || s != 2468 {
				t.Fatalf("budgets changed: %d %d %v", c, s, err)
			}
			items, err := reopened.ItemsOf([]int64{job.ID})
			if err != nil || !reflect.DeepEqual(before, items) {
				t.Fatalf("memory changed on reopen: %v", err)
			}
			tok2, created, err := observe.LoadToken(filepath.Join(second, "token.txt"))
			if err != nil || created || tok2.Value() != token.Value() {
				t.Fatalf("token changed: created=%v err=%v", created, err)
			}
			keyAfter, err := os.ReadFile(filepath.Join(second, "secret.key"))
			if err != nil || !bytes.Equal(keyBefore, keyAfter) {
				t.Fatal("encryption key changed")
			}
			t.Log("model settings, sealed key, budgets, token, and memory preserved across DB/Box reopen from another package root")
		})
	}
}
