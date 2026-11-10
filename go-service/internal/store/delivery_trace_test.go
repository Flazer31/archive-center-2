package store

import (
	"testing"
	"time"
)

// A chat keeps its latest deliveries only; another chat's are untouched.
func TestDeliveryTracesKeepTheLatest(t *testing.T) {
	db := testDB(t)
	at := time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)
	for i := 0; i < deliveryTracesKept+2; i++ {
		sum := DeliverySummary{Input: "입력", Candidates: i}
		if _, err := db.SaveDeliveryTrace("chat:A", at.Add(time.Duration(i)*time.Second), sum, "", []TraceRow{{ID: "M1", Result: "over_ceiling"}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.SaveDeliveryTrace("chat:B", at, DeliverySummary{Input: "다른 채팅"}, "", nil); err != nil {
		t.Fatal(err)
	}
	got, err := db.DeliveryTraces("chat:A", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != deliveryTracesKept || got[0].Summary.Candidates != deliveryTracesKept+1 || got[len(got)-1].Summary.Candidates != 2 {
		t.Fatalf("kept %d, newest %d, oldest %d", len(got), got[0].Summary.Candidates, got[len(got)-1].Summary.Candidates)
	}
	if got[0].Trace[0].Result != "over_ceiling" {
		t.Fatalf("trace rows: %+v", got[0].Trace)
	}
	if b, err := db.DeliveryTraces("chat:B", 10); err != nil || len(b) != 1 || b[0].Trace == nil {
		t.Fatalf("other chat: %+v %v", b, err)
	}
}
