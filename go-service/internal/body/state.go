package body

import "slices"

// Record is one stored body event of a person with its story day.
type Record struct {
	ID         int64
	User       bool // recorded by the user, not read from a turn
	Kind       string
	When       string
	Scope      string // flashback | planned | hypothetical: history only
	Visibility string
	Q          string
	Partners   []string
	Exposure   ExposureInput // Day is the record's day
	Paternity  string        // confirmed | candidates | unknown
	Outcome    string        // a pregnancy_ended's: birth | loss | unknown
	Candidates []string
	// Unaware are the people the text shows not knowing who the father is;
	// Child is the child a birth brings (1.1)
	Unaware []string
	Child   string
	Learns  []string // the people who learn who the father is (1.1)
	Day     Day
	Result  *Exposure // the pregnancy model's frozen result
}

// current reports an event that happens now in the story (AC1: a flashback,
// a plan or a hypothesis is history only).
func (r Record) current() bool { return r.Scope == "" }

// Person is a tracked person's settings.
type Person struct {
	Who  string
	Spec Spec
	// Ref is the model's own cycle start when no period start was seen
	// (AC1 model_initialization), kept once chosen.
	Ref Day
	// Intensity is how strongly she feels body changes (drawn once, kept)
	Intensity *Intensity
}

// Pregnancy is the person's pregnancy as read on a day.
type Pregnancy struct {
	Observed   bool   // the story established it
	Stage      string // observed, or the model's stage
	Since      Day    // confirmation day, or the modeled implantation day
	BirthDay   Day    // modeled birth day, never an observed birth
	Paternity  string // confirmed | candidates | modeled_link | unknown
	Candidates []string
	Visibility string // a story fact: public | private
	// ModeledParent: an observed pregnancy took its parent link from the
	// model (private, modeled; AC1 basis modeled_exposure), not the story
	ModeledParent bool
	// Unaware are the people the story shows not knowing who the father is
	// (1.1): the father is their secret
	Unaware []string
	// the model's ovulation day and whether an exposure of it named no
	// partner (AC1 unidentified_partner), for partners in the same cycle
	ovulation    int
	unidentified bool
	// start: the modeled ovulation (startModeled), or the confirmation day
	// as the latest start; the pregnancy's time counts from it (tendencies)
	start        Day
	startModeled bool
}

// Birth is a child born in the story (1.1: after a birth the mother and
// the father are kept; a father the mother does not know is a secret):
// the mother is the person read, the father as the pregnancy had it, or
// as the birth names it.
type Birth struct {
	Day        Day
	Child      string // the child's name id, when the text names it
	Visibility string // the birth's: public | private
	Paternity  string // confirmed | candidates | modeled_link | unknown
	Candidates []string
	// ModeledParent: the father is the model's (known to no one, the mother
	// too); Unaware: the people the story shows not knowing who he is
	ModeledParent bool
	Unaware       []string
	// of numbers the delivery: the children of one delivery (twins, also
	// born across midnight) share their parents
	of int
}

// RecoveryEpisode is one recovery after a pregnancy ended: its outcome
// (birth | loss | unknown) and the days it counts from — the end, and a
// recovery the story started after it.
type RecoveryEpisode struct {
	Outcome string
	Ended   Day // the pregnancy's end
	Started Day // a recovery_started on or after it
}

// State is a person's body as read on day Now.
type State struct {
	Who string
	// Owner is the name whose settings the state was read with (Who, or a
	// name merged into Who); Species is from the latest profile (1.1)
	Owner   string
	Species string
	// Joined are the other names read as this person (merged by the user or
	// linked by the story), for the screen and for finding her in a scene
	Joined    []string
	Now       Day
	Cycle     *Cycle
	CycleRef  Day
	RefKind   string // observed_period_start | model_initialization
	Pregnancy *Pregnancy
	Recovery  bool // a recovery started and has not ended
	// RecoveryVisibility is the recovery fact's visibility (public | private)
	RecoveryVisibility string
	RecoverySince      Day // the recovery's start, as recorded
	// Recovering is the recovery after a pregnancy ended, followed in story
	// order (1.1: it follows an end by itself; a recovery end or a new
	// pregnancy closes it, whatever their dates); nil when none is open
	Recovering *RecoveryEpisode
	Facts      []Record // the events of the story, in order (for the screen)
	// Births are the children born, in story order (1.1)
	Births []Birth
	// Pending: the latest reply's memory is not made yet, so the days it
	// moved on and the body events in it are not counted
	Pending  bool
	Tendency *Tendency // typical body changes on Now (1.1)
}

