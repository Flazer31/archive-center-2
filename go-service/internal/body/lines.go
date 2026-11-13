package body

import (
	"fmt"
	"strings"
)

// Budget is the body section's own ceiling (AC1: 3,000 chars for both
// switches together), apart from the memory and secret budgets.
const Budget = 3000

// Header opens the body section of the delivered memory. PendingLine
// says the body is read before the latest reply was: the main model
// goes on from the reply itself.
const PendingLine = "- body note: the latest reply is not read yet; the days it moved on and the body events in it are not counted in these lines, so follow the reply where they differ"

const Header = "[Archive Center: body state — no character knows a body fact unless the story shows it; calculated and modeled values are known to no one]"

func dayRange(axis string, r Range) string {
	a, b := Day{axis, r.Min}.Text(), Day{axis, r.Max}.Text()
	if a == b {
		return a
	}
	return a + "~" + b
}

func span(r Range) string {
	if r.Min == r.Max {
		return fmt.Sprint(r.Min)
	}
	return fmt.Sprintf("%d~%d", r.Min, r.Max)
}

// unawareOf marks a father the story keeps from some people as their
// secret.
func unawareOf(ids []string, name func(string) string) string {
	if len(ids) == 0 {
		return ""
	}
	return " (secret: " + names(ids, name) + " do not know who the father is)"
}

// fatherOf is a birth's father as the story and the model have him.
func fatherOf(b Birth, mother string, name func(string) string) string {
	switch {
	case b.ModeledParent && b.Paternity == "modeled_link":
		return names(b.Candidates, name) + " (modeled, a secret known to no one, " + mother + " included; not a confirmed father)"
	case b.ModeledParent:
		return "one of " + names(b.Candidates, name) + " (modeled candidates, a secret known to no one, " + mother + " included)"
	case b.Paternity == "confirmed":
		return names(b.Candidates, name) + unawareOf(b.Unaware, name)
	case b.Paternity == "candidates":
		return "not established; candidates " + names(b.Candidates, name) + unawareOf(b.Unaware, name)
	case b.Paternity == "modeled_link":
		return names(b.Candidates, name) + " (not a confirmed father)" + unawareOf(b.Unaware, name)
	}
	return "unknown" + unawareOf(b.Unaware, name)
}

func names(ids []string, name func(string) string) string {
	var out []string
	for _, id := range ids {
		out = append(out, name(id))
	}
	return strings.Join(out, ", ")
}

func visibility(v string) string {
	if v == "public" {
		return "public"
	}
	return "private"
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// Lines writes each person's body as delivered lines, in the order given,
// up to Budget chars: a story fact keeps its visibility; a calculated cycle
// and the pregnancy model are private and known to no one (AC1).
func Lines(states []State, name func(string) string) []string {
	var out []string
	used := len([]rune(Header)) + 1 // the section's header is inside the budget
	add := func(line string) bool {
		if used+len([]rune(line))+1 > Budget {
			return false
		}
		out = append(out, line)
		used += len([]rune(line)) + 1
		return true
	}
	for _, st := range states {
		if st.Pending {
			if !add(PendingLine) {
				return out
			}
			break
		}
	}
	for _, st := range states {
		who := name(st.Who)
		if p := st.Pregnancy; p != nil {
			var line string
			if p.Observed {
				line = fmt.Sprintf("- body %s [story fact, %s]: pregnancy confirmed %s", who, visibility(p.Visibility), p.Since.Text())
				// the configured term is the model's, never an observed birth (AC1)
				switch {
				case p.Stage == "modeled_birth_completed":
					line = fmt.Sprintf("- body %s [story fact, %s]: pregnancy confirmed %s; modeled term reached %s (no birth observed, modeled, not known to anyone)", who, visibility(p.Visibility), p.Since.Text(), p.BirthDay.Text())
				case p.BirthDay.Known():
					line += fmt.Sprintf("; modeled term ends %s (modeled, not known to anyone)", p.BirthDay.Text())
				}
			} else {
				line = fmt.Sprintf("- body %s [modeled, private, not known to anyone]: %s since %s, modeled birth %s", who,
					map[string]string{"modeled_implanted_pregnancy": "pregnancy", "modeled_birth_completed": "modeled term reached (no birth observed)"}[p.Stage],
					p.Since.Text(), p.BirthDay.Text())
			}
			switch {
			case p.ModeledParent && p.Paternity == "modeled_link":
				// a link the model gave a confirmed pregnancy stays the model's
				// (AC1 basis modeled_exposure, private, not known)
				line += "; modeled partner: " + names(p.Candidates, name) + " (modeled, not known to anyone, not a confirmed father)"
			case p.ModeledParent:
				line += "; modeled parent candidates: " + names(p.Candidates, name) + " (modeled, not known to anyone, not confirmed)"
			case p.Paternity == "confirmed":
				line += "; parent established: " + names(p.Candidates, name) + unawareOf(p.Unaware, name)
			case p.Paternity == "modeled_link":
				line += "; modeled partner: " + names(p.Candidates, name) + " (not a confirmed father)"
			case p.Paternity == "candidates":
				line += "; parent candidates: " + names(p.Candidates, name) + unawareOf(p.Unaware, name)
			}
			if !add(line) {
				return out
			}
		}
		if st.Recovery && !add(fmt.Sprintf("- body %s [story fact, %s]: recovering", who, visibility(st.RecoveryVisibility))) {
			return out
		}
		// the children she bore, the latest first (1.1: the mother and
		// the father stay after a birth; a father the mother does not
		// know is a secret)
		for i := len(st.Births) - 1; i >= 0; i-- {
			b := st.Births[i]
			line := fmt.Sprintf("- body %s [story fact, %s]: gave birth %s", who, visibility(b.Visibility), b.Day.Text())
			if b.Child != "" {
				line += "; child " + name(b.Child)
			}
			line += "; mother " + who + "; father " + fatherOf(b, who, name)
			if !add(line) {
				return out
			}
		}
		if c := st.Cycle; c != nil && c.Status == "estimated" {
			ref := "model start"
			if st.RefKind == "observed_period_start" {
				ref = "period started " + st.CycleRef.Text()
			}
			// the species is the critic's record, private like the calculation
			kind := ""
			if st.Species != "" {
				kind = "species " + st.Species + "; "
			}
			line := fmt.Sprintf("- body %s [calculated, private, not known to anyone]: %son %s cycle day %s (%s); period %s; fertile window possible: %s; next period %s",
				who, kind, st.Now.Text(), span(c.CycleDay), ref,
				map[bool]string{true: "now", false: map[bool]string{true: "possible", false: "no"}[c.MenstruationPossible]}[c.Menstruation],
				yesNo(c.FertilePossible), dayRange(st.Now.Axis, c.NextPeriod))
			if c.PeriodDays > 0 {
				// one drawn cycle: its days, so the main model can place a
				// scene a day or two on
				line += fmt.Sprintf("; fertile window %s, ovulation %s", dayRange(st.Now.Axis, c.Fertile), Day{st.Now.Axis, c.Fertile.Max}.Text())
			}
			if !add(line) {
				return out
			}
		}
	}
	return out
}
