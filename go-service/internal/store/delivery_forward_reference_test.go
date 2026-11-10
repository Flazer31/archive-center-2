package store

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"archivecenter2/goservice/internal/memory"
)

// Only SQL is substituted. The production Deliver owner still selects, charges,
// carries awareness, shares basis text and partitions the final transport.
func TestDeliverDefinesForwardBasisOnSource(t *testing.T) {
	for _, secret := range []bool{false, true} {
		t.Run(fmt.Sprintf("secret_%t", secret), func(t *testing.T) {
			body := `대상이 "원문 [text B9]"와 달 🌙을 기억했지만 결론은 아직 확인하지 못했다.`
			source := memory.Memory{Kind: "event", Body: body, Roles: &memory.Roles{Actor: memory.Who{"N1"}}, Time: "첫날 밤", Setting: "국경", Terms: "확인 전 단정 금지"}
			if secret {
				source.HideFrom = memory.Who{"N1"}
			}
			mems := []memory.Memory{
				source,
				{Kind: "view", Owner: "N1", Body: "가능성을 의심한다", Basis: &memory.Basis{Ref: "M1", Via: "소문", Confidence: "낮음"}, Affect: &memory.Affect{Intensity: "강함", Persistence: "남음"}, Differs: "unknown"},
				{Kind: "view", Owner: "N1", Body: "그 소문이 불안하다", Basis: &memory.Basis{Ref: "M1", Via: "추론"}},
			}
			for i := range mems {
				mems[i].Scene = "S1"
			}
			f := &contrastFixture{memories: mems}
			db := fixtureDB(t, f)
			req := syntheticRequest()
			req.Query = nil
			got, err := db.Deliver(syntheticScope(), req)
			if err != nil {
				t.Fatal(err)
			}
			wantIDs := make([]string, len(mems))
			for i := range wantIDs {
				wantIDs[i] = fmt.Sprintf("M%d", i+1)
			}
			if !reflect.DeepEqual(got.IDs, wantIDs) {
				t.Fatalf("selected records/order changed: %v", got.IDs)
			}
			lines := strings.Split(strings.TrimSuffix(strings.SplitN(got.Block, "\n", 2)[1], "\n"), "\n")
			definition := `[text B1="` + body + `"]`
			if !strings.Contains(lines[0], definition) || strings.Contains(lines[1], `based on B1 "`) || !strings.Contains(lines[1], "based on B1") || !strings.Contains(lines[2], "based on B1") {
				t.Fatalf("forward definition was not moved to its source: %q", lines)
			}
			// Reconstruct the previous representation from the immutable fixture,
			// then undo just C's two marker substitutions, byte for byte.
			names := map[string]string{"N1": "대상"}
			var aware []memory.KnownAwareness
			if secret {
				aware = []memory.KnownAwareness{{Target: "M1", Kind: "hide_from", Knows: memory.Knows{Who: "N1"}}}
			}
			var oldLines []string
			charge := 0
			for i, m := range mems {
				km := memory.KnownMemory{ID: wantIDs[i], Turn: 1, Job: 1, Memory: m}
				basis := ""
				if i == 1 {
					basis = `based on B1 "` + body + `"`
				} else if i == 2 {
					basis = "based on B1"
				}
				line := deliveryLine(km, names, basis, aware)
				charge += recordSize(memory.RenderKnown(memory.Selection{Memories: []memory.KnownMemory{km}, Awareness: aware}, nil), line)
				if i == 0 {
					km.Body = "[text B1]"
					line = deliveryLine(km, names, basis, aware)
				}
				oldLines = append(oldLines, line)
			}
			inverse := append([]string(nil), lines...)
			inverse[0] = strings.Replace(inverse[0], definition, "[text B1]", 1)
			inverse[1] = strings.Replace(inverse[1], "based on B1", `based on B1 "`+body+`"`, 1)
			oldBody := strings.Join(oldLines, "\n") + "\n"
			if strings.Join(inverse, "\n")+"\n" != oldBody || len([]rune(strings.Join(lines, "\n"))) != len([]rune(strings.Join(oldLines, "\n"))) {
				t.Fatalf("content, qualifiers, awareness or combined Unicode count changed: %q / %q", inverse, oldLines)
			}
			if got.Chars+got.SecretChars != charge || (secret && (got.SecretChars != charge || got.Chars != 0)) || (!secret && (got.Chars != charge || got.SecretChars != 0)) {
				t.Fatalf("selection charges changed: general=%d secret=%d want=%d", got.Chars, got.SecretChars, charge)
			}
			for _, id := range wantIDs {
				if got.SecretIDs[id] != secret {
					t.Fatalf("lane changed for %s", id)
				}
			}
			assertTransportLines(t, got)
			if got.BlockChars != len([]rune(got.Block)) || got.Block != deliveryBlock(lines) {
				t.Fatal("final block size or fingerprint is stale")
			}
		})
	}
}

