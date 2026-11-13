package store

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/observe"
)

// A body an edit or a reroll replaced leaves no copy: it is gone from the
// chat's raw observations (the text only; that row's length and fingerprint
// stay) and from the generation job sources. A text the chat still holds
// stays, and a normal new turn removes nothing. Observations are stored as
// the plugin writes them (JSON.stringify: "<" is not escaped), and a row
// without a chat id is found too.
func TestReplacedBodiesLeaveNoCopy(t *testing.T) {
	db := testDB(t)
	sink := RawSink{DB: db}
	seq := 0
	// rows: role, chat id ("" = none), text
	observe1 := func(rows ...[3]string) {
		t.Helper()
		seq++
		var parts []string
		for i, r := range rows {
			id := "null"
			if r[1] != "" {
				id = `"` + r[1] + `"`
			}
			// written by hand the way JSON.stringify writes it (no <)
			text := strings.ReplaceAll(r[2], `"`, `\"`)
			parts = append(parts, fmt.Sprintf(`{"i":%d,"role":"%s","chat_id":%s,"data":{"text":"%s","len":%d,"h":"h%d"}}`, i, r[0], id, text, len([]rune(r[2])), i))
		}
		ev := fmt.Sprintf(`{"boot_id":"b","seq":%d,"hook":"output","stored_chat":{"chat_id":"A","rows":[%s]}}`, seq, strings.Join(parts, ","))
		if !json.Valid([]byte(ev)) {
			t.Fatalf("fixture event: %s", ev)
		}
		if _, err := sink.Write([]observe.Record{{Event: json.RawMessage(ev), ReceivedAt: time.Now().UTC().Format(time.RFC3339Nano), ReceiveSeq: uint64(seq)}}); err != nil {
			t.Fatal(err)
		}
	}
	turn := 1
	job := func(key, data string) {
		t.Helper()
		if _, err := db.JobFor(memory.Source{Session: "chat:A", Turn: turn, Members: []memory.SourceMember{{Key: "c:u1", Data: "입력"}}, Responses: []memory.SourceResponse{{Key: key, Seq: 1, Data: data}}}); err != nil {
			t.Fatal(err)
		}
	}
	s := lifecycle.NewSession("chat:A")
	save := func() {
		t.Helper()
		if err := db.SaveSession(s, s.TakeEvents(), s.TakeRenames()); err != nil {
			t.Fatal(err)
		}
	}
	// stored texts of row i of observation n
	rowOf := func(n, i int) map[string]any {
		t.Helper()
		var ev string
		if err := db.sql.QueryRow(`SELECT event FROM raw_observations WHERE seq=?`, n).Scan(&ev); err != nil {
			t.Fatal(err)
		}
		var e struct {
			StoredChat struct {
				Rows []struct {
					Data map[string]any `json:"data"`
				} `json:"rows"`
			} `json:"stored_chat"`
		}
		if err := json.Unmarshal([]byte(ev), &e); err != nil {
			t.Fatal(err)
		}
		return e.StoredChat.Rows[i].Data
	}
	sourceHas := func(text string) bool {
		t.Helper()
		n := 0
		if err := db.sql.QueryRow(`SELECT COUNT(*) FROM gen_jobs WHERE INSTR(source, ?)>0`, text).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n > 0
	}

	// the first reply carries markup the plugin stores unescaped
	first := `<b>첫 답</b> & 끝`
	observe1([3]string{"user", "u1", "입력"}, [3]string{"char", "a1", first})
	job("c:a1", first)
	// a source written while the reply row had no chat id names it f:<n>
	turn = 2 // its own job (a source's key does not name rows)
	job("f:1", first)
	turn = 1
	// translation records kept beside the rows: one shows the first reply,
	// one shows a later body together with the input (a text still held)
	vars := `{"boot_id":"b","seq":100,"hook":"output","stored_chat":{"chat_id":"A","rows":[],"scriptstate":{"t1":{"text":"<T><b>첫 답</b> & 끝</T>","len":20,"h":"v1"},"t2":{"text":"<T>고친 답 / 입력</T>","len":16,"h":"v2"}}}}`
	if _, err := sink.Write([]observe.Record{{Event: json.RawMessage(vars), ReceivedAt: time.Now().UTC().Format(time.RFC3339Nano), ReceiveSeq: 100}}); err != nil {
		t.Fatal(err)
	}
	varOf := func(name string) map[string]any {
		t.Helper()
		var ev string
		if err := db.sql.QueryRow(`SELECT event FROM raw_observations WHERE seq=100`).Scan(&ev); err != nil {
			t.Fatal(err)
		}
		var e struct {
			StoredChat struct {
				ScriptState map[string]map[string]any `json:"scriptstate"`
			} `json:"stored_chat"`
		}
		if err := json.Unmarshal([]byte(ev), &e); err != nil {
			t.Fatal(err)
		}
		return e.StoredChat.ScriptState[name]
	}
	s.Apply(lsnap(lifecycle.SourceOutputEvent, lrow("u1", "user", "", "입력"), lrow("a1", "char", "g1", first)))
	save()

	// an edit of the reply: the earlier body leaves that row of the stored
	// observation (its length and fingerprint stay) and the job source
	observe1([3]string{"user", "u1", "입력"}, [3]string{"char", "a1", "고친 답"})
	job("c:a1", "고친 답")
	s.Apply(lsnap(lifecycle.SourceBeforeRequest, lrow("u1", "user", "", "입력"), lrow("a1", "char", "g1", "고친 답")))
	save()
	d := rowOf(1, 1)
	if _, has := d["text"]; has || d["h"] != "h1" || d["len"] == nil {
		t.Fatalf("after the edit the first observation's reply is %v (want no text, its len and h kept)", d)
	}
	if rowOf(1, 0)["text"] != "입력" {
		t.Fatal("the input's text was removed")
	}
	if sourceHas("첫 답") {
		t.Fatal("after the edit a job source still holds the earlier body")
	}
	if v := varOf("t1"); v["text"] != nil || v["h"] != "v1" {
		t.Fatalf("the first reply's translation record is %v (want no text, its h kept)", v)
	}

	// a reroll into a new reply row: the replaced body leaves too
	observe1([3]string{"user", "u1", "입력"}, [3]string{"char", "a2", "새 답"})
	job("c:a2", "새 답")
	s.Apply(lsnap(lifecycle.SourceOutputEvent, lrow("u1", "user", "", "입력"), lrow("a2", "char", "g2", "새 답")))
	save()
	if _, has := rowOf(2, 1)["text"]; has || sourceHas("고친 답") {
		t.Fatal("after the reroll the replaced body is still stored")
	}
	if varOf("t2")["text"] != "<T>고친 답 / 입력</T>" {
		t.Fatal("a record that also shows a text the chat holds was cleared")
	}
	if rowOf(3, 1)["text"] != "새 답" || !sourceHas("새 답") {
		t.Fatal("the new reply was removed")
	}

	// a normal new turn removes nothing; a reply without a chat id that
	// says what the input said keeps it (the chat holds that text)
	observe1([3]string{"user", "u1", "입력"}, [3]string{"char", "a2", "새 답"}, [3]string{"user", "u2", "다음"}, [3]string{"char", "", "입력"})
	s.Apply(lsnap(lifecycle.SourceOutputEvent, lrow("u1", "user", "", "입력"), lrow("a2", "char", "g2", "새 답"), lrow("u2", "user", "", "다음"), lrow("", "char", "g3", "입력")))
	save()
	if rowOf(4, 1)["text"] != "새 답" || rowOf(4, 3)["text"] != "입력" || !sourceHas("새 답") {
		t.Fatal("a new turn removed stored texts")
	}

	// that reply (no chat id) is edited: its earlier text stays, since the
	// input still holds the same words; the edited one is then rerolled away
	observe1([3]string{"user", "u1", "입력"}, [3]string{"char", "a2", "새 답"}, [3]string{"user", "u2", "다음"}, [3]string{"char", "", "다음 답"})
	s.Apply(lsnap(lifecycle.SourceBeforeRequest, lrow("u1", "user", "", "입력"), lrow("a2", "char", "g2", "새 답"), lrow("u2", "user", "", "다음"), lrow("", "char", "g3", "다음 답")))
	save()
	if rowOf(4, 3)["text"] != "입력" {
		t.Fatal("a text the chat still holds was removed")
	}
	s.Apply(lsnap(lifecycle.SourceBeforeRequest, lrow("u1", "user", "", "입력"), lrow("a2", "char", "g2", "새 답"), lrow("u2", "user", "", "다음"), lrow("", "char", "g3", "고친 다음 답")))
	save()
	if _, has := rowOf(5, 3)["text"]; has {
		t.Fatal("the edited reply without a chat id kept its earlier body")
	}
}
