// Package generate makes the memory of confirmed turns: it builds the model
// input from what earlier turns stored, calls the provider, and stores the
// result with its source. A Worker runs it outside the observation path, so
// waiting for the model never holds observation processing.
package generate

import (
	"archivecenter2/goservice/internal/body"
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/provider"
	"archivecenter2/goservice/internal/store"
)

// Provider is the model call.
type Provider interface {
	Chat(ctx context.Context, prompt string) (provider.Result, error)
}

// Store is the part of store.DB generation uses.
type Store interface {
	JobFor(src memory.Source) (store.Job, error)
	CurrentJobs(sources []memory.Source) ([]int64, error)
	KnownFor(sources []memory.Source) (memory.Known, error)
	SaveGeneration(j store.Job, g store.Generation) ([]string, error)
	FailJob(j store.Job, model, request, readItems, reason string, retryAt time.Time) error
	KeepResponse(j store.Job, model, request, readItems, response string) error
	StartAttempt(j store.Job) error
	KeepUnreadable(j store.Job, model, request, readItems, response string, notes []string, retryAt time.Time) error
	DoneKeys(session string) (map[string]bool, error)
	LoadSession(key string) (*lifecycle.Session, bool, error)
	SessionKeys() ([]string, error)
	Grant(session string, turns []int) error
	Grants(session string) (map[int]bool, error)
	Ungrant(session string, turn int) error
	RearmFailedJobs() (int64, error)
}

// Report is what one turn's generation did.
type Report struct {
	Turn   int
	Job    int64
	Done   bool // the job was already done; nothing was called
	Reused bool // the answer received on an earlier run was stored; nothing was called
	// Answer is the answer in hand (received now or earlier); when storing
	// it failed, the caller keeps it for the next run
	Answer     *Answer
	Prompt     string
	Known      string
	Selection  memory.Selection
	KnownItems int // stored memories the selection read from
	Result     provider.Result
	Output     memory.Output
	Notes      []string // parse and save diagnostics
}

// Turn generates the memory of one turn. earlier are the sources of the
// turns before it, in order; their current jobs are what the model reads.
func Turn(ctx context.Context, st Store, p Provider, model string, earlier []memory.Source, src memory.Source) (Report, error) {
	return turn(ctx, st, p, model, earlier, 0, src, nil, nil, 0)
}

// errNewer: a newer notice of the chat came before the call began; the
// next run goes on from it.
var errNewer = errors.New("a newer notice of the chat is waiting")

// ErrUnreadable is returned for an answer in which no response object could
// be read; it is kept and read or made again after a wait.
var ErrUnreadable = errors.New("no response object could be read in the answer")

// RetryDelays are the waits after the 1st, 2nd, 3rd... failed call of a job
// (the last one repeats).
var RetryDelays = []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute}

// RetryDelay is the wait after the n-th failed call (n from 1).
func RetryDelay(n int) time.Duration {
	if n < 1 { // no call made yet (a read or store failed first)
		n = 1
	}
	if n > len(RetryDelays) {
		n = len(RetryDelays)
	}
	return RetryDelays[n-1]
}

// Answer is a received model answer with what was sent for it.
type Answer struct {
	Model, Request, ReadItems, Response string
}