func TestShareDeliveryBasisKeepsOtherRepresentations(t *testing.T) {
	longBody := `긴 원문 "인용"과 🌙을 그대로 보존하며 같은 글이라도 다른 기록은 공유하지 않는다.`
	for _, tc := range []struct {
		name     string
		body     string
		ref      string
		viewOnly bool
	}{
		{name: "backward_reference", body: longBody, ref: "M1"},
		{name: "short_body", body: "옴", ref: "M1"},
		{name: "effect_reference", body: longBody, ref: "M1.1"},
		{name: "source_absent", body: longBody, ref: "M1", viewOnly: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := memory.KnownMemory{ID: "M1", Turn: 1, Memory: memory.Memory{Kind: "event", Body: tc.body}}
			view := memory.KnownMemory{ID: "M2", Turn: 2, Memory: memory.Memory{Kind: "view", Body: "자신의 별도 해석", Basis: &memory.Basis{Ref: tc.ref}}}
			basis := `based on B1 "` + tc.body + `"`
			rendered := []renderedRecord{{memory: &view, basis: basis}}
			ids := []string{view.ID}
			if !tc.viewOnly {
				rendered = append(rendered, renderedRecord{memory: &source})
				ids = append(ids, source.ID)
			}
			var lines []string
			for _, r := range rendered {
				lines = append(lines, deliveryLine(*r.memory, nil, r.basis, nil))
			}
			want := append([]string(nil), lines...)
			if tc.name == "backward_reference" {
				source.Body = "[text B1]"
				want[1] = deliveryLine(source, nil, "", nil)
				source.Body = tc.body
			}
			shareDeliveryBasis(lines, ids, rendered, map[string]string{tc.ref: "B1"}, nil)
			if !reflect.DeepEqual(lines, want) || source.Body != tc.body || rendered[0].basis != basis {
				t.Fatalf("existing representation or stored input changed: %q want %q", lines, want)
			}
		})
	}
}

func TestShareDeliveryBasisKeepsNestedViewDefinitions(t *testing.T) {
	mems := []memory.KnownMemory{
		{ID: "M1", Turn: 1, Memory: memory.Memory{Kind: "event", Body: "처음 사건 본문은 충분히 길며 손대지 않은 정확한 원문이다."}},
		{ID: "M2", Turn: 2, Memory: memory.Memory{Kind: "view", Body: "첫 사건에 대한 자신의 해석도 다음 관점의 근거가 된다.", Basis: &memory.Basis{Ref: "M1"}}},
		{ID: "M3", Turn: 3, Memory: memory.Memory{Kind: "view", Body: "두 번째 해석을 믿는다", Basis: &memory.Basis{Ref: "M2"}}},
		{ID: "M4", Turn: 4, Memory: memory.Memory{Kind: "event", Body: "처음 사건 본문은 충분히 길며 손대지 않은 정확한 원문이다."}},
	}
	var rendered []renderedRecord
	var lines, ids []string
	for i := range mems {
		basis := ""
		if i == 1 || i == 2 {
			basis = fmt.Sprintf("based on B%d \"%s\"", i, mems[i-1].Body)
		}
		rendered = append(rendered, renderedRecord{memory: &mems[i], basis: basis})
		lines = append(lines, deliveryLine(mems[i], nil, basis, nil))
		ids = append(ids, mems[i].ID)
	}
	otherOccurrence := lines[3]
	shareDeliveryBasis(lines, ids, rendered, map[string]string{"M1": "B1", "M2": "B2"}, nil)
	for i, mark := range []string{"B1", "B2"} {
		if !strings.Contains(lines[i], `[text `+mark+`="`+mems[i].Body+`"]`) || strings.Contains(lines[i+1], "based on "+mark+" \"") {
			t.Fatalf("nested source/definition overwritten: %q", lines)
		}
	}
	if lines[3] != otherOccurrence || rendered[1].basis != `based on B1 "`+mems[0].Body+`"` {
		t.Fatal("another occurrence or original render input changed")
	}
}
