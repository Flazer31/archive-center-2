// Package pipeline runs stored observations through ingest and lifecycle and
// saves the result. The raw observation is already stored when it runs. A
// failure stops the batch and is returned, so the plugin keeps and resends
// it; the host output is never affected.
package pipeline

import (
	"encoding/json"
	"strings"
	"sync"
	"time"

	"archivecenter2/goservice/internal/ingest"
	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/observe"
)

// Store is the part of store.DB the pipeline uses.
type Store interface {
	LoadSession(key string) (*lifecycle.Session, bool, error)
	SaveSession(s *lifecycle.Session, events []lifecycle.Event, renames []lifecycle.Rename) error
	// RawTextsOf passes the stored raw events of a plugin chat (every text
	// the plugin sent, so every text it may later send as a fingerprint).
	RawTextsOf(chat string, fn func(json.RawMessage)) error
}

type Pipeline struct {
	store Store

	// Changed, when set, is told of a chat whose turns changed (it runs with
	// the observation lock held and must not block: the generation worker
	// only records the turns).
	Changed func(s *lifecycle.Session)
	// Observed is diagnostic only, after SaveSession succeeded. It must not
	// block or modify the session or its already-persisted events.
	Observed func(s *lifecycle.Session, events []lifecycle.Event, started time.Time, elapsedMS int64)

	// Label, when set, is told a chat's host names (chat list) when they
	// differ from the last ones told; a failure is only logged by it.
	Label  func(key, charName, chatName string)
	labels map[string][2]string

	mu        sync.Mutex
	sessions  map[string]*lifecycle.Session
	texts     *ingest.TextCache
	applied   uint64
	resent    uint64
	failures  uint64
	lastError string
}

func New(store Store) *Pipeline {
	p := &Pipeline{store: store, sessions: map[string]*lifecycle.Session{}, texts: ingest.NewTextCache()}
	// runs inside Process (lock held): after a restart, the chat's stored
	// raw events give back every text the plugin may now send as a
	// fingerprint (display texts, translation records, rows without chatId)
	p.texts.Load = func(chat string) bool {
		if chat != "" {
			if _, err := p.session("chat:" + chat); err != nil { // stored revisions
				return false
			}
		}
		return p.store.RawTextsOf(chat, p.texts.LearnEvent) == nil
	}
	return p
}

// session returns the chat's session, loading it (and warming the text
// cache with its stored texts) when it is not in memory.
func (p *Pipeline) session(key string) (*lifecycle.Session, error) {
	if s, ok := p.sessions[key]; ok {
		return s, nil
	}
	loaded, found, err := p.store.LoadSession(key)
	if err != nil {
		return nil, err
	}
	if !found {
		loaded = lifecycle.NewSession(key)
	} else {
		p.warm(loaded)
	}
	p.sessions[key] = loaded
	return loaded, nil
}

// Process applies every chat snapshot in the records, in order. It reports
// whether the plugin should resend texts (some arrived as fingerprints only
// and are not known here). On the first failure it stops and returns the
// error; records before it stay applied and are recognised when resent.
func (p *Pipeline) Process(records []observe.Record) (resendText bool, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, rec := range records {
		var head struct {
			BootID string `json:"boot_id"`
			Seq    int64  `json:"seq"`
		}
		_ = json.Unmarshal(rec.Event, &head)
		results, err := ingest.FromEvent(rec.Event, p.texts)
		if err != nil {
			// an undecodable event will not decode on a resend either
			p.fail(err)
			continue
		}
		for part, res := range results {
			resendText = resendText || res.NeedText
			p.label(res)
			if err := p.apply(res.Snapshot, head.BootID, progressMark(head.Seq, part)); err != nil {
				p.fail(err)
				return resendText, err
			}
		}
	}
	return resendText, nil
}

