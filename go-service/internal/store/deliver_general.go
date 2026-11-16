package store

import (
	"math"
	"sort"
	"strconv"
	"strings"

	"archivecenter2/goservice/internal/memory"
)

// The general lane's order (1.2). The single rank (similarity to the input
// and to the latest turn plus recency, summed as places) let the long event
// lines of recent turns fill the 2,000 chars and pushed older memories the
// scene needed past the ceiling. The general lane now starts from the
// scene's anchors (and Select's candidates) and takes the kinds of memory in turn (Archive Center 1's
// lanes), each kind in order of similarity to the input with a weak recency
// (Archive Center 1: halves every 32 turns, never below 0.2). What no
// anchor reaches is not delivered; the ceiling only cuts. The secret lane
// keeps the single rank (measured 2026-11-15: anchored secrets filled the
// secret lane with unneeded ones).

// Kinds taken in turn. The order of one round takes current facts about
// people and things twice (on the 21 stored scenes they carried the most
// needed text per char, events the least).
const (
	laneFact  = 3 // a current fact of a person, thing or group
	laneOpen  = 5 // an open obligation, question or risk, or shared goal
	laneEvent = 2
	laneView  = 6
	laneRule  = 4 // a world rule
)

var generalRound = []int{laneFact, laneFact, laneOpen, laneEvent, laneView, laneRule}

var openModes = map[string]bool{"의무": true, "공동 목표": true, "질문·위험": true}

// sceneAnchors are where the general lane starts: the entities the input or
// the latest shown turn names, the people seen in the shown turns, the
// latest shown place and the places its name lies in, and for an entity the
// input or latest turn names but the shown turns do not show, the last run
// of turns it was seen in.
type sceneAnchors struct {
	root      func(string) string
	kind      map[string]string          // entity -> person, place, thing, group
	inside    map[string]map[string]bool // place -> places named inside it (itself too)
	called    map[string]bool
	inputs    []string        // entities the input names, in id order
	entering  map[string]bool // of those, the ones a shown turn's text does not name
	roster    map[string]bool
	here      map[string]bool
	returning map[string]bool
	segment   map[string]map[int]bool // returning entity -> scope places of its last run
	entities  map[string]map[string]bool
	said      []string // the input and the latest shown turn's text
}

