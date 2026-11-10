// Package lifecycle turns RisuAI host chat snapshots into logical turns,
// response rows and revisions. It only reconciles observations; it never
// rejects an observation because a coordinate is missing or changed.
//
// Signal meanings follow the RisuAI source (commit 02a8369) and are not yet
// confirmed on a live host (STEP1-TEST-LOG.md): marked "unverified" where it matters.
package lifecycle

// Row is one message of a host chat snapshot, with every field optional.
type Row struct {
	Index        int    `json:"i"`
	ChatID       string `json:"chat_id,omitempty"`
	Role         string `json:"role"`               // "user" | "char"
	Disabled     string `json:"disabled,omitempty"` // "", "true", "allBefore"
	IsComment    bool   `json:"is_comment,omitempty"`
	GenerationID string `json:"generation_id,omitempty"`
	Time         int64  `json:"time,omitempty"`
	Data         string `json:"data"`
	// Unreadable: the row exists but its text could not be read (translation
	// source missing, text not captured). It is present, not deleted.
	Unreadable bool `json:"unreadable,omitempty"`
}

// Source says where a snapshot came from; sources are never merged.
type Source string

const (
	SourceBeforeRequest Source = "before_request"
	SourceOutputEvent   Source = "output_event" // the listener's own chat argument
	SourceOutputReread  Source = "output_reread"
	SourceLoad          Source = "load"
)

// Snapshot is the stored chat as one source saw it.
type Snapshot struct {
	SessionKey string // host chat.id when present, else a coordinate key (unverified)
	Source     Source
	Rows       []Row
	// TargetIndex is the row the output listener named (messageIndex); -1 if none.
	TargetIndex int
	// Dates are the chat variables whose value is a date (the story date a
	// script keeps), by name, as this source saw them.
	Dates map[string]string
}

// RowState is the status of a response row or a member.
type RowState string

const (
	Active   RowState = "active"
	Replaced RowState = "replaced" // superseded by a reroll of the same turn
	Deleted  RowState = "deleted"  // no longer present in the host chat
)

// Revision is one body state of a response row.
type Revision struct {
	Seq          int
	Data         string
	GenerationID string
	Source       Source
	Confirmed    bool // this body was the current one when the turn was confirmed
}

// ResponseRow is one assistant row that answers a turn. A turn can have
// several active rows (continue, group); they are parts, not alternatives.
type ResponseRow struct {
	Key       string
	State     RowState
	Gen       string // last known generation id (also from unreadable observations)
	Revisions []Revision
	// AwaitingReplacement: deleted while it was the turn's last active reply
	// (RisuAI reroll removes the old reply before the request); the next new
	// reply on the turn replaces it.
	AwaitingReplacement bool
	// Off: the row is disabled in the host. Its text is not read into the
	// turn's memory; the turn's other replies are.
	Off bool
	// Cut: the row left the front of the host chat (/cut, /del) and is kept,
	// text and memory (user 11-08); stored, so a restart keeps it cut.
	Cut bool
}

// Member is one user row linked to a turn. Links are kept once made.
type Member struct {
	Key   string
	Data  string
	State RowState
	Cut   bool // left the front of the host chat and kept (ResponseRow.Cut)
}

// Turn is a logical turn with a Go-issued ID.
type Turn struct {
	ID        int
	Members   []*Member
	Responses []*ResponseRow
	Confirmed bool
	Inactive  bool // every row of the turn was deleted from the host tail
	Greeting  bool // turn 0: assistant rows before the first user row
	// Off: the turn's user row is disabled in the host (the user turned it
	// off, e.g. a one-turn OOC). The turn is kept, but its memory is not
	// made or read; turning it on again brings it back. A disabled reply
	// alone turns only that reply off (ResponseRow.Off).
	Off bool
	// Dates are the date variables last seen while this turn was the newest
	// one (after its reply, the story date the reply left).
	Dates map[string]string
}

// Event is a lifecycle record kept for diagnosis and later stages.
type Event struct {
	Kind   string
	TurnID int
	Detail string
}
