package generate

import (
	"context"
	"testing"
	"time"
)

func TestU2ForgetWaitingRetryAndHeldState(t *testing.T) {
	w := NewWorker(nil, nil, "", time.Second)
	w.pending["chat:A"], w.pending["chat:B"] = notice{}, notice{}
	w.order = []string{"chat:A", "chat:B"}
	w.resumed = []string{"chat:B", "chat:A"}
	w.inOrder["chat:A"], w.inOrder["chat:B"] = true, true
	w.backlog["chat:A"], w.backlog["chat:B"] = true, true
	w.retryAt["chat:A"], w.retryAt["chat:B"] = time.Now(), time.Now()
	w.last["chat:A"], w.last["chat:B"] = notice{}, notice{}
	w.gaveUp["chat:A"], w.gaveUp["chat:B"] = map[string]bool{"a": true}, map[string]bool{"b": true}
	w.held["a"], w.held["b"] = map[int64]*Answer{1: {}}, map[int64]*Answer{2: {}}
	if err := w.WithChatOperation(func() error { w.Forget("chat:A", []string{"a"}); return nil }); err != nil {
		t.Fatal(err)
	}
	if got := w.Chat("chat:A"); got.Waiting || got.RetryWaiting || got.GivenUp != 0 {
		t.Fatalf("chat state retained %+v", got)
	}
	if _, ok := w.last["chat:A"]; ok || w.backlog["chat:A"] || w.held["a"] != nil {
		t.Fatal("retry/index/answer retained")
	}
	if len(w.order) != 1 || w.order[0] != "chat:B" || len(w.resumed) != 1 || w.resumed[0] != "chat:B" || w.inOrder["chat:A"] {
		t.Fatal("queues retained forgotten chat")
	}
	if got := w.Chat("chat:B"); !got.Waiting || !got.RetryWaiting || got.GivenUp != 1 || w.held["b"] == nil {
		t.Fatal("other chat state changed")
	}
	w.retryDue()
	if w.Chat("chat:A").Waiting {
		t.Fatal("retry resurrected forgotten chat")
	}
}

func TestU2ChatOperationWaitsForRunningGeneration(t *testing.T) {
	db := testDB(t)
	hold := make(chan struct{})
	model := &fakeModel{hold: hold}
	w := NewWorker(db, model, "fake", time.Second)
	s := oneTurn()
	save(w, s)
	w.Notify(s.Key, nil)
	// Reload from the real store, so this test covers the production Run.
	w.mu.Lock()
	w.pending[s.Key] = notice{reload: true}
	w.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ended := make(chan struct{})
	go func() { defer close(ended); w.Run(ctx) }()
	if !waitFor(t, func() bool { return model.calls() > 0 }) {
		close(hold)
		return
	}
	started := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		close(started)
		_ = w.WithChatOperation(func() error { w.Forget(s.Key, nil); close(finished); return nil })
	}()
	<-started
	select {
	case <-finished:
		t.Fatal("chat operation overlapped an in-flight provider")
	case <-time.After(20 * time.Millisecond):
	}
	close(hold)
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("chat operation never resumed")
	}
	cancel()
	select {
	case <-ended:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not stop")
	}
}