// label passes on a chat's names once they change; a name not sent keeps
// the one known.
func (p *Pipeline) label(res ingest.Result) {
	if p.Label == nil || (res.ChatName == "" && res.CharName == "") {
		return
	}
	key := res.Snapshot.SessionKey
	if p.labels == nil {
		p.labels = map[string][2]string{}
	}
	was := p.labels[key]
	now := was
	if res.CharName != "" {
		now[0] = res.CharName
	}
	if res.ChatName != "" {
		now[1] = res.ChatName
	}
	if now == was {
		return
	}
	p.labels[key] = now
	p.Label(key, now[0], now[1])
}

// progressMark orders the snapshots of the plugin's events: seq, then the
// snapshot's place in its event (output: event chat, then re-read).
func progressMark(seq int64, part int) int64 { return seq*4 + int64(part) }

// apply applies one snapshot to its chat. A snapshot already applied to the
// same chat from the same plugin boot, or older than one applied (a resent
// or late observation), would repeat history or move the chat back to an
// older view and is skipped. The plugin sends its outbox in order and removes
// an event only after success, so a snapshot is never applied before an
// earlier one of the same boot that was not.
func (p *Pipeline) apply(snap lifecycle.Snapshot, boot string, mark int64) error {
	started := time.Now()
	s, err := p.session(snap.SessionKey)
	if err != nil {
		return err
	}
	if boot != "" {
		if last, ok := s.Progress[boot]; ok && mark <= last {
			p.resent++
			return nil
		}
		s.Progress[boot] = mark
	}
	s.Apply(snap)
	// a branch first seen with its parent records which parent turns its
	// copies are, once (also when a late marker names another parent)
	if s.BranchOf[0] != "" && s.CopiesOf != s.BranchOf {
		_, err := memory.RecordCopies(s, func(key string) (*lifecycle.Session, bool, error) {
			if key == s.Key {
				return nil, false, nil
			}
			ps, err := p.session(key)
			if err != nil {
				return nil, false, err
			}
			return ps, len(ps.Turns) > 0, nil
		})
		if err != nil {
			delete(p.sessions, snap.SessionKey)
			return err
		}
	}
	events := s.TakeEvents()
	if err := p.store.SaveSession(s, events, s.TakeRenames()); err != nil {
		// the stored state is the last good one; reload it next time
		delete(p.sessions, snap.SessionKey)
		return err
	}
	p.applied++
	if p.Observed != nil {
		p.Observed(s, events, started, time.Since(started).Milliseconds())
	}
	if len(events) > 0 && p.Changed != nil {
		p.Changed(s)
	}
	return nil
}

// warm gives the text cache the stored texts of a loaded chat, so rows the
// plugin sends as fingerprints after a backend restart are filled.
func (p *Pipeline) warm(s *lifecycle.Session) {
	chat := strings.TrimPrefix(s.Key, "chat:")
	if !strings.HasPrefix(s.Key, "chat:") {
		chat = ""
	}
	for _, t := range s.Turns {
		for _, m := range t.Members {
			if strings.HasPrefix(m.Key, "c:") {
				p.texts.Warm(ingest.TextKey{Chat: chat, Row: m.Key}, m.Data)
			}
		}
		for _, r := range t.Responses {
			if strings.HasPrefix(r.Key, "c:") {
				for _, rev := range r.Revisions {
					p.texts.Warm(ingest.TextKey{Chat: chat, Row: r.Key}, rev.Data)
				}
			}
		}
	}
}

func (p *Pipeline) fail(err error) {
	p.failures++
	p.lastError = err.Error()
}

// WithSession runs fn on a chat's session (loaded if not in memory) with
// the observation lock held; fn must only read it.
func (p *Pipeline) WithSession(key string, fn func(s *lifecycle.Session)) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, err := p.session(key)
	if err != nil {
		return err
	}
	fn(s)
	return nil
}

// Diagnostics reports processing counts for /diagnostics.
func (p *Pipeline) Diagnostics() map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	return map[string]any{"snapshots_applied": p.applied, "resent_skipped": p.resent, "process_failures": p.failures, "process_last_error": p.lastError, "sessions": len(p.sessions)}
}
