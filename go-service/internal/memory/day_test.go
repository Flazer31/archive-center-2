package memory

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestElapsedDaysParse(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		want        ElapsedDays
	}{
		{"zero", "0", 0}, {"whole", "3", 3}, {"large", "9223372036854775807", 9223372036854775807},
		{"negative", "-1", 0}, {"fraction", "1.5", 0}, {"overflow", "9223372036854775808", 0},
		{"string", `"2"`, 0}, {"text", `"tomorrow"`, 0}, {"null", "null", 0},
		{"bool", "true", 0}, {"array", "[1]", 0}, {"object", `{"days":2}`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := fmt.Sprintf(`{"elapsed_days":%s,"memories":[{"kind":"event","body":"saved"}]}`, tc.value)
			out, notes := Parse(raw)
			if out.ElapsedDays != tc.want || len(out.Memories) != 1 || out.Memories[0].Body != "saved" || len(notes) != 0 {
				t.Fatalf("out=%+v notes=%v", out, notes)
			}
		})
	}
	out, notes := Parse(`{"memories":[{"kind":"event","body":"missing delta"}]}`)
	if out.ElapsedDays != 0 || len(out.Memories) != 1 || len(notes) != 0 {
		t.Fatalf("missing: %+v %v", out, notes)
	}
	out, notes = Parse(`{"elapsed_days":2}`)
	if out.ElapsedDays != 2 || Unreadable(notes) || len(notes) != 0 {
		t.Fatalf("metadata only: %+v %v", out, notes)
	}
	for _, raw := range []string{
		`{"elapsed_days":3,"entities":"bad","memories":[{"kind":"event","body":"survives"}]}`,
		`{"elapsed_days":3,"memories":[{"kind":"event","body":"survives"},{"body":`,
	} {
		out, notes := Parse(raw)
		if out.ElapsedDays != 3 || len(out.Memories) != 1 || len(notes) == 0 {
			t.Fatalf("salvage: %+v %v", out, notes)
		}
	}
}

func TestElapsedDaysOutputRoundTripAndPrompt(t *testing.T) {
	want := Output{ElapsedDays: 4}
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	out, notes := Parse(string(raw))
	if out.ElapsedDays != want.ElapsedDays || len(notes) != 0 {
		t.Fatalf("%s %+v %v", raw, out, notes)
	}
	prompt := Prompt(Source{Turn: 1}, "")
	if !strings.Contains(prompt, `"elapsed_days":0`) || !strings.Contains(prompt, "since the previous turn") || !strings.Contains(prompt, "On the first turn") {
		t.Fatal("production critic request lacks turn delta instruction and JSON scalar")
	}
}