// turn is Turn with an answer the caller still holds from an earlier run
// whose stores all failed (the job was not marked received): it is stored
// without calling the model again.
// newer, when set, reports a newer notice of the chat; it is asked where a
// new model call would begin (after the input is read and built), and the
// call is left to the run of the newer notice (ASTRA-RECHECK-57 2). An
// answer in hand is stored as before.
// knownMax is the ceiling on the whole request, in chars (0 = none; the
// critic setting known_chars). Beta 0.0.1 had left it unapplied; it applies
// again (user 11-09, a tester's 3,000-char setting changed nothing), on the
// whole request since 1.0.8 (a long reply alone took 15,000-25,000 chars):
// the stored memory gets what the rest leaves, through memory.SelectWithin.
// past: how many of earlier, from the start, are an attached past (C22);
// the prompt then numbers turns in story order (the past 1..past, the
// chat's own turns after it), as injection does.
func turn(ctx context.Context, st Store, p Provider, model string, earlier []memory.Source, past int, src memory.Source, held *Answer, newer func() bool, knownMax int) (Report, error) {
	r := Report{Turn: src.Turn}
	job, err := st.JobFor(src)
	if err != nil {
		return r, err
	}
	r.Job = job.ID
	if job.State == "done" {
		r.Done = true
		return r, nil
	}
	var keepNote []string
	if job.State == "unreadable" && held == nil {
		// read it again first (a later parser may read it); made again only
		// when it is still unreadable (it stays in unread_answers)
		if _, notes := memory.Parse(job.Response); !memory.Unreadable(notes) {
			held = &Answer{job.Model, job.Request, job.ReadItems, job.Response}
		}
	}
	if job.State == "received" && held == nil {
		held = &Answer{job.Model, job.Request, job.ReadItems, job.Response}
	} else if held != nil && job.State != "unreadable" {
		// still not kept: keep it now, then store it
		if err := st.KeepResponse(job, held.Model, held.Request, held.ReadItems, held.Response); err != nil {
			keepNote = append(keepNote, "answer not kept before storing: "+err.Error())
		}
	}
	if held != nil {
		// the answer came on an earlier run and was not stored: store it,
		// without calling the model again
		model, r.Prompt, r.Known = held.Model, held.Request, held.ReadItems
		r.Result = provider.Result{Content: held.Response}
		r.Reused = true
	} else {
		var known memory.Known
		if ks, ok := st.(interface {
			KnownForStory([]memory.Source, int) (memory.Known, error)
		}); ok && past > 0 {
			known, err = ks.KnownForStory(earlier, past)
		} else {
			known, err = st.KnownFor(earlier)
		}
		if err != nil {
			return r, err
		}
		r.KnownItems = len(known.Memories)
		// generation reads the replies without the model's reasoning
		// envelopes; the stored source keeps them
		read := memory.SourceWithoutReasoning(src)
		read.Turn += past // the prompt's number of this turn in story order
		var texts []string
		for _, e := range earlier {
			texts = append(texts, memory.TurnText(memory.SourceWithoutReasoning(e)))
		}
		text := memory.TurnText(read)
		instructions := memory.Instructions
		if ps, ok := st.(interface{ CriticPrompt() (string, error) }); ok {
			text, err := ps.CriticPrompt()
			if err != nil {
				return r, err
			}
			if text != "" {
				instructions = text
			}
		}
		// body tracking (1.0 E09/E10): its rules only while it is on, after
		// the instructions (an edited critic prompt keeps them)
		if bt, ok := st.(interface {
			BodyTracking() (body.Toggles, error)
		}); ok {
			on, err := bt.BodyTracking()
			if err != nil {
				return r, err
			}
			if on.Any() {
				instructions += "\n" + memory.BodyRules
			}
		}
		// the ceiling is on the whole request (user 11-09 "바꾸고 확인해"):
		// what the stored memory may take is what the instructions, the
		// turn's text and its date leave; a long reply leaves less (a
		// record that always stays is still shown, so the request can go
		// over by what must stay)
		known1 := 0
		if knownMax > 0 {
			bare := len([]rune(memory.PromptWithText(read, "x", instructions, text))) - 1
			known1 = max(knownMax-bare, 1)
		}
		var left int
		r.Selection, left = memory.SelectWithin(known, text, texts, known1)
		r.Known = memory.RenderKnown(r.Selection, memory.Superseded(known))
		if left > 0 {
			keepNote = append(keepNote, fmt.Sprintf("request over its ceiling (%d chars, %d for stored memory): %d records not shown", knownMax, known1, left))
		}
		r.Prompt = memory.PromptWithText(read, r.Known, instructions, text)
		if newer != nil && newer() {
			return r, errNewer
		}
		if err := st.StartAttempt(job); err != nil {
			return r, err
		}
		r.Result, err = p.Chat(ctx, r.Prompt)
		if err != nil {
			if ferr := st.FailJob(job, model, r.Prompt, r.Known, err.Error(), time.Now().Add(RetryDelay(job.Attempts+1))); ferr != nil {
				return r, ferr
			}
			return r, err
		}
		// kept before storing; if keeping fails, the answer in hand is
		// still stored now
		if err := st.KeepResponse(job, model, r.Prompt, r.Known, r.Result.Content); err != nil {
			keepNote = append(keepNote, "answer not kept before storing: "+err.Error())
		}
	}
	notes := keepNote
	var parseNotes []string
	r.Output, parseNotes = memory.Parse(r.Result.Content)
	if memory.Unreadable(parseNotes) {
		// nothing could be read: kept, and read or made again after a wait
		// within the job's call limit (a severe loss, not a small error)
		n := job.Attempts
		if !r.Reused {
			n++
		}
		if n < 1 {
			n = 1
		}
		r.Notes = append(notes, parseNotes...)
		if err := st.KeepUnreadable(job, model, r.Prompt, r.Known, r.Result.Content, r.Notes, time.Now().Add(RetryDelay(n))); err != nil {
			// not recorded yet: the answer stays in hand and is recorded on
			// the next run, without a call
			r.Answer = &Answer{model, r.Prompt, r.Known, r.Result.Content}
			return r, err
		}
		return r, ErrUnreadable
	}
	r.Answer = &Answer{model, r.Prompt, r.Known, r.Result.Content}
	notes = append(notes, parseNotes...)
	saveNotes, err := st.SaveGeneration(job, store.Generation{Model: model, Request: r.Prompt, ReadItems: r.Known, Response: r.Result.Content, Output: r.Output, Notes: notes})
	if err != nil {
		r.Notes = notes
		return r, err
	}
	r.Notes = saveNotes
	return r, nil
}

