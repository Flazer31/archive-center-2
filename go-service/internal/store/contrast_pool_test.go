package store

import (
	"reflect"
	"testing"

	"archivecenter2/goservice/internal/memory"
)

// 1.1 B2: the scene contrast is centered on the records the request can
// deliver, not every memory of the chat. Live requests right after the
// recent window got no memory: the chat's other memories pulled the mean
// toward the scene and every baseline pick scored <=0. Here two muted
// memories (never candidates) lie along the query: with the whole chat as
// the mean M1 and M2 both score <=0 (nothing delivered); with the
// candidates as the mean M1 scores above zero and stays.
func TestContrastCenteredOnCandidates(t *testing.T) {
	mems := []memory.Memory{
		{Kind: "fact", About: "N1", Mode: "상태", Body: "첫째 기록"},
		{Kind: "fact", About: "N1", Mode: "성향", Body: "둘째 기록"},
		{Kind: "fact", About: "N1", Mode: "관계", Body: "셋째 기록"},
		{Kind: "fact", About: "N1", Mode: "습관", Body: "넷째 기록"},
	}
	vectors := map[int64][]float32{1: {0.8, 0.6}, 2: {0, 1}, 3: {1, 0}, 4: {1, 0}}
	f := &contrastFixture{memories: mems, vectors: vectors, holds: map[int64]string{3: HoldMute, 4: HoldMute}}
	got, err := fixtureDB(t, f).Deliver(syntheticScope(), syntheticRequest())
	if err != nil {
		t.Fatal(err)
	}
	result := map[string]string{}
	for _, r := range got.Trace {
		result[r.ID] = r.Result
	}
	if got.Candidates != 2 {
		t.Fatalf("fixture: the muted records are not candidates: %d %v", got.Candidates, result)
	}
	if !reflect.DeepEqual(got.IDs, []string{"M1"}) || result["M1"] != "taken" || result["M2"] != "contrast_removed" {
		t.Fatalf("ids %v results %v", got.IDs, result)
	}
}
