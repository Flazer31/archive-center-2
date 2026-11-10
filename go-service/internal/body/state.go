package body

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
	Candidates []string
	Day        Day
	Result     *Exposure // the pregnancy model's frozen result
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
}

// State is a person's body as read on day Now.
type State struct {
	Who       string
	Now       Day
	Cycle     *Cycle
	CycleRef  Day
	RefKind   string // observed_period_start | model_initialization
	Pregnancy *Pregnancy
	Recovery  bool // a recovery started and has not ended
	// RecoveryVisibility is the recovery fact's visibility (public | private)
	RecoveryVisibility string
	Facts              []Record // the events of the story, in order (for the screen)
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
	// observed pregnancy and recovery, in story order
	var observed *Pregnancy
	for _, r := range recs {
		if !r.current() || !seen(r.Day) {
			continue
		}
		switch r.Kind {
		case "pregnancy_confirmed":
			if observed != nil {
				// the same pregnancy confirmed again: an omitted parent keeps
				// the one established (AC1); a stated one replaces it
				if r.Paternity != "" {
					observed.Paternity, observed.Candidates = r.Paternity, r.Candidates
				}
				if r.Visibility != "" {
					observed.Visibility = r.Visibility
				}
			} else {
				observed = &Pregnancy{Observed: true, Stage: "observed", Since: r.Day, Paternity: r.Paternity, Candidates: r.Candidates, Visibility: r.Visibility}
			}
			if observed.Paternity == "" || (observed.Paternity != "unknown" && len(observed.Candidates) == 0) {
				observed.Paternity = "unknown"
			}
		case "pregnancy_ended":
			observed = nil
		case "recovery_started":
			st.Recovery, st.RecoveryVisibility = true, r.Visibility
		case "recovery_ended":
			st.Recovery, st.RecoveryVisibility = false, ""
		}
	}
	// the pregnancy model: exposures on the person's cycle
	var modeled *Pregnancy
	if t.Pregnancy {
		for i, r := range recs {
			if r.Kind != "conception_exposure" || !r.current() || !r.Day.Known() {
				continue
			}
			x := r.Result
			if x == nil {
				ref, _, ok := refFor(r.Day)
				in := r.Exposure
				in.Day = r.Day.N
				var got Exposure
				if ok && ref.Same(r.Day) {
					got = Conceive(p.Spec, seed, p.Who, ref.N, in)
				} else {
					got = Exposure{Version: ModelVersion, Status: "unknown", Reason: "exact_event_date_or_reference_unresolved"}
				}
				frozen[r.ID] = got
				x = &got
				recs[i].Result = x
			}
			if !x.Implanted || !r.Day.Same(now) || r.Day.N > now.N || modeled != nil {
				continue
			}
			// a frozen result keeps its days relative to its exposure: the
			// story's day numbers move when a past is attached before it
			shift := r.Day.N - x.ExposureDay
			y := *x
			y.OvulationDay, y.ImplantationDay, y.BirthDay = x.OvulationDay+shift, x.ImplantationDay+shift, x.BirthDay+shift
			x = &y
			// ended by a later observed end of pregnancy
			ended := false
			for _, e := range recs {
				if e.Kind == "pregnancy_ended" && e.current() && e.Day.Same(r.Day) && e.Day.N >= r.Day.N {
					ended = true
				}
			}
			if ended {
				continue
			}
			modeled = &Pregnancy{Stage: x.Stage(now.N), Since: Day{now.Axis, x.ImplantationDay}, BirthDay: Day{now.Axis, x.BirthDay}, Paternity: "unknown"}
			if len(r.Partners) == 1 {
				modeled.Paternity, modeled.Candidates = "modeled_link", r.Partners
			} else if len(r.Partners) > 1 {
				modeled.Paternity, modeled.Candidates = "candidates", r.Partners
			}
		}
	}
	switch {
	case observed != nil:
		st.Pregnancy = observed
	case modeled != nil && modeled.Stage != "future_modeled_potential" && modeled.Stage != "modeled_pre_implantation":
		st.Pregnancy = modeled
	}
	pregnant := st.Pregnancy != nil && st.Pregnancy.Stage != "modeled_birth_completed"
	if t.Cycle && p.Spec.CycleEnabled && !pregnant && now.Known() {
		// AC1: after a pregnancy ends (stated, or the model's term reached)
		// the cycle is unknown until a new period start follows it; an end
		// on another axis cannot be placed, so the cycle stays unknown
		ended, endedOK, unplaced := Day{}, false, false
		for _, r := range recs {
			if r.Kind == "pregnancy_ended" && r.current() && seen(r.Day) {
				if r.Day.Same(now) {
					ended, endedOK = r.Day, true
				} else {
					unplaced = true
				}
			}
		}
		if st.Pregnancy != nil && st.Pregnancy.Stage == "modeled_birth_completed" && (!endedOK || st.Pregnancy.BirthDay.N > ended.N) {
			ended, endedOK = st.Pregnancy.BirthDay, true
		}
		if ref, kind, ok := refFor(now); ok && !unplaced && ref.Same(now) && (!endedOK || (kind == "observed_period_start" && ref.N >= ended.N)) {
			c := EstimateCycle(p.Spec, ref.N, now.N)
			st.Cycle, st.CycleRef, st.RefKind = &c, ref, kind
		}
	}
	st.Facts = recs
	return st, newRef, frozen
}