func newSceneAnchors(k memory.Known, place map[int64]int, scope []memory.ScopedTurn, cut, n int, input string) sceneAnchors {
	a := sceneAnchors{kind: map[string]string{}, inside: map[string]map[string]bool{}, called: map[string]bool{},
		roster: map[string]bool{}, here: map[string]bool{}, returning: map[string]bool{},
		segment: map[string]map[int]bool{}, entities: map[string]map[string]bool{}}
	same := map[string]string{}
	for _, nm := range k.Names {
		same[nm.ID] = nm.ID
		a.kind[nm.ID] = nm.Type
	}
	a.root = func(id string) string {
		if _, ok := same[id]; !ok {
			return id
		}
		r := id
		for same[r] != r {
			r = same[r]
		}
		return r
	}
	for _, r := range k.Relations {
		_, x := same[r.Name]
		_, y := same[r.SameAs]
		if x && y {
			same[a.root(r.Name)] = a.root(r.SameAs)
		}
	}
	names := map[string][]string{}
	for _, nm := range k.Names {
		if nm.Name != "" {
			names[a.root(nm.ID)] = append(names[a.root(nm.ID)], nm.Name)
		}
	}
	for p := range names {
		if a.kind[p] != "place" {
			continue
		}
		a.inside[p] = map[string]bool{p: true}
		for q, qs := range names {
			if q == p || a.kind[q] != "place" {
				continue
			}
			for _, pn := range names[p] {
				for _, qn := range qs {
					if pn != qn && strings.Contains(qn, pn) {
						a.inside[p][q] = true
					}
				}
			}
		}
	}
	appear := map[string]map[int]bool{}
	for i := range k.Memories {
		m := &k.Memories[i]
		e := a.entitiesOf(m)
		a.entities[m.ID] = e
		p, ok := place[m.Job]
		if !ok {
			continue
		}
		for x := range e {
			if appear[x] == nil {
				appear[x] = map[int]bool{}
			}
			appear[x][p] = true
		}
	}
	appears := func(x string, p int) bool {
		if appear[x][p] {
			return true
		}
		for q := range a.inside[x] {
			if appear[q][p] {
				return true
			}
		}
		return false
	}
	last := ""
	if n > 0 && n-1 >= cut {
		last = memory.TurnText(scope[n-1].Source)
	}
	a.said = []string{input, last}
	for x, ns := range names {
		for _, nm := range ns {
			if mentions(input, nm) {
				a.called[x] = true
				a.inputs = append(a.inputs, x)
				break
			}
		}
		if !a.called[x] && last != "" {
			for _, nm := range ns {
				if mentions(last, nm) {
					a.called[x] = true
					break
				}
			}
		}
	}
	// entities the input names that a shown turn's text does not name (their
	// facts come first, as 1.1's enteringFirst puts them first)
	var texts []string
	for p := cut; p < n; p++ {
		texts = append(texts, memory.TurnText(scope[p].Source))
	}
	entering := map[string]bool{}
	for _, x := range a.inputs {
		for _, text := range texts {
			named := false
			for _, nm := range names[x] {
				named = named || mentions(text, nm)
			}
			if !named {
				entering[x] = true
				break
			}
		}
	}
	sort.Strings(a.inputs)
	a.entering = entering
	for x := range appear {
		if t := a.kind[x]; t != "person" && t != "group" {
			continue
		}
		for p := cut; p < n; p++ {
			if appear[x][p] {
				a.roster[x] = true
			}
		}
	}
	latest := -1
	for i := range k.Memories {
		m := &k.Memories[i]
		p, ok := place[m.Job]
		if !ok || p < cut || p >= n || m.Roles == nil || len(m.Roles.Place) == 0 || p < latest {
			continue
		}
		if p > latest {
			latest, a.here = p, map[string]bool{}
		}
		for _, w := range m.Roles.Place {
			a.here[a.root(w)] = true
		}
	}
	for p := range a.inside {
		for q := range a.here {
			if a.inside[p][q] {
				a.here[p] = true
			}
		}
	}
	for x := range a.called {
		lastSeen, shown := -1, false
		for p := 0; p < n; p++ {
			if appears(x, p) {
				if p >= cut {
					shown = true
				} else {
					lastSeen = p
				}
			}
		}
		if shown || lastSeen < 0 {
			continue
		}
		a.returning[x] = true
		run := map[int]bool{}
		for p := lastSeen; p >= 0; p-- {
			if appears(x, p) {
				run[p] = true
			} else if !(p > 0 && appears(x, p-1)) {
				break
			}
		}
		a.segment[x] = run
	}
	return a
}

// entitiesOf are the entities a record is about, owned by, addressed to or
// has in a role.
func (a sceneAnchors) entitiesOf(m *memory.KnownMemory) map[string]bool {
	e := map[string]bool{}
	add := func(id string) {
		if id != "" {
			e[a.root(id)] = true
		}
	}
	add(m.About)
	add(m.Owner)
	for _, w := range m.To {
		add(w)
	}
	if r := m.Roles; r != nil {
		for _, l := range [][]string{r.Actor, r.Target, r.Object, r.Place, r.Speaker, r.Listener} {
			for _, w := range l {
				add(w)
			}
		}
	}
	return e
}

// holds says whether e has x, or a place named inside x.
func (a sceneAnchors) holds(e map[string]bool, x string) bool {
	if e[x] {
		return true
	}
	for q := range a.inside[x] {
		if e[q] {
			return true
		}
	}
	return false
}

