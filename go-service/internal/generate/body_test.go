package generate

import (
	"strings"
	"testing"
	"time"

	"archivecenter2/goservice/internal/body"
	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
)

// 1.0 E09/E10: the critic gets the body rules only while body tracking is
// on — off, its prompt is what it was — and they follow an edited prompt.
func TestBodyRulesOnlyWhileOn(t *testing.T) {
	db := testDB(t)
	m := &fakeModel{}
	w := NewWorker(db, m, "fake", time.Minute)
	turn := func(id string, on body.Toggles) string {
		t.Helper()
		if err := db.SetBodyTracking(on); err != nil {
			t.Fatal(err)
		}
		s := lifecycle.NewSession("chat:" + id)
		s.Apply(snap(lifecycle.SourceOutputEvent, row("u1", "user", "", "하나"), row("a1", "char", "g1", "응답 "+id)))
		s.Key = "chat:" + id
		goOn(s, row("u1", "user", "", "하나"), row("a1", "char", "g1", "응답 "+id))
		run(w, s)
		return m.prompts[len(m.prompts)-1]
	}
	off := turn("A", body.Toggles{})
	if strings.Contains(off, "body_events") || !strings.HasPrefix(off, memory.Instructions) {
		t.Fatal("rules while off")
	}
	if on := turn("B", body.Toggles{Pregnancy: true}); !strings.Contains(on, memory.BodyRules) {
		t.Fatal("no rules while on")
	}
	if err := db.SetCriticPrompt("내 지시"); err != nil {
		t.Fatal(err)
	}
	if on := turn("C", body.Toggles{Cycle: true}); !strings.HasPrefix(on, "내 지시\n"+memory.BodyRules) {
		t.Fatalf("an edited prompt keeps the rules after it:\n%.200s", on)
	}
}
