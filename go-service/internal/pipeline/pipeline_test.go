package pipeline

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"archivecenter2/goservice/internal/ingest"
	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/observe"
)

// memStore keeps saved sessions as copies made by replaying Restore, and can
// fail the next save.
type memStore struct {
	saved    map[string][]*lifecycle.Turn
	progress map[string]map[string]int64
	raw      []json.RawMessage // what the server's raw sink would hold
	failNext bool
	loads    int
	saves    int
}

func (m *memStore) LoadSession(key string) (*lifecycle.Session, bool, error) {
	m.loads++
	turns, ok := m.saved[key]
	if !ok {
		return nil, false, nil
	}
	return lifecycle.Restore(key, deepCopy(turns), m.progress[key]), true, nil
}

func (m *memStore) RawTextsOf(chat string, fn func(json.RawMessage)) error {
	for _, ev := range m.raw {
		for _, c := range ingest.ChatsOf(ev) {
			if c == chat {
				fn(ev)
				break
			}
		}
	}
	return nil
}

func (m *memStore) SaveSession(s *lifecycle.Session, _ []lifecycle.Event, _ []lifecycle.Rename) error {
	if m.failNext {
		m.failNext = false
		return errors.New("disk full")
	}
	m.saves++
	m.saved[s.Key] = deepCopy(s.Turns)
	if m.progress == nil {
		m.progress = map[string]map[string]int64{}
	}
	m.progress[s.Key] = map[string]int64{}
	for b, q := range s.Progress {
		m.progress[s.Key][b] = q
	}
	return nil
}

func deepCopy(t []*lifecycle.Turn) []*lifecycle.Turn {
	b, _ := json.Marshal(t)
	var out []*lifecycle.Turn
	_ = json.Unmarshal(b, &out)
	return out
}

func beforeRequest(rows string) observe.Record {
	return observe.Record{Event: json.RawMessage(`{"hook":"beforeRequest","coords":{},"stored_chat":{"chat_id":"A","rows":` + rows + `}}`)}
}

const r1 = `[{"i":0,"role":"user","chat_id":"u1","data":{"text":"one"}},{"i":1,"role":"char","chat_id":"a1","generation_id":"g1","data":{"text":"r1"}}]`
const r2 = `[{"i":0,"role":"user","chat_id":"u1","data":{"text":"one"}},{"i":1,"role":"char","chat_id":"a1","generation_id":"g1","data":{"text":"r1"}},{"i":2,"role":"user","chat_id":"u2","data":{"text":"two"}}]`

func TestSnapshotsAppliedAndSaved(t *testing.T) {
	st := &memStore{saved: map[string][]*lifecycle.Turn{}}
	p := New(st)
	p.Process([]observe.Record{beforeRequest(r1), {Event: json.RawMessage(`{"hook":"input"}`)}, beforeRequest(r2)})
	turns := st.saved["chat:A"]
	if st.saves != 2 || len(turns) != 2 || st.loads != 1 {
		t.Fatalf("saves %d loads %d turns %d", st.saves, st.loads, len(turns))
	}
}

// After a failed save the in-memory session is dropped; the next snapshot
// starts from the last saved state, not from the unsaved one.
func TestFailedSaveReloadsLastSavedState(t *testing.T) {
	st := &memStore{saved: map[string][]*lifecycle.Turn{}}
	p := New(st)
	p.Process([]observe.Record{beforeRequest(r1)})
	st.failNext = true
	p.Process([]observe.Record{beforeRequest(r2)})
	if d := p.Diagnostics(); d["process_failures"].(uint64) != 1 || d["sessions"].(int) != 0 {
		t.Fatalf("diagnostics %+v", d)
	}
	p.Process([]observe.Record{beforeRequest(r2)})
	turns := st.saved["chat:A"]
	if st.loads != 2 || len(turns) != 2 || turns[1].ID != 2 || len(turns[1].Members) != 1 {
		t.Fatalf("loads %d turns %+v", st.loads, turns)
	}
}