// reaches says whether an anchor reaches a record at scope place p.
func (a sceneAnchors) reaches(m *memory.KnownMemory, p int) bool {
	e := a.entities[m.ID]
	for x := range e {
		if a.called[x] || a.roster[x] {
			return true
		}
	}
	if m.Kind != "fact" {
		for x := range a.returning {
			if a.segment[x][p] && a.holds(e, x) {
				return true
			}
		}
	}
	return m.Kind == "fact" && m.Mode == "세계 규칙" && a.here[a.root(m.About)]
}

func generalLane(m *memory.KnownMemory) int {
	switch {
	case m.Kind == "event":
		return laneEvent
	case m.Kind == "view":
		return laneView
	case m.Mode == "세계 규칙":
		return laneRule
	case openModes[m.Mode]:
		return laneOpen
	}
	return laneFact
}

// unitSum is the sum and count of a population's unit vectors (the center
// sceneContrast takes), kept from the read that gave its similarities.
type unitSum struct {
	sum   []float64
	count int
}

// generalOrder is the general lane's order: one current fact for each
// entity the input names, then the kinds in turn. Its records are those
// Select found (by words, the open scene and links) and the current
// records an anchor reaches (a fact current as CurrentFacts reads it); it
// leaves out what is replaced, held, in the request, a link, or a line
// secretLine says is shown as a secret (the secret lane keeps those).
// similar gives each record's similarity to the input (none when the
// request has no input vector) and, from the same read, the sum and count
// of their unit vectors.
func generalOrder(k memory.Known, place map[int64]int, scope []memory.ScopedTurn, cut, n int, input string,
	sup map[string]bool, holds map[string]string, selected, current map[string]bool, secretLine func(*memory.KnownMemory) bool,
	similar func(ids []string) (map[string]float64, []float64, int, error)) ([]candidate, map[string]int, map[string]string, map[string]float64, unitSum, sceneAnchors, error) {
	a := newSceneAnchors(k, place, scope, cut, n, input)
	lane := map[string]int{}
	var pool []candidate
	for i := range k.Memories {
		m := &k.Memories[i]
		p, ok := place[m.Job]
		if !ok || p >= cut || sup[m.ID] || holds[m.ID] != "" || m.Kind == "link" || m.Kind == "" {
			continue
		}
		if !selected[m.ID] && (m.Kind == "fact" && !current[m.ID] || !a.reaches(m, p)) || secretLine(m) {
			continue
		}
		lane[m.ID] = generalLane(m)
		pool = append(pool, candidate{id: m.ID, pos: p, mem: m})
	}
	if len(pool) == 0 {
		return nil, lane, nil, nil, unitSum{}, a, nil
	}
	ids := make([]string, len(pool))
	for i, c := range pool {
		ids[i] = c.id
	}
	sim, sum, count, err := similar(ids)
	if err != nil {
		return nil, nil, nil, nil, unitSum{}, a, err
	}
	value := func(c candidate) float64 {
		return 0.6*score(sim, c.id) + 0.15*math.Max(0.2, math.Pow(0.5, float64(n-1-c.pos)/32))
	}
	sort.SliceStable(pool, func(x, y int) bool { return value(pool[x]) > value(pool[y]) })
	// one fact per entity the input names: an entering entity's first, then
	// in the pool's order
	type pick struct {
		entering bool
		at       int
		c        candidate
	}
	taken := map[string]bool{}
	var picks []pick
	for _, x := range a.inputs {
		for i, c := range pool {
			if c.mem.Kind != "fact" || taken[c.id] {
				continue
			}
			subject := map[string]bool{a.root(c.mem.About): true}
			for _, w := range c.mem.To {
				subject[a.root(w)] = true
			}
			if a.holds(subject, x) {
				picks = append(picks, pick{a.entering[x], i, c})
				taken[c.id] = true
				break
			}
		}
	}
	sort.SliceStable(picks, func(i, j int) bool {
		if picks[i].entering != picks[j].entering {
			return picks[i].entering
		}
		return picks[i].at < picks[j].at
	})
	why := map[string]string{}
	for _, c := range pool {
		why[c.id] = a.why(c.mem, c.pos, selected[c.id])
	}
	var order []candidate
	for _, p := range picks {
		why[p.c.id] = WhyInputFact
		order = append(order, p.c)
	}
	byLane := map[int][]candidate{}
	for _, c := range pool {
		if !taken[c.id] {
			byLane[lane[c.id]] = append(byLane[lane[c.id]], c)
		}
	}
	next := map[int]int{}
	for more := true; more; {
		more = false
		for _, l := range generalRound {
			if next[l] < len(byLane[l]) {
				order = append(order, byLane[l][next[l]])
				next[l]++
				more = true
			}
		}
	}
	return order, lane, why, sim, unitSum{sum, count}, a, nil
}

