package store

import (
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"

	"archivecenter2/goservice/internal/body"
	"archivecenter2/goservice/internal/ingest"
	"archivecenter2/goservice/internal/memory"
)

// DefaultWindow is how many latest turns a request is taken to hold when
// none of their rows are found in it and it does not say the roles of its
// messages (an older plugin); their memory is not delivered.
const DefaultWindow = 5

// BlockTitle opens the memory text's first line (not counted in the
// ceilings). The line ends with the fingerprint of the text after it, so the
// host adapter knows its own message again however the host copied it:
// "[Archive Center: records of earlier turns of this story | <h>]".
const BlockTitle = "[Archive Center: records of earlier turns of this story | "

// TextPrint is a text's fingerprint as the host adapter sends it.
type TextPrint struct {
	Len  int    `json:"len"`
	H    string `json:"h"`
	Role string `json:"role,omitempty"` // the message's role in the request, when sent
	Text string `json:"text,omitempty"` // retained host wire field; baseline selection uses fingerprints
}

// DeliveryRequest is what one request asks of the chat's memory.
type DeliveryRequest struct {
	Input       string      // the user's input of this request
	Payload     []TextPrint // fingerprints of the request's messages, in order
	Query       []float32   // the input's query vector; nil = order by recency only
	RecentQuery []float32   // the immediately preceding completed turn, embedded separately
	Model       string      // the embedding model of the stored vectors
	Chars       int         // ceiling of the memory text
	Secret      int         // separate ceiling of secret records (someone must not know or does not know)
	// Body is the body section (1.0 E09/E10, its own budget, already within
	// it): delivered after the records, kept as text like secret records.
	Body []string
	// Only, when set, is the records a preprocessing mode chose (E01~E03,
	// 11-07 plan step 2): the same order, ceilings and scene contrast, but a
	// candidate not in it is left out ("not_chosen"); a pinned record is
	// taken regardless. nil = every candidate, the Go basic delivery.
	Only map[string]bool
}

// Delivery is the memory a request gets.
type Delivery struct {
	Block            string   `json:"block"`                // empty: nothing to deliver
	IDs              []string `json:"ids"`                  // records, in order
	Chars            int      `json:"chars"`                // used of the memory ceiling
	SecretChars      int      `json:"secret_chars"`         // used of the secret ceiling
	BlockChars       int      `json:"block_chars"`          // length of the delivered text
	BodyChars        int      `json:"body_chars,omitempty"` // of the body section (its own budget)
	Window           int      `json:"window"`               // the latest turns the request holds
	WindowFrom       string   `json:"window_from"`          // "payload" (their rows found in it), "count" (its chat messages counted) or "default"
	Ranked           string   `json:"ranked"`               // "similarity+recency" or "recency"
	Candidates       int      `json:"candidates"`           // ranked records plus facts considered to complete delivered events
	RecentQueryUsed  bool     `json:"recent_query_used"`
	RecentQueryError error    `json:"-"` // optional ranking failed; input ranking remains usable
	// SecretIDs records the existing delivery lane decision for management
	// diagnostics, including protection carried through a view's basis.
	SecretIDs map[string]bool `json:"-"`
	// Transport representations are rendered only after selection.
	PDFBlock      string `json:"-"`
	RetainedBlock string `json:"-"`
	// Trace is every candidate in the order considered and what became of
	// it: the record of one delivery kept for checking it later (10-29).
	Trace []TraceRow `json:"-"`
	// Lines are the delivered records' rendered lines, one per IDs entry
	// (a record's own line breaks kept): what a preprocessing mode shows a
	// model for each record (review 18 B-02).
	Lines []string `json:"-"`
	// Preprocess is what a preprocessing mode did for this delivery (set by
	// the request handler; the HUD shows it)
	Preprocess *PreprocessRecord `json:"-"`
	// Narrative is what narrative support did (set by the request handler)
	Narrative *NarrativeRecord `json:"-"`
}