// ---- Astra 16 N6 counterexample, kept as a regression ----
func TestAstra16ResentOldObservationDoesNotRevertCurrentBody(t *testing.T) {
	st := &memStore{saved: map[string][]*lifecycle.Turn{}}
	p := New(st)
	record := func(seq, body string) observe.Record {
		return observe.Record{Event: json.RawMessage(`{"boot_id":"b","seq":` + seq + `,"hook":"beforeRequest","stored_chat":{"chat_id":"A","rows":[{"i":0,"role":"user","chat_id":"u1","data":{"text":"q"}},{"i":1,"role":"char","chat_id":"a1","generation_id":"g1","data":{"text":"` + body + `"}}]}}`)}
	}
	old := record("1", "old")
	newer := record("2", "current")
	p.Process([]observe.Record{old})
	p.Process([]observe.Record{newer})
	p.Process([]observe.Record{old})
	r := st.saved["chat:A"][0].Responses[0]
	if got := r.Revisions[len(r.Revisions)-1].Data; got != "current" {
		t.Fatalf("same boot+seq replay reverted selected body to %q; revisions=%d", got, len(r.Revisions))
	}
}

func ev(seq int, body string) observe.Record {
	return observe.Record{Event: json.RawMessage(fmt.Sprintf(`{"boot_id":"b","seq":%d,%s}`, seq, body))}
}

func chatJSON(id string, rows ...string) string {
	return `{"chat_id":"` + id + `","rows":[` + strings.Join(rows, ",") + `]}`
}

func row(i int, role, chatID, gen, text string) string {
	return fmt.Sprintf(`{"i":%d,"role":%q,"chat_id":%q,"generation_id":%q,"data":{"text":%q}}`, i, role, chatID, gen, text)
}

// Judgment table 13/14: a late output for chat A that arrives after the user
// moved to chat B goes to A only; the re-read of the new current chat B
// updates B from B's own rows and confirms nothing.
func TestLateOutputAfterChatSwitchStaysInItsChat(t *testing.T) {
	st := &memStore{saved: map[string][]*lifecycle.Turn{}}
	p := New(st)
	p.Process([]observe.Record{
		ev(1, `"hook":"beforeRequest","stored_chat":`+chatJSON("A", row(0, "user", "ua", "", "A 입력"))),
		ev(2, `"hook":"beforeRequest","stored_chat":`+chatJSON("B", row(0, "user", "ub", "", "B 입력"))),
		ev(3, `"hook":"output","listener":{"message_index":1},"event_chat":`+chatJSON("A", row(0, "user", "ua", "", "A 입력"), row(1, "char", "aa", "g1", "A 응답"))+
			`,"reread_chat":`+chatJSON("B", row(0, "user", "ub", "", "B 입력"))),
	})
	a, b := st.saved["chat:A"], st.saved["chat:B"]
	if len(a) != 1 || len(a[0].Responses) != 1 {
		t.Fatalf("chat A: %+v", a)
	}
	if len(b) != 1 || len(b[0].Responses) != 0 || b[0].Confirmed || a[0].Confirmed {
		t.Fatalf("chat B got A's reply, or an output confirmed a turn: %+v %+v", a[0], b[0])
	}
}

// Judgment table 15: a branch is its own chat with the parent recorded from
// the RisuAI marker; copied rows (same message chatIds) do not touch the parent.
func TestBranchIsSeparateChatWithParent(t *testing.T) {
	st := &memStore{saved: map[string][]*lifecycle.Turn{}}
	p := New(st)
	marker := `{"i":2,"role":"char","chat_id":"m","is_comment":true,"disabled":true,"data":{"text":"{{specialcomment::branchedfrom::A::이름::a1::}}"}}`
	p.Process([]observe.Record{
		ev(1, `"hook":"beforeRequest","stored_chat":`+chatJSON("A", row(0, "user", "u1", "", "하나"), row(1, "char", "a1", "g1", "응답"))),
		ev(2, `"hook":"beforeRequest","stored_chat":`+chatJSON("C", row(0, "user", "u1", "", "하나"), row(1, "char", "a1", "g1", "응답"), marker, row(3, "user", "u2", "", "분기 입력"))),
	})
	if a := st.saved["chat:A"]; len(a) != 1 || len(a[0].Members) != 1 {
		t.Fatalf("parent changed: %+v", a)
	}
	c := p.sessions["chat:C"]
	if c == nil || len(c.ActiveTurns()) != 2 || c.BranchOf != [2]string{"chat:A", "c:a1"} {
		t.Fatalf("branch session: %+v", c)
	}
}

