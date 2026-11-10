package generate

import (
	"context"
	"sync"
	"time"

	"archivecenter2/goservice/internal/memory"
)

func (w *Worker) signal() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Run dispatches one transient runner per active chat and per index lane.
// The shared read lock lives until a runner has stopped all writes. Reset
// cancels every registered runner before acquiring the exclusive lock.
func (w *Worker) Run(ctx context.Context) {
	var runs sync.WaitGroup
	defer runs.Wait()
	for ctx.Err() == nil {
		var maintenance bool
		for ctx.Err() == nil {
			var started bool
			started, maintenance = w.dispatch(ctx, &runs)
			if !started {
				break
			}
		}
		var due <-chan time.Time
		var timer *time.Timer
		if at, ok := w.nextRetry(); ok && !maintenance {
			timer = time.NewTimer(time.Until(at))
			due = timer.C
		}
		select {
		case <-ctx.Done():
		case <-w.wake:
		case <-due:
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

// dispatch never waits for a maintenance writer while holding startMu:
// Reset must remain able to cancel runners even while Maintain waits.
func (w *Worker) dispatch(ctx context.Context, runs *sync.WaitGroup) (started, maintenance bool) {
	w.startMu.Lock()
	defer w.startMu.Unlock()
	if !w.workMu.TryRLock() {
		// Maintain signals after releasing its writer lock. An expired retry
		// timer must not spin while that callback is still running.
		return false, true
	}
	w.retryDue()
	w.rearmTurnsNow()
	w.mu.Lock()
	rearm, idle := w.rearm, len(w.running) == 0
	w.mu.Unlock()
	ready := !rearm
	if rearm && idle {
		ready = w.rearmNow()
	}
	// Backlog is admitted before new generation. Its store reads and vector
	// work execute in the index runner, so neither delays another chat.
	session, n, jobs, indexing := w.nextIndex()
	if !indexing && ready {
		if p, _ := w.active(); p != nil {
			var ok bool
			session, n, ok = w.next(true)
			if !ok {
				session = ""
			}
		}
	}
	if session == "" {
		w.workMu.RUnlock()
		return false, false
	}
	runCtx, cancel := context.WithCancel(ctx)
	w.mu.Lock()
	lane := w.running
	if indexing {
		lane = w.indexing
	}
	lane[session] = cancel
	w.mu.Unlock()
	runs.Add(1)
	go func() {
		defer runs.Done()
		defer w.workMu.RUnlock()
		defer w.signal()
		defer cancel()
		defer func() {
			w.mu.Lock()
			delete(lane, session)
			w.mu.Unlock()
		}()
		if indexing {
			if jobs == nil {
				w.indexScope(runCtx, session, n)
			} else {
				w.index(runCtx, session, jobs)
			}
		} else {
			w.chat(runCtx, session, n)
		}
	}()
	return true, false
}

func (w *Worker) queueIndex(session string, jobs []int64) {
	if w.Index == nil || len(jobs) == 0 {
		return
	}
	w.mu.Lock()
	w.indexQueue[session] = append(w.indexQueue[session], append([]int64(nil), jobs...))
	w.mu.Unlock()
	w.signal()
}

// nextIndex takes a backlog request exactly once. A newer request made
// during its execution stays set and is processed after the current index.
func (w *Worker) nextIndex() (string, notice, []int64, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for session, queue := range w.indexQueue {
		if w.indexing[session] != nil || w.turnRearmPending(session) {
			continue
		}
		jobs := queue[0]
		if len(queue) == 1 {
			delete(w.indexQueue, session)
		} else {
			queue[0] = nil
			w.indexQueue[session] = queue[1:]
		}
		return session, notice{}, jobs, true
	}
	for session := range w.backlog {
		if w.Index == nil || w.indexing[session] != nil || w.turnRearmPending(session) || w.now().Before(w.indexRetry[session]) {
			continue
		}
		n, ok := w.pending[session]
		if !ok {
			n = notice{reload: true}
		}
		delete(w.backlog, session)
		delete(w.indexRetry, session)
		return session, n, nil, true
	}
	return "", notice{}, nil, false
}

func (w *Worker) indexScope(ctx context.Context, session string, n notice) {
	if n.reload {
		s, ok, err := w.st.LoadSession(session)
		if err != nil {
			w.indexReadFailed(session, err)
			return
		}
		if !ok {
			return
		}
		n = notice{turns: memory.WithPast(s)}
	}
	jobs, err := w.st.CurrentJobs(scopeSources(memory.On(n.turns)))
	if err != nil {
		w.indexReadFailed(session, err)
		return
	}
	w.index(ctx, session, jobs)
}

func (w *Worker) indexReadFailed(session string, err error) {
	w.record(false, err)
	w.mu.Lock()
	w.backlog[session] = true
	w.indexRetry[session] = w.now().Add(RetryDelay(1))
	w.mu.Unlock()
}

// Branches may index the same inherited job. Claim the entire batch atomically
// and wait on its owner without holding any worker lock; disjoint jobs run in
// parallel. Claims disappear after use. The indexer itself reuses stored vectors.
func (w *Worker) claimIndex(ctx context.Context, jobs []int64) bool {
	for ctx.Err() == nil {
		w.mu.Lock()
		var wait chan struct{}
		for _, job := range jobs {
			if wait = w.indexJobs[job]; wait != nil {
				break
			}
		}
		if wait == nil {
			for _, job := range jobs {
				if w.indexJobs[job] == nil {
					w.indexJobs[job] = make(chan struct{})
				}
			}
			w.mu.Unlock()
			return true
		}
		w.mu.Unlock()
		select {
		case <-ctx.Done():
			return false
		case <-wait:
		}
	}
	return false
}

func (w *Worker) releaseIndex(jobs []int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, job := range jobs {
		if done := w.indexJobs[job]; done != nil {
			delete(w.indexJobs, job)
			close(done)
		}
	}
}
