package store

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestMariaDBMinimumVersion(t *testing.T) {
	for _, version := range []string{"10.5.27-MariaDB", "10.6.8-MariaDB", "10.6.17-MariaDB", "10.6", "10.11.7-MariaDB", "11.3.2-MariaDB", "11.4.1-MariaDB", "11.5.1-MariaDB", "10.4.34-MariaDB", "9.8.1", "", "unknown"} {
		t.Run(version, func(t *testing.T) {
			err := checkMariaDBVersion(version)
			if err == nil || !strings.Contains(err.Error(), MinimumMariaDB) || !strings.Contains(err.Error(), version) {
				t.Fatalf("must report required and actual versions for %q: %v", version, err)
			}
			// typed, so the start window can name the version (it holds no credentials)
			var old VersionError
			if !errors.As(err, &old) || old.Version != version {
				t.Fatalf("not a VersionError for %q: %v", version, err)
			}
		})
	}
	// 1.1: vectors are compared in Go; the floor is each series' first release with snapshot isolation
	for _, version := range []string{"10.6.18-MariaDB", "10.6.20-MariaDB", "10.11.8-MariaDB", "10.11.14-MariaDB-0ubuntu0.24.04.1", "11.0.6-MariaDB", "11.1.5-MariaDB", "11.2.4-MariaDB", "11.4.2-MariaDB", "11.5.2-MariaDB", "11.6.0-MariaDB", "11.8", "11.4.10-MariaDB", "11.8.9-MariaDB-ubu2404", "12.3.2-MariaDB"} {
		t.Run(version, func(t *testing.T) {
			if err := checkMariaDBVersion(version); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestEmbeddingVectorColumnAndCosine(t *testing.T) {
	db := testDB(t)
	var version string
	if err := db.sql.QueryRow(`SELECT VERSION()`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	t.Logf("actual MariaDB version: %s", version)
	a, b := []float32{1, 2, -3}, []float32{4, -2, 1}
	ha, hb := TextHash("문서 하나"), TextHash("문서 둘")
	if have, err := db.HaveEmbeddings("m", []string{ha}); err != nil || len(have) != 0 {
		t.Fatalf("unknown model: %v %v", have, err)
	}
	var tables int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name LIKE 'embedding_vectors_%'`).Scan(&tables); err != nil || tables != 0 {
		t.Fatalf("vector tables must be lazy: %d %v", tables, err)
	}
	if err := db.SaveEmbeddings("m", []string{ha, hb}, [][]float32{a, b}); err != nil {
		t.Fatal(err)
	}
	// 1.1: plain bytes, compared in Go (no MariaDB vector type needed)
	var columnType string
	if err := db.sql.QueryRow(`SELECT column_type FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name='embedding_vectors_3' AND column_name='vector'`).Scan(&columnType); err != nil || strings.ToLower(columnType) != "mediumblob" {
		t.Fatalf("stored type %q, %v", columnType, err)
	}
	var indexes int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name='embedding_vectors_3' AND index_name<>'PRIMARY'`).Scan(&indexes); err != nil || indexes != 0 {
		t.Fatalf("unexpected vector index: %d %v", indexes, err)
	}
	got, ok, err := db.Embedding("m", ha)
	if err != nil || !ok || !reflect.DeepEqual(got, a) {
		t.Fatalf("binary readback %v %v %v", got, ok, err)
	}
	have, err := db.HaveEmbeddings("m", []string{ha, hb, TextHash("missing")})
	if err != nil || len(have) != 2 || !have[ha] || !have[hb] {
		t.Fatalf("have %v %v", have, err)
	}
	if err := db.SaveEmbeddings("m", []string{ha}, [][]float32{b}); err != nil {
		t.Fatal(err)
	}
	got, ok, err = db.Embedding("m", ha)
	if err != nil || !ok || !reflect.DeepEqual(got, a) {
		t.Fatalf("existing vector overwritten %v %v %v", got, ok, err)
	}
	if err := db.SaveEmbeddings("other", []string{ha}, [][]float32{{7, 8}}); err != nil {
		t.Fatal(err)
	}
	got, ok, err = db.Embedding("other", ha)
	if err != nil || !ok || !reflect.DeepEqual(got, []float32{7, 8}) {
		t.Fatalf("different model/dimension %v %v %v", got, ok, err)
	}
	if _, ok, err := db.Embedding("absent", ha); err != nil || ok {
		t.Fatalf("unknown model read: %v %v", ok, err)
	}
}

func TestEmbeddingRejectsDimensionChange(t *testing.T) {
	db := testDB(t)
	hash, fresh := TextHash("original"), TextHash("fresh")
	original := []float32{1, 2}
	if err := db.SaveEmbeddings("m", []string{hash}, [][]float32{original}); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveEmbeddings("m", []string{hash, fresh}, [][]float32{{3, 4, 5}, {6, 7, 8}}); err == nil {
		t.Fatal("accepted another dimension for the same model")
	}
	got, ok, err := db.Embedding("m", hash)
	if err != nil || !ok || !reflect.DeepEqual(got, original) {
		t.Fatalf("dimension failure changed original: %v %v %v", got, ok, err)
	}
	if _, ok, err := db.Embedding("m", fresh); err != nil || ok {
		t.Fatalf("dimension failure saved new vector: %v %v", ok, err)
	}
	if err := db.SaveEmbeddings("new", []string{hash, fresh}, [][]float32{{1, 2}, {3, 4, 5}}); err == nil {
		t.Fatal("accepted mixed dimensions in first batch")
	}
	if err := db.SaveEmbeddings("new", []string{hash}, [][]float32{{9, 8, 7}}); err != nil {
		t.Fatalf("failed batch registered a model: %v", err)
	}
}
