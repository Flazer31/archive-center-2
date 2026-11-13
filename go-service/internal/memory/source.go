package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"archivecenter2/goservice/internal/lifecycle"
)

// Source is what one generation job read from a turn: the turn's active
// user rows (the input bundle, with their text) and its active response
// rows at their current revision. A job is identified by its turn and this
// source, not by a confirmation event (a turn can be confirmed more than
// once, and a late original keeps an earlier confirmation).
type Source struct {
	Session   string           `json:"session"`
	Turn      int              `json:"turn"`
	Members   []SourceMember   `json:"members"`
	Responses []SourceResponse `json:"responses"`
	// Dates are the turn's date variables (story date); they are read by
	// the model but are not part of the source key.
	Dates map[string]string `json:"dates,omitempty"`
}

type SourceMember struct {
	Key  string `json:"key"`
	Data string `json:"data"`
}

type SourceResponse struct {
	Key  string `json:"key"`
	Seq  int    `json:"seq"` // revision seq; 0 while the row (or its newest generation) is unread
	Gen  string `json:"gen,omitempty"`
	Data string `json:"data"`
}

// SourceOf reads a turn's current source, in host order: the active rows,
// without the replies turned off in the host.
func SourceOf(session string, t *lifecycle.Turn) Source { return sourceOf(session, t, false) }

// WholeSourceOf reads a turn as the host holds it, for matching a branch's
// copies: the replies turned off too, and for a turn deleted from the
// host, the rows it had when deleted.
func WholeSourceOf(session string, t *lifecycle.Turn) Source { return sourceOf(session, t, true) }

func wholeRow(state lifecycle.RowState, inactive bool) bool {
	return state == lifecycle.Active || (inactive && state == lifecycle.Deleted)
}

func sourceOf(session string, t *lifecycle.Turn, whole bool) Source {
	s := Source{Session: session, Turn: t.ID, Dates: t.Dates}
	gone := whole && t.Inactive
	for _, m := range t.Members {
		if wholeRow(m.State, gone) {
			s.Members = append(s.Members, SourceMember{Key: m.Key, Data: m.Data})
		}
	}
	for _, r := range t.Responses {
		if !wholeRow(r.State, gone) || (r.Off && !whole) {
			continue
		}
		sr := SourceResponse{Key: r.Key, Gen: r.Gen}
		// a newer generation was observed but its text not yet: the row is
		// unread, not its earlier generation's text. A blank text is the
		// marker an emptied reply keeps (only the newest value is kept; a
		// reply's real text is never blank): unread as well.
		if n := len(r.Revisions); n > 0 && r.Revisions[n-1].Data != "" && (r.Gen == "" || r.Revisions[n-1].GenerationID == "" || r.Revisions[n-1].GenerationID == r.Gen) {
			sr.Seq, sr.Data = r.Revisions[n-1].Seq, r.Revisions[n-1].Data
		}
		s.Responses = append(s.Responses, sr)
	}
	return s
}

// Key identifies the source: the same turn read at the same input texts and
// response revisions gives the same key, so a resent or retried job reuses
// its stored result. Row keys are left out: a row first seen without chatId
// keeps its turn and revision when the host gives it one (the snapshot keeps
// the keys as they were). The generation id is left out too: the revision
// seq already names the text read, and a generation id learned later for the
// same text is not a new source.
func (s Source) Key() string {
	type member struct{ Data string }
	type response struct {
		Seq  int
		Data string
	}
	p := struct {
		Session   string
		Turn      int
		Members   []member
		Responses []response
	}{Session: s.Session, Turn: s.Turn}
	for _, m := range s.Members {
		p.Members = append(p.Members, member{m.Data})
	}
	for _, r := range s.Responses {
		p.Responses = append(p.Responses, response{r.Seq, r.Data})
	}
	b, _ := json.Marshal(p)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:16])
}

// Readable reports whether every response row has text (an unreadable row,
// e.g. a translation whose original has not arrived, has none yet).
func (s Source) Readable() bool {
	for _, r := range s.Responses {
		if r.Seq == 0 {
			return false
		}
	}
	return len(s.Responses) > 0
}
