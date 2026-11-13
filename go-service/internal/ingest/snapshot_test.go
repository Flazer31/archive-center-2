package ingest

import (
	"encoding/json"
	"os"
	"testing"

	"archivecenter2/goservice/internal/lifecycle"
)

// testdata/plugin-events.json is recorded from the real plugin by
// tools/make-ingest-fixture.cjs: load, beforeRequest, output, beforeRequest
// of a chat without id. After the load is acknowledged the plugin sends
// unchanged rows as fingerprints only, so events are read in order through
// one TextCache, as the pipeline does.
func fixtureEvents(t *testing.T) []json.RawMessage {
	t.Helper()
	b, err := os.ReadFile("testdata/plugin-events.json")
	if err != nil {
		t.Fatal(err)
	}
	var evs []json.RawMessage
	if err := json.Unmarshal(b, &evs); err != nil {
		t.Fatal(err)
	}
	if len(evs) != 4 {
		t.Fatalf("fixture has %d events, want 4", len(evs))
	}
	return evs
}

// ingestAll reads the fixture in order and returns each event's results.
func ingestAll(t *testing.T, cache *TextCache) [][]Result {
	t.Helper()
	var out [][]Result
	for _, ev := range fixtureEvents(t) {
		res, err := FromEvent(ev, cache)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, res)
	}
	return out
}

func TestBeforeRequestStoredChatRows(t *testing.T) {
	all := ingestAll(t, NewTextCache())
	res := all[1]
	if len(res) != 1 || res[0].NeedText {
		t.Fatalf("got %d results, need text %v", len(res), len(res) > 0 && res[0].NeedText)
	}
	s := res[0].Snapshot
	if s.SessionKey != "chat:chat-A" || s.Source != lifecycle.SourceBeforeRequest || len(s.Rows) != 8 {
		t.Fatalf("snapshot header %q %q rows=%d", s.SessionKey, s.Source, len(s.Rows))
	}
	r := s.Rows
	if r[1].Role != "user" || r[1].ChatID != "u1" || r[1].Data != "첫 입력" || r[1].Time != 100 || r[2].Data != "둘째 입력 😀🇰🇷" {
		t.Fatalf("user rows (filled from the cache): %+v %+v", r[1], r[2])
	}
	if r[3].Data != "ORIGINAL REPLY" || r[3].GenerationID != "gen-1" || res[0].Notes[3].Status != OriginalYumi {
		t.Fatalf("translated row must carry the original: %+v %+v", r[3], res[0].Notes[3])
	}
	if r[3].Unreadable || !r[4].Unreadable || s.TargetIndex != -1 {
		t.Fatalf("unreadable flags %v %v target %d", r[3].Unreadable, r[4].Unreadable, s.TargetIndex)
	}
	if r[4].Data != "" || res[0].Notes[4].Status != OriginalUnavailable || res[0].Notes[4].Reason != "translation_source_missing" {
		t.Fatalf("missing record must be unavailable, not the translation: %+v %+v", r[4], res[0].Notes[4])
	}
	if !r[5].IsComment || r[5].Disabled != "true" || r[6].Disabled != "allBefore" {
		t.Fatalf("branch/disabled fields: %+v %+v", r[5], r[6])
	}
}

func TestOutputGivesEventAndRereadSeparately(t *testing.T) {
	res := ingestAll(t, NewTextCache())[2]
	if len(res) != 2 {
		t.Fatalf("got %d results", len(res))
	}
	if res[0].Snapshot.Source != lifecycle.SourceOutputEvent || res[1].Snapshot.Source != lifecycle.SourceOutputReread {
		t.Fatalf("sources %q %q", res[0].Snapshot.Source, res[1].Snapshot.Source)
	}
	if res[0].Snapshot.TargetIndex != 3 {
		t.Fatalf("listener messageIndex not carried: %d", res[0].Snapshot.TargetIndex)
	}
	// the re-read changed row 3's display; the record (sent once) still gives the original
	if res[1].Snapshot.Rows[3].Data != "ORIGINAL REPLY" || res[1].NeedText {
		t.Fatalf("re-read row 3 %+v need %v", res[1].Snapshot.Rows[3], res[1].NeedText)
	}
}

