package generate

import (
	"context"
	"time"
)

// Maintain runs between worker runs, so an in-flight answer/vector is stored
// before maintenance starts and cannot restore deleted data afterwards.
func (w *Worker) Maintain(fn func() error) error {
	w.workMu.Lock()
	defer func() { w.workMu.Unlock(); w.signal() }()
	return fn()
}

// Reset cancels the active run and waits for it to stop writing before the
// callback deletes data. Holding startMu prevents a new run from starting in
// the gap between cancellation and taking workMu.
func (w *Worker) Reset(fn func() error) error {
	w.startMu.Lock()
	defer w.startMu.Unlock()
	w.mu.Lock()
	for _, cancel := range w.running {
		cancel()
	}
	for _, cancel := range w.indexing {
		cancel()
	}
	w.mu.Unlock()
	return w.Maintain(fn)
}

// Clear forgets runtime work after a successful database reset. The caller
// holds Maintain and the observation/pipeline locks; configured providers stay.
func (w *Worker) Clear() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pending = map[string]notice{}
	w.order, w.resumed = nil, nil
	w.inOrder = map[string]bool{}
	w.resumeAt, w.rearmAt = time.Time{}, time.Time{}
	w.lastError, w.rearm = "", false
	w.running = map[string]context.CancelFunc{}
	w.indexing = map[string]context.CancelFunc{}
	w.indexQueue = map[string][][]int64{}
	w.indexRetry = map[string]time.Time{}
	w.indexJobs = map[int64]chan struct{}{}
	w.turnRearms = nil
	w.done, w.failed, w.indexFailed = 0, 0, 0
	w.backlog = map[string]bool{}
	w.held = map[string]map[int64]*Answer{}
	w.retryAt = map[string]time.Time{}
	w.last = map[string]notice{}
	w.gaveUp = map[string]map[string]bool{}
}
