package main

import (
	"encoding/json"
	"fmt"
	"testing"

	"archivecenter2/goservice/internal/ingest"
	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/provider"
	"archivecenter2/goservice/internal/store"
)

func prints(role string, texts ...string) []store.TextPrint {
	var out []store.TextPrint
	for _, s := range texts {
		n, h := ingest.Fingerprint(s)
		out = append(out, store.TextPrint{Len: n, H: h, Role: role})
	}
	return out
}

// The equal beginning of two requests (1.1 A2): a turn appended keeps it,
// the oldest message trimmed loses it all, a changed role breaks it.
func TestComparePrefix(t *testing.T) {
	prev := prints("user", "a", "bb", "ccc")
	cases := []struct {
		name            string
		cur             []store.TextPrint
		same, chars, sc int
	}{
		{"appended", prints("user", "a", "bb", "ccc", "dddd"), 3, 10, 6},
		{"front trimmed", prints("user", "bb", "ccc", "dddd"), 0, 9, 0},
		{"middle changed", prints("user", "a", "xx", "ccc"), 1, 6, 1},
		{"same", prints("user", "a", "bb", "ccc"), 3, 6, 6},
		{"role changed", append(prints("system", "a"), prints("user", "bb", "ccc")...), 0, 6, 0},
	}
	for _, c := range cases {
		got := store.ComparePrefix(prev, c.cur)
		if got.Same != c.same || got.Chars != c.chars || got.SameChars != c.sc || got.Total != len(c.cur) || got.PrevTotal != 3 {
			t.Fatalf("%s: %+v", c.name, got)
		}
	}
}

// Through the production handler: the first request has nothing to
// compare; the next is compared with it, in the log form, the kept trace
// and the last-delivery view; another chat keeps its own.
func TestRequestPrefixRecorded(t *testing.T) {
	h := hudFixture(t, "", 0)
	injectionSeed(t, h, "P")
	injectionSeed(t, h, "Q")
	srv := observe.NewServer(store.RawSink{DB: h.db})
	srv.After = h.pipe
	inject := injectHandler(srv, h.pipe, h.db, func() *provider.Embedder { return nil }, h.st)
	send := func(chat string, p []store.TextPrint) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"chat_id": chat, "input": "리나의 약속과 생각", "type": "model", "payload": p})
		if a := callMem(t, inject, "POST", "/memory/inject", string(body)); a.Code != 200 {
			t.Fatalf("inject: %d %s", a.Code, a.Raw)
		}
	}
	first := prints("user", "시스템", "입력 2", "응답 2", "리나의 약속과 생각")
	send("P", first)
	if rec := lastTrace(t, h, "P").Summary; rec.Prefix != nil {
		t.Fatalf("first request compared: %+v", rec.Prefix)
	}
	if v := injectionGet(t, h, "P"); v.V["prefix"] != nil {
		t.Fatalf("first view: %v", v.V["prefix"])
	}
	// the host trimmed the oldest message: nothing equal from the start
	send("P", first[1:])
	rec := lastTrace(t, h, "P").Summary
	if rec.Prefix == nil || rec.Prefix.Same != 0 || rec.Prefix.Total != 3 || rec.Prefix.PrevTotal != 4 {
		t.Fatalf("trimmed: %+v", rec.Prefix)
	}
	// a turn appended: all of the previous request is the same start
	next := append(append([]store.TextPrint(nil), first[1:]...), prints("user", "다음 입력")...)
	send("P", next)
	rec = lastTrace(t, h, "P").Summary
	if rec.Prefix == nil || rec.Prefix.Same != 3 || rec.Prefix.Total != 4 || rec.Prefix.SameChars != first[1].Len+first[2].Len+first[3].Len {
		t.Fatalf("appended: %+v", rec.Prefix)
	}
	v := injectionGet(t, h, "P")
	p, _ := v.V["prefix"].(map[string]any)
	if p == nil || p["same"] != float64(3) || p["total"] != float64(4) {
		t.Fatalf("last-delivery view: %v", v.V["prefix"])
	}
	if got := prefixText(rec.Prefix); got != "same start as the previous request: 3/4 messages, "+itoa(rec.Prefix.SameChars)+"/"+itoa(rec.Prefix.Chars)+" chars (previous 3 messages)" {
		t.Fatalf("log form: %s", got)
	}
	// after a restart the view is read from the kept trace
	h.st.mu.Lock()
	delete(h.st.last, "P")
	h.st.mu.Unlock()
	if p, _ := injectionGet(t, h, "P").V["prefix"].(map[string]any); p == nil || p["same"] != float64(3) {
		t.Fatalf("view after a restart: %v", p)
	}
	// another chat has its own previous request
	send("Q", next)
	if rec := lastTrace(t, h, "Q").Summary; rec.Prefix != nil {
		t.Fatalf("another chat compared with P: %+v", rec.Prefix)
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// The previous requests are kept for the injectionsKept chats used last
// (one per chat without end otherwise); a chat dropped has nothing to
// compare, a recent one keeps its comparison.
func TestRequestPrefixKeptForRecentChats(t *testing.T) {
	h := hudFixture(t, "", 0)
	srv := observe.NewServer(store.RawSink{DB: h.db})
	srv.After = h.pipe
	inject := injectHandler(srv, h.pipe, h.db, func() *provider.Embedder { return nil }, h.st)
	p := prints("user", "시스템", "입력")
	send := func(chat string) *store.RequestPrefix {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"chat_id": chat, "input": "입력", "type": "model", "payload": p})
		if a := callMem(t, inject, "POST", "/memory/inject", string(body)); a.Code != 200 {
			t.Fatalf("inject: %d %s", a.Code, a.Raw)
		}
		return lastTrace(t, h, chat).Summary.Prefix
	}
	for i := 0; i <= injectionsKept; i++ {
		send(fmt.Sprintf("C%d", i))
	}
	if got := send(fmt.Sprintf("C%d", injectionsKept)); got == nil || got.Same != 2 {
		t.Fatalf("a recent chat compared: %+v", got)
	}
	if got := send("C0"); got != nil {
		t.Fatalf("the chat used longest ago kept its request: %+v", got)
	}
}