func TestMissingChatIDFallsBackToCoordinates(t *testing.T) {
	res := ingestAll(t, NewTextCache())[3]
	if len(res) != 1 || res[0].Snapshot.SessionKey != "coord:2/5" {
		t.Fatalf("got %+v", res)
	}
}

// The Go fingerprint equals the plugin's for every text the plugin sent
// (Korean, emoji with surrogate pairs and flags, markers).
func TestFingerprintMatchesPlugin(t *testing.T) {
	n := 0
	for _, raw := range fixtureEvents(t) {
		var ev event
		if err := json.Unmarshal(raw, &ev); err != nil {
			t.Fatal(err)
		}
		for _, c := range []*eventChat{ev.StoredChat, ev.EventChat, ev.RereadChat} {
			if c == nil {
				continue
			}
			for _, r := range c.Rows {
				if r.Data.Text == nil {
					continue
				}
				if l, h := Fingerprint(*r.Data.Text); l != r.Data.Len || h != r.Data.H {
					t.Fatalf("row %q: go %d/%s plugin %d/%s", *r.Data.Text, l, h, r.Data.Len, r.Data.H)
				}
				n++
			}
		}
	}
	if n < 7 {
		t.Fatalf("only %d texts compared", n)
	}
}

// Without the earlier texts (backend restart) the fingerprint-only rows are
// unreadable, not empty, and the plugin is asked to resend.
func TestUnknownFingerprintAsksForResend(t *testing.T) {
	res, err := FromEvent(fixtureEvents(t)[1], NewTextCache())
	if err != nil || len(res) != 1 || !res[0].NeedText || !res[0].Snapshot.Rows[1].Unreadable {
		t.Fatalf("got %+v err %v", res, err)
	}
}

// A cached text whose fingerprint differs from the sent one is not used.
func TestStaleCachedTextNotUsed(t *testing.T) {
	cache := NewTextCache()
	cache.Warm(TextKey{"c", "c:u"}, "old text")
	raw := json.RawMessage(`{"hook":"beforeRequest","stored_chat":{"chat_id":"c","rows":[{"i":0,"role":"user","chat_id":"u","data":{"len":3,"h":"ab"}}]}}`)
	res, _ := FromEvent(raw, cache)
	if !res[0].NeedText || !res[0].Snapshot.Rows[0].Unreadable || res[0].Snapshot.Rows[0].Data != "" {
		t.Fatalf("stale text used: %+v", res[0])
	}
}

func TestUncapturedTextIsNotedNotEmptied(t *testing.T) {
	raw := json.RawMessage(`{"hook":"beforeRequest","coords":{},"stored_chat":{"chat_id":"c","rows":[{"i":0,"role":"user","chat_id":"u","data":{"len":3,"h":"ab"}}]}}`)
	res, err := FromEvent(raw, nil)
	if err != nil || len(res) != 1 || len(res[0].Notes) != 1 || res[0].Notes[0].Reason != "text_not_captured" || !res[0].Snapshot.Rows[0].Unreadable {
		t.Fatalf("got %+v err %v", res, err)
	}
}

func TestHooksWithoutChatGiveNoSnapshot(t *testing.T) {
	for _, raw := range []string{`{"hook":"input","content":{"text":"x"}}`, `{"hook":"beforeRequest","stored_chat":{"error":"boom"}}`, `{"hook":"output","event_chat":{"missing":true}}`} {
		res, err := FromEvent(json.RawMessage(raw), nil)
		if err != nil || len(res) != 0 {
			t.Fatalf("%s: got %+v err %v", raw, res, err)
		}
	}
}

