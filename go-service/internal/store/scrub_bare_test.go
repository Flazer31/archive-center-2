package store

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/observe"
)

// 1.4 P2: the input and afterRequest hooks carry one text and no chat id.
// After an edit replaces a reply, its text leaves those rows too (length and
// fingerprint stay). A text the chat still holds stays, and so does a text
// another chat's observations also hold (which chat a bare row came from is
// not known).
func TestReplacedBodyLeavesBareHooks(t *testing.T) {
	db := testDB(t)
	sink := RawSink{DB: db}
	seq := 0
	write := func(ev string) {
		t.Helper()
		if _, err := sink.Write([]observe.Record{{Event: json.RawMessage(ev), ReceivedAt: time.Now().UTC().Format(time.RFC3339Nano), ReceiveSeq: uint64(seq)}}); err != nil {
			t.Fatal(err)
		}
	}
	bare := func(hook, text string) int {
		t.Helper()
		seq++
		write(fmt.Sprintf(`{"boot_id":"b","seq":%d,"hook":"%s","coords":{"character_index":0,"chat_index":0},"content":{"len":%d,"h":"x%d","text":%q}}`, seq, hook, len([]rune(text)), seq, text))
		return seq
	}
	content := func(n int) map[string]any {
		t.Helper()
		var ev string
		if err := db.sql.QueryRow(`SELECT event FROM raw_observations WHERE seq=?`, n).Scan(&ev); err != nil {
			t.Fatal(err)
		}
		var e struct {
			Content map[string]any `json:"content"`
		}
		json.Unmarshal([]byte(ev), &e)
		return e.Content
	}
	first, shared := "첫 답은 이렇게 끝났다", "알겠습니다"
	in := bare("input", "입력")
	after := bare("afterRequest", first)
	afterShared := bare("afterRequest", shared)
	other := bare("afterRequest", "다른 채팅의 답")
	// chat B's own observation holds the shared words
	seq++
	write(fmt.Sprintf(`{"boot_id":"b","seq":%d,"hook":"output","stored_chat":{"chat_id":"B","rows":[{"i":0,"role":"char","chat_id":"b1","data":{"text":%q,"len":5,"h":"hb"}}]}}`, seq, shared))
	s := lifecycle.NewSession("chat:A")
	save := func() {
		t.Helper()
		if err := db.SaveSession(s, s.TakeEvents(), s.TakeRenames()); err != nil {
			t.Fatal(err)
		}
	}
	s.Apply(lsnap(lifecycle.SourceOutputEvent, lrow("u1", "user", "", "입력"), lrow("a1", "char", "g1", first), lrow("u2", "user", "", "다음"), lrow("a2", "char", "g2", shared)))
	save()
	if content(after)["text"] != first || content(afterShared)["text"] != shared {
		t.Fatal("a normal turn removed a reply")
	}
	// both replies are edited
	s.Apply(lsnap(lifecycle.SourceBeforeRequest, lrow("u1", "user", "", "입력"), lrow("a1", "char", "g1", "고친 답"), lrow("u2", "user", "", "다음"), lrow("a2", "char", "g2", "고친 둘째 답")))
	save()
	if c := content(after); c["text"] != nil || c["h"] == nil || c["len"] == nil {
		t.Fatalf("afterRequest row after the edit: %v (want no text, len and h kept)", c)
	}
	if content(afterShared)["text"] != shared {
		t.Fatal("a text another chat's observation holds was removed from a bare row")
	}
	if content(in)["text"] != "입력" || content(other)["text"] != "다른 채팅의 답" {
		t.Fatal("a held or unrelated text was removed")
	}
}
