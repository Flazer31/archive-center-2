package generate

// WithChatOperation preserves the existing exclusive operation point: all
// generation and index runs finish before a source is copied/deleted.
// Acquire this before the pipeline lock; notifications only take w.mu.
func (w *Worker) WithChatOperation(fn func() error) error {
	return w.Maintain(fn) // the same between-runs point as maintenance
}

// Forget drops only this chat's pending/retry state and retained answers.
// Called with WithChatOperation and the observation lock held, after commit.
func (w *Worker) Forget(session string, sourceKeys []string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.pending, session)
	delete(w.inOrder, session)
	delete(w.backlog, session)
	delete(w.indexQueue, session)
	delete(w.indexRetry, session)
	for key, ask := range w.turnRearms {
		if ask.session == session {
			delete(w.turnRearms, key)
		}
	}
	delete(w.retryAt, session)
	delete(w.last, session)
	delete(w.gaveUp, session)
	for _, key := range sourceKeys {
		delete(w.held, key)
	}
	keep := func(in []string) []string {
		out := in[:0]
		for _, key := range in {
			if key != session {
				out = append(out, key)
			}
		}
		return out
	}
	w.order, w.resumed = keep(w.order), keep(w.resumed)
}
