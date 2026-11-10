package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/provider"
	"archivecenter2/goservice/internal/store"
)

// Preprocessing (1.0 E01~E03, plan 20261107-ac2-preprocessing/PLAN-01.md
// revision 4). A mode chooses among the records Go ranked; every record
// stays the stored original and Go renders the choice
// (store.DeliveryRequest.Only). Measured on 21 scenes (Sol judge, 11-07):
//   - JEV (GXW): the decision model is asked of every line of the wide list
//     (the same ranking with no ceiling) whether it is unrelated to the
//     scene, the basic delivery's lines in a request of their own; lines of the basic delivery at 0.5 or more are left out, lines
//     outside it below 0.5 are added, least unrelated first, within each
//     lane's ceiling. U+D 11.05%, 19 of 28 items.
//   - AI (AX): the AI is shown the basic delivery and names unrelated lines;
//     they are left out. 15.03%, 17.
//   - AI + JEV (AND): both are asked of the basic delivery; a line both call
//     unrelated is left out. 19.63%, 17.
//
// Any failure delivers the basic delivery as it is (output is never held).

// prepTimeout bounds the whole preprocessing of one request; the measured
// slowest AI answer was 62 s (deepseek-v4.1-flash, 236 lines).
const prepTimeout = 90 * time.Second

// prepWide is "no ceiling" for the wide list (as measured 11-07).
const prepWide = 10000000

// prepParallel is the most decision requests sent at once.
const prepParallel = 4

const prepUnrelatedYes = "지금 장면과 관계가 없다: 지금 장면의 인물·장소·진행 중인 일과 이어지지 않는 다른 일·다른 장소·끝난 다른 사건이다"
const prepUnrelatedNo = "지금 장면과 이어진다: 지금 장면의 인물·장소·진행 중인 일, 또는 이 장면이 떠올리게 하는 지난 일과 관련 있다"
const prepQuestion = "아래 기억이 지금 장면과 관계없는 다른 일인가?\n기억: "

// prepAIHead is the AI's instruction (the measured AX wording).
const prepAIHead = `너는 롤플레이 기억 편집자다. [대화]는 다음 응답을 쓰는 모델이 이미 받는 현재 사용자 입력과 최근 대화다. [기억 줄]은 이번에 함께 넣을 저장된 기억이다.
지금 장면과 관계가 없는 줄만 골라라. 나머지는 모두 그대로 넣는다.
관계가 없다: 지금 장면의 인물·장소·진행 중인 일과 이어지지 않는 다른 일·다른 장소·끝난 다른 사건이다.
관계가 있다(고르지 마라): 지금 장면의 인물·장소·진행 중인 일, 또는 이 장면이 떠올리게 하는 지난 일과 관련 있다. 관계가 있으면 필요해 보이지 않아도 고르지 마라.
출력은 JSON 하나만: {"unrelated":["L3","L7"]} — 없으면 {"unrelated":[]}.`

// prepDeps are the connections of one request's preprocessing.
type prepDeps struct {
	mode    string
	decider *provider.Decider // nil: none set or it lacks something
	ai      provider.Chatter  // nil: none set or it lacks something
	// why a needed connection is missing ("decision_off", "ai_off", ...)
	decisionProblem, aiProblem string
}

// prepFor reads the saved mode and builds its connections (nil = off). A
// failed read is told apart from off (review 18 B-04): the basic delivery
// goes and the delivery's record says why.
func (m *models) prepFor() (*prepDeps, string) {
	if m == nil || m.db == nil {
		return nil, ""
	}
	s, err := m.db.PreprocessSettings(m.box)
	if err != nil {
		return nil, "store_failed"
	}
	if s.Mode == store.PreprocessOff {
		return nil, ""
	}
	p := &prepDeps{mode: s.Mode}
	if s.Mode == store.PreprocessJev || s.Mode == store.PreprocessAIJev {
		ds, err := m.db.DecisionSettings(m.box)
		switch {
		case err != nil:
			p.decisionProblem = "store_failed"
		case ds.Provider == "":
			p.decisionProblem = "decision_off"
		default:
			if p.decider, err = provider.NewDecider(provider.DecisionConfig{Provider: ds.Provider, URL: ds.URL, Model: ds.Model, Key: ds.Key}); err != nil {
				p.decisionProblem = err.Error()
			}
		}
	}
	if s.Mode == store.PreprocessAI || s.Mode == store.PreprocessAIJev {
		cfg := s.AI
		if !s.AIOwn {
			m.mu.RLock()
			saved := m.saved
			m.mu.RUnlock()
			cfg, _ = m.critic(saved)
		}
		// the key and the story go only to the address set (review 18 A-01)
		cfg.NoRedirect = true
		if cfg.Provider == "" {
			p.aiProblem = "ai_off"
		} else if c, err := provider.New(cfg); err != nil {
			p.aiProblem = err.Error()
		} else {
			p.ai = c
		}
	}
	return p, ""
}

