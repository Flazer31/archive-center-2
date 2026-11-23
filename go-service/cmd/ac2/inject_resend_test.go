package main

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	"archivecenter2/goservice/internal/ingest"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/provider"
	"archivecenter2/goservice/internal/store"
)

// 1.4 I081: RisuAI calls the request hook again for each provider retry
// (request.ts: the replacer runs inside the retry loop), and the request then
// still holds this plugin's messages and the input as improved. Production
// /memory/inject answers such a resend, at the same turn and input and with
// exactly the kept answer's messages, with that answer again (no second
// publisher call, the same block and guide, placed in the request as it is
// now); a resend never gets another input improvement; every other request
// is answered anew as before.
func TestResentRequestGetsTheSameAnswer(t *testing.T) {
	h := hudFixture(t, "", 0)
	srv := narrSeed(t, h, "R")
	narrSeed(t, h, "R2")
	f := newNarrFake(t)
	f.guide = `{"connections":[{"text":"리나의 약속이 아직 남았다","refs":["L1"]}]}`
	f.input = `{"input":"리나에게 약속을 묻는다"}`
	narrSet(t, h, f, store.GuideWeak, store.InputClarify, store.DefaultGuideChars)
	inject := injectHandler(srv, h.pipe, h.db, func() *provider.Embedder { return nil }, h.st)
	roles := func(rs ...string) []memory.PayloadMessage {
		out := []memory.PayloadMessage{}
		for _, r := range rs {
			out = append(out, memory.PayloadMessage{Role: r})
		}
		return out
	}
	plain := roles("system", "user", "assistant", "user")
	ask := func(chat string, back []string, placement []memory.PayloadMessage) memAnswer {
		t.Helper()
		prints := []store.TextPrint{}
		for _, text := range []string{"입력 2", "응답 2", "리나의 약속과 생각"} {
			n, hash := ingest.Fingerprint(text)
			prints = append(prints, store.TextPrint{Len: n, H: hash})
		}
		body, _ := json.Marshal(map[string]any{"chat_id": chat, "input": "리나의 약속과 생각", "payload": prints, "handed_back": back, "placement_messages": placement})
		a := callMem(t, inject, "POST", "/memory/inject", string(body))
		if a.Code != 200 {
			t.Fatalf("inject: %d %s", a.Code, a.Raw)
		}
		return a
	}
	// the fingerprints the plugin reads back from the messages it put in
	// (text mode: the block and the guide parts, and the improved input it
	// finds in the request)
	sent := func(a memAnswer) []string {
		var out []string
		after, before := guideOf(a)
		for _, m := range []struct{ text, title string }{{a.V["block"].(string), store.BlockTitle}, {after, store.GuideTitle}, {before, store.GuideTitle}} {
			if h := titlePrint(m.text, m.title); h != "" {
				out = append(out, h)
			}
		}
		if rw, _ := a.V["input_rewrite"].(map[string]any); rw != nil {
			_, to := ingest.Fingerprint(rw["to"].(string))
			_, from := ingest.Fingerprint(rw["from"].(string))
			out = append(out, rewritePrint+to+":"+from)
		}
		return out
	}
	index := func(a memAnswer) float64 { return a.V["placement"].(map[string]any)["index"].(float64) }

	first := ask("R", nil, plain)
	if n := len(f.take()); n != 2 {
		t.Fatalf("first request: %d publisher calls, want 2 (guide, input)", n)
	}
	back := sent(first)
	if len(back) != 3 || first.V["input_rewrite"] == nil {
		t.Fatalf("fixture: want a block, a guide and an improved input, got %v: %s", back, first.Raw)
	}

	// the host resends the same request, a request trigger having put one
	// more message at its start: the same answer, placed in this request,
	// without the input improvement the request holds already
	again := ask("R", back, append(roles("system"), plain...))
	if n := len(f.take()); n != 0 {
		t.Fatalf("resend: %d publisher calls, want 0", n)
	}
	if again.V["block"] != first.V["block"] || fmt.Sprint(again.V["guide"]) != fmt.Sprint(first.V["guide"]) {
		t.Fatalf("resend answer differs:\n%s\n%s", first.Raw, again.Raw)
	}
	if index(again) != index(first)+1 {
		t.Fatalf("resend placement %v, want the current request's %v", index(again), index(first)+1)
	}
	if again.V["input_rewrite"] != nil {
		t.Fatalf("a resend must not improve the input again: %v", again.V["input_rewrite"])
	}

	// negative: a request without the plugin's messages (a reroll, a new
	// request) is made anew, input improvement included
	if a := ask("R", nil, plain); len(f.take()) != 2 || a.V["input_rewrite"] == nil {
		t.Fatal("a request without the plugin's messages must be answered anew, input improved")
	}
	// negative: another request B came in between with another guide; A's
	// resend (A's guide, not B's) is answered anew, never with B's answer,
	// and without a second input improvement over A's
	f.guide = `{"connections":[{"text":"리나가 약국 일을 걱정한다","refs":["L2"]}]}`
	bAnswer := ask("R", nil, plain)
	f.take()
	if guideAfter(bAnswer) == guideAfter(first) {
		t.Fatal("fixture: B's guide equals A's")
	}
	c := ask("R", back, plain)
	if n := len(f.take()); n != 1 || c.V["input_rewrite"] != nil {
		t.Fatalf("a resend carrying another answer's messages: %d calls (want the guide only), rewrite %v", n, c.V["input_rewrite"])
	}
	// that answer made anew is resent in turn (its messages, A's improved
	// input still in the request): reused, no call
	if ask("R", append(sent(c), back[len(back)-1]), plain); len(f.take()) != 0 {
		t.Fatal("the answer made anew for a request holding an improved input must be reused on its resend")
	}
	// negative: B with the same block and guide but another improvement;
	// A's resend (A's improvement in the request) is answered anew, its
	// improvement not overwritten
	f.guide = `{"connections":[{"text":"리나의 약속이 아직 남았다","refs":["L1"]}]}`
	f.input = `{"input":"리나에게 생각을 묻는다"}`
	b2 := ask("R", nil, plain)
	f.take()
	if guideAfter(b2) != guideAfter(first) || b2.V["block"] != first.V["block"] {
		t.Fatal("fixture: B2 must share A's block and guide")
	}
	if c := ask("R", back, plain); len(f.take()) != 1 || c.V["input_rewrite"] != nil {
		t.Fatalf("a resend with another improvement than the kept one: rewrite %v", c.V["input_rewrite"])
	}
	// the request holds only the improved input (no memory or guide message
	// came back): no improvement over it
	if c := ask("R", back[2:], plain); len(f.take()) != 1 || c.V["input_rewrite"] != nil {
		t.Fatalf("a request holding only the improved input: rewrite %v", c.V["input_rewrite"])
	}
	// an improvement of another text than this turn's input (a user-role
	// preset equal to an earlier improved input): the input is improved
	_, other := ingest.Fingerprint("다른 입력")
	if c := ask("R", []string{rewritePrint + "0000000000000000:" + other}, plain); len(f.take()) != 2 || c.V["input_rewrite"] == nil {
		t.Fatalf("an improvement of another text must not stop this input's: %v", c.V["input_rewrite"])
	}
	// the plugin's messages came back but the input not improved (a trigger
	// put the user's text back): the input is improved as usual
	fresh := ask("R", nil, plain)
	f.take()
	if c := ask("R", sent(fresh)[:2], plain); len(f.take()) != 2 || c.V["input_rewrite"] == nil {
		t.Fatalf("a request without the improved input must get one: %v", c.V["input_rewrite"])
	}
	// negative: the input edited since (the same host row): made anew
	cur := ask("R", nil, plain)
	f.take()
	rows := []string{memRow(6, "user", "R-current", "", "리나의 약속과 생각을 다시 묻는다")}
	if _, err := h.pipe.Process([]observe.Record{memEvent(9, "beforeRequest", "R", append(seedRows("R"), rows...)...)}); err != nil {
		t.Fatal(err)
	}
	// (the old improvement is of another text: the new input is improved)
	if c := ask("R", sent(cur), plain); len(f.take()) != 2 || c.V["input_rewrite"] == nil {
		t.Fatal("a resend after the input changed must be answered anew, the new input improved")
	}
	// negative: a resend in a chat with no answer kept is made anew
	if ask("R2", back, plain); len(f.take()) != 1 {
		t.Fatal("a resend with nothing kept for its chat must be answered anew")
	}
}

