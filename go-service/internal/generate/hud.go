package generate

import (
	"strconv"
	"time"
)

// Progress is diagnostic data only; it cannot affect turn acceptance or storage.
// Source and Jobs attach timing to the actual revision being worked on.
type Progress struct {
	Kind, Session, Source string
	Turn                  int
	Jobs                  []int64
	Err                   error
}

func (w *Worker) progress(kind, session string, turn int, source string, jobs []int64, err error) {
	if w.Progress != nil {
		w.Progress(Progress{kind, session, source, turn, jobs, err})
	}
}

// RetryTurn uses the worker's existing rearm point, between runs. reset is
// the store's scoped rearm, not another model-call or persistence path.
func (w *Worker) RetryTurn(session string, turn int, reset func() error) {
	w.mu.Lock()
	if w.turnRearms == nil {
		w.turnRearms = map[string]turnRearm{}
	}
	w.turnRearms[session+":"+strconv.Itoa(turn)] = turnRearm{session: session, reset: reset}
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

type turnRearm struct {
	session string
	reset   func() error
	at      time.Time
}

// w.mu held; only this chat pauses for an explicit HUD retry.
func (w *Worker) turnRearmPending(session string) bool {
	for _, ask := range w.turnRearms {
		if ask.session == session {
			return true
		}
	}
	return false
}

func (w *Worker) rearmTurnsNow() bool {
	w.mu.Lock()
	asks := map[string]turnRearm{}
	for key, ask := range w.turnRearms {
		if w.running[ask.session] == nil && w.indexing[ask.session] == nil && !w.now().Before(ask.at) {
			asks[key] = ask
			delete(w.turnRearms, key)
		}
	}
	w.mu.Unlock()
	ok := true
	for key, ask := range asks {
		if err := ask.reset(); err != nil {
			w.record(false, err)
			w.mu.Lock()
			if w.turnRearms == nil {
				w.turnRearms = map[string]turnRearm{}
			}
			ask.at = w.now().Add(RetryDelay(1))
			if _, newer := w.turnRearms[key]; !newer {
				w.turnRearms[key] = ask
			}
			w.mu.Unlock()
			ok = false
			continue
		}
		w.mu.Lock()
		delete(w.retryAt, ask.session)
		delete(w.last, ask.session)
		delete(w.gaveUp, ask.session)
		w.mu.Unlock()
		w.keep(ask.session, notice{reload: true})
	}
	return ok
}

func (w *Worker) RetryLimit() int { return w.retries() }