// TraceRow is one candidate of a delivery as it was considered.
type TraceRow struct {
	ID     string   `json:"id"`
	Turn   int      `json:"turn"` // the turn that wrote it
	Kind   string   `json:"kind"`
	Sim    *float64 `json:"sim,omitempty"`    // similarity to the request's query
	Rank   int      `json:"rank"`             // rank sum (similarity + recency)
	Parent string   `json:"parent,omitempty"` // a completing fact's event
	Secret bool     `json:"secret,omitempty"`
	Size   int      `json:"size,omitempty"` // what it counts against its ceiling
	// Result: taken, over_ceiling (did not fit), parent_not_taken,
	// contrast_removed, not_chosen (a preprocessing mode left it out)
	Result string `json:"result"`
	Hold   string `json:"hold,omitempty"` // C18 pin: taken first, unranked
	// preprocessing (E01~E03), when asked: the decision model's probability
	// that the record is unrelated to the scene, and the AI's answer
	Unrelated   *float64 `json:"unrelated,omitempty"`
	AIUnrelated *bool    `json:"ai_unrelated,omitempty"`
}

// candidate is a stored record considered for delivery.
type candidate struct {
	id  string
	pos int // its turn's place in the read scope
	mem *memory.KnownMemory
}

type renderedRecord struct {
	memory *memory.KnownMemory
	basis  string
	aware  []memory.KnownAwareness
}

// Deliver chooses the memory for a request (step 7). Generation's Select
// gives the candidate records; those of the turns the
// request already holds are left out. The order is the rank by similarity
// to the input and preceding completed turn, plus recency in story order;
// candidates are taken in that order while they fit the ceiling, secret
// records under their own ceiling. A delivered event can also bring current
// facts explicitly recorded at that event that Select did not find. Those
// facts do not change the ranked candidates or expand further connections.
// Each uses its own lane's ceiling; unused space adds no other connections.
func (d *DB) Deliver(scope []memory.ScopedTurn, req DeliveryRequest) (Delivery, error) {
	var out Delivery
	// finished turns: the input of this request has no reply yet
	n := len(scope)
	for n > 0 && len(scope[n-1].Source.Responses) == 0 {
		n--
	}
	// an attached past (C22, first in the scope) is another chat's turns:
	// never in this request, so the window counts the chat's own turns only
	past := 0
	for past < n && scope[past].Past {
		past++
	}
	own := n - past
	out.Window = heldTurns(scope[past:n], req.Payload[:priorEnd(req.Payload, req.Input)])
	out.WindowFrom = "payload"
	if out.Window == 0 {
		// no row found (the host changed the texts): the request's own chat
		// messages tell how many earlier turns it holds
		out.Window, out.WindowFrom = min(chatTurns(req.Payload), own), "count"
		if out.Window < 0 {
			out.Window, out.WindowFrom = min(DefaultWindow, own), "default"
		}
	}
	cut := n - out.Window // scope places from here on are in the request
	text := req.Input
	var earlier []string
	for i, t := range scope[:n] {
		if i >= cut {
			text += "\n" + memory.TurnText(t.Source)
		} else {
			earlier = append(earlier, memory.TurnText(t.Source))
		}
	}
	k, place, err := d.knownScope(scope, true)
	if err != nil {
		return out, err
	}
	sel := memory.Select(k, text, earlier)
	sup := memory.Superseded(k)
	// a fact later records changed in part is delivered with the slots
	// they changed, so its earlier values of those are not read as current:
	// Select sets them on what it selects; the pinned and completing records
	// come from k
	partly := memory.PartlyChanged(k)
	for i := range k.Memories {
		k.Memories[i].Changed = partly[k.Memories[i].ID]
	}
	// C18 flags (memory_hold.go): a muted memory is never delivered; a
	// pinned one is taken first, without Select or ranking
	scopeJobs := make([]int64, 0, len(place))
	for j := range place {
		scopeJobs = append(scopeJobs, j)
	}
	holds, err := d.holdsOf(scopeJobs)
	if err != nil {
		return out, err
	}
	var cands, pinned []candidate
	for i := range sel.Memories {
		m := &sel.Memories[i]
		// a value a later record replaced is not delivered (current values
		// only; measured 2026-10-25: Select's earlier values and current
		// values of events shown alone were 1 needed of 40 lines judged)
		if p := place[m.Job]; p < cut && !sup[m.ID] && holds[m.ID] == "" {
			cands = append(cands, candidate{id: m.ID, pos: p, mem: m})
		}
	}
	for i := range k.Memories {
		m := &k.Memories[i]
		if p := place[m.Job]; holds[m.ID] == HoldPin && p < cut && !sup[m.ID] {
			pinned = append(pinned, candidate{id: m.ID, pos: p, mem: m})
		}
	}
	sort.SliceStable(pinned, func(a, b int) bool { return pinned[a].pos > pinned[b].pos }) // the latest first
	out.Candidates = len(cands) + len(pinned)
	if len(cands) == 0 && len(pinned) == 0 {
		bodyOnly(&out, req.Body) // no record to deliver; the body section still goes
		return out, nil
	}
	// Facts missing from Select are completion candidates, not new roots.
	// Keep the original ranking population so finding more such facts cannot
	// change the relative order of existing candidates.
	ranked := map[string]bool{}
	events := map[string]bool{}
	for _, c := range cands {
		ranked[c.id] = true
		if c.mem.Kind == "event" {
			events[c.id] = true
		}
	}
	for _, c := range pinned {
		if c.mem.Kind == "event" {
			events[c.id] = true // a pinned event brings its facts as a ranked one does
		}
	}
	position := map[string]int{}
	for i, m := range k.Memories {
		position[m.ID] = i
	}
	parent := map[string]string{}
	var completion []candidate
	facts := memory.CurrentFacts(k)
	for i := range facts {
		m := &facts[i]
		at := strings.SplitN(m.At, ".", 2)[0]
		if events[at] && !ranked[m.ID] && holds[m.ID] == "" && place[m.Job] < cut {
			completion = append(completion, candidate{id: m.ID, pos: place[m.Job], mem: m})
			parent[m.ID] = at
		}
	}
	sim := map[string]float64{}
	var similarities []map[string]float64
	if len(req.Query) > 0 && len(cands)+len(completion) > 0 {
		docs := []string{}
		seen := map[string]bool{}
		for _, c := range cands {
			if !seen[c.id] {
				seen[c.id] = true
				docs = append(docs, c.id)
			}
		}
		for _, c := range completion {
			docs = append(docs, c.id)
		}
		if sim, err = d.similarities(req.Model, req.Query, docs); err != nil {
			return out, err
		}
		similarities = append(similarities, sim)
		if len(req.RecentQuery) > 0 {
			recent, err := d.similarities(req.Model, req.RecentQuery, docs)
			if err != nil {
				out.RecentQueryError = err
				log.Printf("memory delivery: recent turn similarity: %v", err)
			} else if len(recent) == 0 {
				out.RecentQueryError = fmt.Errorf("recent turn has no matching stored vectors")
			} else {
				similarities = append(similarities, recent)
			}
		}
	}
	out.Ranked = "recency"
	rank := map[string]int{}
	hasSimilarity := false
	for _, c := range cands {
		if _, ok := sim[c.id]; ok {
			hasSimilarity = true
			break
		}
	}
	if hasSimilarity {
		out.Ranked = "similarity+recency"
		out.RecentQueryUsed = len(similarities) > 1
		for _, similarity := range similarities {
			bySim := append([]candidate(nil), cands...)
			sort.SliceStable(bySim, func(a, b int) bool { return score(similarity, bySim[a].id) > score(similarity, bySim[b].id) })
			for i, c := range bySim {
				rank[c.id] += i
			}
		}
	}
	byAge := append([]candidate(nil), cands...)
	sort.SliceStable(byAge, func(a, b int) bool { return byAge[a].pos > byAge[b].pos })
	for i, c := range byAge {
		rank[c.id] += i
	}
	// Compare each completion with the original population, using exactly
	// the same similarity and recency ordering, including storage-order ties.
	// Completions do not rank one another or renumber the original records.
	for _, f := range completion {
		for _, c := range cands {
			earlier := position[c.id] < position[f.id]
			if hasSimilarity {
				for _, similarity := range similarities {
					if score(similarity, c.id) > score(similarity, f.id) || (score(similarity, c.id) == score(similarity, f.id) && earlier) {
						rank[f.id]++
					}
				}
			}
			if c.pos > f.pos || (c.pos == f.pos && earlier) {
				rank[f.id]++
			}
		}
		// Its source event is considered first; a skipped event adds no facts.
		rank[f.id] = max(rank[f.id], rank[parent[f.id]])
	}
	order := append([]candidate(nil), cands...)
	order = append(order, completion...)
	sort.SliceStable(order, func(a, b int) bool { return rank[order[a].id] < rank[order[b].id] })
	order = append(pinned, order...)

	aware := map[string][]memory.KnownAwareness{}
	secret := map[string]bool{}
	for _, a := range k.Awareness {
		aware[a.Target] = append(aware[a.Target], a)
		if a.Kind == "hide_from" || a.Kind == "unaware" {
			secret[a.Target] = true
		}
	}
	names := map[string]string{}
	for _, nm := range k.Names {
		names[nm.ID] = nm.Name
	}
	// what a view's basis can point to: a record, or a value an event set
	bodies := map[string]string{}
	// the records whose protection goes with a text: a record's line shows
	// its effects too; an effect's text shows its event's body
	carried := map[string][]string{}
	for _, m := range k.Memories {
		if holds[m.ID] == HoldMute {
			continue // its text is not shown as another line's basis either
		}
		bodies[m.ID] = m.Body
		carried[m.ID] = []string{m.ID}
		for i, e := range m.Effects {
			ref := m.ID + "." + strconv.Itoa(i+1)
			v := strings.TrimSpace(strings.TrimSpace(e.Slot+": "+e.Before+" -> "+e.Value) + " " + e.Status)
			bodies[ref] = m.Body + " [" + v + "]"
			carried[m.ID] = append(carried[m.ID], ref)
			carried[ref] = []string{m.ID, ref}
		}
	}
	protect := func(ids []string, own []memory.KnownAwareness, isSecret bool) ([]memory.KnownAwareness, bool) {
		for _, id := range ids {
			own = append(own, aware[id]...)
			isSecret = isSecret || secret[id]
		}
		return own, isSecret
	}
	var lines []string
	var rendered []renderedRecord
	// a basis text shown once gets a mark (B1, B2, ...); later views on the
	// same basis point to the mark instead of repeating the text
	marks := map[string]string{}
	lastSize, lastSecret := 0, false
	// a completing fact delivered without its event (the event was not
	// chosen) keeps that event's protection on its own line (review 17 A-01)
	alone := map[string]string{}
	take := func(c candidate) bool {
		// a line carries the awareness of every record whose text it shows
		// (its own effects, the record its basis points to), and is a
		// secret when one of them is
		own, isSecret := protect(carried[c.id], nil, false)
		if at := alone[c.id]; at != "" {
			own, isSecret = protect(carried[at], own, isSecret)
		}
		basis, newMark := "", ""
		if bs := c.mem.Basis; bs != nil && bodies[bs.Ref] != "" {
			own, isSecret = protect(carried[bs.Ref], own, isSecret)
			if mk, ok := marks[bs.Ref]; ok {
				basis = "based on " + mk
			} else {
				newMark = "B" + strconv.Itoa(len(marks)+1)
				basis = "based on " + newMark + " \"" + bodies[bs.Ref] + "\""
			}
		}
		line := deliveryLine(*c.mem, names, basis, own)
		full := memory.RenderKnown(memory.Selection{Memories: []memory.KnownMemory{*c.mem}, Awareness: own}, sup)
		size := recordSize(full, line)
		lastSize, lastSecret = size, isSecret
		if isSecret {
			if out.SecretChars+size > req.Secret {
				return false
			}
			out.SecretChars += size
		} else {
			if out.Chars+size > req.Chars {
				return false
			}
			out.Chars += size
		}
		if newMark != "" {
			marks[c.mem.Basis.Ref] = newMark
		}
		lines = append(lines, line)
		rendered = append(rendered, renderedRecord{c.mem, basis, own})
		out.IDs = append(out.IDs, c.id)
		if isSecret {
			if out.SecretIDs == nil {
				out.SecretIDs = map[string]bool{}
			}
			out.SecretIDs[c.id] = true
		}
		return true
	}
	delivered, notChosen := map[string]bool{}, map[string]bool{}
	for _, c := range order {
		row := TraceRow{ID: c.id, Turn: c.mem.Turn, Kind: c.mem.Kind, Rank: rank[c.id], Parent: parent[c.id], Hold: holds[c.id]}
		if v, ok := sim[c.id]; ok {
			row.Sim = &v
		}
		if at := parent[c.id]; at != "" {
			// with a chosen set, an event left out only because it was not
			// chosen (the wide list showed its fact alone after scene
			// contrast took the event) does not hold back its chosen fact
			if !delivered[at] && !notChosen[at] {
				row.Result = "parent_not_taken"
				out.Trace = append(out.Trace, row)
				continue
			}
			if !delivered[at] {
				alone[c.id] = at
			}
			out.Candidates++
		}
		if req.Only != nil && !req.Only[c.id] && holds[c.id] != HoldPin {
			notChosen[c.id] = true
			row.Result = "not_chosen"
			out.Trace = append(out.Trace, row)
			continue
		}
		delivered[c.id] = take(c)
		row.Size, row.Secret, row.Result = lastSize, lastSecret, "over_ceiling"
		if delivered[c.id] {
			row.Result = "taken"
		}
		out.Trace = append(out.Trace, row)
	}
	// Freeze the baseline selection; removal never fills freed capacity.
	contrast, err := d.sceneContrast(k, req)
	if err != nil {
		log.Printf("scene contrast skipped: %v", err)
		contrast = nil // Discard partial scores and render the frozen baseline.
	}
	if len(contrast) > 0 {
		originalRendered, originalIDs := rendered, out.IDs
		lines, rendered, out.IDs = nil, nil, nil
		marks, out.SecretIDs = map[string]string{}, nil
		out.Chars, out.SecretChars = 0, 0
		results := map[string]string{}
		traceIndex := map[string]int{}
		for i, row := range out.Trace {
			traceIndex[row.ID] = i
		}
		removed := func(id string) bool {
			value, present := contrast[id]
			return present && value <= 0 && holds[id] != HoldPin
		}
		for i, r := range originalRendered {
			id := originalIDs[i]
			if removed(id) {
				results[id] = "contrast_removed"
				continue
			}
			// a completing fact whose event contrast removed is now shown
			// without it: it keeps the event's protection (review 17 A-01)
			if at := parent[id]; at != "" && removed(at) {
				alone[id] = at
			}
			// A removed basis definition can make this row longer. Skip only
			// that row when it no longer fits and continue the frozen list.
			results[id] = "over_ceiling"
			if take(candidate{id: id, mem: r.memory}) {
				results[id] = "taken"
			}
			if i, ok := traceIndex[id]; ok {
				out.Trace[i].Size, out.Trace[i].Secret = lastSize, lastSecret
			}
		}
		for i := range out.Trace {
			if result, ok := results[out.Trace[i].ID]; ok {
				out.Trace[i].Result = result
			}
		}
	}
	shareDeliveryBasis(lines, out.IDs, rendered, marks, names)
	out.Lines = append([]string(nil), lines...)
	var bodyLines []string
	if len(req.Body) > 0 {
		bodyLines = append([]string{body.Header}, req.Body...)
		for _, l := range bodyLines {
			out.BodyChars += len([]rune(l)) + 1
		}
	}
	if all := append(append([]string(nil), lines...), bodyLines...); len(all) > 0 {
		body := strings.Join(all, "\n") + "\n"
		out.Block = BlockTitle + textPrint(body).H + "]\n" + body
		out.BlockChars = len([]rune(out.Block))
		// Split the already selected, rendered lines verbatim. Basis marks
		// resolve across the PDF and retained text together; neither lane
		// repeats definitions or spends additional memory budget.
		lanes := [2][]string{}
		for i, id := range out.IDs {
			lane := 0
			if out.SecretIDs[id] {
				lane = 1
			}
			lanes[lane] = append(lanes[lane], lines[i])
		}
		lanes[1] = append(lanes[1], bodyLines...)
		out.PDFBlock, out.RetainedBlock = deliveryBlock(lanes[0]), deliveryBlock(lanes[1])
	}
	return out, nil
}

// Selection is finished. Share only a whole record's exact body, preserving
// every record and qualifier without refilling the saved space. If its source
// precedes the defining view, move that same quotation into the source marker.
// Effect references remain distinct from whole-record IDs.
func shareDeliveryBasis(lines []string, ids []string, rendered []renderedRecord, marks, names map[string]string) {
	definitions := map[string]int{}
	for i, r := range rendered {
		if bs := r.memory.Basis; bs != nil {
			if mark := marks[bs.Ref]; mark != "" && strings.HasPrefix(r.basis, "based on "+mark+" \"") {
				definitions[bs.Ref] = i
			}
		}
	}
	bodies, bases := map[int]string{}, map[int]string{}
	for i, id := range ids {
		if mark, ok := marks[id]; ok {
			r := rendered[i]
			m := *r.memory
			m.Body = "[text " + mark + "]"
			line := deliveryLine(m, names, r.basis, r.aware)
			if len([]rune(line)) < len([]rune(lines[i])) {
				bodies[i] = m.Body
				if j, ok := definitions[id]; ok && i < j {
					bodies[i] = "[text " + mark + "=\"" + r.memory.Body + "\"]"
					bases[j] = "based on " + mark
				}
			}
		}
	}
	for i, r := range rendered {
		body, share := bodies[i]
		basis, rebase := bases[i]
		if !share && !rebase {
			continue
		}
		m := *r.memory
		if share {
			m.Body = body
		}
		if !rebase {
			basis = r.basis
		}
		lines[i] = deliveryLine(m, names, basis, r.aware)
	}
}

// GuideTitle opens a narrative guide message (E03); like the memory block it
// carries the fingerprint of the text after it, so the plugin knows its own
// message when the host hands a request back again.
const GuideTitle = "[Archive Center: narrative guide | "

// GuideMessage is a guide text as the message sent ("" stays "").
func GuideMessage(text string) string {
	if text == "" {
		return ""
	}
	body := text + "\n"
	return GuideTitle + textPrint(body).H + "]\n" + body
}

func deliveryBlock(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	body := strings.Join(lines, "\n") + "\n"
	return BlockTitle + textPrint(body).H + "]\n" + body
}

// recordSize is what a candidate counts against its ceiling. The ceilings
// were measured in the full record format (ids and fields, 2026-10-25);
// the line sent (and its line break) is counted when it is longer, so the
// delivered lines never pass the ceiling.
func recordSize(full, line string) int {
	return max(len([]rune(full)), len([]rune(line))+1)
}

// heldTurns counts the latest finished turns the request holds: from the
// end, each turn's rows are looked for before the place where the next
// turn was found, so one repeated text ("네") stands for one turn only.
// priorEnd is where the request's messages before this input end: the
// latest user message (with roles), else the latest message with the
// input's text; the earlier turns are looked for before it only.
func priorEnd(payload []TextPrint, input string) int {
	for i := len(payload) - 1; i >= 0; i-- {
		if payload[i].Role == "user" {
			return i
		}
	}
	in := textPrint(input)
	for i := len(payload) - 1; i >= 0; i-- {
		if payload[i].Len == in.Len && payload[i].H == in.H {
			return i
		}
	}
	return len(payload)
}