// Process reports a resend when a fingerprint-only row is unknown (backend
// restarted), and not once the texts are known again.
func TestProcessAsksForResendOnlyWhenTextUnknown(t *testing.T) {
	st := &memStore{saved: map[string][]*lifecycle.Turn{}}
	p := New(st)
	fpOnly := ev(1, `"hook":"beforeRequest","stored_chat":{"chat_id":"A","rows":[{"i":0,"role":"user","chat_id":"u1","data":{"len":3,"h":"1"}}]}`)
	if resend, _ := p.Process([]observe.Record{fpOnly}); !resend {
		t.Fatal("unknown fingerprint-only row did not ask for a resend")
	}
	full := ev(2, `"hook":"beforeRequest","stored_chat":`+chatJSON("A", row(0, "user", "u1", "", "abc")))
	if resend, _ := p.Process([]observe.Record{full}); resend {
		t.Fatal("full text asked for a resend")
	}
}

// ---- Astra 17 recheck counterexamples, kept as regressions ----
func TestAstra17FailedSaveRetryWithPluginIdentity(t *testing.T) {
	st := &memStore{saved: map[string][]*lifecycle.Turn{}}
	p := New(st)
	r := beforeRequest(r1)
	var e map[string]any
	json.Unmarshal(r.Event, &e)
	e["boot_id"] = "real-plugin-boot"
	e["seq"] = 1
	r.Event, _ = json.Marshal(e)
	st.failNext = true
	p.Process([]observe.Record{r})
	p.Process([]observe.Record{r})
	if len(st.saved["chat:A"]) != 1 {
		t.Fatalf("failed seq marked applied: saves=%d diag=%v", st.saves, p.Diagnostics())
	}
}
func TestAstra17LaterNonSnapshotDoesNotSuppressUnseenOutput(t *testing.T) {
	st := &memStore{saved: map[string][]*lifecycle.Turn{}}
	p := New(st)
	p.Process([]observe.Record{{Event: json.RawMessage(`{"boot_id":"b","seq":2,"hook":"input"}`)}})
	p.Process([]observe.Record{{Event: json.RawMessage(`{"boot_id":"b","seq":1,"hook":"output","event_chat":{"chat_id":"A","rows":[{"i":0,"role":"user","chat_id":"u1","data":{"text":"q"}},{"i":1,"role":"char","chat_id":"a1","data":{"text":"reply"}}]}}`)}})
	if len(st.saved["chat:A"]) != 1 {
		t.Fatalf("first-delivery output suppressed as resend: %v", p.Diagnostics())
	}
}
func TestAstra17FailedNextInputSaveKeepsCompletion(t *testing.T) {
	st := &memStore{saved: map[string][]*lifecycle.Turn{}}
	p := New(st)
	out := `{"chat_id":"A","rows":` + r1 + `}`
	p.Process([]observe.Record{{Event: json.RawMessage(`{"boot_id":"b","seq":1,"hook":"output","event_chat":` + out + `,"reread_chat":` + out + `}`)}})
	// the next input confirms T1; its save fails once and is sent again
	st.failNext = true
	r := observe.Record{Event: json.RawMessage(`{"boot_id":"b","seq":2,"hook":"beforeRequest","stored_chat":{"chat_id":"A","rows":` + r2 + `}}`)}
	p.Process([]observe.Record{r})
	p.Process([]observe.Record{r})
	if !st.saved["chat:A"][0].Confirmed {
		t.Fatalf("the next input's save failed and the resent input lost the completion: %v", p.Diagnostics())
	}
}

// The two snapshots of one output event share a seq: the re-read (newer
// view) is applied after the event snapshot, not skipped as a resend.
func TestOutputRereadSameSeqIsApplied(t *testing.T) {
	st := &memStore{saved: map[string][]*lifecycle.Turn{}}
	p := New(st)
	p.Process([]observe.Record{ev(1, `"hook":"output","listener":{"message_index":1},"event_chat":`+chatJSON("A", row(0, "user", "u1", "", "q"), row(1, "char", "a1", "g1", "AT EVENT"))+
		`,"reread_chat":`+chatJSON("A", row(0, "user", "u1", "", "q"), row(1, "char", "a1", "g1", "LATER EDIT")))})
	r := st.saved["chat:A"][0].Responses[0]
	if got := r.Revisions[len(r.Revisions)-1].Data; got != "LATER EDIT" {
		t.Fatalf("re-read not applied: current %q", got)
	}
}

// A failure stops the batch and is returned, so the plugin resends it; the
// snapshots after the failed one are not applied out of order.
func TestFailureStopsBatchAndIsReturned(t *testing.T) {
	st := &memStore{saved: map[string][]*lifecycle.Turn{}, failNext: true}
	p := New(st)
	_, err := p.Process([]observe.Record{
		ev(1, `"hook":"beforeRequest","stored_chat":`+chatJSON("A", row(0, "user", "u1", "", "q"))),
		ev(2, `"hook":"beforeRequest","stored_chat":`+chatJSON("A", row(0, "user", "u1", "", "q"), row(1, "user", "u2", "", "r"))),
	})
	if err == nil || st.saves != 0 {
		t.Fatalf("err %v saves %d", err, st.saves)
	}
}