// Worker generates confirmed turns in order within each chat. Different chats
// run independently; indexing runs separately. Notify only records the turns; a
// newer notice of the same chat replaces an older one still waiting.
type Worker struct {
	startMu sync.Mutex   // coordinates entering a run with reset cancellation
	workMu  sync.RWMutex // concurrent runs, exclusive maintenance
	st      Store
	p       Provider
	model   string
	timeout time.Duration
	// Index, when set, indexes the memory of the given jobs (vectors). A
	// failed index is filled on the chat's next run.
	Index func(ctx context.Context, jobs []int64) error
	// Retries is how many more model calls a job gets after a failed one
	// (user setting, default 3); waits between them are RetryDelays.
	Retries int
	// knownMax is the ceiling on the stored memory a call reads (user
	// setting; 0 = none), set by SetKnownMax
	knownMax int
	// Event, when set, hears what happened to a turn, for the status screen:
	// "made", "failed" (err), "given_up" (its calls used up), "indexed" or
	// "index_failed" (err; turn 0: the chat's vectors), "rearmed" (session
	// "", turn = the turns tried again). It is called without the worker's
	// lock.
	Event func(kind, session string, turn int, err error)
	// Progress reports measured HUD phases without changing the existing status
	// event stream (which reports outcomes only).
	Progress func(Progress)
	now      func() time.Time

	mu       sync.Mutex
	pending  map[string]notice
	order    []string        // chats with a notice, first
	inOrder  map[string]bool // chats in order
	resumed  []string        // chats queued by Resume, after the noticed ones
	resumeAt time.Time       // a failed Resume is tried again then
	wake     chan struct{}

	running    map[string]context.CancelFunc // active generation only, w.mu held
	indexing   map[string]context.CancelFunc // active index runs, w.mu held
	indexQueue map[string][][]int64          // per-chat FIFO, no idle goroutines
	indexRetry map[string]time.Time          // indexing waits independently of generation
	indexJobs  map[int64]chan struct{}       // shared parent jobs cannot index concurrently
	rearm      bool                          // a rearm waits for the end of the turn under way
	rearmAt    time.Time                     // a failed rearm is tried again then
	turnRearms map[string]turnRearm          // explicit HUD retries, handled at the same rearm point

	done, failed int
	lastError    string
	backlog      map[string]bool              // chats whose indexing failed
	held         map[string]map[int64]*Answer // answers whose stores all failed, by source and job
	indexFailed  int
	retryAt      map[string]time.Time       // chats with a failed turn waiting for its next call
	last         map[string]notice          // the notice to run again for them
	gaveUp       map[string]map[string]bool // per chat: source keys whose calls are used up
}

// notice is a chat as the observation path last saw it.
type notice struct {
	turns []memory.ScopedTurn // the chat's read scope as last observed
	// reload: only the chat's key; its turns are read from the store when it
	// runs (a resume after a restart, a retry after a wait)
	reload bool
}

func NewWorker(st Store, p Provider, model string, timeout time.Duration) *Worker {
	return &Worker{st: st, p: p, model: model, timeout: timeout, pending: map[string]notice{}, inOrder: map[string]bool{}, backlog: map[string]bool{}, held: map[string]map[int64]*Answer{}, wake: make(chan struct{}, 1),
		running: map[string]context.CancelFunc{}, indexing: map[string]context.CancelFunc{}, indexQueue: map[string][][]int64{}, indexRetry: map[string]time.Time{}, indexJobs: map[int64]chan struct{}{},
		Retries: 3, now: time.Now, retryAt: map[string]time.Time{}, last: map[string]notice{}, gaveUp: map[string]map[string]bool{}}
}

