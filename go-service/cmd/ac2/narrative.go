package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/provider"
	"archivecenter2/goservice/internal/store"
)

// Narrative support (1.0 E03, preprocessing step 4). After the memory is chosen (and
// a preprocessing mode has chosen among it), one AI ("the publisher") may:
//   - write a guide at one of four levels, each level
//     adding one kind of item so the levels differ plainly: weak =
//     connections (reference), mid = + reactions (may be used), strong = +
//     beats (to follow, placed right before the reply);
//   - improve the user's input for the main model only: clarify the user's
//     own words, or weave related memories in as short phrases (blending was
//     removed and brought back on 2026-11-10 with a head that forbids copying
//     the memory lines). The chat RisuAI keeps is never changed.
//
// Every item must name its basis (a memory line or the dialog); one without
// is dropped from the guide — the guide and the output
// are never held. Secret-lane memories are not shown to the publisher:
// their protection is Go's and a guide or a rewritten input could carry them
// past it. Any failure sends the request as it would go without support.

// narrTimeout bounds one publisher call; narrDeadline is the latest the
// whole memory answer may run from the request's start (the plugin waits
// 100 s for it): the handler's context ends there.
const narrTimeout = 60 * time.Second

var narrDeadline = 95 * time.Second // a var only so a test can shorten it

type narrDeps struct {
	s       store.NarrativeSettings
	head    string // the guide's rules: the saved override, "" for the default
	ai      provider.Chatter
	problem string // why the AI is missing ("ai_off", ...)
}

// narrativeFor reads the saved choice and builds the publisher's connection:
// the critic's unless its own is saved. nil when both are off.
func (m *models) narrativeFor() (*narrDeps, string) {
	if m == nil || m.db == nil {
		return nil, ""
	}
	s, err := m.db.NarrativeSettings(m.box)
	if err != nil {
		return nil, "store_failed"
	}
	if s.Level == store.GuideOff && s.Input == store.InputOff {
		return nil, ""
	}
	n := &narrDeps{s: s}
	// a saved publisher prompt replaces the default rules; one that cannot be
	// read leaves the default (the guide still runs)
	if head, err := m.db.PublisherPrompt(); err == nil {
		n.head = head
	}
	cfg := s.AI
	if !s.AIOwn {
		m.mu.RLock()
		saved := m.saved
		m.mu.RUnlock()
		cfg, _ = m.critic(saved)
	}
	cfg.NoRedirect = true // the key and the story go only to the address set
	if cfg.Provider == "" {
		n.problem = "ai_off"
	} else if c, err := provider.New(cfg); err != nil {
		n.problem = err.Error()
	} else {
		n.ai = c
	}
	return n, ""
}

// narrGuide is what goes into the request: AfterMemory right after the
// memory block, BeforeReply as the request's last message (strong only).
type narrGuide struct {
	AfterMemory string `json:"after_memory,omitempty"`
	BeforeReply string `json:"before_reply,omitempty"`
}

// inputRewrite replaces the user's text From (the host row Go read) by To in
// the main model's request only.
type inputRewrite struct {
	From string `json:"from"`
	To   string `json:"to"`
}

const narrGuideHead = `You are a narrative editor who helps with the next reply in a roleplay. Instead of writing the reply itself, you write a short guide.

[Conversation] holds the current user input and the recent conversation. [Current user input] at the end repeats that same input. The recent conversation is background; this reply continues from the scene that [Current user input] sets. [Memory lines] are stored memories sent along with this request.

Rules:
- Follow the time, place, direction and pace of the current user input. If the user has moved the scene, do not resume the previous one.
- Take facts only from the conversation and the memory lines. Do not invent new backstory, settings, characters or event outcomes. Do not turn guesses or plans into settled facts.
- Do not decide anything the user's character has not yet written: words, actions, thoughts, feelings or choices. Actions the user has already written are the starting point of this reply.
- Keep to what each character knows and to their relationships, and do not present anything that has not happened yet as a past fact.
- Write only what helps this reply. Do not retell memories at length or repeat the same content across fields.
- Put the memory line numbers that support each item (e.g. "L3") or the conversation mark "D" in that item's "refs" list, never inside its "text". "D" means the item rests on what is actually in the conversation; it is not a mark that turns a guess into fact. Do not write items without a basis.
- Write in the language of the story, not the language of these instructions: follow a language the user explicitly asks for; otherwise use the language of the user input, or of the recent conversation when the input is empty or too short to tell. One or two short sentences per item.`