// ---- ingest counterexamples (late original, load), kept as regressions ----
func TestAstra16IngestLateYumiOriginalKeepsCompletedGeneration(t *testing.T) {
	raw := json.RawMessage(`{"hook":"output","listener":{"character_index":0,"chat_index":0,"message_index":1},"event_chat":{"chat_id":"c","rows":[{"i":0,"role":"user","chat_id":"u","data":{"text":"input"}},{"i":1,"role":"char","chat_id":"a","generation_id":"g1","data":{"text":"<!-- yumi-tr:v1:abc:start -->translation<!-- yumi-tr:v1:abc:end -->"}}]},"reread_chat":{"chat_id":"c","rows":[{"i":0,"role":"user","chat_id":"u","data":{"text":"input"}},{"i":1,"role":"char","chat_id":"a","generation_id":"g1","data":{"text":"<!-- yumi-tr:v1:abc:start -->translation<!-- yumi-tr:v1:abc:end -->"}}],"scriptstate":{"$__yumi_tr.abc":{"translation_record":true,"text":"u:{\"model\":\"original\"}"}}}}`)
	res, err := FromEvent(raw, nil)
	if err != nil || len(res) != 2 {
		t.Fatalf("ingest %v %d", err, len(res))
	}
	s := lifecycle.NewSession(res[0].Snapshot.SessionKey)
	s.Apply(res[0].Snapshot)
	s.Apply(res[1].Snapshot)
	// the next input confirms the turn with the original the reread gave
	next := res[1].Snapshot
	next.Source = lifecycle.SourceBeforeRequest
	next.Rows = append(append([]lifecycle.Row{}, next.Rows...), lifecycle.Row{Index: 2, ChatID: "n", Role: "user", Data: "next"})
	s.Apply(next)
	tr := s.ActiveTurns()[0]
	if tr.Responses[0].Revisions[0].Data != "original" || len(tr.Responses[0].Revisions) != 1 {
		t.Fatalf("original not preserved: %+v", tr.Responses[0].Revisions)
	}
	if !tr.Confirmed || !tr.Responses[0].Revisions[0].Confirmed {
		t.Fatal("output event + later reread original + next input lost the completion through production ingest -> lifecycle")
	}
}

func TestAstra16IngestLoadSnapshotIsConsumed(t *testing.T) {
	res, err := FromEvent(json.RawMessage(`{"hook":"load","stored_chat":{"chat_id":"c","rows":[{"i":0,"role":"user","chat_id":"u","data":{"text":"q"}}]}}`), nil)
	if err != nil || len(res) != 1 || res[0].Snapshot.Source != lifecycle.SourceLoad {
		t.Fatalf("load snapshot results=%d error=%v", len(res), err)
	}
}

// A translation record sent as fingerprint only and not cached asks for a resend.
func TestUnknownTranslationRecordAsksForResend(t *testing.T) {
	raw := json.RawMessage(`{"hook":"beforeRequest","stored_chat":{"chat_id":"c","rows":[],"scriptstate":{"$__yumi_tr.ab":{"translation_record":true,"len":5,"h":"1f"}}}}`)
	res, _ := FromEvent(raw, NewTextCache())
	if len(res) != 1 || !res[0].NeedText {
		t.Fatalf("got %+v", res)
	}
}

// ---- fingerprint collision, lone surrogate ----

// "Aa"/"B@" collided under the old 32-bit fingerprint; the 64-bit
// one separates them, and a changed body is never filled with the old text.
func TestAstra18EqualLengthHashCollision(t *testing.T) {
	a, b := "Aa", "B@"
	if na, ha := Fingerprint(a); true {
		if nb, hb := Fingerprint(b); na == nb && ha == hb {
			t.Fatalf("still a collision: %s", ha)
		}
	}
	cache := NewTextCache()
	cache.resolve(TextKey{"A", "c:a"}, textField{Text: &a})
	nb, hb := Fingerprint(b)
	if got, ok := cache.resolve(TextKey{"A", "c:a"}, textField{Len: nb, H: hb}); ok {
		t.Fatalf("changed body filled with %q", got)
	}
}

