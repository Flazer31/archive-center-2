package generate

import (
	"testing"
	"time"

	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/store"
)

// A reply another plugin rewrites with markup only (1.2: an illustration
// plugin wraps the stored reply in HTML; the critic reads the text without
// markup) gets a new job as before, and when that job builds exactly the
// request an earlier job of the turn sent, the earlier answer is stored
// without calling the model. Anything that changes the request (the critic
// prompt, the text) calls the model, and so does a remake the user asks
// for. The other identity cases (retry, reroll, edit after deleting the
// reply, same text in a new row, new turn) are
// TestTurnChangesReplaceOrAppendMemory.
func TestSameRequestReusesTheAnswer(t *testing.T) {
	db := testDB(t)
	m := &fakeModel{}
	w := NewWorker(db, m, "fake", time.Minute)
	s := lifecycle.NewSession("chat:A")
	reply := func(text string) {
		s.Apply(snap(lifecycle.SourceOutputEvent, row("u1", "user", "", "입력"), row("a1", "char", "g1", text)))
		goOn(s, row("u1", "user", "", "입력"), row("a1", "char", "g1", text))
		run(w, s)
	}
	job := func() store.Job {
		j, err := db.JobFor(memory.Own(s)[0].Source)
		if err != nil {
			t.Fatal(err)
		}
		return j
	}
	check := func(step string, calls int, body string) {
		t.Helper()
		if got := bodies(t, db, s); m.calls() != calls || len(got[1]) != 1 || got[1][0] != body {
			t.Fatalf("%s: calls %d (want %d) %v", step, m.calls(), calls, got)
		}
	}
	reply("첫 응답")
	check("new turn", 1, "첫 응답 기록")
	first := job()

	// the same reply row rewritten with markup only: a new done job, no call
	reply(`<span class="am-illustration">첫 응답</span><img src="scene.png">`)
	if j := job(); j.ID == first.ID || j.State != "done" {
		t.Fatalf("a markup rewrite is a new revision and a new done job, as before: %+v (first %d)", j, first.ID)
	}
	check("markup-only rewrite", 1, "첫 응답 기록")

	// a remake the user asks for is made by the model
	if err := db.RemakeGeneration(memory.Own(s)[0].Source); err != nil {
		t.Fatal(err)
	}
	run(w, s)
	if m.calls() != 2 {
		t.Fatalf("a remake reused an earlier answer: calls %d", m.calls())
	}

	// a changed critic prompt changes the request: made again, even for a
	// markup-only rewrite
	if err := db.SetCriticPrompt(memory.Instructions + "\n- extra rule"); err != nil {
		t.Fatal(err)
	}
	reply(`<span class="am-illustration">첫 응답</span>`)
	check("changed critic prompt", 3, "첫 응답 기록")

	// a rewrite that changes the text is made again
	reply("고친 응답")
	check("text change", 4, "고친 응답 기록")
}