// narrFields are each level's items, in the order they are explained.
var narrFields = map[string][]string{
	store.GuideWeak:   {"connections"},
	store.GuideMid:    {"connections", "reactions"},
	store.GuideStrong: {"connections", "reactions", "beats"},
}

var narrFieldText = map[string]string{
	"connections": `connections: promises, states and relationships still in force in this scene, and how they bear on the current situation. Find them in the memory lines or the conversation. Do not write a plain summary of the past story. Format {"text":"…","refs":["L2"]}`,
	"reactions":   `reactions: possible reactions of NPCs to this input. Make clear whose words or actions each one answers, and write it as a possible reaction for the reply to come, not as something that has happened. Do not write reactions of the user's character. Not every character needs one. Format {"who":"character name","text":"…","refs":["D"]}`,
	"beats":       `beats: directions for the order in which this reply unfolds. They are plans for the reply still to be written, not past events. Start from the reaction to the current input, develop as far as needed, then end this reply. part is whichever of "opening", "development" and "closing" applies; not every part is needed. "closing" does not mean resolving the event or moving on to the next day. Do not decide choices that are left to the user. Format {"part":"opening","text":"…","refs":["D"]}`,
}

func narrGuidePrompt(head, level, state, lines, input string) string {
	var b strings.Builder
	if strings.TrimSpace(head) == "" {
		head = narrGuideHead
	}
	b.WriteString(strings.TrimRight(head, "\n") + "\n\n")
	fields := narrFields[level]
	shape := map[string][]any{}
	for _, f := range fields {
		b.WriteString("- " + narrFieldText[f] + "\n")
		shape[f] = []any{}
	}
	b.WriteString("\n")
	out, _ := json.Marshal(shape)
	if lines == "" {
		lines = "(none)"
	}
	b.WriteString("Output exactly one JSON object, with no code block and no commentary. Use only the keys below, each exactly once, each value a list; give a key [] when it has nothing useful, and add no other keys.\n" + string(out) + "\n\n[Conversation]\n" + state + "\n\n[Memory lines]\n" + lines + "\n\n[Current user input]\n" + input + "\n\nWrite the guide for the reply that follows this input, in the JSON above.")
	return b.String()
}

const narrClarifyHead = `You are an editor who polishes the wording of a roleplay user's input. [Conversation] is context for understanding the meaning and includes the current input. Polish only the text in the final [User input]. Do not write the next part of the story or anyone's answer.

Keep the meaning and the voice while making the sentences natural and clear.
- Copy dialogue inside quotation marks and directions inside parentheses exactly, character for character and punctuation included. This comes before every other change below.
- Outside them, you may split sentences and fix particles, spacing, word order and needlessly repeated wording.
- Fill in an omitted subject or object only when the current input and the recent conversation make clear who or what it refers to. Leave anything that can be read more than one way as it is; do not decide it.
- Do not add new facts, actions, lines, feelings, reasons or descriptions. Do not add explanations taken from the conversation or memories.
- Keep the original's voice, point of view, person and language, and the distinction between dialogue, narration and directions.
- Keep names, numbers, negations, conditions, questions, hesitation, emphasis and the order of actions. Do not turn an attempt or a plan into something done. Do not drop meaning to make it shorter.
- If it is already clear and natural, do not change it. Change only what needs fixing and keep the rest.

If you changed it, output one JSON: {"input":"the full polished user input"}
If nothing needs changing: {"unchanged":true}
Output only one of the two, with no code block or commentary.`