// chatTurns counts the earlier turns a request's messages hold: its user
// messages before the latest one. -1 when the request does not say roles.
func chatTurns(payload []TextPrint) int {
	users, roles := 0, false
	for _, m := range payload {
		if m.Role != "" {
			roles = true
		}
		if m.Role == "user" {
			users++
		}
	}
	if !roles {
		return -1
	}
	return max(users-1, 0)
}

func heldTurns(done []memory.ScopedTurn, payload []TextPrint) int {
	p := len(payload)
	held := 0
	for i := len(done) - 1; i >= 0; i-- {
		// each of the turn's rows stands for one payload message
		rows := map[TextPrint]int{}
		for _, m := range done[i].Source.Members {
			rows[textPrint(m.Data)]++
		}
		for _, r := range done[i].Source.Responses {
			rows[textPrint(r.Data)]++
		}
		at := func(j int) TextPrint { return TextPrint{Len: payload[j].Len, H: payload[j].H} }
		j := p - 1
		for j >= 0 && rows[at(j)] == 0 {
			j--
		}
		if j < 0 {
			break
		}
		rows[at(j)]--
		// the turn's other rows next to the one found are its own
		for j > 0 && rows[at(j-1)] > 0 {
			j--
			rows[at(j)]--
		}
		p = j
		held++
	}
	return held
}