// prepState is what the models read of the scene: the input and the latest
// turns the request already holds (the delivery's window), as the measured
// judge packets showed them.
func prepState(input string, scope []memory.ScopedTurn, window int) string {
	n := len(scope)
	for n > 0 && len(scope[n-1].Source.Responses) == 0 {
		n--
	}
	from := max(0, n-window)
	var b strings.Builder
	b.WriteString("### 현재 사용자 입력\n" + input + "\n\n### 최근 대화 (직전 " + fmt.Sprint(n-from) + "턴, 전문)\n")
	for _, t := range scope[from:n] {
		for _, m := range t.Source.Members {
			b.WriteString("[user]\n" + m.Data + "\n")
		}
		for _, r := range t.Source.Responses {
			b.WriteString("[assistant]\n" + r.Data + "\n")
		}
	}
	return b.String()
}

// deliveryLines are a delivery's rendered record lines, by id, as Go
// rendered each record (one record may hold line breaks; review 18 B-02).
func deliveryLines(d store.Delivery) map[string]string {
	out := map[string]string{}
	for i, id := range d.IDs {
		if i < len(d.Lines) {
			out[id] = d.Lines[i]
		}
	}
	return out
}

// askUnrelated asks the decision model of each line (by id, in order) and
// returns P(unrelated) by id; requests of 64 go at most prepParallel at once.
// A failed request leaves its lines unanswered; err is the first failure.
func askUnrelated(ctx context.Context, dec *provider.Decider, state string, ids []string, lines map[string]string, rec *store.PreprocessRecord) (map[string]float64, error) {
	var qs []provider.Question
	for _, id := range ids {
		qs = append(qs, provider.Question{ID: id, Text: prepQuestion + lines[id], Yes: prepUnrelatedYes, No: prepUnrelatedNo})
	}
	rec.Asked = len(qs)
	out := map[string]float64{}
	var mu sync.Mutex
	var first error
	var wg sync.WaitGroup
	slots := make(chan struct{}, prepParallel)
	for b := 0; b < len(qs); b += 64 {
		part := qs[b:min(b+64, len(qs))]
		wg.Add(1)
		slots <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			a, err := dec.Ask(ctx, state, part)
			mu.Lock()
			defer mu.Unlock()
			for k, v := range a.Yes {
				out[k] = v
			}
			rec.DecisionCalls += a.Calls
			rec.DecisionRejected += a.Rejected
			rec.InputTokens += a.InputTokens
			if err != nil && first == nil {
				first = err
			}
		}()
	}
	wg.Wait()
	return out, first
}

// askAI shows the AI the lines (in order) and returns the ids it calls
// unrelated. The answer's first JSON object is read (a model may add prose
// after it); an answer without one is a failure.
func askAI(ctx context.Context, ai provider.Chatter, state string, ids []string, lines map[string]string, rec *store.PreprocessRecord) (map[string]bool, error) {
	var b strings.Builder
	ref := map[string]string{}
	for i, id := range ids {
		l := strings.TrimPrefix(lines[id], "- ")
		ref[fmt.Sprintf("L%d", i+1)] = id
		fmt.Fprintf(&b, "L%d %s\n", i+1, l)
	}
	rec.AICalls++
	r, err := ai.Chat(ctx, prepAIHead+"\n\n[대화]\n"+state+"\n\n[기억 줄]\n"+b.String())
	if err != nil {
		return nil, err
	}
	at := strings.Index(r.Content, "{")
	if at < 0 {
		return nil, errors.New("ai_answer_without_json")
	}
	var a struct {
		Unrelated []string `json:"unrelated"`
	}
	if err := json.NewDecoder(strings.NewReader(r.Content[at:])).Decode(&a); err != nil {
		return nil, errors.New("ai_answer_not_json")
	}
	out := map[string]bool{}
	for _, l := range a.Unrelated {
		if id, ok := ref[strings.TrimSpace(l)]; ok {
			out[id] = true
		}
	}
	return out, nil
}