// narrBlendHead brings memory blending back ("되살리는데 이전에는 기억 입력에 있던 내용을
// 그대로 붙여 넣어서 그래"). The earlier head only said to use facts from the
// memory lines, and the model copied the lines; this one says how a
// memory may enter the input.
const narrBlendHead = `You are an editor who works past events into a roleplay user's input. [Conversation] is the recent conversation and includes the current input. [Memory lines] are stored memories sent along with this request. Change only the text in the final [User input]. Do not write the next part of the story or anyone's answer.

Only when [Memory lines] hold a past event that the user input points to or relies on, work that event into the user's own sentences as a short phrase.
- Do not copy sentences from the memory lines. Turn each fact into one short phrase in the user's voice and point of view, and put it inside the sentence that touches that fact.
- Work in three facts at most. Do not append explanatory sentences to the end of the input, and do not make lists, headings or parenthetical explanations.
- Do not add again what the input or [Conversation] already says. Leave out memories that do not touch the current input. Do not go looking for something to add.
- Use only facts in [Memory lines]. Do not add new facts, actions, lines, feelings, reasons or descriptions.
- Work in only what the user's character has seen, heard or knows. Do not add other characters' inner thoughts, feelings or hidden circumstances.
- Do not change the text outside the phrases you work in, not even a single word or punctuation mark. Keep the user's meaning, actions, order of actions, voice, point of view, person and language. Copy dialogue inside quotation marks and directions inside parentheses exactly, character for character and punctuation included.

If you changed it, output one JSON: {"input":"the full changed user input","refs":["L2"]}
If there is nothing to work in: {"unchanged":true}
Output only one of the two, with no code block or commentary.`

// firstJSON decodes the answer's first JSON object into v (a model may add
// prose around it).
func firstJSON(s string, v any) error {
	at := strings.Index(s, "{")
	if at < 0 {
		return errors.New("ai_answer_without_json")
	}
	if err := json.NewDecoder(strings.NewReader(s[at:])).Decode(v); err != nil {
		return errors.New("ai_answer_not_json")
	}
	return nil
}

// guideItems reads the guide answer's first JSON object key by key (11-10
// measurement: a model added an object key or wrote a key twice, and reading
// the whole object into one map lost every item). A key whose value is not a
// list of items is not a guide field and is passed over; a key written twice
// keeps both lists; an item of the wrong shape is passed over alone; a cut
// after whole keys keeps them. renderGuide still takes only the level's
// fields and drops items without a basis.
func guideItems(s string) (map[string][]narrItem, error) {
	at := strings.Index(s, "{")
	if at < 0 {
		return nil, errors.New("ai_answer_without_json")
	}
	dec := json.NewDecoder(strings.NewReader(s[at:]))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, errors.New("ai_answer_not_json")
	}
	out := map[string][]narrItem{}
	read := 0
	for dec.More() {
		t, err := dec.Token()
		key, ok := t.(string)
		var raw json.RawMessage
		if err != nil || !ok || dec.Decode(&raw) != nil {
			// a cut-off answer keeps the keys read whole before the cut,
			// as a cut-off critic answer keeps its finished memories
			// (11-10 audit); nothing read whole is still not JSON
			if read == 0 {
				return nil, errors.New("ai_answer_not_json")
			}
			break
		}
		read++
		var list []json.RawMessage
		if json.Unmarshal(raw, &list) != nil {
			continue
		}
		for _, one := range list {
			var it narrItem
			if json.Unmarshal(one, &it) == nil { // a malformed item drops itself, not its list
				out[key] = append(out[key], it)
			}
		}
	}
	return out, nil
}

type narrItem struct {
	Who  string   `json:"who"`
	Part string   `json:"part"`
	Text string   `json:"text"`
	Refs []string `json:"refs"`
}