// In a PDF mode the plugin puts the memory in as the PDF part and the part
// kept as text (or, without its PDF hook, as the block): either set of
// fingerprints, with the guide's, is the answer's.
func TestResendPrintsFollowTheTransport(t *testing.T) {
	titled := func(title, body string) string {
		b := body + "\n"
		_, h := ingest.Fingerprint(b)
		return title + h + "]\n" + b
	}
	block, pdf, kept := titled(store.BlockTitle, "a\nb"), titled(store.BlockTitle, "a"), titled(store.BlockTitle, "b")
	guide := titled(store.GuideTitle, "g")
	a := injectAnswer{Delivery: store.Delivery{Block: block}, Transport: &memoryTransport{Mode: "google_pdf", PDFText: pdf, RetainedText: kept}, Guide: &narrGuide{AfterMemory: guide}}
	got := a.prints()
	want := [][]string{sortedCopy([]string{titlePrint(block, store.BlockTitle), titlePrint(guide, store.GuideTitle)}),
		sortedCopy([]string{titlePrint(pdf, store.BlockTitle), titlePrint(kept, store.BlockTitle), titlePrint(guide, store.GuideTitle)})}
	if len(got) != 2 || !slices.Equal(got[0], want[0]) || !slices.Equal(got[1], want[1]) {
		t.Fatalf("prints %v, want %v", got, want)
	}
	// text mode: the block only
	a.Transport = &memoryTransport{Mode: "text"}
	if got := a.prints(); len(got) != 1 || !slices.Equal(got[0], want[0]) {
		t.Fatalf("text-mode prints %v", got)
	}
}

func guideAfter(a memAnswer) string {
	after, _ := guideOf(a)
	return after
}

// seedRows are injectionSeed's first three turns of chat.
func seedRows(chat string) []string {
	rows := []string{}
	for i := 0; i < 3; i++ {
		rows = append(rows, memRow(2*i, "user", fmt.Sprintf("%s-u%d", chat, i), "", fmt.Sprintf("입력 %d", i)), memRow(2*i+1, "char", fmt.Sprintf("%s-a%d", chat, i), "g", fmt.Sprintf("응답 %d", i)))
	}
	return rows
}
