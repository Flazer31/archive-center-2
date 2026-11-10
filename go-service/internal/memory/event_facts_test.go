package memory

import (
	"slices"
	"testing"
)

func TestSelectEventDoesNotExpandFactsForGeneration(t *testing.T) {
	for _, indirect := range []bool{false, true} {
		for _, reversed := range []bool{false, true} {
			name := "recall"
			if indirect {
				name = "view_basis"
			}
			if reversed {
				name += "_reversed"
			}
			t.Run(name, func(t *testing.T) {
				k := Known{
					Names: []KnownName{{ID: "N1", Name: "리나"}, {ID: "N2", Name: "세계 규칙"}, {ID: "N3", Name: "도윤"}},
					Memories: []KnownMemory{
						{ID: "M1", Turn: 1, Memory: Memory{Kind: "event", Body: "진홍인장이 빛났다", Roles: &Roles{Actor: []string{"N1"}}}},
						{ID: "M2", Turn: 1, Memory: Memory{Kind: "fact", About: "N2", Body: "봉인은 같은 등급의 열쇠로만 열린다", At: "M1"}},
						{ID: "M3", Turn: 1, Memory: Memory{Kind: "fact", About: "N2", Body: "새벽에는 달빛이 약해진다", At: "M4"}},
						{ID: "M4", Turn: 1, Memory: Memory{Kind: "event", Body: "호수에서 새벽을 맞았다"}},
						{ID: "M5", Turn: 1, Memory: Memory{Kind: "fact", About: "N2", Body: "바닷물은 짜다"}},
						{ID: "M6", Turn: 1, Memory: Memory{Kind: "view", Owner: "N1", Body: "실패가 두렵다", At: "M1", Basis: &Basis{Ref: "M1"}}},
						{ID: "M7", Turn: 1, Memory: Memory{Kind: "fact", About: "N2", Body: "고대 문헌의 기록", At: "M2"}},
					},
					Awareness: []KnownAwareness{{Target: "M2", Kind: "unaware", Knows: Knows{Who: "N3"}}},
				}
				input := "진홍인장을 떠올렸다"
				want := []string{"M1"}
				if indirect {
					input = "리나가 앉았다"
					k.Scenes = []KnownScene{{ID: "S2", Change: "open", Turn: 2}}
					k.Memories = append(k.Memories, KnownMemory{ID: "M8", Turn: 2, Memory: Memory{Kind: "view", Owner: "N1", Body: "그때의 경험을 믿는다", Basis: &Basis{Ref: "M1"}}})
					want = append(want, "M8")
				}
				if reversed {
					slices.Reverse(k.Memories)
				}
				sel := Select(k, input, nil)
				var got []string
				for _, m := range sel.Memories {
					got = append(got, m.ID)
				}
				slices.Sort(got)
				if !slices.Equal(got, want) {
					t.Fatalf("selected %v, want %v: delivery completion must not expand generation's reading range", got, want)
				}
				if len(sel.Awareness) != 0 {
					t.Fatalf("unselected fact's awareness expanded generation input: %+v", sel.Awareness)
				}
			})
		}
	}
}

func TestCurrentFactsPreserveLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status string
		effect string
		want   bool
	}{
		{"open", "open", "", true},
		{"closed", "fulfilled", "", false},
		{"closed_by_effect", "open", "fulfilled", false},
		{"reopened_by_effect", "fulfilled", "resumed", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := Known{Memories: []KnownMemory{
				{ID: "M1", Turn: 1, Memory: Memory{Kind: "event", Body: "진홍인장이 빛났다"}},
				{ID: "M2", Turn: 1, Memory: Memory{Kind: "fact", Body: "낡은 통행 규칙", At: "M1"}},
				{ID: "M3", Turn: 2, Memory: Memory{Kind: "fact", Body: "새로운 통행 규칙", At: "M1", Was: Refs{"M2"}}},
				{ID: "M4", Turn: 1, Memory: Memory{Kind: "fact", Mode: "의무", Body: "문지기에게 열쇠를 돌려준다", At: "M1", Status: tc.status}},
			}}
			if tc.effect != "" {
				k.Memories = append(k.Memories, KnownMemory{ID: "M5", Turn: 3, Memory: Memory{Kind: "event", Body: "약속의 상황이 달라졌다", Effects: []Effect{{About: "M4", Status: tc.effect}}}})
			}
			ids := map[string]bool{}
			for _, m := range CurrentFacts(k) {
				ids[m.ID] = true
			}
			if ids["M1"] || !ids["M3"] || ids["M2"] || ids["M4"] != tc.want || ids["M5"] {
				t.Fatalf("selected %v; want current rule, no replaced rule, obligation present=%v", ids, tc.want)
			}
		})
	}
}