// grounded says whether an item names a basis that exists: a line shown
// ("L3") or the dialog ("D").
func grounded(it narrItem, shown map[string]bool) bool {
	for _, r := range it.Refs {
		r = strings.TrimSpace(r)
		if r == "D" || shown[r] {
			return true
		}
	}
	return false
}

// renderGuide puts the grounded items in within the ceiling (counted in
// characters, headers included): beats first at strong, then connections,
// then reactions; an item that does not fit is skipped. A ceiling, never a
// fill target.
func renderGuide(level string, items map[string][]narrItem, shown map[string]bool, ceiling int, rec *store.NarrativeRecord) narrGuide {
	type sec struct {
		head  string
		lines []string
	}
	refHead := "[Narrative notes — background on past events and characters; use what fits, none of it must appear. The current input's direction and pace come first]"
	if level == store.GuideWeak {
		refHead = "[Narrative notes — background on past events and characters; no need to explain them. The current input's direction and pace come first]"
	}
	after := []*sec{{head: "Scene connections:"}, {head: "NPC reaction options:"}}
	before := &sec{}
	render := func() narrGuide {
		var g narrGuide
		var b strings.Builder
		for _, s := range after {
			if len(s.lines) > 0 {
				b.WriteString(s.head + "\n" + strings.Join(s.lines, "\n") + "\n")
			}
		}
		if b.Len() > 0 {
			g.AfterMemory = refHead + "\n" + strings.TrimSuffix(b.String(), "\n")
		}
		if len(before.lines) > 0 {
			g.BeforeReply = "[Plan for this reply — follow this order within the current input's direction and pace. Do not decide the user character's unchosen actions. These are plans, not past events; do not narrate them]\n" + strings.Join(before.lines, "\n")
		}
		return g
	}
	// counted as sent: each part with its title and fingerprint
	size := func(g narrGuide) int {
		return utf8.RuneCountInString(store.GuideMessage(g.AfterMemory)) + utf8.RuneCountInString(store.GuideMessage(g.BeforeReply))
	}
	type slot struct {
		field string
		to    *sec
	}
	order := []slot{{"connections", after[0]}, {"reactions", after[1]}}
	if level == store.GuideStrong {
		order = append([]slot{{"beats", before}}, order...)
	}
	allowed := map[string]bool{}
	for _, f := range narrFields[level] {
		allowed[f] = true
	}
	for _, o := range order {
		if !allowed[o.field] {
			continue // a field the level does not have is thrown away
		}
		for _, it := range items[o.field] {
			text := strings.Join(strings.Fields(it.Text), " ")
			if text == "" {
				continue
			}
			if !grounded(it, shown) {
				rec.Dropped++
				continue
			}
			line := "- " + text
			switch o.field {
			case "reactions":
				if w := strings.TrimSpace(it.Who); w != "" {
					line = "- " + w + ": " + text
				}
			case "beats":
				if p := strings.TrimSpace(it.Part); p != "" {
					line = "- " + p + ": " + text
				}
			}
			o.to.lines = append(o.to.lines, line)
			if size(render()) > ceiling {
				o.to.lines = o.to.lines[:len(o.to.lines)-1]
				rec.OverBudget++
				continue
			}
			rec.Kept++
		}
	}
	g := render()
	rec.Chars = size(g)
	return g
}

// narrLines are the delivered non-secret records' lines, labelled L1.. in
// the delivery's order.
func narrLines(d store.Delivery) (string, map[string]bool) {
	var b strings.Builder
	shown := map[string]bool{}
	n := 0
	for i, id := range d.IDs {
		if d.SecretIDs[id] || i >= len(d.Lines) {
			continue
		}
		n++
		label := fmt.Sprintf("L%d", n)
		shown[label] = true
		fmt.Fprintf(&b, "%s %s\n", label, strings.TrimPrefix(d.Lines[i], "- "))
	}
	return b.String(), shown
}