// ---- Astra 18 recheck counterexamples, kept as regressions (R1 left out: see apply) ----
func a18field(s string, full bool) map[string]any {
	n, h := ingest.Fingerprint(s)
	f := map[string]any{"len": n, "h": h}
	if full {
		f["text"] = s
	}
	return f
}
func a18rec(seq int, hook, text string, full bool, reread string) observe.Record {
	chat := func(text string, full bool) map[string]any {
		return map[string]any{"chat_id": "A", "rows": []any{map[string]any{"i": 0, "role": "user", "chat_id": "u1", "data": a18field("q", true)}, map[string]any{"i": 1, "role": "char", "chat_id": "a1", "generation_id": "g1", "data": a18field(text, full)}}}
	}
	e := map[string]any{"boot_id": "b18", "seq": seq, "hook": hook}
	if hook == "output" {
		e["event_chat"] = chat(text, full)
		e["listener"] = map[string]any{"message_index": 1}
		if reread != "" {
			e["reread_chat"] = chat(reread, false)
		}
	} else {
		e["stored_chat"] = chat(text, full)
	}
	b, _ := json.Marshal(e)
	return observe.Record{Event: b}
}
func a18current(st *memStore) string {
	r := st.saved["chat:A"][0].Responses[0]
	if len(r.Revisions) == 0 {
		return "<unreadable>"
	}
	return r.Revisions[len(r.Revisions)-1].Data
}
func TestAstra18InflightReturnToAcknowledgedCandidate(t *testing.T) {
	st := &memStore{saved: map[string][]*lifecycle.Turn{}}
	p := New(st)
	p.Process([]observe.Record{a18rec(1, "load", "candidate A", true, "")})
	// Plugin acknowledged A. Output event B sends text; re-read A omits it.
	resend, err := p.Process([]observe.Record{a18rec(2, "output", "candidate B", true, "candidate A")})
	t.Logf("resend=%v err=%v current=%q", resend, err, a18current(st))
	if a18current(st) != "candidate A" {
		t.Fatal("current selected candidate lost; later hook is not guaranteed")
	}
}
func TestAstra18ExactOutputReplayDoesNotAddRevisions(t *testing.T) {
	st := &memStore{saved: map[string][]*lifecycle.Turn{}}
	p := New(st)
	r := a18rec(1, "output", "event body", true, "")
	var e map[string]any
	json.Unmarshal(r.Event, &e)
	rr := a18rec(1, "load", "postprocessed body", true, "")
	var re map[string]any
	json.Unmarshal(rr.Event, &re)
	e["reread_chat"] = re["stored_chat"]
	r.Event, _ = json.Marshal(e)
	p.Process([]observe.Record{r})
	n := len(st.saved["chat:A"][0].Responses[0].Revisions)
	p.Process([]observe.Record{r})
	got := len(st.saved["chat:A"][0].Responses[0].Revisions)
	if got != n {
		t.Fatalf("same boot+seq response-loss retry grew revisions %d -> %d", n, got)
	}
}
func TestAstra18RestartFingerprintMissingThenDeleteLosesRevision(t *testing.T) {
	st := &memStore{saved: map[string][]*lifecycle.Turn{}}
	p := New(st)
	p.Process([]observe.Record{a18rec(1, "load", "A", true, "")})
	p.Process([]observe.Record{a18rec(2, "load", "B", true, "")})
	p = New(st)
	resend, _ := p.Process([]observe.Record{a18rec(3, "output", "B", false, "A")})
	// Both fingerprints can be recovered from DB history in principle, but current route cannot.
	p.Process([]observe.Record{ev(4, `"hook":"beforeRequest","stored_chat":{"chat_id":"A","rows":[{"i":0,"role":"user","chat_id":"u1","data":{"text":"q"}}]}`)})
	t.Logf("resend=%v final=%s", resend, fmt.Sprint(st.saved["chat:A"][0].Responses[0].Revisions))
	r := st.saved["chat:A"][0].Responses[0]
	if len(r.Revisions) < 3 || r.Revisions[len(r.Revisions)-1].Data != "A" {
		t.Fatal("return-to-A observation was ACKed then deleted without ever applied")
	}
}