// Two remembered texts with the same fingerprint are ambiguous: not filled.
func TestAmbiguousFingerprintIsNotFilled(t *testing.T) {
	cache := NewTextCache()
	cache.Warm(TextKey{"A", "c:a"}, "x")
	cache.texts[TextKey{"A", "c:a"}] = append(cache.texts[TextKey{"A", "c:a"}], cache.texts[TextKey{"A", "c:a"}][0]) // same fingerprint twice only via a direct write
	n, h := Fingerprint("x")
	if _, ok := cache.resolve(TextKey{"A", "c:a"}, textField{Len: n, H: h}); ok {
		t.Fatal("ambiguous fingerprint was filled")
	}
}

// A lone surrogate arrives as U+FFFD; the plugin fingerprints the same text
// (the fixture row u4 is checked by TestFingerprintMatchesPlugin too).
func TestLoneSurrogateRowMatchesPlugin(t *testing.T) {
	r := ingestAll(t, NewTextCache())[0][0].Snapshot.Rows[7]
	if r.Data != "lone \ufffd surrogate" || r.Unreadable {
		t.Fatalf("row %+v", r)
	}
}

// After a load, an earlier candidate text of the same row is filled from the
// row's remembered texts (the plugin omits it because it was acknowledged).
func TestReturnToEarlierCandidateIsFilled(t *testing.T) {
	cache := NewTextCache()
	a, b := "candidate A", "candidate B"
	cache.resolve(TextKey{"A", "c:a1"}, textField{Text: &a})
	cache.resolve(TextKey{"A", "c:a1"}, textField{Text: &b})
	n, h := Fingerprint(a)
	if got, ok := cache.resolve(TextKey{"A", "c:a1"}, textField{Len: n, H: h}); !ok || got != a {
		t.Fatalf("got %q %v", got, ok)
	}
}

// A chat variable whose value is a date is the snapshot's story date,
// whatever its name; other values are not; a value sent once fills its
// later fingerprint; a variable known only by fingerprint leaves the date
// unread without asking for a resend.
func TestStoryDateFromVariableValues(t *testing.T) {
	cache := NewTextCache()
	ev := func(seq int, vars string) json.RawMessage {
		return json.RawMessage(`{"boot_id":"b","seq":` + string(rune('0'+seq)) + `,"hook":"load","stored_chat":{"chat_id":"A","rows":[{"i":0,"role":"user","chat_id":"u1","data":{"text":"hi"}}],"scriptstate":` + vars + `}}`)
	}
	res, err := FromEvent(ev(1, `{"clock":{"type":"string","text":"1423/05/15 13:00"},"day":{"type":"string","text":"\"1423-05-16\""},"hp":{"type":"number","text":"30"},"note":{"type":"string","text":"5/15 장날"}}`), cache)
	if err != nil {
		t.Fatal(err)
	}
	d := res[0].Snapshot.Dates
	if len(d) != 2 || d["clock"] != "1423/05/15 13:00" || d["day"] != "1423-05-16" || res[0].NeedText {
		t.Fatalf("dates %v need %v", d, res[0].NeedText)
	}
	n, h := Fingerprint("1423/05/15 13:00")
	res, _ = FromEvent(ev(2, `{"clock":{"type":"string","len":`+itoa(n)+`,"h":"`+h+`"},"unknown":{"type":"string","len":10,"h":"0000000000000000"}}`), cache)
	if d := res[0].Snapshot.Dates; len(d) != 1 || d["clock"] != "1423/05/15 13:00" || res[0].NeedText {
		t.Fatalf("fingerprint: dates %v need %v", d, res[0].NeedText)
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }
