package body

import (
	"strings"
	"testing"
)

// A cycle stored with a pregnancy result keeps its own period length: the
// line and the tendency read the same one, whatever the settings now (the
// tendency read the settings now).
func TestStoredCycleKeepsItsPeriod(t *testing.T) {
	stored := DefaultSpec() // period 5
	now := DefaultSpec()
	now.PeriodDays = 1
	x := Exposure{Version: ModelVersion, Status: "evaluated", ExposureDay: 2, Ref: 0,
		Cycles: []StoredCycle{{Index: 0, Start: 0, End: 28, Luteal: 14, Lag: 8, Spec: stored}}}
	recs := []Record{{ID: 1, Kind: "period_start", Day: day(0)},
		{ID: 2, Kind: "conception_exposure", Day: day(2), Exposure: poss, Result: &x}}
	st, _, _ := Assess(on, "seed", Person{Who: "N1", Spec: now}, recs, day(3))
	if st.Cycle == nil || !st.Cycle.Menstruation || st.Cycle.PeriodDays != 5 {
		t.Fatalf("cycle day 4 of a stored cycle with a 5-day period: %+v", st.Cycle)
	}
	if st.Tendency == nil || !strings.HasPrefix(st.Tendency.Phase, "menstrual (cycle day 4)") {
		t.Fatalf("tendency: %+v", st.Tendency)
	}
}