// narrative runs the guide and the input improvement (at once) over the
// delivery being sent. started is the request's start.
func narrative(ctx context.Context, n *narrDeps, d store.Delivery, scope []memory.ScopedTurn, input string, started time.Time) (narrGuide, *inputRewrite, *store.NarrativeRecord) {
	t0 := time.Now()
	rec := &store.NarrativeRecord{Level: n.s.Level, Input: n.s.Input, CharsMax: n.s.Chars}
	defer func() { rec.MS = time.Since(t0).Milliseconds() }()
	var g narrGuide
	var rw *inputRewrite
	lines, shown := narrLines(d)
	state := prepState(input, scope, d.Window)
	wantGuide := n.s.Level != store.GuideOff
	wantInput := n.s.Input != store.InputOff
	switch {
	case wantGuide && n.ai == nil:
		rec.Guide = n.problem
		wantGuide = false
	case wantGuide && n.s.Chars == 0:
		rec.Guide = "no_budget"
		wantGuide = false
	}
	switch {
	case wantInput && n.ai == nil:
		rec.InputResult = n.problem
		wantInput = false
	case wantInput && strings.TrimSpace(input) == "":
		rec.InputResult = "no_input_text"
		wantInput = false
	case wantInput && n.s.Input == store.InputBlend && len(shown) == 0:
		// nothing delivered to blend: the input goes as the user wrote it
		rec.InputResult = "unchanged"
		wantInput = false
	}
	if !wantGuide && !wantInput {
		return g, nil, rec
	}
	limit := narrTimeout
	if left := narrDeadline - time.Since(started); left < limit {
		limit = left
	}
	if limit <= 0 {
		if wantGuide {
			rec.Guide = "no_time_left"
		}
		if wantInput {
			rec.InputResult = "no_time_left"
		}
		return g, nil, rec
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	var wg sync.WaitGroup
	var mu sync.Mutex
	if wantGuide {
		wg.Add(1)
		go func() {
			defer wg.Done()
			t := time.Now()
			r, err := n.ai.Chat(ctx, narrGuidePrompt(n.head, n.s.Level, state, lines, input))
			mu.Lock()
			rec.GuideMS = time.Since(t).Milliseconds()
			defer mu.Unlock()
			rec.AICalls++
			if err == nil {
				var items map[string][]narrItem
				if items, err = guideItems(r.Content); err == nil {
					g = renderGuide(n.s.Level, items, shown, n.s.Chars, rec)
					if g.AfterMemory == "" && g.BeforeReply == "" {
						rec.Guide = "nothing_kept"
					} else {
						rec.Guide = "made"
						rec.GuideText = strings.TrimSpace(g.AfterMemory + "\n\n" + g.BeforeReply)
					}
					return
				}
			}
			rec.Guide, rec.GuideError = "ai_failed", err.Error()
		}()
	}
	if wantInput {
		wg.Add(1)
		go func() {
			defer wg.Done()
			prompt := narrClarifyHead + "\n\n[Conversation]\n" + state + "\n\n[User input]\n" + input
			if n.s.Input == store.InputBlend {
				prompt = narrBlendHead + "\n\n[Conversation]\n" + state + "\n\n[Memory lines]\n" + lines + "\n[User input]\n" + input
			}
			t := time.Now()
			r, err := n.ai.Chat(ctx, prompt)
			mu.Lock()
			rec.InputMS = time.Since(t).Milliseconds()
			defer mu.Unlock()
			rec.AICalls++
			if err == nil {
				var a struct {
					Input     string `json:"input"`
					Unchanged bool   `json:"unchanged"`
				}
				if err = firstJSON(r.Content, &a); err == nil {
					to := strings.TrimSpace(a.Input)
					if a.Unchanged || to == "" || to == strings.TrimSpace(input) {
						rec.InputResult = "unchanged"
						return
					}
					rw = &inputRewrite{From: input, To: to}
					rec.InputResult, rec.InputBefore, rec.InputAfter = "changed", input, to
					return
				}
			}
			rec.InputResult, rec.InputError = "ai_failed", err.Error()
		}()
	}
	wg.Wait()
	return g, rw, rec
}