// pickWide is the JEV choice (GXW): the basic delivery's records the model
// does not call unrelated (an unanswered one stays), then records of the
// wide list outside it that it calls related (answered, below 0.5), least
// unrelated first, each within its lane's ceiling counted in Go's sizes.
func pickWide(basic, wide store.Delivery, p map[string]float64, chars, secret int) (map[string]bool, []string) {
	size := map[string]int{}
	for _, r := range wide.Trace {
		if r.Result == "taken" {
			size[r.ID] = r.Size
		}
	}
	only := map[string]bool{}
	used := [2]int{}
	lane := func(id string) int {
		if wide.SecretIDs[id] || basic.SecretIDs[id] {
			return 1
		}
		return 0
	}
	pinned := map[string]bool{}
	for _, r := range basic.Trace {
		if r.Hold == store.HoldPin {
			pinned[r.ID] = true // taken regardless of the answer, in its budget
		}
	}
	inBasic := map[string]bool{}
	for _, id := range basic.IDs {
		inBasic[id] = true
		if v, ok := p[id]; ok && v >= 0.5 && !pinned[id] {
			continue
		}
		only[id] = true
		used[lane(id)] += size[id]
	}
	order := map[string]int{}
	var extra []string
	for i, id := range wide.IDs {
		order[id] = i
		if v, ok := p[id]; ok && v < 0.5 && !inBasic[id] {
			extra = append(extra, id)
		}
	}
	sort.SliceStable(extra, func(a, b int) bool {
		if p[extra[a]] != p[extra[b]] {
			return p[extra[a]] < p[extra[b]]
		}
		return order[extra[a]] < order[extra[b]]
	})
	ceiling := [2]int{chars, secret}
	var added []string
	for _, id := range extra {
		l := lane(id)
		if used[l]+size[id] <= ceiling[l] {
			used[l] += size[id]
			only[id] = true
			added = append(added, id)
		}
	}
	return only, added
}

