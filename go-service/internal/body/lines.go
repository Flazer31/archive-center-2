package body

import (
	"fmt"
	"strings"
)

// Budget is the body section's own ceiling (AC1: 3,000 chars for both
// switches together), apart from the memory and secret budgets.
const Budget = 3000

// Header opens the body section of the delivered memory.
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
		who := name(st.Who)
		if p := st.Pregnancy; p != nil {
			var line string
			if p.Observed {
				line = fmt.Sprintf("- body %s [story fact, %s]: pregnant since %s", who, visibility(p.Visibility), p.Since.Text())
			} else {
				line = fmt.Sprintf("- body %s [modeled, private, not known to anyone]: %s since %s, modeled birth %s", who,
					map[string]string{"modeled_implanted_pregnancy": "early pregnancy", "modeled_birth_completed": "modeled term reached (no birth observed)"}[p.Stage],
					p.Since.Text(), p.BirthDay.Text())
			}
			switch p.Paternity {
			case "confirmed":
				line += "; parent established: " + names(p.Candidates, name)
			case "modeled_link":
				line += "; modeled partner: " + names(p.Candidates, name) + " (not a confirmed father)"
			case "candidates":
				line += "; parent candidates: " + names(p.Candidates, name)
			}
			if !add(line) {
				return out
			}
		}
		if st.Recovery && !add(fmt.Sprintf("- body %s [story fact, %s]: recovering", who, visibility(st.RecoveryVisibility))) {
			return out
		}
		if c := st.Cycle; c != nil && c.Status == "estimated" {
			ref := "model start"
			if st.RefKind == "observed_period_start" {
				ref = "period started " + st.CycleRef.Text()
			}
			line := fmt.Sprintf("- body %s [calculated, private, not known to anyone]: on %s cycle day %s (%s); period %s; fertile window possible: %s; next period %s",
				who, st.Now.Text(), span(c.CycleDay), ref,
				map[bool]string{true: "now", false: map[bool]string{true: "possible", false: "no"}[c.MenstruationPossible]}[c.Menstruation],
				yesNo(c.FertilePossible), dayRange(st.Now.Axis, c.NextPeriod))
			if !add(line) {
				return out
			}
		}
	}
	return out
}
