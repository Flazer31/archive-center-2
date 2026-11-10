package ingest

import "testing"

func TestU2ForgetTextsOfOneChat(t *testing.T) {
	c := NewTextCache()
	c.Warm(TextKey{"A", "row"}, "지울 본문")
	c.Warm(TextKey{"B", "row"}, "남길 본문")
	c.loaded["A"], c.loaded["B"] = true, true
	c.Forget("A")
	if len(c.texts[TextKey{"A", "row"}]) != 0 || c.loaded["A"] {
		t.Fatal("forgotten chat still cached")
	}
	if len(c.texts[TextKey{"B", "row"}]) != 1 || !c.loaded["B"] {
		t.Fatal("other chat evicted")
	}
}
