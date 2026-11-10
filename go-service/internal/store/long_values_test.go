package store

import (
	"os"
	"strings"
	"testing"
)

// 11-05 tester report: "status좀 늘려줄수 있음? 32칸이 한계라 모델이 그 이상하면
// 평론가 아웃풋 버려짐". A model may write long values in the short columns;
// the turn's whole save must not fail because of it.
func TestLongModelValuesAreStored(t *testing.T) {
	db := testDB(t)
	long := strings.Repeat("아직 결정되지 않았고 다음 장면에서 다시 다룬다 ", 3) // 75+ characters
	raw := `{"entities":[{"id":"n1","name":"리나","type":"` + long + `"},{"id":"n2","name":"가면","same_as":"n1","how":"` + long + `"}],` +
		`"memories":[` +
		`{"id":"m1","kind":"event","body":"리나가 약속을 미뤘다","scene":"` + long + `","effects":[{"about":"n1","slot":"약속","value":"미룸","status":"` + long + `"}],"knows":[{"who":"n1","via":"` + long + `","extent":"` + long + `"}]},` +
		`{"id":"m2","kind":"fact","about":"n1","mode":"` + long + `","body":"리나는 답을 미룬다","status":"` + long + `"},` +
		`{"id":"m3","kind":"view","owner":"n1","aspect":"` + long + `","body":"리나는 서두르고 싶지 않다"}],` +
		`"links":[{"from":"m1","rel":"` + long + `","to":"m2"}]}`
	_, it, _ := a24Save(t, db, turnSource(twoTurnSession(), 0), raw)
	if len(it.Memories) != 3 || len(it.Links) != 1 {
		t.Fatalf("not everything saved: memories=%d links=%d", len(it.Memories), len(it.Links))
	}
	var n int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM memories WHERE status=? OR mode=? OR aspect=?`, long, long, long).Scan(&n); err != nil || n != 2 {
		t.Fatalf("memory columns hold the full value: rows=%d err=%v", n, err)
	}
	for _, q := range []string{`SELECT COUNT(*) FROM effects WHERE status=?`, `SELECT COUNT(*) FROM links WHERE rel=?`, `SELECT COUNT(*) FROM names WHERE type=?`} {
		if err := db.sql.QueryRow(q, long).Scan(&n); err != nil || n != 1 {
			t.Errorf("%s: rows=%d err=%v", q, n, err)
		}
	}
}

// An existing database made with the old 32-character columns is widened when
// the backend starts again (Open runs the schema).
func TestOldShortColumnsWidenOnOpen(t *testing.T) {
	db := testDB(t)
	for _, q := range []string{`ALTER TABLE memories MODIFY status VARCHAR(32) NOT NULL DEFAULT ''`, `ALTER TABLE effects MODIFY status VARCHAR(32) NOT NULL DEFAULT ''`, `ALTER TABLE links MODIFY rel VARCHAR(32) NOT NULL`} {
		if _, err := db.sql.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	var name string
	if err := db.sql.QueryRow(`SELECT DATABASE()`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	again, err := Open(os.Getenv("AC2_TEST_MARIADB_DSN") + name + "?parseTime=true")
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	for _, col := range [][2]string{{"memories", "status"}, {"effects", "status"}, {"links", "rel"}} {
		var n int
		if err := again.sql.QueryRow(`SELECT CHARACTER_MAXIMUM_LENGTH FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=? AND TABLE_NAME=? AND COLUMN_NAME=?`, name, col[0], col[1]).Scan(&n); err != nil || n < 500 {
			t.Errorf("%s.%s is %d characters after reopening (err %v)", col[0], col[1], n, err)
		}
	}
}
