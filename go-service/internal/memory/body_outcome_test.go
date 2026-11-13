package memory

import (
	"strings"
	"testing"
)

// The critic says how a pregnancy ended (1.1): read as given, asked for
// in the body rules.
func TestBodyOutcomeRead(t *testing.T) {
	r, notes := Parse(`{"memories":[],"body_events":[{"kind":"pregnancy_ended","who":"n1","outcome":"birth","q":"아이를 낳았다"},{"kind":"pregnancy_ended","who":"n1","outcome":"loss"},{"kind":"pregnancy_ended","who":"n1"}]}`)
	if len(notes) > 0 || len(r.BodyEvents) != 3 || r.BodyEvents[0].Outcome != "birth" || r.BodyEvents[1].Outcome != "loss" || r.BodyEvents[2].Outcome != "" {
		t.Fatalf("%+v %v", r.BodyEvents, notes)
	}
	if !strings.Contains(BodyRules, `"outcome":""`) || !strings.Contains(BodyRules, "pregnancy_ended gives outcome: birth") {
		t.Fatal("the body rules do not ask for the outcome")
	}
}

// The critic gives times on Archive Center's days: a calendar date or days
// before or after the turn, never the story's day number.
func TestBodyWhenRule(t *testing.T) {
	if !strings.Contains(BodyRules, `"3 days before"`) || !strings.Contains(BodyRules, `Never copy a day number the story uses`) {
		t.Fatal("the body rules do not say how to give a time")
	}
}
