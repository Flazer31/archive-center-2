package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
)

func TestC26SourceProjectionMatchesGo(t *testing.T) {
	db := testDB(t)
	text := "한글 😀 <>& \" \\ / \u2028\u2029 \\u001B\n"
	for c := 0; c < 32; c++ {
		text += string(rune(c))
	}
	for _, txt := range []string{text, "", strings.Repeat("가", 400000)} {
		for _, state := range []struct{ off, unread, empty bool }{{}, {off: true}, {unread: true}, {empty: true}} {
			if len(txt) > 1000000 && (state.off || state.unread || state.empty) {
				continue
			}
			tr := &lifecycle.Turn{ID: 7, Confirmed: true, Members: []*lifecycle.Member{{Key: "u", Data: txt, State: lifecycle.Active}, {Key: "gone", Data: "deleted", State: lifecycle.Deleted}}, Responses: []*lifecycle.ResponseRow{
				{Key: "a", State: lifecycle.Active, Gen: "new", Off: state.off, Revisions: []lifecycle.Revision{{Seq: 1, Data: "old", GenerationID: "old"}, {Seq: 2, Data: txt, GenerationID: "new"}}},
				{Key: "b", State: lifecycle.Active, Revisions: []lifecycle.Revision{{Seq: 1, Data: "second", GenerationID: ""}}},
				{Key: "old", State: lifecycle.Replaced, Revisions: []lifecycle.Revision{{Seq: 1, Data: "replaced"}}},
			}}
			if state.unread {
				tr.Responses[0].Gen = "unread"
			}
			if state.empty {
				tr.Members = nil
				tr.Responses = nil
			}
			// A unique chat per case avoids old rows from another fixture.
			key := fmt.Sprintf("chat:Q-%d-%v", len(txt), state)
			s := lifecycle.Restore(key, []*lifecycle.Turn{tr}, nil)
			if err := db.SaveSession(s, nil, nil); err != nil {
				t.Fatal(err)
			}
			for _, whole := range []bool{false, true} {
				want := memory.SourceOf(key, tr)
				if whole {
					want = memory.WholeSourceOf(key, tr)
				}
				var got string
				err := db.sql.QueryRow(`SET STATEMENT group_concat_max_len=4294967295 FOR SELECT `+sqlLocalSourceKey(whole)+` FROM logical_turns t WHERE t.session_key=? AND t.turn_id=?`, key, tr.ID).Scan(&got)
				if err != nil || got != want.Key() {
					t.Fatalf("SQL source key != Go, bytes=%d state=%+v whole=%v got=%s want=%s err=%v", len(txt), state, whole, got, want.Key(), err)
				}
				b, _ := json.Marshal(want)
				if _, err = db.sql.Exec(`INSERT INTO branch_copies(session_key,turn_id,own_key,source) VALUES(?,?,?,?) ON DUPLICATE KEY UPDATE source=VALUES(source)`, key, tr.ID, want.Key(), string(b)); err != nil {
					t.Fatal(err)
				}
				err = db.sql.QueryRow(`SET STATEMENT group_concat_max_len=4294967295 FOR SELECT `+sqlCopySourceKey()+` FROM branch_copies c WHERE c.session_key=? AND c.turn_id=?`, key, tr.ID).Scan(&got)
				if err != nil || got != want.Key() {
					t.Fatalf("copy key != Go got=%s want=%s err=%v", got, want.Key(), err)
				}
			}
		}
	}
}

func c26Branch(t *testing.T, db *DB, parent *lifecycle.Session, key string) *lifecycle.Session {
	t.Helper()
	var tr lifecycle.Turn
	b, _ := json.Marshal(parent.ActiveTurns()[0])
	if err := json.Unmarshal(b, &tr); err != nil {
		t.Fatal(err)
	}
	tr.ID = 3
	child := lifecycle.Restore(key, []*lifecycle.Turn{&tr}, nil)
	child.BranchOf = [2]string{parent.Key, tr.Responses[len(tr.Responses)-1].Key}
	src := memory.SourceOf(parent.Key, parent.ActiveTurns()[0])
	frozen, _ := json.Marshal(src)
	child.RecordCopies(child.BranchOf, map[int]lifecycle.Copy{tr.ID: {Own: memory.WholeSourceOf(child.Key, &tr).Key(), Source: string(frozen)}})
	if err := db.SaveSession(child, nil, nil); err != nil {
		t.Fatal(err)
	}
	return child
}