// aliasLines are the stored name relations (an alias, a true name) whose
// name the input or the latest turn uses, one short line each in stored
// order: the line says whom that name is (1.2 #6; 11-15 trace: an alias
// stored at T15 was never said by a delivered line, and the state facts
// that carried it were replaced by ones without it). A relation kept from someone (its unaware or hide_from
// awareness) is a secret line carrying that awareness, as a record's line
// does.
func aliasLines(k memory.Known, a sceneAnchors, aware map[string][]memory.KnownAwareness, secret map[string]bool) (general, secrets []string) {
	name := map[string]string{}
	for _, nm := range k.Names {
		name[nm.ID] = nm.Name
	}
	label := map[string]string{"alias": "별칭", "true_name": "본명"}
	seen := map[string]bool{}
	for _, r := range k.Relations {
		x, y := name[r.Name], name[r.SameAs]
		if x == "" || y == "" || x == y || !mentions(a.said[0], x) && !mentions(a.said[1], x) {
			continue
		}
		how := label[r.How]
		if how == "" {
			how = r.How
		}
		line := "- (T" + strconv.Itoa(r.Turn) + ") name/" + how + " " + x + " = " + y
		for _, w := range aware[r.ID] {
			who := w.Who
			if n, ok := name[who]; ok {
				who = n
			}
			line += " | " + strings.ReplaceAll(w.Kind, "_", " ") + ": " + who
		}
		if seen[line] {
			continue
		}
		seen[line] = true
		if secret[r.ID] {
			secrets = append(secrets, line)
		} else {
			general = append(general, line)
		}
	}
	return general, secrets
}

// Why a record is in the general lane (TraceRow.Why; the trace view shows
// it). The first that holds, in this order.
const (
	WhyInputFact = "input_fact" // the current fact taken first for an entity the input names
	WhyCalled    = "called"     // about an entity the input or the latest turn names
	WhyReturning = "returning"  // from the last run of an entity named again
	WhyHere      = "here"       // a world rule of the latest shown place
	WhyRoster    = "roster"     // about a person seen in the shown turns
	WhySelected  = "selected"   // found by Select (words, the open scene, links)
	WhyWithEvent = "with_event" // a current fact recorded at a general event, right after it
	WhySecret    = "secret"     // the secret lane's single rank
)

func (a sceneAnchors) why(m *memory.KnownMemory, p int, selected bool) string {
	e := a.entities[m.ID]
	for x := range e {
		if a.called[x] {
			return WhyCalled
		}
	}
	if m.Kind != "fact" {
		for x := range a.returning {
			if a.segment[x][p] && a.holds(e, x) {
				return WhyReturning
			}
		}
	}
	if m.Kind == "fact" && m.Mode == "세계 규칙" && a.here[a.root(m.About)] {
		return WhyHere
	}
	for x := range e {
		if a.roster[x] {
			return WhyRoster
		}
	}
	if selected {
		return WhySelected
	}
	return ""
}
