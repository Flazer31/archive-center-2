package pipeline

import (
	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/observe"
	"testing"
)

func TestU2ForgetSessionAndLabels(t *testing.T) {
	st := &memStore{saved: map[string][]*lifecycle.Turn{}}
	p := New(st)
	if _, err := p.Process([]observe.Record{beforeRequest(r1)}); err != nil {
		t.Fatal(err)
	}
	p.labels = map[string][2]string{"chat:A": {"a", "a"}, "chat:B": {"b", "b"}}
	if err := p.WithSession("chat:A", func(_ *lifecycle.Session) { p.ForgetLocked("chat:A") }); err != nil {
		t.Fatal(err)
	}
	if p.sessions["chat:A"] != nil || p.labels["chat:A"] != [2]string{} {
		t.Fatal("session or label retained")
	}
	if p.labels["chat:B"] != [2]string{"b", "b"} {
		t.Fatal("other label removed")
	}
	st.saved["chat:A"] = nil
	if err := p.WithSession("chat:A", func(s *lifecycle.Session) {
		if len(s.Turns) != 0 {
			t.Fatal("old cached turns resurrected")
		}
	}); err != nil {
		t.Fatal(err)
	}
}
