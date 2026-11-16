package store

import (
	"database/sql"
	"encoding/binary"
	"fmt"
	"log"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

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

// RequestPrefix compares a main-model request with the chat's previous one
// from the start (1.1 A2): a provider's prompt cache reuses only an equal
// beginning, so a beginning that changes every turn is a cache missed
// every turn. Compared are the host's messages as sent to Archive Center,
// before its own memory lines go in.
type RequestPrefix struct {
	Same      int `json:"same"`       // messages equal from the start
	Total     int `json:"total"`      // messages in this request
	PrevTotal int `json:"prev_total"` // messages in the previous request
	SameChars int `json:"same_chars"` // chars of the equal beginning
	Chars     int `json:"chars"`      // chars of this request
}

// ComparePrefix counts the messages equal from the start (length, hash and
// role), and their chars.
func ComparePrefix(prev, cur []TextPrint) RequestPrefix {
	out := RequestPrefix{Total: len(cur), PrevTotal: len(prev)}
	same := true
	for i, m := range cur {
		out.Chars += m.Len
		if same && i < len(prev) && prev[i].Len == m.Len && prev[i].H == m.H && prev[i].Role == m.Role {
			out.Same++
			out.SameChars += m.Len
		} else {
			same = false
		}
	}
	return out
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
	BodyChars        int      `json:"body_chars,omitempty"` // of the body sections (facts and tendencies, each its own ceiling)
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
	// model for each record.
	Lines []string `json:"-"`
	// Whole are the same lines before a shared basis is cut out of them: a
	// record whose body another line quotes keeps its own body here
	// ("event [text B1]" in Lines is the whole event). The decision model is
	// asked of one line at a time from Whole; asked of the stub, it called a
	// needed secret event unrelated in every run (S-026 M6, 0.72-0.81; 0.24
	// asked whole, 11-16). A view pointing to a mark another view defined
	// shows "based on B1" in both.
	Whole []string `json:"-"`
	// Names are the name relation lines (an alias, a true name) whose name
	// the scene uses, in the room the records left and shown first, counted
	// in Chars (1.2 #6); SecretNames those kept from someone, counted in
	// SecretChars. Not records, so not in IDs or Lines.
	Names       []string `json:"-"`
	SecretNames []string `json:"-"`
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
	// Why: the reason a record is in the general lane (Why* in
	// deliver_general.go), or "secret" for the secret lane's single rank;
	// empty for a pinned record and for a delivery without the general lane
	Why string `json:"why,omitempty"`
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
	kept := memory.KeptEvents(k)
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
	// (no early return when Select found nothing: the general lane's
	// anchors may still reach records, 1.2)
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
	order = enteringFirst(order, k, scope[cut:n], req.Input, parent)
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
		// a muted record's text is not shown as another line's basis either;
		// its protection still goes with the texts recorded at it
		muted := holds[m.ID] == HoldMute
		if !muted {
			bodies[m.ID] = m.Body
		}
		carried[m.ID] = []string{m.ID}
		for i, e := range m.Effects {
			ref := m.ID + "." + strconv.Itoa(i+1)
			if !muted {
				v := strings.TrimSpace(strings.TrimSpace(e.Slot+": "+e.Before+" -> "+e.Value) + " " + e.Status)
				bodies[ref] = m.Body + " [" + v + "]"
			}
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
	// chosen) keeps that event's protection on its own line
	alone := map[string]string{}
	// what a record was recorded at (an event, or one value it set): its
	// text shown away from that line (a basis quote, a pinned fact taken
	// before any event) keeps that protection, along the whole chain of
	// records recorded at records
	recordedAt := map[string]string{}
	for _, m := range k.Memories {
		if m.At != "" {
			recordedAt[m.ID] = m.At
		}
	}
	protectAt := func(id string, own []memory.KnownAwareness, isSecret bool) ([]memory.KnownAwareness, bool) {
		seen := map[string]bool{}
		for at := recordedAt[id]; at != "" && !seen[at]; at = recordedAt[strings.SplitN(at, ".", 2)[0]] {
			seen[at] = true
			own, isSecret = protect(carried[at], own, isSecret)
		}
		return own, isSecret
	}
	// 1.2: the general lane takes generalOrder (deliver_general.go); the
	// secret lane keeps the single rank. Both read whether a line is a
	// secret with take's own rule below (secretAsShown), so a record is
	// never placed in one lane and shown in the other.
	singleRank := order
	// secretAsShown is take's rule for a record counted in the secret lane:
	// its own records' awareness, the record its basis points to, or the
	// event it completes (a completing fact, as when that event is not
	// delivered). A record only recorded at a secret event is not a secret
	// itself: it is shown with that protection but counted in the general
	// lane (take below).
	secretAsShown := func(m *memory.KnownMemory, completes string) bool {
		_, s := protect(carried[m.ID], nil, false)
		if completes != "" {
			_, s = protect(carried[completes], nil, s)
		}
		if bs := m.Basis; bs != nil && bodies[bs.Ref] != "" {
			_, s = protect(carried[bs.Ref], nil, s)
			_, s = protectAt(strings.SplitN(bs.Ref, ".", 2)[0], nil, s)
		}
		return s
	}
	selected := make(map[string]bool, len(cands))
	for _, c := range cands {
		selected[c.id] = true
	}
	current := make(map[string]bool, len(facts))
	for i := range facts {
		current[facts[i].ID] = true
	}
	general, lanes, generalWhy, generalSim, generalSum, anchors, err := generalOrder(k, place, scope, cut, n, req.Input, sup, holds, selected, current,
		func(m *memory.KnownMemory) bool { return secretAsShown(m, "") },
		func(ids []string) (map[string]float64, []float64, int, error) {
			if len(req.Query) == 0 {
				return map[string]float64{}, nil, 0, nil
			}
			return d.similaritiesWithSum(req.Model, req.Query, ids)
		})
	if err != nil {
		return out, err
	}
	isPinned := map[string]bool{}
	for _, c := range pinned {
		isPinned[c.id] = true
	}
	// A general event brings, right after it, the current facts recorded at
	// it that the general order does not hold (as the single rank does for
	// its events); one shown as a secret stays with the secret lane.
	inGeneral := map[string]bool{}
	generalEvent := map[string]bool{}
	for _, c := range general {
		inGeneral[c.id] = true
		if c.mem.Kind == "event" {
			generalEvent[c.id] = true
		}
	}
	completes := map[string][]candidate{}
	for i := range facts {
		m := &facts[i]
		at := strings.SplitN(m.At, ".", 2)[0]
		if !generalEvent[at] || inGeneral[m.ID] || isPinned[m.ID] || holds[m.ID] != "" || place[m.Job] >= cut || secretAsShown(m, at) {
			continue
		}
		completes[at] = append(completes[at], candidate{id: m.ID, pos: place[m.Job], mem: m})
	}
	placed := map[string]bool{}
	order = append([]candidate(nil), pinned...)
	for i, c := range general {
		if isPinned[c.id] {
			continue
		}
		order = append(order, c)
		placed[c.id] = true
		delete(parent, c.id) // an independent general record completes nothing
		rank[c.id] = lanes[c.id]*10000 + i
		if _, ok := sim[c.id]; !ok {
			if v, ok := generalSim[c.id]; ok {
				sim[c.id] = v
			}
		}
		if c.mem.Kind != "event" {
			continue
		}
		for _, f := range completes[c.id] {
			if placed[f.id] {
				continue
			}
			order = append(order, f)
			placed[f.id] = true
			parent[f.id] = c.id
			rank[f.id] = rank[c.id]
			generalWhy[f.id] = WhyWithEvent
		}
	}
	// then the secret lane's records in the single rank (after the general
	// ones, so a completing secret fact comes after a general event it
	// completes, and a basis source delivered in the general lane is defined
	// once, on its own line)
	for _, c := range singleRank {
		if !isPinned[c.id] && !placed[c.id] && secretAsShown(c.mem, parent[c.id]) {
			order = append(order, c)
			if generalWhy == nil {
				generalWhy = map[string]string{}
			}
			generalWhy[c.id] = WhySecret
		}
	}
	if len(order) == 0 {
		bodyOnly(&out, req.Body) // no record to deliver; the body section still goes
		return out, nil
	}

	// a secret view whose basis is a record already shown on a general line
	// refers to that line's text (1.2): the definition goes on the source
	// line, its few added chars counted in the general lane, instead of the
	// whole text quoted again on the secret line
	generalShown := map[string]bool{}
	sourceDef := map[string]string{}
	// the records counted in the secret lane (a line shown as a secret may
	// be counted in the general lane: a record only recorded at a secret)
	secretCounted := map[string]bool{}
	take := func(c candidate) bool {
		// a line carries the awareness of every record whose text it shows
		// (its own effects, the record its basis points to), and is a
		// secret when one of them is
		own, isSecret := protect(carried[c.id], nil, false)
		inherited := false
		if at := alone[c.id]; at != "" {
			own, isSecret = protect(carried[at], own, isSecret)
		} else if holds[c.id] == HoldPin {
			own, isSecret = protectAt(c.id, own, isSecret)
		} else if parent[c.id] == "" {
			// an independent record shown away from the record it was
			// recorded at keeps that protection (1.2: an independent fact
			// recorded at a secret event was shown without it). It is shown
			// as a secret but counted in the general lane, where it was
			// chosen: it is not a secret itself.
			own, inherited = protectAt(c.id, own, false)
		}
		basis, newMark, sourceAdded := "", "", 0
		if bs := c.mem.Basis; bs != nil && bodies[bs.Ref] != "" {
			own, isSecret = protect(carried[bs.Ref], own, isSecret)
			own, isSecret = protectAt(strings.SplitN(bs.Ref, ".", 2)[0], own, isSecret)
			if mk, ok := marks[bs.Ref]; ok {
				basis = "based on " + mk
			} else {
				newMark = "B" + strconv.Itoa(len(marks)+1)
				basis = "based on " + newMark + " \"" + bodies[bs.Ref] + "\""
				if (isSecret || inherited) && generalShown[bs.Ref] {
					if added := len([]rune("[text " + newMark + "=\"\"]")); out.Chars+added <= req.Chars {
						basis, sourceAdded = "based on "+newMark, added
					}
				}
			}
		}
		own = distinctAwareness(own)
		shown := currentValues(*c.mem, kept, sup)
		line := deliveryLine(shown, names, basis, own)
		full := memory.RenderKnown(memory.Selection{Memories: []memory.KnownMemory{shown}, Awareness: own}, sup)
		size := lineSize(line) // the general lane counts the line sent
		if isSecret {
			size = recordSize(full, line)
		}
		if !isSecret && sourceAdded > 0 && out.Chars+size+sourceAdded > req.Chars {
			// the line and the chars it adds to its source line are counted
			// together in the general lane; when both do not fit, the basis
			// is quoted on this line as before
			basis = "based on " + newMark + " \"" + bodies[c.mem.Basis.Ref] + "\""
			sourceAdded = 0
			line = deliveryLine(shown, names, basis, own)
			full = memory.RenderKnown(memory.Selection{Memories: []memory.KnownMemory{shown}, Awareness: own}, sup)
			size = lineSize(line)
		}
		shownSecret := isSecret || inherited
		lastSize, lastSecret = size, shownSecret
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
		if sourceAdded > 0 {
			sourceDef[c.mem.Basis.Ref] = newMark
			out.Chars += sourceAdded
		}
		if !shownSecret {
			generalShown[c.id] = true
		}
		if isSecret {
			secretCounted[c.id] = true
		}
		lines = append(lines, line)
		rendered = append(rendered, renderedRecord{&shown, basis, own})
		out.IDs = append(out.IDs, c.id)
		if shownSecret {
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
		if !isPinned[c.id] {
			row.Why = generalWhy[c.id]
		}
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
	// The contrast is centered on the records this request can deliver (the
	// candidates, outside the recent window), not every memory of the chat
	// (1.1 B2): centered on the whole chat, right
	// after the recent window its memories pulled the mean toward the scene
	// and every baseline pick scored <=0, so the request got no memory.
	// The single rank's records keep a contrast centered on that rank's
	// population (1.2: the secret lane chooses as before); records only the
	// general order reached take theirs from the delivered order's population.
	pool := memory.Known{Memories: make([]memory.KnownMemory, 0, len(singleRank))}
	for _, c := range singleRank {
		pool.Memories = append(pool.Memories, *c.mem)
	}
	contrast, err := d.sceneContrast(pool, req)
	if err == nil && len(contrast) > 0 && len(order) > 1 && len(generalSum.sum) == len(req.Query) {
		// the delivered order's center: the general records' unit vectors
		// were summed when their similarities were read; the rest of the
		// order (pinned, completing facts, the secret lane) is read here
		inPool, inRank := map[string]bool{}, map[string]bool{}
		for _, c := range general {
			inPool[c.id] = true
		}
		for _, c := range singleRank {
			inRank[c.id] = true
		}
		var rest, scored []string
		for _, c := range order {
			if !inPool[c.id] {
				rest = append(rest, c.id)
			}
			if !inRank[c.id] {
				scored = append(scored, c.id)
			}
		}
		sum, count := append([]float64(nil), generalSum.sum...), generalSum.count
		moreErr := d.unitVectors(req.Model, len(req.Query), rest, func(_ string, v []float64) {
			count++
			for i := range sum {
				sum[i] += v[i]
			}
		})
		var more map[string]float64
		if moreErr == nil {
			more, moreErr = d.contrastAgainst(req, sum, count, scored)
		}
		if moreErr != nil {
			log.Printf("scene contrast of the general order skipped: %v", moreErr)
		} else {
			for id, v := range more {
				if _, ok := contrast[id]; !ok {
					contrast[id] = v
				}
			}
		}
	}
	if err != nil {
		log.Printf("scene contrast skipped: %v", err)
		contrast = nil // Discard partial scores and render the frozen baseline.
	}
	if len(contrast) > 0 {
		originalRendered, originalIDs := rendered, out.IDs
		lines, rendered, out.IDs = nil, nil, nil
		marks, out.SecretIDs = map[string]string{}, nil
		generalShown, sourceDef, secretCounted = map[string]bool{}, map[string]string{}, map[string]bool{}
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
			// without it: it keeps the event's protection
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
	out.Whole = append([]string(nil), lines...)
	shareDeliveryBasis(lines, out.IDs, rendered, marks, names, secretCounted, sourceDef)
	out.Lines = append([]string(nil), lines...)
	// the name relations whose name the scene uses take the room the
	// records left in their lane (they never push a record out: measured
	// 11-15, put first they pushed out L-140's needed old memory); shown
	// first in the text
	generalNames, secretNames := aliasLines(k, anchors, aware, secret)
	for _, l := range generalNames {
		if size := len([]rune(l)) + 1; out.Chars+size <= req.Chars {
			out.Names = append(out.Names, l)
			out.Chars += size
		}
	}
	for _, l := range secretNames {
		if size := len([]rune(l)) + 1; out.SecretChars+size <= req.Secret {
			out.SecretNames = append(out.SecretNames, l)
			out.SecretChars += size
		}
	}
	var bodyLines []string
	if len(req.Body) > 0 {
		bodyLines = append([]string{body.Header}, req.Body...)
		for _, l := range bodyLines {
			out.BodyChars += len([]rune(l)) + 1
		}
	}
	if all := append(append(append(append([]string(nil), out.Names...), out.SecretNames...), lines...), bodyLines...); len(all) > 0 {
		body := strings.Join(all, "\n") + "\n"
		out.Block = BlockTitle + textPrint(body).H + "]\n" + body
		out.BlockChars = len([]rune(out.Block))
		// Split the already selected, rendered lines verbatim. Basis marks
		// resolve across the PDF and retained text together; neither lane
		// repeats definitions or spends additional memory budget.
		lanes := [2][]string{append([]string(nil), out.Names...), append([]string(nil), out.SecretNames...)}
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
// precedes the defining view in the same lane, move that same quotation into
// the source marker.
// Effect references remain distinct from whole-record IDs.
func shareDeliveryBasis(lines []string, ids []string, rendered []renderedRecord, marks, names map[string]string, secret map[string]bool, sourceDef map[string]string) {
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
		if mark, ok := sourceDef[id]; ok {
			// a secret line refers to this general line's text; the added
			// chars were counted when that line was taken
			bodies[i] = "[text " + mark + "=\"" + rendered[i].memory.Body + "\"]"
			continue
		}
		if mark, ok := marks[id]; ok {
			r := rendered[i]
			m := *r.memory
			m.Body = "[text " + mark + "]"
			line := deliveryLine(m, names, r.basis, r.aware)
			if len([]rune(line)) < len([]rune(lines[i])) {
				bodies[i] = m.Body
				// moved only within one counted lane: each lane's counted
				// chars stay those of its lines (1.2: the general lane counts
				// the line)
				if j, ok := definitions[id]; ok && i < j && secret[ids[i]] == secret[ids[j]] {
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

// distinctAwareness keeps each awareness a line shows once: the same
// protection reaches a line along several records (its basis and the record
// it was recorded at often carry the same awareness), and showing it twice
// only lengthens the line.
func distinctAwareness(own []memory.KnownAwareness) []memory.KnownAwareness {
	type shownAs struct{ kind, who, via, from, extent, part string }
	seen := map[shownAs]bool{}
	out := own[:0:0]
	for _, a := range own {
		k := shownAs{a.Kind, a.Who, a.Via, a.From, a.Extent, a.Part}
		if !seen[k] {
			seen[k] = true
			out = append(out, a)
		}
	}
	return out
}

// lineSize is what a general-lane candidate counts against its ceiling
// (1.2): the line sent and its line break.
func lineSize(line string) int {
	return len([]rune(line)) + 1
}

// recordSize is what a secret-lane candidate counts against its ceiling
// (the general lane counts the line sent, 1.2). The ceilings were measured
// in the full record format (ids and fields, 2026-10-25); the line sent
// (and its line break) is counted when it is longer, so the delivered lines
// never pass the ceiling.
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

// enteringFirst puts first, in rank order, the current facts (not a
// completing fact) about a name the input gives that is missing from at
// least one turn of the request's window, so the dialog does not keep
// carrying its state (1.1 B1). With a long window this reads close to "a
// name the input gives". Names the chat records as the same are one. Only
// the order changes; the ceilings decide what is delivered as before; with
// no window turn nothing moves.
func enteringFirst(order []candidate, k memory.Known, window []memory.ScopedTurn, input string, parent map[string]string) []candidate {
	same := map[string]string{}
	for _, nm := range k.Names {
		same[nm.ID] = nm.ID
	}
	root := func(id string) string {
		if _, ok := same[id]; !ok {
			return id
		}
		r := id
		for same[r] != r {
			r = same[r]
		}
		for same[id] != r { // path compression: long alias chains stay cheap
			id, same[id] = same[id], r
		}
		return r
	}
	for _, r := range k.Relations {
		_, a := same[r.Name]
		_, b := same[r.SameAs]
		if a && b {
			same[root(r.Name)] = root(r.SameAs)
		}
	}
	names := map[string][]string{}
	for _, nm := range k.Names {
		if nm.Name != "" {
			names[root(nm.ID)] = append(names[root(nm.ID)], nm.Name)
		}
	}
	texts := make([]string, 0, len(window))
	for _, t := range window {
		texts = append(texts, memory.TurnText(t.Source))
	}
	entering := map[string]bool{}
	for id, ns := range names {
		in := func(text string) bool {
			for _, n := range ns {
				if mentions(text, n) {
					return true
				}
			}
			return false
		}
		if !in(input) {
			continue
		}
		held := 0
		for _, t := range texts {
			if in(t) {
				held++
			}
		}
		if held < len(texts) {
			entering[id] = true
		}
	}
	if len(entering) == 0 {
		return order
	}
	var first, rest []candidate
	for _, c := range order {
		on := false
		if c.mem.Kind == "fact" && parent[c.id] == "" {
			on = entering[root(c.mem.About)]
			for _, w := range c.mem.To {
				on = on || entering[root(w)]
			}
		}
		if on {
			first = append(first, c)
		} else {
			rest = append(rest, c)
		}
	}
	return append(first, rest...)
}

// mentions says whether text names n as a word. In scripts written with
// spaces a name is not glued to letters before it ("린이" names 린, "어린"
// does not) nor, for a name not ending in Hangul, to letters after it
// ("Ann" is not in "Anna"); a Hangul name may take a particle, so a longer
// word that begins with it ("린넨") still counts. Next to Han, Hiragana or
// Katakana there are no spaces to read, so it counts there (花子は, 我看见小明).
func mentions(text, n string) bool {
	unspaced := func(r rune) bool {
		return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana)
	}
	word := func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }
	first, size := utf8.DecodeRuneInString(n)
	last, _ := utf8.DecodeLastRuneInString(n)
	for i := 0; size > 0; {
		j := strings.Index(text[i:], n)
		if j < 0 {
			return false
		}
		at := i + j
		end := at + len(n)
		prev, _ := utf8.DecodeLastRuneInString(text[:at])
		next, _ := utf8.DecodeRuneInString(text[end:])
		before := at == 0 || !word(prev) || unspaced(prev) || unspaced(first)
		after := end == len(text) || !word(next) || unspaced(next) || unspaced(last) ||
			unicode.Is(unicode.Hangul, last) || unicode.Is(unicode.Hangul, next)
		if before && after {
			return true
		}
		i = at + size // the next occurrence may overlap this one
	}
	return false
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
// currentValues drops from an event a fact or view named by was (kept, 11-14)
// the values later records replaced: the delivered line shows what happened
// without an earlier value read as the current one. Other events keep their
// lines as they were (dropping their replaced values everywhere shortened
// lines enough to change which records fit: needed memories 13 -> 11 of 39
// on the 21 stored scenes). The line still carries the awareness of the
// values it leaves out: records recorded at those values rely on it for
// their protection (Sol r5).
func currentValues(m memory.KnownMemory, kept, sup map[string]bool) memory.KnownMemory {
	if !kept[m.ID] {
		return m
	}
	var keep []memory.Effect
	for i, e := range m.Effects {
		if !sup[m.ID+"."+strconv.Itoa(i+1)] {
			keep = append(keep, e)
		}
	}
	if len(keep) < len(m.Effects) {
		m.Effects = keep
	}
	return m
}

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
		fmt.Fprintf(&b, ", %s", memory.DateShown(m.Date))
	}
	b.WriteString(") " + memory.DocText(m, names))
	if bs := m.Basis; bs != nil {
		var parts []string
		if bs.Via != "" {
			parts = append(parts, bs.Via)
		}
		if bs.From != "" {
			var from []string
			for _, p := range memory.SourceParts(bs.From) {
				from = append(from, name(p))
			}
			parts = append(parts, "from "+strings.Join(from, ", "))
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
// to the query vector, computed here from the stored bytes (1.1: no MariaDB
// vector function, so a server older than 11.7 serves too). A record without
// a vector, or a zero vector, has no similarity.
func (d *DB) similarities(model string, query []float32, ids []string) (map[string]float64, error) {
	out := map[string]float64{}
	dim := len(query)
	var qnorm float64
	q := make([]float64, dim)
	for i, v := range query {
		q[i] = float64(v)
		if math.IsNaN(q[i]) || math.IsInf(q[i], 0) {
			return nil, fmt.Errorf("query vector is not finite")
		}
		qnorm += q[i] * q[i]
	}
	if qnorm == 0 {
		return out, nil
	}
	qnorm = math.Sqrt(qnorm)
	for start := 0; start < len(ids); start += 500 {
		part := ids[start:min(start+500, len(ids))]
		args := []any{model}
		for _, id := range part {
			args = append(args, strings.TrimPrefix(id, "M"))
		}
		stmt := fmt.Sprintf("SELECT m.id, e.`vector` FROM memories m JOIN embedding_vectors_%d e ON e.model=? AND e.text_hash=m.doc_hash WHERE m.id IN (", dim) +
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
			var data sql.RawBytes
			if err := rows.Scan(&id, &data); err != nil {
				rows.Close()
				return nil, err
			}
			if len(data) != dim*4 {
				continue // a stored vector of another size has no similarity
			}
			var dot, norm float64
			for i := 0; i < dim; i++ {
				v := float64(math.Float32frombits(binary.LittleEndian.Uint32(data[4*i:])))
				dot += v * q[i]
				norm += v * v
			}
			s := dot / (math.Sqrt(norm) * qnorm)
			if norm == 0 || math.IsNaN(s) || math.IsInf(s, 0) {
				continue // a stored vector that cannot be compared has no similarity
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