// Notify gives a chat's active turns in order (memory.Own) and, for a
// branch, its parent and branch point. It does not block.
func (w *Worker) Notify(session string, turns []memory.ScopedTurn) {
	w.mu.Lock()
	if !w.inOrder[session] { // also a chat queued by Resume: a notice goes first
		w.order = append(w.order, session)
		w.inOrder[session] = true
	}
	w.pending[session] = notice{turns: turns}
	delete(w.indexRetry, session) // an explicit new run fills failed indexing
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Configure sets the model connection and the retry count the next job
// uses (the settings screen); a nil provider makes generation wait, with
// every notice kept, until one is set.
func (w *Worker) Configure(p Provider, model string, retries int) {
	w.mu.Lock()
	w.p, w.model, w.Retries = p, model, retries
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Rearm asks for the turns given up to be tried again (the critic was
// changed). The worker does it between chats, so a call of the old critic
// under way ends and counts first: the store resets the attempts, the
// worker forgets the turns given up and the waits, and every chat runs
// again.
func (w *Worker) Rearm() {
	w.mu.Lock()
	w.rearm = true
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// rearmNow does an asked rearm; on a store failure it stays asked and is
// tried again after a wait. It reports whether chats may run now.
func (w *Worker) rearmNow() bool {
	w.mu.Lock()
	asked, waiting := w.rearm, !w.rearmAt.IsZero()
	w.mu.Unlock()
	if waiting {
		return false // a failed reset waits for its time (retryDue)
	}
	if !asked {
		return true
	}
	n, err := w.st.RearmFailedJobs()
	if err != nil {
		w.record(false, err)
		w.mu.Lock()
		w.rearmAt = w.now().Add(RetryDelay(1)) // Run waits for it
		w.mu.Unlock()
		return false
	}
	w.mu.Lock()
	w.rearm, w.rearmAt = false, time.Time{}
	w.gaveUp = map[string]map[string]bool{}
	w.retryAt = map[string]time.Time{}
	w.last = map[string]notice{}
	w.mu.Unlock()
	if n > 0 {
		w.event("rearmed", "", int(n), nil)
	}
	w.Resume()
	return true
}

// keep puts a chat's run back in front of the queue, unless a newer notice
// of it is already waiting.
func (w *Worker) keep(session string, n notice) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, newer := w.pending[session]; newer {
		return
	}
	w.pending[session] = n
	if !w.inOrder[session] {
		w.order = append([]string{session}, w.order...)
		w.inOrder[session] = true
	}
}

func (w *Worker) active() (Provider, string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.p, w.model
}

// SetKnownMax sets the reading ceiling the next turn uses (the settings
// screen).
func (w *Worker) SetKnownMax(n int) {
	w.mu.Lock()
	w.knownMax = n
	w.mu.Unlock()
}

func (w *Worker) readMax() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.knownMax
}

func (w *Worker) retries() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.Retries
}

// Reindex runs a chat again to make the vectors its memories lack (a
// memory's text was edited): only texts without a vector are sent.
func (w *Worker) Reindex(session string) {
	w.mu.Lock()
	delete(w.indexRetry, session)
	if w.Index != nil {
		w.backlog[session] = true
	}
	if _, waiting := w.pending[session]; !waiting {
		w.pending[session] = notice{reload: true}
		if !w.inOrder[session] {
			w.order = append(w.order, session)
			w.inOrder[session] = true
		}
	}
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// ChatState is one chat's generation as the status screen shows it.
type ChatState struct {
	Waiting      bool `json:"waiting"`       // a notice waits for its run
	Running      bool `json:"running"`       // being worked on now
	RetryWaiting bool `json:"retry_waiting"` // a failed turn waits for its next call
	GivenUp      int  `json:"given_up"`      // turns whose calls are used up
}

// Chat reports a chat's generation state.
func (w *Worker) Chat(session string) ChatState {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, waiting := w.pending[session]
	_, retry := w.retryAt[session]
	_, indexRetry := w.indexRetry[session]
	return ChatState{Waiting: waiting, Running: w.running[session] != nil, RetryWaiting: retry || indexRetry, GivenUp: len(w.gaveUp[session])}
}

// nextRetry is the earliest time a waiting chat may call again.
func (w *Worker) nextRetry() (time.Time, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	first := w.resumeAt
	if !w.rearmAt.IsZero() && (first.IsZero() || w.rearmAt.Before(first)) {
		first = w.rearmAt
	}
	for _, at := range w.retryAt {
		if first.IsZero() || at.Before(first) {
			first = at
		}
	}
	for _, at := range w.indexRetry {
		if first.IsZero() || at.Before(first) {
			first = at
		}
	}
	for _, ask := range w.turnRearms {
		if !ask.at.IsZero() && (first.IsZero() || ask.at.Before(first)) {
			first = ask.at
		}
	}
	return first, !first.IsZero()
}

// retryDue runs again the chats whose wait is over (and Resume, if it
// failed).
func (w *Worker) retryDue() {
	w.mu.Lock()
	resume := !w.resumeAt.IsZero() && !w.now().Before(w.resumeAt)
	if resume {
		w.resumeAt = time.Time{}
	}
	if !w.rearmAt.IsZero() && !w.now().Before(w.rearmAt) {
		w.rearmAt = time.Time{} // the rearm still asked runs on the next loop
	}
	for s, at := range w.indexRetry {
		if !w.now().Before(at) {
			delete(w.indexRetry, s)
		}
	}
	for key, ask := range w.turnRearms {
		if !ask.at.IsZero() && !w.now().Before(ask.at) {
			ask.at = time.Time{}
			w.turnRearms[key] = ask
		}
	}
	w.mu.Unlock()
	if resume {
		w.Resume()
	}
	w.mu.Lock()
	for s, at := range w.retryAt {
		if w.now().Before(at) {
			continue
		}
		n := w.last[s]
		delete(w.retryAt, s)
		delete(w.last, s)
		// a newer notice waiting is the chat's latest observation; the
		// check and the queueing are one locked step
		if _, newer := w.pending[s]; !newer {
			w.pending[s] = n
			if !w.inOrder[s] {
				w.order = append(w.order, s)
				w.inOrder[s] = true
			}
		}
	}
	w.mu.Unlock()
}

// waitRetry remembers that a chat has a failed turn whose next call may
// start at at.
func (w *Worker) waitRetry(session string, n notice, at time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if cur, ok := w.retryAt[session]; !ok || at.Before(cur) {
		w.retryAt[session] = at
	}
	w.last[session] = notice{reload: true} // the latest stored state runs then
}

// callable reports whether a job may start a model call now: its calls are
// not used up and its wait after a failed call is over. A received answer
// needs no call.
func (w *Worker) callable(session string, n notice, src memory.Source) bool {
	j, err := w.st.JobFor(src)
	if err != nil || j.State == "received" || j.State == "done" {
		return true
	}
	if j.State == "unreadable" {
		if _, notes := memory.Parse(j.Response); !memory.Unreadable(notes) {
			return true // read again now, without a call
		}
	}
	if j.Attempts >= 1+w.retries() {
		w.giveUp(session, src.Key())
		return false
	}
	if j.Attempts > 0 && w.now().Before(j.NextAt) {
		w.waitRetry(session, n, j.NextAt)
		return false
	}
	w.mu.Lock()
	delete(w.gaveUp[session], src.Key()) // e.g. the setting was raised
	w.mu.Unlock()
	return true
}

func (w *Worker) giveUp(session, key string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.gaveUp[session] == nil {
		w.gaveUp[session] = map[string]bool{}
	}
	w.gaveUp[session][key] = true
}

func (w *Worker) next(generating bool) (string, notice, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !generating {
		// no critic: a chat whose vectors are missing, its notice left in
		// place for the generation to come
		if w.Index == nil {
			return "", notice{}, false
		}
		for _, s := range append(append([]string{}, w.order...), w.resumed...) {
			if t, ok := w.pending[s]; ok && w.backlog[s] {
				delete(w.pending, s)
				return s, t, true
			}
		}
		return "", notice{}, false
	}
	// noticed chats first, then the ones queued by Resume
	for left := len(w.order); left > 0; left-- {
		s := w.order[0]
		w.order = w.order[1:]
		if w.running[s] != nil || w.turnRearmPending(s) {
			w.order = append(w.order, s)
			continue
		}
		delete(w.inOrder, s)
		if t, ok := w.pending[s]; ok {
			delete(w.pending, s)
			return s, t, true
		}
	}
	for left := len(w.resumed); left > 0; left-- {
		s := w.resumed[0]
		w.resumed = w.resumed[1:]
		if w.running[s] != nil || w.turnRearmPending(s) {
			w.resumed = append(w.resumed, s)
			continue
		}
		if t, ok := w.pending[s]; ok {
			delete(w.pending, s)
			return s, t, true
		}
	}
	return "", notice{}, false
}

// chat generates every ready, readable turn not yet done, in order. A turn
// not ready (not confirmed, or a reply not readable yet) is passed over; later turns go on, and it is made when a later notice finds it
// ready. A failed turn is recorded and tried again on a later notice. A
// branch reads its parent's turns up to the branch point first; those are
// the parent's memory and are not made here.
func (w *Worker) chat(ctx context.Context, session string, n notice) {
	// this run decides the chat's waits again from the latest observation
	w.mu.Lock()
	delete(w.retryAt, session)
	delete(w.last, session)
	delete(w.gaveUp, session)
	w.mu.Unlock()
	if n.reload {
		s, ok, err := w.st.LoadSession(session)
		if err != nil {
			w.record(false, err)
			w.waitRetry(session, n, w.now().Add(RetryDelay(1)))
			return
		}
		if !ok {
			return
		}
		n = notice{turns: memory.WithPast(s)}
	}
	done, err := w.st.DoneKeys(session)
	if err != nil {
		w.record(false, err)
		w.waitRetry(session, n, w.now().Add(RetryDelay(1))) // read again later
		return
	}
	// history turns the user asked to make (Backfill), kept in the store
	grants, err := w.st.Grants(session)
	if err != nil {
		w.record(false, err)
		w.waitRetry(session, n, w.now().Add(RetryDelay(1)))
		return
	}
	// the read scope: a recorded copy of a parent turn is already the
	// parent's source (memory.Own)
	turns := memory.On(n.turns)
	// As AC1, a branch makes only its own memory. A copy read as the
	// parent's source is read from the parent's memory when the parent made
	// it; a copy the parent has not made (the branch went on from the
	// parent's last reply) is made as the branch's own turn.
	parentDone := map[string]map[string]bool{}
	var earlier []memory.Source
	past := 0
	for _, t := range turns {
		if t.Past {
			past++
		}
	}
	for _, t := range turns {
		src := t.Source
		if t.Inherited {
			d, ok := parentDone[src.Session]
			if !ok {
				var err error
				if d, err = w.st.DoneKeys(src.Session); err != nil {
					w.record(false, err)
					w.waitRetry(session, n, w.now().Add(RetryDelay(1)))
					return
				}
				parentDone[src.Session] = d
			}
			if d[src.Key()] {
				earlier = append(earlier, src)
				continue
			}
			src = t.OwnSource()
			if t.Past {
				// an attached past (C22) is read, never made by this chat
				earlier = append(earlier, src)
				continue
			}
		}
		// ready: confirmed by the chat going on (the next input), or a turn
		// of earlier history the user asked to make (Backfill)
		ready := t.Confirmed || (!t.Inherited && grants[src.Turn])
		key := src.Key()
		var held *Answer
		if ready && src.Readable() && (!done[key] || grants[src.Turn]) {
			j, e := w.st.JobFor(src)
			if e != nil {
				w.record(false, e)
				w.waitRetry(session, n, w.now().Add(RetryDelay(1)))
				return
			}
			done[key] = j.State == "done"
			w.mu.Lock()
			held = w.held[key][j.ID]
			w.mu.Unlock()
		}
		if done[key] && grants[src.Turn] {
			if uerr := w.st.Ungrant(session, src.Turn); uerr != nil {
				w.record(false, uerr)
				w.waitRetry(session, n, w.now().Add(RetryDelay(1)))
			}
		}
		if ready && src.Readable() && !done[key] && (held != nil || w.callable(session, n, src)) {
			p, model := w.active()
			timeout := w.timeout
			if c, ok := p.(interface{ CallTimeout() time.Duration }); ok && c.CallTimeout() > 0 {
				timeout = c.CallTimeout()
			}
			tctx, cancel := context.WithTimeout(ctx, timeout)
			// a newer notice of this chat came while this run went on: the
			// next run goes on from it, so a turn the chat no longer has
			// (deleted, rerolled) is not made from this one
			newer := func() bool {
				w.mu.Lock()
				defer w.mu.Unlock()
				_, ok := w.pending[session]
				return ok
			}
			w.mu.Lock()
			rearm := w.rearm || w.turnRearmPending(session)
			w.mu.Unlock()
			if rearm {
				// another critic was saved: the attempts are reset before the
				// next turn is called, then the chat runs again
				cancel()
				w.keep(session, n)
				return
			}
			if p == nil && held == nil {
				// generation was turned off while this run went on: the
				// chat waits, as every notice does, until a critic is set
				cancel()
				w.keep(session, n)
				return
			}
			w.progress("generating", session, src.Turn, src.Key(), nil, nil)
			r, err := turn(provider.WithSession(tctx, strings.TrimPrefix(session, "chat:")), w.st, p, model, earlier, past, src, held, newer, w.readMax())
			w.progress("generation_finished", session, src.Turn, src.Key(), nil, err)
			cancel()
			if errors.Is(err, errNewer) {
				return
			}
			w.mu.Lock()
			if err != nil && r.Answer != nil {
				w.held[key] = map[int64]*Answer{r.Job: r.Answer}
			} else {
				delete(w.held, key) // stored, or recorded as unreadable
			}
			w.mu.Unlock()
			w.record(err == nil && !r.Done, err)
			switch {
			case err != nil:
				w.event("failed", session, src.Turn, err)
			case !r.Done:
				w.event("made", session, src.Turn, nil)
			}
			if err != nil {
				w.afterFailure(session, n, src, r)
			} else {
				w.mu.Lock()
				delete(w.gaveUp[session], key)
				w.mu.Unlock()
			}
			if err == nil && grants[src.Turn] {
				if uerr := w.st.Ungrant(session, src.Turn); uerr != nil {
					w.record(false, uerr) // made; dropped on the next run
					w.waitRetry(session, n, w.now().Add(RetryDelay(1)))
				}
			}
			if err == nil && !r.Done {
				w.queueIndex(session, []int64{r.Job})
			}
		}
		earlier = append(earlier, src)
		if ctx.Err() != nil {
			return
		}
	}
}

// scopeSources are the sources a read scope's memory can come from: each
// turn's, and for a copy the branch's own (made as the branch's when the
// parent has not made it).
func scopeSources(turns []memory.ScopedTurn) []memory.Source {
	var srcs []memory.Source
	for _, t := range turns {
		srcs = append(srcs, t.Source)
		if t.Inherited {
			srcs = append(srcs, t.OwnSource())
		}
	}
	return srcs
}

// takeBacklog takes a chat's index request.
func (w *Worker) takeBacklog(session string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	asked := w.backlog[session]
	delete(w.backlog, session)
	return asked
}

// afterFailure decides when a failed turn runs again, without a new notice:
// an answer in hand is stored again after a short wait (it needs no model
// call, so the call limit does not apply); a failed call waits for the job's
// next_at while calls remain; with calls used up it is reported as given up.
func (w *Worker) afterFailure(session string, n notice, src memory.Source, r Report) {
	if r.Answer != nil {
		w.waitRetry(session, n, w.now().Add(RetryDelay(1)))
		return
	}
	j, err := w.st.JobFor(src)
	if err != nil {
		w.waitRetry(session, n, w.now().Add(RetryDelay(1)))
		return
	}
	if j.Attempts >= 1+w.retries() {
		w.giveUp(session, src.Key())
		// heard here only, where its last call failed; later runs find it
		// given up again
		w.event("given_up", session, src.Turn, nil)
		return
	}
	at := j.NextAt
	if !at.After(w.now()) {
		at = w.now().Add(RetryDelay(j.Attempts))
	}
	w.waitRetry(session, n, at)
}

func (w *Worker) event(kind, session string, turn int, err error) {
	if w.Event != nil {
		w.Event(kind, session, turn, err)
	}
}

func (w *Worker) record(made bool, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err != nil {
		w.failed++
		w.lastError = err.Error()
	} else if made {
		w.done++
	}
}

// index runs Index on the jobs; a failure puts the chat on the backlog.
func (w *Worker) index(ctx context.Context, session string, jobs []int64) bool {
	if w.Index == nil || len(jobs) == 0 {
		return true
	}
	if !w.claimIndex(ctx, jobs) {
		return false
	}
	defer w.releaseIndex(jobs)
	w.progress("indexing", session, 0, "", jobs, nil)
	err := w.Index(ctx, jobs)
	w.progress("index_finished", session, 0, "", jobs, err)
	if err != nil {
		w.event("index_failed", session, 0, err)
	} else {
		w.event("indexed", session, 0, nil)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if err != nil {
		w.backlog[session] = true
		w.indexFailed++
		w.lastError = err.Error()
		// indexed again after a wait, without a notice
		if cur, ok := w.indexRetry[session]; !ok || w.now().Add(RetryDelay(1)).Before(cur) {
			w.indexRetry[session] = w.now().Add(RetryDelay(1))
		}
		return false
	}
	return true
}

// Resume queues every stored chat after a restart, by key only: each is
// read from the store when it runs, so the turns left to make (confirmed or
// asked for, failed or kept answers, missing vectors) go on without a new
// notice. Only active chats retain execution goroutines. A notice that comes
// later replaces the queued key.
func (w *Worker) Resume() error {
	keys, err := w.st.SessionKeys()
	if err != nil {
		w.mu.Lock()
		w.resumeAt = w.now().Add(RetryDelay(1)) // tried again by Run
		w.mu.Unlock()
		select {
		case w.wake <- struct{}{}:
		default:
		}
		return err
	}
	w.mu.Lock()
	for _, k := range keys {
		if _, waiting := w.pending[k]; !waiting {
			w.resumed = append(w.resumed, k)
			w.pending[k] = notice{reload: true}
		}
		if w.Index != nil {
			w.backlog[k] = true // only texts still without a vector are sent
		}
	}
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
	return nil
}

// Diagnostics reports generation counts for /diagnostics.
func (w *Worker) Diagnostics() map[string]any {
	w.mu.Lock()
	defer w.mu.Unlock()
	givenUp := 0
	for _, keys := range w.gaveUp {
		givenUp += len(keys)
	}
	running, indexing := []string{}, []string{}
	for s := range w.running {
		running = append(running, s)
	}
	for s := range w.indexing {
		indexing = append(indexing, s)
	}
	sort.Strings(running)
	sort.Strings(indexing)
	return map[string]any{"generation_runs": w.done, "generation_failures": w.failed, "generation_last_error": w.lastError, "generation_waiting_chats": len(w.pending), "index_failures": w.indexFailed, "index_backlog_chats": len(w.backlog), "generation_retry_waiting_chats": len(w.retryAt), "generation_given_up_turns": givenUp, "generation_retries": w.Retries, "generation_known_max": w.knownMax, "generation_running": len(w.running) != 0,
		"generation_running_chats": len(running), "generation_running_sessions": running, "index_running_chats": len(indexing), "index_running_sessions": indexing, "index_retry_waiting_chats": len(w.indexRetry)}
}

// Backfill makes turns of a chat's earlier history (loaded, never confirmed
// by an output event: a chat opened after install, or rows copied into a
// branch), as AC1's rebuild of missing turns did: at most max of them (0 =
// all), the oldest first or the most recent first (which turns are chosen;
// they are made in story order). It returns the turns it asked for; they are
// made on the worker like any other turn. Call it with the observation lock
// held (pipeline.WithSession), so turns is the chat as last observed and no
// later notice is replaced by an older one.
func (w *Worker) Backfill(session string, turns []memory.ScopedTurn, max int, recentFirst bool) ([]int, error) {
	done, err := w.st.DoneKeys(session)
	if err != nil {
		return nil, err
	}
	// the read scope the worker uses: a branch's copies of parent turns are
	// the parent's and are not asked for here
	scope := memory.On(turns)
	var want []int
	for _, t := range scope {
		if !t.Inherited && !t.Confirmed && t.Source.Readable() && !done[t.Source.Key()] {
			want = append(want, t.Source.Turn)
		}
	}
	if recentFirst {
		for i, j := 0, len(want)-1; i < j; i, j = i+1, j-1 {
			want[i], want[j] = want[j], want[i]
		}
	}
	if max > 0 && len(want) > max {
		want = want[:max]
	}
	if err := w.st.Grant(session, want); err != nil {
		return nil, err
	}
	w.mu.Lock()
	_, waiting := w.pending[session]
	w.mu.Unlock()
	if waiting {
		// a notice of this chat waits: it was given later than these turns
		// were read, so it is kept and runs the asked turns
		select {
		case w.wake <- struct{}{}:
		default:
		}
		return want, nil
	}
	w.Notify(session, turns)
	return want, nil
}
