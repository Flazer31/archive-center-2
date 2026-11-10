package store

import (
	"math"
	"reflect"
	"testing"

	"archivecenter2/goservice/internal/memory"
)

func TestDeliverRecentQueryAddsIndependentRank(t *testing.T) {
	db, session, ids := deliverChat(t)
	req := DeliveryRequest{Input: "리나에게 묻는다", Payload: printsOf(turnSource(session, 3)), Query: []float32{1, 0, 0}, Model: "m", Chars: 2000, Secret: 4000}
	scope := memory.ScopeOf(session)
	base, err := db.Deliver(scope, req)
	if err != nil {
		t.Fatal(err)
	}
	// In deliverChat only the short record points along this axis. Its
	// additional similarity rank is first; every other candidate follows it.
	req.RecentQuery = []float32{0, 1, 0}
	recent, err := db.Deliver(scope, req)
	if err != nil {
		t.Fatal(err)
	}
	if !recent.RecentQueryUsed || recent.Candidates != base.Candidates {
		t.Fatalf("recent query not used or candidates changed: %+v", recent)
	}
	baseRanks := map[string]int{}
	for _, row := range base.Trace {
		baseRanks[row.ID] = row.Rank
	}
	foundShort, changed := false, false
	for _, row := range recent.Trace {
		if row.ID == ids["short"] {
			foundShort = true
			if row.Rank != baseRanks[row.ID] {
				t.Fatalf("closest recent candidate must receive no additional rank: %v", row)
			}
		} else if row.Rank > baseRanks[row.ID] {
			changed = true
		}
	}
	if !foundShort || !changed {
		t.Fatal("removing the additional rank must change the production trace")
	}
	for _, bad := range [][]float32{{float32(math.NaN()), 0, 0}, {1, 0}} {
		req.RecentQuery = bad
		fallback, err := db.Deliver(scope, req)
		if err != nil {
			t.Fatal(err)
		}
		if fallback.RecentQueryUsed || fallback.RecentQueryError == nil {
			t.Fatalf("missing optional-query failure diagnostic: %+v", fallback)
		}
		if fallback.Block != base.Block || !reflect.DeepEqual(fallback.IDs, base.IDs) || !reflect.DeepEqual(fallback.Trace, base.Trace) {
			t.Fatal("optional query failure changed input-only delivery")
		}
	}
}
