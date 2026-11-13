package store

import (
	"database/sql"
	"strings"
	"testing"
)

func entries(t *testing.T, d *DB, q string, args ...any) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	err := d.each(q, args, func(rows *sql.Rows) error {
		var a, b, c string
		if err := rows.Scan(&a, &b, &c); err != nil {
			return err
		}
		out[a+" "+b+" "+c] = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// A saved memory's words are indexed by field without particles, and its
// references by field with the persistent ids they resolved to; a
// re-mention's quote is indexed under the item it mentions.
func TestSavedItemsAreIndexedForSearch(t *testing.T) {
	db := testDB(t)
	s := twoTurnSession()
	job, _ := db.JobFor(turnSource(s, 0))
	if _, err := db.SaveGeneration(job, Generation{Response: turn1Output, Output: parse(t, turn1Output)}); err != nil {
		t.Fatal(err)
	}
	it, _ := db.ItemsOf([]int64{job.ID})
	n1, m1 := it.Names[0].ID, it.Memories[0].ID
	m2, m3 := it.Memories[1].ID, it.Memories[2].ID
	words := entries(t, db, `SELECT item, field, word FROM search_words WHERE session_key=? AND job_id=?`, job.Session, job.ID)
	for _, want := range []string{m1 + " body 한얼", m1 + " body 30냥", m1 + " q 갚았다", m1 + " effect 50냥", m1 + " effect_slot 돈", m2 + " slot 돈", m3 + " body 후련함"} {
		if !words[want] {
			t.Errorf("word %q not indexed: %v", want, words)
		}
	}
	if words[m1+" body 빚"] {
		t.Error("a one-letter word was indexed")
	}
	refs := entries(t, db, `SELECT item, field, ref FROM search_refs WHERE session_key=? AND job_id=?`, job.Session, job.ID)
	for _, want := range []string{m1 + " actor " + n1, m1 + " effect " + n1, m2 + " about " + n1, m2 + " at " + m1, m3 + " owner " + n1, m3 + " basis " + m1, m2 + " was " + m1 + ".1"} {
		if !refs[want] {
			t.Errorf("ref %q not indexed: %v", want, refs)
		}
	}
	for r := range refs {
		if strings.HasSuffix(r, " n1") || strings.HasSuffix(r, " m1") {
			t.Errorf("a temporary id was indexed: %q", r)
		}
	}

	job2, _ := db.JobFor(turnSource(s, 1))
	out2 := `{"memories":[{"ref":"` + m1 + `","q":"빚 갚은 일을 떠올렸다"}],"entities":[{"id":"n3","name":"서진"}]}`
	if _, err := db.SaveGeneration(job2, Generation{Response: out2, Output: parse(t, out2)}); err != nil {
		t.Fatal(err)
	}
	words = entries(t, db, `SELECT item, field, word FROM search_words WHERE session_key=? AND job_id=?`, job2.Session, job2.ID)
	if !words[m1+" mention 떠올렸다"] || !words[m1+" mention 갚은"] {
		t.Fatalf("mention quote not indexed under its target: %v", words)
	}
}

// The reverse lookups records are reached through use their indexes.
func TestReverseLookupsHaveIndexes(t *testing.T) {
	db := testDB(t)
	for _, c := range []struct{ table, key string }{{"links", "by_from"}, {"links", "by_to"}, {"memory_was", "by_target"}, {"effects", "by_about"}, {"search_words", "by_word"}, {"search_refs", "by_ref"}} {
		var n int
		if err := db.sql.QueryRow(`SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name=? AND index_name=?`, c.table, c.key).Scan(&n); err != nil || n == 0 {
			t.Errorf("%s has no index %s (%v)", c.table, c.key, err)
		}
	}
}

// Values as long as the stored memory allows (a long body word, a whole
// slot, an unresolved reference, a mention's target) are stored and indexed
// whole: the derived index never refuses a memory.
func TestLongValuesAreStoredAndIndexedWhole(t *testing.T) {
	db := testDB(t)
	s := twoTurnSession()
	job, _ := db.JobFor(turnSource(s, 0))
	// a word longer than a TEXT column holds (the body is LONGTEXT)
	word, slot, about := strings.Repeat("ab", 40000), strings.Repeat("나", 450), strings.Repeat("다", 480)
	out := `{"memories":[{"id":"m1","kind":"fact","body":"` + word + ` 남았다","slot":"` + slot + `","about":"` + about + `"}]}`
	if _, err := db.SaveGeneration(job, Generation{Response: out, Output: parse(t, out)}); err != nil {
		t.Fatalf("a long value was refused: %v", err)
	}
	if count(t, db, "memories") != 1 {
		t.Fatal("memory not stored")
	}
	words := entries(t, db, `SELECT item, field, word FROM search_words WHERE job_id=?`, job.ID)
	refs := entries(t, db, `SELECT item, field, ref FROM search_refs WHERE job_id=?`, job.ID)
	if !words["M1 body "+word] || !words["M1 slot "+slot] || !refs["M1 about "+about] {
		t.Fatalf("long values not indexed whole: %d words, %d refs", len(words), len(refs))
	}
	target := strings.Repeat("라", 400)
	job2, _ := db.JobFor(turnSource(s, 1))
	out2 := `{"memories":[{"ref":"` + target + `","q":"그 일을 떠올렸다"}]}`
	if _, err := db.SaveGeneration(job2, Generation{Response: out2, Output: parse(t, out2)}); err != nil {
		t.Fatalf("a long mention target was refused: %v", err)
	}
	if w := entries(t, db, `SELECT item, field, word FROM search_words WHERE job_id=?`, job2.ID); !w[target+" mention 떠올렸다"] {
		t.Fatalf("long mention target not indexed whole: %v", len(w))
	}
}