// turnJobs gives the current jobs a read scope's turn reads, and their
// source: the parent's memory of a branch's copy; the branch's own when the
// parent has not made it (the worker made it as the branch's).
func (d *DB) turnJobs(t memory.ScopedTurn) ([]int64, memory.Source, error) {
	src := t.Source
	ids, err := d.CurrentJobs([]memory.Source{src})
	if err != nil || !t.Inherited || len(ids) > 0 {
		return ids, src, err
	}
	src = t.OwnSource()
	ids, err = d.CurrentJobs([]memory.Source{src})
	return ids, src, err
}

// knownScope reads the current memory of a read scope and the place in the
// scope of each job's turn (the scope's story order: a branch's copied
// parent turns keep the parent's turn numbers). With story, every item's
// turn is its place in that order (from 1), so Select orders the states of
// a branch as the branch reads them.
func (d *DB) knownScope(scope []memory.ScopedTurn, story bool) (memory.Known, map[int64]int, error) {
	sources := make([]memory.Source, 0, len(scope)*2)
	indices := make([]int, len(scope))
	for i, t := range scope {
		indices[i] = len(sources)
		sources = append(sources, t.Source)
		if t.Inherited {
			sources = append(sources, t.OwnSource())
		}
	}
	ids, err := d.jobsForSources(sources)
	if err != nil {
		return memory.Known{}, nil, err
	}
	var jobs []int64
	var first int64
	place := map[int64]int{}
	dates := map[int64]string{}
	for i, t := range scope {
		src := t.Source
		id := ids[indices[i]]
		if t.Inherited && id == 0 {
			// the parent's memory of the copy; the branch's own when the
			// parent has not made it (the worker made it as the branch's)
			src = t.OwnSource()
			id = ids[indices[i]+1]
		}
		if id != 0 {
			if i == 0 {
				first = id
			}
			jobs = append(jobs, id)
			place[id] = i
			dates[id] = memory.DateText(src.Dates)
		}
	}
	var at map[int64]int
	if story {
		at = map[int64]int{}
		for j, p := range place {
			at[j] = p + 1
		}
	}
	k, err := d.knownOfAt(jobs, at, dates, first)
	if err != nil {
		return k, nil, err
	}
	return k, place, nil
}

// score is a record's similarity; one without a stored vector comes last.
func score(sim map[string]float64, id string) float64 {
	if v, ok := sim[id]; ok {
		return v
	}
	return -2
}

func textPrint(s string) TextPrint {
	n, h := ingest.Fingerprint(s)
	return TextPrint{Len: n, H: h}
}