// Assess reads a person's body on day now from their records, given in
// story order (the order the story has them, not sorted by day: days on
// different axes cannot be compared).
// It returns the state, the person's model start when one was chosen now
// (to keep), and pregnancy results computed now (to freeze by record id).
func Assess(t Toggles, seed string, p Person, records []Record, now Day) (State, Day, map[int64]Exposure) {
	st := State{Who: p.Who, Now: now}
	frozen := map[int64]Exposure{}
	recs := append([]Record(nil), records...)
	st.Facts = recs
	// a fact the story has reached: not dated after now on the same axis
	// (a fact on another axis or undated keeps its place in story order;
	// AC1: a missing optional date never removes an observed fact)
	seen := func(d Day) bool { return !d.Same(now) || d.N <= now.N }
	// the latest period start seen before a day
	periodBefore := func(day Day) (Day, bool) {
		var ref Day
		found := false
		for _, r := range recs {
			if r.Kind == "period_start" && r.current() && r.Day.Same(day) && r.Day.N <= day.N && (!found || r.Day.N > ref.N) {
				ref, found = r.Day, true
			}
		}
		return ref, found
	}
	// the model's own start, when no period was seen (AC1: a seeded phase
	// back from the first day known)
	newRef := Day{}
	modelRef := func() (Day, bool) {
		if p.Ref.Same(now) {
			return p.Ref, true
		}
		if seed == "" || !now.Known() || !p.Spec.Valid() {
			return Day{}, false
		}
		anchor := now
		for _, r := range recs {
			if r.Day.Same(now) && r.Day.N < anchor.N {
				anchor = r.Day
			}
		}
		phase := integer(seed, p.Who, "automatic_initial_cycle.v1", 0, "initial_phase", 0, p.Spec.CycleDays-1)
		newRef = Day{now.Axis, anchor.N - phase}
		p.Ref = newRef
		return newRef, true
	}
	refFor := func(day Day) (Day, string, bool) {
		if r, ok := periodBefore(day); ok {
			return r, "observed_period_start", true
		}
		r, ok := modelRef()
		return r, "model_initialization", ok
	}
	// pregnancies, recovery and the pregnancy model, in story order (1.1,
	// AC1 parity): the story's latest pregnancy is the one read on now.
	// A confirmed pregnancy takes the model's dates and link when the model
	// has one under way that day, else the confirmation day as its latest
	// start, and its configured term (AC1 bodyPregnancyConfirmedTerm). Once
	// the term has passed in story time the birth is complete in the model
	// only; no birth is observed. A stated end ends the pregnancy current at
	// that point of the story. The model starts a new pregnancy only when
	// none is under way; an exposure in the same cycle adds its partners.
	var latest *Pregnancy // the pregnancy current at this point of the story
	var modelX *Exposure  // the model's result behind latest, when latest is modeled
	// termEnd: the term end of a pregnancy that a later one replaced, so the
	// cycle stays unknown until a period start after it (AC1
	// cycle_reference_after_pregnancy_unresolved); freeFrom: the first record
	// after the last stated end
	termEnd, freeFrom := Day{}, 0
	elapsed := func(pg *Pregnancy, d Day) bool {
		return pg != nil && pg.BirthDay.Same(d) && pg.BirthDay.N <= d.N
	}
	replace := func(pg *Pregnancy, d Day) {
		if elapsed(latest, d) && (!termEnd.Same(latest.BirthDay) || latest.BirthDay.N > termEnd.N) {
			termEnd = latest.BirthDay
		}
		latest = pg
	}
	// the children the births come to have, with the names given later:
	// a learning about a child reaches its birth whether the name comes
	// before or after it in the story. Learnings change no birth or
	// name, so the births are read once without them.
	var named []string
	var namedOf []int // the delivery of each of those births
	if slices.ContainsFunc(recs, func(r Record) bool { return r.Kind == "paternity_learned" && r.Child != "" }) {
		var rest []Record
		for _, r := range recs {
			if r.Kind != "paternity_learned" {
				rest = append(rest, r)
			}
		}
		pre, _, _ := Assess(t, seed, p, rest, now)
		for _, b := range pre.Births {
			named, namedOf = append(named, b.Child), append(namedOf, b.of)
		}
	}
	learn := func(births []*Birth, r Record) {
		for _, b := range births {
			b.Unaware = unawareAfter(b.Unaware, b.Candidates, r)
			if r.Paternity != "" {
				b.Paternity, b.Candidates, b.ModeledParent = r.Paternity, r.Candidates, false
			}
		}
	}
	// the births of one delivery so far: of the child's (named then or
	// later, born or yet to be born), else of the latest birth
	birthsOf := func(child string) []*Birth {
		of := -1
		for k := range max(len(st.Births), len(named)) {
			switch {
			case k < len(st.Births) && (child == "" || st.Births[k].Child == child):
				of = st.Births[k].of
			case child != "" && k < len(named) && named[k] == child:
				of = namedOf[k]
			}
		}
		var out []*Birth
		for k := range st.Births {
			if of >= 0 && st.Births[k].of == of {
				out = append(out, &st.Births[k])
			}
		}
		return out
	}
	// the settings an exposure result was drawn with (results from before 1.1
	// carry none: the person's settings now)
	specOf := func(x *Exposure) Spec {
		if x != nil && x.Spec.Valid() {
			return x.Spec
		}
		return p.Spec
	}
	// an exposure in the fertile window of the model's ovulation day, read
	// with the settings the pregnancy was drawn with (an exposure whose
	// contraception never conceives — magic — is no parent of it)
	inWindow := func(e Record, x *Exposure, ovulation int) bool {
		return e.Exposure.Classification != "not_potentially_conceiving" && !e.Exposure.neverConceives() && conditional(specOf(x), float64(e.Day.N-ovulation)) > 0
	}
	addPartners := func(pg *Pregnancy, r Record) {
		if len(r.Partners) == 0 {
			pg.unidentified = true
		}
		for _, who := range r.Partners {
			if !slices.Contains(pg.Candidates, who) {
				pg.Candidates = append(pg.Candidates, who)
			}
		}
		switch {
		case len(pg.Candidates) == 1 && !pg.unidentified:
			pg.Paternity = "modeled_link"
		case len(pg.Candidates) > 0:
			pg.Paternity = "candidates"
		default:
			pg.Paternity = "unknown"
		}
	}
	for i, r := range recs {
		if !r.current() {
			continue
		}
		switch r.Kind {
		case "pregnancy_confirmed":
			if !seen(r.Day) {
				continue
			}
			if latest != nil && latest.Observed && !elapsed(latest, r.Day) {
				// the same pregnancy confirmed again: an omitted parent keeps
				// the one established (AC1); a stated one replaces it
				if r.Paternity != "" {
					latest.Unaware = unawareAfter(latest.Unaware, latest.Candidates, r)
					latest.Paternity, latest.Candidates, latest.ModeledParent = r.Paternity, r.Candidates, false
				}
				if r.Visibility != "" {
					latest.Visibility = r.Visibility
				}
			} else {
				pg := &Pregnancy{Observed: true, Stage: "observed", Since: r.Day, Paternity: r.Paternity, Candidates: r.Candidates, Visibility: r.Visibility, Unaware: r.Unaware}
				if modelX != nil && latest != nil && !latest.Observed && r.Day.Same(latest.Since) &&
					(modelX.Stage(r.Day.N) == "modeled_pre_implantation" || modelX.Stage(r.Day.N) == "modeled_implanted_pregnancy") {
					// the model's pregnancy under way: its dates, and its link
					// when the story names no parent (AC1 existing_modeled_conception)
					pg.BirthDay, pg.start, pg.startModeled = latest.BirthDay, latest.start, latest.startModeled
					if r.Paternity == "" {
						pg.Paternity, pg.Candidates = latest.Paternity, latest.Candidates
						pg.ModeledParent = len(pg.Candidates) > 0
					}
					// the model's pregnancy becomes the observed one; its term is
					// not a pregnancy ended before this one
					latest = nil
				} else if r.Day.Known() {
					// AC1 confirmation_as_latest_start, with the term kept as first
					// read (AC1 stores modeled_birth_time): a later settings change
					// does not move a confirmed pregnancy's term
					g := p.Spec.GestationDays
					if x := r.Result; x != nil && x.Status == TermStatus && x.Spec.GestationDays > 0 {
						g = x.Spec.GestationDays
					} else if g > 0 {
						frozen[r.ID] = Exposure{Version: ModelVersion, Status: TermStatus, ExposureDay: r.Day.N, Spec: Spec{GestationDays: g}}
					}
					if g > 0 {
						pg.BirthDay, pg.start = Day{r.Day.Axis, r.Day.N + g}, r.Day
					}
				}
				replace(pg, r.Day)
				st.Recovering = nil // a new pregnancy: the recovery before it is over
				modelX = nil
			}
			if latest.Paternity == "" || (latest.Paternity != "unknown" && len(latest.Candidates) == 0) {
				latest.Paternity = "unknown"
			}
		case "pregnancy_ended":
			if seen(r.Day) {
				if r.Outcome == "birth" {
					// the parents stay after the birth: the father as the
					// pregnancy had him, or as the birth names him
					b := Birth{Day: r.Day, Child: r.Child, Visibility: r.Visibility, of: len(st.Births)}
					if latest != nil {
						b.Paternity, b.Candidates, b.Unaware = latest.Paternity, latest.Candidates, latest.Unaware
						b.ModeledParent = latest.ModeledParent || (!latest.Observed && len(latest.Candidates) > 0)
					} else if n := len(st.Births); n > 0 && sameDelivery(st.Births[n-1], b) {
						// another child of the same delivery (twins recorded one
						// by one, also across midnight): the same parents; a
						// birth later on is another pregnancy's even when that
						// pregnancy was never recorded
						prev := st.Births[n-1]
						b.of = prev.of
						b.Paternity, b.Candidates, b.Unaware, b.ModeledParent = prev.Paternity, prev.Candidates, prev.Unaware, prev.ModeledParent
					}
					if r.Paternity != "" {
						b.Unaware = unawareAfter(b.Unaware, b.Candidates, r)
						b.Paternity, b.Candidates, b.ModeledParent = r.Paternity, r.Candidates, false
					}
					if b.Paternity == "" || (b.Paternity != "unknown" && len(b.Candidates) == 0) {
						b.Paternity = "unknown"
					}
					st.Births = append(st.Births, b)
				}
				latest, modelX, freeFrom = nil, nil, i+1
				st.Recovering = &RecoveryEpisode{Outcome: r.Outcome, Ended: r.Day}
			}
		case "child_named":
			// a child born earlier named later: a child already joined to a
			// birth is not joined again; the unnamed birth of the day the
			// record names (its when: the birth day), else the latest unnamed
			// birth before
			if !seen(r.Day) || r.Child == "" || slices.ContainsFunc(st.Births, func(b Birth) bool { return b.Child == r.Child }) {
				continue
			}
			target := -1
			for k := range st.Births {
				if b := st.Births[k]; b.Child == "" && b.Day == r.Day {
					target = k
					break
				}
			}
			for k := len(st.Births) - 1; k >= 0 && target < 0; k-- {
				if b := st.Births[k]; b.Child == "" && (!b.Day.Same(r.Day) || b.Day.N <= r.Day.N) {
					target = k
				}
			}
			if target >= 0 {
				st.Births[target].Child = r.Child
			}
		case "paternity_learned":
			// someone learns who the father is (a stated father as the text
			// gives him): the record's child and the others of its birth;
			// else the pregnancy under way (also the one the child is yet to
			// be born of); else the latest birth's children
			if seen(r.Day) {
				var births []*Birth
				if r.Child != "" {
					births = birthsOf(r.Child)
				}
				switch {
				case len(births) > 0:
				case r.Child != "" && (latest == nil || !slices.Contains(named[min(len(st.Births), len(named)):], r.Child)):
					// a child no birth comes to have: nothing to change
				case latest != nil:
					latest.Unaware = unawareAfter(latest.Unaware, latest.Candidates, r)
					if r.Paternity != "" {
						latest.Paternity, latest.Candidates, latest.ModeledParent = r.Paternity, r.Candidates, false
					}
				default:
					births = birthsOf("")
				}
				learn(births, r)
			}
		case "recovery_started":
			if seen(r.Day) {
				st.Recovery, st.RecoveryVisibility, st.RecoverySince = true, r.Visibility, r.Day
				if st.Recovering == nil {
					st.Recovering = &RecoveryEpisode{}
				}
				st.Recovering.Started = r.Day
			}
		case "recovery_ended":
			if seen(r.Day) {
				st.Recovery, st.RecoveryVisibility, st.Recovering = false, "", nil
			}
		case "conception_exposure":
			if !t.Pregnancy || !r.Day.Known() {
				continue
			}
			x := r.Result
			if x != nil && Redrawn[x.Reason] {
				// skipped by a rule that no longer holds, read again (1.1)
				x = nil
			}
			if x == nil {
				ref, _, ok := refFor(r.Day)
				in := r.Exposure
				in.Day = r.Day.N
				var got Exposure
				// a pregnancy under way that day: not drawn (AC1
				// observed/modeled_pregnancy_ongoing)
				ongoing := ""
				if latest != nil && !elapsed(latest, r.Day) {
					if latest.Observed && latest.Since.Same(r.Day) && latest.Since.N <= r.Day.N {
						ongoing = "observed_pregnancy_ongoing"
					} else if !latest.Observed && modelX != nil && r.Day.Same(latest.Since) {
						if stage := modelX.Stage(r.Day.N); stage == "modeled_pre_implantation" || stage == "modeled_implanted_pregnancy" {
							ongoing = "modeled_pregnancy_ongoing"
						}
					}
				}
				if ongoing != "" {
					got = Exposure{Version: ModelVersion, Status: "not_applicable", Reason: ongoing, ExposureDay: r.Day.N}
				} else if ok && ref.Same(r.Day) {
					// around the cycles already stored, which keep their days and
					// settings; whether the person can conceive is the setting now
					got = conceiveWith(p.Spec, seed, p.Who, ref.N, storedCycles(recs, ref, r.Day), in)
				} else {
					got = Exposure{Version: ModelVersion, Status: "unknown", Reason: "exact_event_date_or_reference_unresolved"}
				}
				if ongoing != "modeled_pregnancy_ongoing" {
					frozen[r.ID] = got
				}
				x = &got
				recs[i].Result = x
			}
			if !r.Day.Same(now) || r.Day.N > now.N {
				continue
			}
			underway := latest != nil && !elapsed(latest, r.Day)
			// emergency contraception after it (1.1): read with the result,
			// on the result's days, never frozen into it
			stopped := false
			for _, e := range recs {
				if e.Kind == "emergency_contraception" && e.current() && e.Day.Same(r.Day) && x.EmergencyStops(e.Day.N-(r.Day.N-x.ExposureDay)) {
					stopped = true
				}
			}
			if x.Implanted && !underway && !stopped {
				// a frozen result keeps its days relative to its exposure: the
				// story's day numbers move when a past is attached before it
				shift := r.Day.N - x.ExposureDay
				y := *x
				// the pregnancy's settings are those of the cycle it ovulated in,
				// which may be the cycle after the exposure's
				y.Spec = x.ovulationSpec()
				y.OvulationDay, y.ImplantationDay, y.BirthDay = x.OvulationDay+shift, x.ImplantationDay+shift, x.BirthDay+shift
				pg := &Pregnancy{Since: Day{now.Axis, y.ImplantationDay}, BirthDay: Day{now.Axis, y.BirthDay}, Paternity: "unknown", ovulation: y.OvulationDay, start: Day{now.Axis, y.OvulationDay}, startModeled: true}
				// every exposure of its fertile window adds its partners, the
				// ones before it included (AC1 bodyTrackingModelPaternity reads
				// the whole record): after the last stated end and after the
				// term of the pregnancy it follows
				for j := freeFrom; j < i; j++ {
					e := recs[j]
					if e.Kind == "conception_exposure" && e.current() && e.Day.Same(now) && e.Day.N <= now.N &&
						(latest == nil || !latest.BirthDay.Same(e.Day) || e.Day.N >= latest.BirthDay.N) && inWindow(e, &y, pg.ovulation) {
						addPartners(pg, e)
					}
				}
				addPartners(pg, r)
				replace(pg, r.Day)
				st.Recovering = nil // a new pregnancy: the recovery before it is over
				modelX = &y
				continue
			}
			// a later exposure in the cycle of the model's pregnancy under way
			// adds its partners (AC1 bodyTrackingModelPaternity)
			if underway && !latest.Observed && inWindow(r, modelX, latest.ovulation) {
				addPartners(latest, r)
			}
		}
	}
	switch {
	case latest == nil:
	case latest.Observed:
		if elapsed(latest, now) {
			latest.Stage = "modeled_birth_completed"
		}
		st.Pregnancy = latest
	default:
		latest.Stage = modelX.Stage(now.N)
		if latest.Stage != "future_modeled_potential" && latest.Stage != "modeled_pre_implantation" {
			st.Pregnancy = latest
		}
	}
	pregnant := st.Pregnancy != nil && st.Pregnancy.Stage != "modeled_birth_completed"
	if t.Cycle && p.Spec.CycleEnabled && !pregnant && now.Known() {
		// AC1: after a pregnancy ends (stated, or the model's term reached)
		// the cycle is unknown until a new period start follows it; an end
		// on another axis cannot be placed, so the cycle stays unknown
		// in story order: an end on another axis stays unplaced only until a
		// period start on now's axis follows it in the story
		ended, endedOK, unplaced := Day{}, false, false
		for _, r := range recs {
			if !r.current() || !seen(r.Day) {
				continue
			}
			switch {
			case r.Kind == "pregnancy_ended" && r.Day.Same(now):
				ended, endedOK = r.Day, true
			case r.Kind == "pregnancy_ended":
				unplaced = true
			case r.Kind == "period_start" && r.Day.Same(now) && unplaced:
				unplaced = false
				if !endedOK || r.Day.N > ended.N {
					// the end is before this period start in the story
					ended, endedOK = r.Day, true
				}
			}
		}
		if st.Pregnancy != nil && st.Pregnancy.Stage == "modeled_birth_completed" && (!endedOK || st.Pregnancy.BirthDay.N > ended.N) {
			ended, endedOK = st.Pregnancy.BirthDay, true
		}
		if termEnd.Same(now) && (!endedOK || termEnd.N > ended.N) {
			ended, endedOK = termEnd, true
		}
		if ref, kind, ok := refFor(now); ok && !unplaced && ref.Same(now) && (!endedOK || (kind == "observed_period_start" && ref.N >= ended.N)) {
			// the cycles the pregnancy model draws, one day each (a range of
			// every possible length tells nothing); without a seed, the range
			// of every length
			c := EstimateCycle(p.Spec, ref.N, now.N)
			if seed != "" {
				c = CycleOn(p.Spec, seed, p.Who, ref.N, storedCycles(recs, ref, now), now.N)
			}
			st.Cycle, st.CycleRef, st.RefKind = &c, ref, kind
		}
	}
	st.Facts = recs
	st.Tendency = tendencyOf(st, p)
	return st, newRef, frozen
}

