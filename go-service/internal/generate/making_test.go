package generate

import (
	"testing"
	"time"
)

// Making: a chat whose notice waits or whose run goes on is being made
// when a critic is set; while a run is being started the answer is
// "making" (never idle between the notice and the run); with no critic, a
// waiting notice is not being made.
func TestMaking(t *testing.T) {
	w := NewWorker(nil, &fakeModel{}, "m", time.Minute)
	if w.Making("chat:A") {
		t.Fatal("nothing asked")
	}
	w.mu.Lock()
	w.pending["chat:A"] = notice{}
	w.mu.Unlock()
	if !w.Making("chat:A") {
		t.Fatal("a waiting notice")
	}
	w.mu.Lock()
	delete(w.pending, "chat:A") // taken by the dispatcher, run not registered yet
	w.mu.Unlock()
	w.startMu.Lock()
	if !w.Making("chat:A") {
		t.Fatal("handed from its notice to its run")
	}
	w.startMu.Unlock()
	w.mu.Lock()
	w.running["chat:A"] = func() {}
	w.mu.Unlock()
	if !w.Making("chat:A") {
		t.Fatal("a run under way")
	}
	none := NewWorker(nil, nil, "", time.Minute)
	none.mu.Lock()
	none.pending["chat:A"] = notice{}
	none.mu.Unlock()
	if none.Making("chat:A") {
		t.Fatal("no critic: a notice kept for one to come")
	}
}
