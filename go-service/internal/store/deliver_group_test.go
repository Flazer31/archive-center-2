package store

import (
	"strings"
	"testing"

	"archivecenter2/goservice/internal/memory"
)

// Lines sharing one boundary show it once under a header; a line alone keeps
// it compact on its line; the text never grows. Names are made up.
func TestGroupLines(t *testing.T) {
	names := map[string]string{"N1": "가람", "N2": "다온", "N3": "성문 경비대", "N4": "하율"}
	aw := func(kind, who string) memory.KnownAwareness {
		return memory.KnownAwareness{Kind: kind, Knows: memory.Knows{Who: who}}
	}
	shared := []memory.KnownAwareness{aw("unaware", "N2"), aw("unaware", "N3"), aw("hide_from", "N3")}
	alone := []memory.KnownAwareness{aw("unaware", "N4"), aw("unaware", "N4")}
	via := []memory.KnownAwareness{{Kind: "knows", Knows: memory.Knows{Who: "N4", Via: "전언", From: "N1"}}}
	recs := []struct {
		body  string
		aware []memory.KnownAwareness
	}{
		{"가람은 북쪽 기사단 소속이다.", shared},
		{"평범한 줄.", nil},
		{"가람이 상인 행세로 성문을 지났다.", shared},
		{"하율은 밀서가 바뀐 줄 모른다.", alone},
		{"하율이 다온에게서 들었다.", via},
		{"가람은 밀서를 짐 바닥에 숨겼다.", shared},
	}
	var lines []string
	var rendered []renderedRecord
	var at []int
	for i, r := range recs {
		m := memory.KnownMemory{ID: "M" + string(rune('1'+i)), Turn: i + 1, Memory: memory.Memory{Kind: "event", Body: r.body}}
		lines = append(lines, deliveryLine(m, names, "", r.aware))
		rendered = append(rendered, renderedRecord{&m, "", r.aware})
		at = append(at, i)
	}
	got := groupLines(lines, rendered, at, names)
	want := []string{
		"- [for the lines below] unaware, hide from: 성문 경비대 | unaware: 다온",
		"  - (T1) event 가람은 북쪽 기사단 소속이다.",
		"  - (T3) event 가람이 상인 행세로 성문을 지났다.",
		"  - (T6) event 가람은 밀서를 짐 바닥에 숨겼다.",
		"- (T2) event 평범한 줄.",
		"- (T4) event 하율은 밀서가 바뀐 줄 모른다. | unaware: 하율",
		lines[4], // how one record came to know stays on its line
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if n, o := len([]rune(strings.Join(got, "\n"))), len([]rune(strings.Join(lines, "\n"))); n >= o {
		t.Fatalf("grouped %d chars, lines %d", n, o)
	}
	// two short lines whose header would cost more than it saves stay apart
	short := []memory.KnownAwareness{aw("unaware", "N2")}
	m1 := memory.KnownMemory{ID: "M1", Turn: 1, Memory: memory.Memory{Kind: "event", Body: "a"}}
	m2 := memory.KnownMemory{ID: "M2", Turn: 2, Memory: memory.Memory{Kind: "event", Body: "b"}}
	l2 := []string{deliveryLine(m1, names, "", short), deliveryLine(m2, names, "", short)}
	got = groupLines(l2, []renderedRecord{{&m1, "", short}, {&m2, "", short}}, []int{0, 1}, names)
	if strings.Join(got, "\n") != strings.Join(l2, "\n") {
		t.Fatalf("a header longer than it saves was written:\n%s", strings.Join(got, "\n"))
	}
}