func TestAstra18ExactRetryComparison(t *testing.T) {
	st := &memStore{saved: map[string][]*lifecycle.Turn{}}
	p := New(st)
	r := observe.Record{Event: json.RawMessage(`{"boot_id":"b18","seq":1,"hook":"output","event_chat":{"chat_id":"A","rows":[{"i":0,"role":"user","chat_id":"u","data":{"text":"q"}},{"i":1,"role":"char","chat_id":"a","data":{"text":"event"}}]},"reread_chat":{"chat_id":"A","rows":[{"i":0,"role":"user","chat_id":"u","data":{"text":"q"}},{"i":1,"role":"char","chat_id":"a","data":{"text":"reread"}}]}}`)}
	p.Process([]observe.Record{r})
	n := len(st.saved["chat:A"][0].Responses[0].Revisions)
	p.Process([]observe.Record{r})
	got := len(st.saved["chat:A"][0].Responses[0].Revisions)
	if got != n {
		t.Fatalf("revisions %d -> %d", n, got)
	}
}

// The generation hook hears of a chat whose turns changed (a new confirmed
// reply), and not of a snapshot that changed nothing (the same observation
// seen again under a new seq).
func TestChangedHeardOnlyWhenTurnsChange(t *testing.T) {
	st := &memStore{saved: map[string][]*lifecycle.Turn{}}
	p := New(st)
	var heard []string
	p.Changed = func(s *lifecycle.Session) { heard = append(heard, s.Key) }
	out := `"hook":"output","listener":{"message_index":1},"event_chat":` + chatJSON("A", row(0, "user", "ua", "", "입력"), row(1, "char", "aa", "g1", "응답"))
	p.Process([]observe.Record{ev(1, out)})
	if len(heard) != 1 || heard[0] != "chat:A" || len(st.saved["chat:A"]) != 1 || st.saved["chat:A"][0].Confirmed {
		t.Fatalf("heard %v (an output alone must not confirm)", heard)
	}
	p.Process([]observe.Record{ev(2, `"hook":"load","stored_chat":`+chatJSON("A", row(0, "user", "ua", "", "입력"), row(1, "char", "aa", "g1", "응답")))})
	if len(heard) != 1 {
		t.Fatalf("an unchanged snapshot was heard: %v", heard)
	}
	// the next input confirms the turn, and is heard
	p.Process([]observe.Record{ev(3, `"hook":"beforeRequest","stored_chat":`+chatJSON("A", row(0, "user", "ua", "", "입력"), row(1, "char", "aa", "g1", "응답"), row(2, "user", "ub", "", "다음")))})
	if len(heard) != 2 || !st.saved["chat:A"][0].Confirmed {
		t.Fatalf("the next input: heard %v confirmed %v", heard, st.saved["chat:A"][0].Confirmed)
	}
}

// The branch's first observation (the first input's beforeRequest: the
// plugin does not observe a chat switch) records its copies once.
func TestBranchCopiesRecordedAtTheFirstObservation(t *testing.T) {
	st := &memStore{saved: map[string][]*lifecycle.Turn{}}
	p := New(st)
	mk := `{"i":2,"role":"char","chat_id":"mk","is_comment":true,"disabled":true,"data":{"text":"{{specialcomment::branchedfrom::A::이름::aa::}}"}}`
	p.Process([]observe.Record{
		ev(1, `"hook":"output","listener":{"message_index":1},"event_chat":`+chatJSON("A", row(0, "user", "ua", "", "입력"), row(1, "char", "aa", "g1", "응답"))),
		ev(2, `"hook":"beforeRequest","stored_chat":`+chatJSON("B", row(0, "user", "xb", "", "입력"), row(1, "char", "yb", "g1", "응답"), mk, row(3, "user", "zb", "", "분기 첫 입력"))),
	})
	b := p.sessions["chat:B"]
	if b == nil || b.BranchOf != [2]string{"chat:A", "c:aa"} || b.CopiesOf != b.BranchOf || len(b.Copies) != 1 {
		t.Fatalf("branch %+v", b)
	}
	first := b.Copies[1]
	// a later observation does not record again
	p.Process([]observe.Record{ev(3, `"hook":"beforeRequest","stored_chat":`+chatJSON("B", row(0, "user", "xb", "", "입력"), row(1, "char", "yb", "g1", "고친 응답"), mk, row(3, "user", "zb", "", "분기 첫 입력")))})
	if b.Copies[1] != first {
		t.Fatalf("copies recorded again: %+v", b.Copies)
	}
}
