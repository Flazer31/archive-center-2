package generate

import (
	"context"
	"testing"
	"time"

	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/provider"
)

type u5DeadlineModel struct {
	fakeModel
	saw time.Duration
}

func (m *u5DeadlineModel) CallTimeout() time.Duration { return 2 * time.Second }
func (m *u5DeadlineModel) Chat(ctx context.Context, prompt string) (provider.Result, error) {
	deadline, _ := ctx.Deadline()
	m.saw = time.Until(deadline)
	return m.fakeModel.Chat(ctx, prompt)
}

// A saved timeout must replace the worker default too, rather than being
// shortened by the old outer deadline before the transport sees it.
func TestU5WorkerUsesConfiguredTimeout(t *testing.T) {
	db := testDB(t)
	p := &u5DeadlineModel{}
	w := NewWorker(db, p, "fake", 20*time.Millisecond)
	s := lifecycle.NewSession("chat:A")
	s.Apply(snap(lifecycle.SourceOutputEvent, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째")))
	goOn(s, row("u1", "user", "", "하나"), row("a1", "char", "g1", "첫째"))
	run(w, s)
	if p.calls() != 1 || p.saw < time.Second || p.saw > 2*time.Second {
		t.Fatal("worker shortened configured timeout")
	}
}
