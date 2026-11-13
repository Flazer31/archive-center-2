package store

import (
	"encoding/binary"
	"math"
	"reflect"
	"strings"
	"testing"

	"archivecenter2/goservice/internal/memory"
)

// setVector overwrites the stored vector bytes of one record.
func setVector(t *testing.T, db *DB, id string, v []float32) {
	t.Helper()
	b := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(x))
	}
	_, err := db.sql.Exec("UPDATE embedding_vectors_3 e JOIN memories m ON e.model='m' AND e.text_hash=m.doc_hash SET e.`vector`=CAST(? AS BINARY) WHERE m.id=?", b, strings.TrimPrefix(id, "M"))
	if err != nil {
		t.Fatal(err)
	}
}

// Similarities are computed in Go from the stored bytes (1.1), on a table
// made now (MEDIUMBLOB): the same values as MariaDB's VEC_DISTANCE_COSINE
// where the server has it; a record without a vector has none; a zero query
// has none and nothing fails. Stored vectors are not of unit length, so the
// norms are part of what is compared.
func TestSimilaritiesInGo(t *testing.T) { similaritiesCase(t, false) }

// The same on a vector table made before 1.1 (a VECTOR column, same bytes):
// existing installs keep working without converting their tables.
func TestSimilaritiesInGoOldTable(t *testing.T) { similaritiesCase(t, true) }

func similaritiesCase(t *testing.T, old bool) {
	db, _, ids := deliverChat(t)
	var version string
	if err := db.sql.QueryRow(`SELECT VERSION()`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	_, vecErr := db.sql.Exec(`SELECT VEC_DISTANCE_COSINE(VEC_FromText('[1]'), VEC_FromText('[1]'))`)
	hasVec := vecErr == nil
	if old {
		if !hasVec {
			t.Skipf("MariaDB %s has no VECTOR type", version)
		}
		if _, err := db.sql.Exec("ALTER TABLE embedding_vectors_3 MODIFY `vector` VECTOR(3) NOT NULL"); err != nil {
			t.Fatal(err)
		}
	}
	query := []float32{0.6, 0.8, 0.1}
	var all []string
	stored := [][]float32{{2, 1, 0.5}, {0, 3, -1}, {-1.5, 0.2, 4}}
	n := 0
	for _, id := range ids {
		all = append(all, id)
		setVector(t, db, id, stored[n%len(stored)])
		n++
	}
	got, err := db.similarities("m", query, append(all, "M999999")) // the last has no record
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(ids) {
		t.Fatalf("similarities for %d of %d records: %v", len(got), len(ids), got)
	}
	if hasVec {
		for name, id := range ids {
			var sql float64
			err := db.sql.QueryRow("SELECT 1 - VEC_DISTANCE_COSINE(e.`vector`, VEC_FromText('[0.6,0.8,0.1]')) FROM memories m JOIN embedding_vectors_3 e ON e.model='m' AND e.text_hash=m.doc_hash WHERE m.id=?", strings.TrimPrefix(id, "M")).Scan(&sql)
			if err != nil {
				t.Fatal(err)
			}
			// MariaDB computes in float32; Go in float64 from the same bytes
			if math.Abs(got[id]-sql) > 1e-6 {
				t.Fatalf("%s: Go %.9g, SQL %.9g", name, got[id], sql)
			}
		}
		t.Logf("MariaDB %s: %d similarities equal to VEC_DISTANCE_COSINE within 1e-6", version, len(ids))
	}
	if z, err := db.similarities("m", []float32{0, 0, 0}, all); err != nil || len(z) != 0 {
		t.Fatalf("zero query: %v %v", z, err)
	}
	if z, err := db.similarities("m", []float32{float32(math.NaN()), 1, 0}, all); err == nil {
		t.Fatalf("non-finite query compared: %v", z)
	}
	if old {
		return // a VECTOR column holds only finite vectors of its size
	}
	// stored vectors that cannot be compared have no similarity; the rest
	// still do, and nothing fails
	bad := [][]float32{{0, 0, 0}, {float32(math.NaN()), 1, 0}, {1, 0}}
	n = 0
	for _, id := range ids {
		setVector(t, db, id, bad[n%len(bad)])
		n++
	}
	got, err = db.similarities("m", query, all)
	if err != nil || len(got) != 0 {
		t.Fatalf("uncomparable stored vectors: %v %v", got, err)
	}
}

// A stored vector that cannot be compared (NaN) is read as no vector by the
// whole delivery — ranking and scene contrast — and the others keep their
// places: the delivery is the one made with that vector absent (a NaN once
// spread through the contrast mean).
func TestUncomparableVectorIsNoVector(t *testing.T) {
	db, s, ids := deliverChat(t)
	scope := memory.ScopeOf(s)
	stored := map[string][]float32{"long": {2, 1, 0.5}, "short": {0, 3, -1}, "secret": {-1.5, 0.2, 4}, "recent": {1, 1, 1}}
	for name, id := range ids {
		setVector(t, db, id, stored[name])
	}
	req := DeliveryRequest{Input: "리나에게 묻는다", Payload: printsOf(turnSource(s, 3)), Model: "m", Chars: 2000, Secret: 4000, Query: []float32{0.6, 0.8, 0.1}}
	setVector(t, db, ids["short"], []float32{float32(math.NaN()), 1, 0})
	bad, err := db.Deliver(scope, req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec("DELETE e FROM embedding_vectors_3 e JOIN memories m ON e.model='m' AND e.text_hash=m.doc_hash WHERE m.id=?", strings.TrimPrefix(ids["short"], "M")); err != nil {
		t.Fatal(err)
	}
	absent, err := db.Deliver(scope, req)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(bad, absent) {
		t.Fatalf("a NaN vector changed the delivery:\n%+v\n%+v", bad, absent)
	}
	if absent.Ranked == "" || len(absent.IDs) == 0 {
		t.Fatalf("fixture: a ranked delivery: %+v", absent)
	}
}