// preprocess runs the request's mode over the basic delivery and returns the
// delivery to send with what was done. On any failure the basic delivery is
// returned unchanged.
func preprocess(ctx context.Context, p *prepDeps, db *store.DB, scope []memory.ScopedTurn, dr store.DeliveryRequest, basic store.Delivery, input string) (store.Delivery, *store.PreprocessRecord) {
	started := time.Now()
	rec := &store.PreprocessRecord{Mode: p.mode, Ran: "go"}
	defer func() { rec.MS = time.Since(started).Milliseconds() }()
	if len(basic.IDs) == 0 && p.mode != store.PreprocessJev {
		rec.Fallback = "nothing_to_choose"
		return basic, rec
	}
	ctx, cancel := context.WithTimeout(ctx, prepTimeout)
	defer cancel()
	state := prepState(input, scope, basic.Window)
	fail := func(code string, err error) (store.Delivery, *store.PreprocessRecord) {
		rec.Fallback, rec.Ran = code, "go"
		if err != nil {
			rec.Error = err.Error() // the provider package masks keys
		}
		return basic, rec
	}
	var p0 map[string]float64
	var ai map[string]bool
	var only map[string]bool
	var added []string // JEV additions, least unrelated first
	switch p.mode {
	case store.PreprocessJev:
		if p.decider == nil {
			return fail(p.decisionProblem, nil)
		}
		wr := dr
		wr.Chars, wr.Secret = prepWide, prepWide
		wide, err := db.Deliver(scope, wr)
		if err != nil {
			return fail("wide_list_failed", err)
		}
		// The basic delivery's lines are asked in a request of their own and
		// the rest of the wide list apart: asked within the wide list, 8 of
		// 193 basic lines fell on the other side of 0.5, while the same
		// request asked twice moved 1 (11-08); the measured GXW asked so.
		lines := deliveryLines(wide)
		inBasic := map[string]bool{}
		for _, id := range basic.IDs {
			inBasic[id] = true
		}
		var rest []string
		for _, id := range wide.IDs {
			if !inBasic[id] {
				rest = append(rest, id)
			}
		}
		for id, l := range deliveryLines(basic) {
			if lines[id] == "" {
				lines[id] = l
			}
		}
		var pb, pr map[string]float64
		var eb, er error
		var rb, rr store.PreprocessRecord
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); pb, eb = askUnrelated(ctx, p.decider, state, basic.IDs, lines, &rb) }()
		go func() { defer wg.Done(); pr, er = askUnrelated(ctx, p.decider, state, rest, lines, &rr) }()
		wg.Wait()
		rec.Asked, rec.DecisionCalls, rec.DecisionRejected, rec.InputTokens = rb.Asked+rr.Asked, rb.DecisionCalls+rr.DecisionCalls, rb.DecisionRejected+rr.DecisionRejected, rb.InputTokens+rr.InputTokens
		p0 = map[string]float64{}
		for k, v := range pr {
			p0[k] = v
		}
		for k, v := range pb {
			p0[k] = v
		}
		askErr := errors.Join(eb, er)
		if len(p0) == 0 {
			if rec.Asked == 0 && askErr == nil {
				return fail("nothing_to_choose", nil) // no line was asked: nothing failed
			}
			return fail("decision_failed", askErr)
		}
		if askErr != nil {
			rec.Error = askErr.Error() // partial: unanswered lines stay as Go chose
		}
		only, added = pickWide(basic, wide, p0, dr.Chars, dr.Secret)
		rec.Ran = store.PreprocessJev
	case store.PreprocessAI:
		if p.ai == nil {
			return fail(p.aiProblem, nil)
		}
		var err error
		if ai, err = askAI(ctx, p.ai, state, basic.IDs, deliveryLines(basic), rec); err != nil {
			return fail("ai_failed", err)
		}
		only = map[string]bool{}
		for _, id := range basic.IDs {
			if !ai[id] {
				only[id] = true
			}
		}
		rec.Ran = store.PreprocessAI
	case store.PreprocessAIJev:
		lines := deliveryLines(basic)
		var aiErr, decErr error
		var wg sync.WaitGroup
		if p.ai != nil {
			wg.Add(1)
			go func() { defer wg.Done(); ai, aiErr = askAI(ctx, p.ai, state, basic.IDs, lines, rec) }()
		} else {
			aiErr = errors.New(p.aiProblem)
		}
		if p.decider != nil {
			wg.Add(1)
			go func() {
				defer wg.Done()
				var r store.PreprocessRecord
				p0, decErr = askUnrelated(ctx, p.decider, state, basic.IDs, lines, &r)
				rec.Asked, rec.DecisionCalls, rec.DecisionRejected, rec.InputTokens = r.Asked, r.DecisionCalls, r.DecisionRejected, r.InputTokens
			}()
		} else {
			decErr = errors.New(p.decisionProblem)
		}
		wg.Wait()
		aiOK, decOK := aiErr == nil, len(p0) > 0
		switch {
		case !aiOK && !decOK:
			return fail("ai_and_decision_failed", errors.Join(aiErr, decErr))
		case aiOK && decOK:
			rec.Ran = store.PreprocessAIJev
		case aiOK:
			rec.Ran, rec.Fallback = store.PreprocessAI, "decision_failed"
		default:
			rec.Ran, rec.Fallback = store.PreprocessJev, "ai_failed"
		}
		if decErr != nil && decOK {
			rec.Error = decErr.Error()
		}
		only = map[string]bool{}
		for _, id := range basic.IDs {
			v, answered := p0[id]
			out := false
			switch rec.Ran {
			case store.PreprocessAIJev:
				out = ai[id] && answered && v >= 0.5
			case store.PreprocessAI:
				out = ai[id]
			default:
				out = answered && v >= 0.5
			}
			if !out {
				only[id] = true
			}
		}
	default:
		return fail("unknown_mode", nil)
	}
	d := basic
	changed := len(added) > 0
	for _, id := range basic.IDs {
		changed = changed || !only[id]
	}
	if changed {
		chosen := dr
		chosen.Only = only
		// Go renders the final set again (a basis quoted by a record left
		// out makes a line longer, a fact alone carries its event's
		// protection): when an addition chosen first does not fit while a
		// later one does, the later one is taken back (review 18 B-03)
		for round := 0; ; round++ {
			var err error
			if d, err = db.Deliver(scope, chosen); err != nil {
				return fail("final_delivery_failed", err)
			}
			result := map[string]string{}
			for _, r := range d.Trace {
				result[r.ID] = r.Result
			}
			first, last := -1, -1
			for i, id := range added {
				if !only[id] {
					continue
				}
				if result[id] != "taken" && first < 0 {
					first = i
				}
				if result[id] == "taken" {
					last = i
				}
			}
			if first < 0 || last <= first || round == 3 {
				break
			}
			delete(only, added[last])
		}
	}
	// what was left out and added, from the delivery sent
	sent := map[string]bool{}
	for _, id := range d.IDs {
		sent[id] = true
	}
	inBasic := map[string]bool{}
	for _, id := range basic.IDs {
		inBasic[id] = true
		if !sent[id] {
			rec.Removed++
		}
	}
	for _, id := range d.IDs {
		if !inBasic[id] {
			rec.Added++
		}
	}
	// the answers go with the delivery's trace (C21); the AI was asked of
	// the basic delivery's lines only
	askedAI := map[string]bool{}
	for _, id := range basic.IDs {
		askedAI[id] = true
	}
	for i := range d.Trace {
		id := d.Trace[i].ID
		if v, ok := p0[id]; ok {
			v := v
			d.Trace[i].Unrelated = &v
		}
		if ai != nil && askedAI[id] {
			u := ai[id]
			d.Trace[i].AIUnrelated = &u
		}
	}
	return d, rec
}