// Refs are the name ids the state names: the person, her other names, the
// partners, parents, children and people who do not know a father in her
// facts and her pregnancy.
func (st State) Refs() []string {
	refs := append([]string{st.Who}, st.Joined...)
	if p := st.Pregnancy; p != nil {
		refs = append(refs, p.Candidates...)
		refs = append(refs, p.Unaware...)
	}
	for _, f := range st.Facts {
		refs = append(refs, f.Partners...)
		refs = append(refs, f.Candidates...)
		refs = append(refs, f.Unaware...)
		if f.Child != "" {
			refs = append(refs, f.Child)
		}
	}
	// (a birth's names come from these facts)
	return refs
}

// unawareAfter is who does not know the father once r speaks of him: the
// people r names as not knowing, else (r names the same father, or none) the
// ones before; less the people r shows learning it. A father named again is
// no sign that anyone learned it.
func unawareAfter(before, fathers []string, r Record) []string {
	out := before
	switch {
	case len(r.Unaware) > 0:
		out = r.Unaware
	case r.Paternity != "" && !sameNames(fathers, r.Candidates):
		out = nil // another father: what was kept from whom was about the one before
	}
	var kept []string
	for _, id := range out {
		if !slices.Contains(r.Learns, id) {
			kept = append(kept, id)
		}
	}
	return kept
}

func sameNames(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for _, id := range a {
		if !slices.Contains(b, id) {
			return false
		}
	}
	return true
}

// sameDelivery: b is a twin of prev, born the same day or the next.
func sameDelivery(prev, b Birth) bool {
	return prev.Day.Same(b.Day) && b.Day.N-prev.Day.N <= 1
}