// deliveryLine is one record as the request reads it: when it was recorded
// (turn, story date), its text with names instead of ids, how a person
// holds a view (basis and the record it rests on, feeling, whether it
// differs from the facts), who
// knows, does not know, must not be told or may be told.
func deliveryLine(m memory.KnownMemory, names map[string]string, basis string, aware []memory.KnownAwareness) string {
	name := func(id string) string {
		if n, ok := names[id]; ok {
			return n
		}
		return id
	}
	var b strings.Builder
	fmt.Fprintf(&b, "- (T%d", m.Turn)
	if m.Date != "" {
		fmt.Fprintf(&b, ", %s", m.Date)
	}
	b.WriteString(") " + memory.DocText(m, names))
	if bs := m.Basis; bs != nil {
		var parts []string
		if bs.Via != "" {
			parts = append(parts, bs.Via)
		}
		if bs.From != "" {
			parts = append(parts, "from "+name(bs.From))
		}
		if bs.Confidence != "" {
			parts = append(parts, "confidence "+bs.Confidence)
		}
		if basis != "" {
			parts = append(parts, basis)
		}
		if len(parts) > 0 {
			b.WriteString(" | basis " + strings.Join(parts, ", "))
		}
	}
	if a := m.Affect; a != nil && strings.TrimSpace(a.Intensity+a.Persistence) != "" {
		b.WriteString(" | feeling " + strings.TrimSpace(a.Intensity+" "+a.Persistence))
	}
	if m.Expressed != "" {
		b.WriteString(" | expressed " + m.Expressed)
	}
	if m.Differs != "" {
		b.WriteString(" | differs from the facts: " + m.Differs)
	}
	if len(m.Changed) > 0 {
		b.WriteString(" | changed later: " + strings.Join(m.Changed, ", "))
	}
	for _, a := range aware {
		fmt.Fprintf(&b, " | %s: %s", strings.ReplaceAll(a.Kind, "_", " "), name(a.Who))
		if a.Via != "" {
			fmt.Fprintf(&b, " (%s", a.Via)
			if a.From != "" {
				fmt.Fprintf(&b, ", from %s", name(a.From))
			}
			b.WriteString(")")
		}
		if a.Extent != "" {
			fmt.Fprintf(&b, " %s", a.Extent)
		}
		if a.Part != "" {
			fmt.Fprintf(&b, " %q", a.Part)
		}
	}
	return b.String()
}

// similarities are the cosine similarities of the records' stored vectors
// to the query vector, computed in MariaDB.
func (d *DB) similarities(model string, query []float32, ids []string) (map[string]float64, error) {
	out := map[string]float64{}
	parts := make([]string, len(query))
	for i, v := range query {
		parts[i] = strconv.FormatFloat(float64(v), 'g', -1, 32)
	}
	q := "[" + strings.Join(parts, ",") + "]"
	for start := 0; start < len(ids); start += 500 {
		part := ids[start:min(start+500, len(ids))]
		args := []any{q, model}
		for _, id := range part {
			args = append(args, strings.TrimPrefix(id, "M"))
		}
		stmt := fmt.Sprintf("SELECT m.id, 1 - VEC_DISTANCE_COSINE(e.`vector`, VEC_FromText(?)) FROM memories m JOIN embedding_vectors_%d e ON e.model=? AND e.text_hash=m.doc_hash WHERE m.id IN (", len(query)) +
			strings.TrimSuffix(strings.Repeat("?,", len(part)), ",") + ")"
		rows, err := d.sql.Query(stmt, args...)
		if err != nil {
			if strings.Contains(err.Error(), "doesn't exist") {
				return out, nil // no vectors of this size stored yet
			}
			return nil, err
		}
		for rows.Next() {
			var id int64
			var s float64
			if err := rows.Scan(&id, &s); err != nil {
				rows.Close()
				return nil, err
			}
			out["M"+strconv.FormatInt(id, 10)] = s
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

// bodyOnly delivers the body section alone, kept as text.
func bodyOnly(out *Delivery, lines []string) {
	if len(lines) == 0 {
		return
	}
	all := append([]string{body.Header}, lines...)
	for _, l := range all {
		out.BodyChars += len([]rune(l)) + 1
	}
	out.Block = deliveryBlock(all)
	out.BlockChars = len([]rune(out.Block))
	out.RetainedBlock = out.Block
}