func TestC26CompareReadOnlyPages(t *testing.T) {
	db := testDB(t)
	a := u2Session(t, db, "chat:LEFT", "같은 문구", "다음", "마지막")
	b := u2Session(t, db, "chat:RIGHT", "같은 문구")
	u2Save(t, db, a, 0, u2Output)
	u2Save(t, db, a, 1, `{"memories":[{"kind":"fact","body":"`+strings.Repeat("길", 650)+`"}]}`)
	u2Save(t, db, b, 0, `{"entities":[{"name":"동명"}],"memories":[{"kind":"view","body":"오른쪽"}]}`)
	if err := db.SetChatLabel("chat:EMPTY", "", "빈 채팅"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`INSERT INTO raw_observations(received_at,receive_seq,chat_id,event) VALUES('2026-11-05 10:00:00',1,'LEFT','CREDENTIAL_NOT_FOR_EXPORT')`); err != nil {
		t.Fatal(err)
	}
	before := u2Dump(t, db)
	pages := [2]ComparePage{{Limit: 1}, {Limit: 1}}
	v, err := db.CompareChats(context.Background(), a.Key, b.Key, pages)
	if err != nil {
		t.Fatal(err)
	}
	if v[0].Turns != len(a.ActiveTurns()) || v[1].Turns != len(b.ActiveTurns()) || v[0].Memories["total"] != 4 || v[1].Memories["view"] != 1 || v[0].Names != 2 || v[1].Names != 1 || v[0].LastAt == nil || v[1].LastAt != nil {
		t.Fatalf("independent counts %+v", v)
	}
	if v[0].NextTurn == nil || v[0].NextMemory == nil || v[1].NextTurn != nil || len(v[0].Originals) != 1 || len(v[0].MemoryPreview) != 1 {
		t.Fatal("page boundary", v)
	}
	seen := map[int64]bool{}
	for {
		for _, m := range v[0].MemoryPreview {
			pk := m["pk"].(int64)
			if seen[pk] {
				t.Fatal("duplicate page")
			}
			seen[pk] = true
			if len([]rune(m["body"].(string))) > 500 {
				t.Fatal("unbounded preview")
			}
		}
		if v[0].NextMemory == nil {
			break
		}
		pages[0].MemoryAfter = *v[0].NextMemory
		v, err = db.CompareChats(context.Background(), a.Key, b.Key, pages)
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != 4 {
		t.Fatal("lost page", seen)
	}
	empty, err := db.CompareChats(context.Background(), "chat:EMPTY", b.Key, pages)
	if err != nil || empty[0].Turns != 0 || empty[0].Memories["total"] != 0 || len(empty[0].Originals) != 0 {
		t.Fatalf("empty %+v %v", empty, err)
	}
	if _, err = db.CompareChats(context.Background(), a.Key, "chat:MISSING", pages); !errors.Is(err, ErrChatNotFound) {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, u2Dump(t, db)) {
		t.Fatal("comparison changed rows/AUTO_INCREMENT")
	}
}

func TestC26ScopeMatchesProductionAndBranch(t *testing.T) {
	db := testDB(t)
	p := u2Session(t, db, "chat:PARENT", "과거", "PARENT_OUTSIDE_BODY")
	u2Save(t, db, p, 0, u2Output)
	u2Save(t, db, p, 1, `{"memories":[{"kind":"fact","body":"PARENT_OUTSIDE_MEMORY"}]}`)
	child := c26Branch(t, db, p, "chat:CHILD")
	check := func(s *lifecycle.Session) {
		t.Helper()
		var expected []int64
		for _, tr := range memory.Own(s) {
			if tr.Off {
				continue
			}
			ids, _, err := db.turnJobs(tr)
			if err != nil {
				t.Fatal(err)
			}
			expected = append(expected, ids...)
		}
		r, err := db.beginChatRead(context.Background(), s.Key)
		if err != nil {
			t.Fatal(err)
		}
		defer r.tx.Rollback()
		got, _, err := r.jobs()
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(expected) {
			t.Fatalf("scope got=%v want=%v", got, expected)
		}
		for i := range got {
			if got[i] != expected[i] {
				t.Fatalf("scope got=%v want=%v", got, expected)
			}
		}
	}
	check(child)
	v, err := db.CompareChats(context.Background(), child.Key, p.Key, [2]ComparePage{})
	if err != nil {
		t.Fatal(err)
	}
	if v[0].Memories["total"] != 3 || v[0].BranchOf["turn_id"] != p.ActiveTurns()[0].ID || v[0].MemoryPreview[0]["inherited"] != true {
		t.Fatal("inherited scope", v)
	}
	// A parent reroll cannot drag the branch to its new revision.
	p.ActiveTurns()[0].Responses[0].Revisions = append(p.ActiveTurns()[0].Responses[0].Revisions, lifecycle.Revision{Seq: 2, Data: "new parent"})
	if err = db.SaveSession(p, nil, nil); err != nil {
		t.Fatal(err)
	}
	u2Save(t, db, p, 0, `{"memories":[{"kind":"view","body":"new parent only"}]}`)
	check(child)
	check(p)
	// Existing own fallback when the parent did not make the copied source.
	if _, err = db.sql.Exec(`UPDATE gen_jobs SET state='failed' WHERE session_key=?`, p.Key); err != nil {
		t.Fatal(err)
	}
	u2Save(t, db, child, 0, `{"memories":[{"kind":"fact","body":"child fallback"}]}`)
	check(child)
	child.ActiveTurns()[0].Responses[0].Off = true
	child.ActiveTurns()[0].Off = true
	if err = db.SaveSession(child, nil, nil); err != nil {
		t.Fatal(err)
	}
	check(child)
	child.ActiveTurns()[0].Off = false
	child.ActiveTurns()[0].Responses[0].Off = false
	child.ActiveTurns()[0].Members[0].Data = "edited child"
	if err = db.SaveSession(child, nil, nil); err != nil {
		t.Fatal(err)
	}
	check(child)
}

type c27Document struct {
	Format   string                      `json:"format"`
	Version  int                         `json:"version"`
	Complete bool                        `json:"complete"`
	Tables   map[string][]map[string]any `json:"tables"`
}

func c27Decode(t *testing.T, b []byte) c27Document {
	t.Helper()
	var v c27Document
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	if v.Format != "archive-center-2.chat" || v.Version != 1 || !v.Complete {
		t.Fatal("missing format/completion", string(b))
	}
	return v
}

type c27Writer struct {
	buf  bytes.Buffer
	hook func()
	err  error
}

func (w *c27Writer) Write(p []byte) (int, error) {
	if w.hook != nil {
		f := w.hook
		w.hook = nil
		f()
	}
	if w.err != nil {
		return 0, w.err
	}
	return w.buf.Write(p)
}
