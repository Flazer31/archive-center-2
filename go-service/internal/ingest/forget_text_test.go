package ingest

import (
	"encoding/json"
	"testing"
)

// A replaced body leaves the chat's cache (every row of it, not another
// chat's), is not learnt again from a resent or stored observation, and
// comes back once the chat holds it again (warmed from storage).
func TestForgetText(t *testing.T) {
	c := NewTextCache()
	a1, i3, other := TextKey{"A", "c:a1"}, TextKey{"A", "i:3"}, TextKey{"B", "c:a1"}
	c.Warm(a1, "첫 답")
	c.Warm(a1, "고친 답")
	c.Warm(i3, "첫 답")
	c.Warm(other, "첫 답")
	// a translation display of the dropped body goes; one of a current
	// reply that contains the same words stays
	shown, kept := TextKey{"A", "v:t1"}, TextKey{"A", "v:t2"}
	c.Warm(shown, "<T>첫 답</T>")
	c.Warm(kept, "<T>첫 답과 둘째</T>")
	held := map[string]bool{"고친 답": true, "첫 답과 둘째": true}
	c.ForgetText("A", "첫 답", held)
	if len(c.texts[shown]) != 0 || len(c.texts[kept]) != 1 {
		t.Fatalf("displays after the drop: %v / %v (want the dropped one gone, the current one kept)", c.texts[shown], c.texts[kept])
	}
	n, h := Fingerprint("첫 답")
	f := textField{Len: n, H: h}
	if _, ok := c.match(a1, f); ok {
		t.Fatal("the forgotten body still fills the row")
	}
	if _, ok := c.match(i3, f); ok {
		t.Fatal("the forgotten body still fills the chat's row read by index")
	}
	if _, ok := c.match(other, f); !ok {
		t.Fatal("another chat lost the same words")
	}
	if g, gh := Fingerprint("고친 답"); func() bool { _, ok := c.match(a1, textField{Len: g, H: gh}); return !ok }() {
		t.Fatal("the row's kept text was dropped")
	}
	for _, m := range c.dropped {
		for k := range m {
			if k == "첫 답" {
				t.Fatal("the drop keeps the body's text")
			}
		}
	}
	// a resent observation of the old reply does not teach it again
	ev, _ := json.Marshal(map[string]any{"stored_chat": map[string]any{"chat_id": "A", "rows": []any{map[string]any{"i": 1, "role": "char", "chat_id": "a1", "data": map[string]any{"text": "첫 답"}}}}})
	c.LearnEvent(ev)
	if _, ok := c.match(a1, f); ok {
		t.Fatal("a resent observation brought the dropped body back")
	}
	// the chat holds it again (the same words in a later turn): learnt again
	c.Held("A", map[string]bool{"첫 답": true})
	c.LearnEvent(ev)
	if _, ok := c.match(a1, f); !ok {
		t.Fatal("a body the chat holds again is still kept out")
	}
	// dropped again, then warmed from storage
	c.ForgetText("A", "첫 답", held)
	c.Warm(a1, "첫 답")
	if _, ok := c.match(a1, f); !ok {
		t.Fatal("a text the chat holds again is not cached")
	}
}
