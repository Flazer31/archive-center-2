package store

import (
	"slices"
	"strings"
	"testing"

	"archivecenter2/goservice/internal/memory"
)

func TestDeliverNamesInheritedSecrets(t *testing.T) {
	hidden := memory.Awareness{HideFrom: memory.Who{"N1"}}
	first, second := "열쇠는 푸른 상자 안에 있다", "편지는 다락에 있다 🌙"
	mems := []memory.Memory{
		{Kind: "event", Body: first, Awareness: hidden},
		{Kind: "event", Body: second, Awareness: hidden},
		{Kind: "view", Owner: "N1", At: "M1", Body: "첫 번째 생각"},
		{Kind: "view", Owner: "N1", At: "M2", Body: "두 번째 생각"},
		{Kind: "view", Owner: "N1", At: "M1", Body: "세 번째 생각"},
		{Kind: "view", Owner: "N1", At: "M2", Body: "네 번째 생각"},
		{Kind: "view", Owner: "N1", At: "M1", Basis: &memory.Basis{Ref: "M2"}, Body: "두 사건에 관한 생각"},
	}
	for i := range mems {
		mems[i].Scene = "S1"
	}
	db := fixtureDB(t, &contrastFixture{memories: mems})
	req := syntheticRequest()
	req.Query = nil
	req.Only = map[string]bool{"M3": true, "M4": true, "M5": true, "M6": true}
	deliver := func() Delivery {
		t.Helper()
		d, err := db.Deliver(syntheticScope(), req)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	d := deliver()
	if len(d.IDs) != len(req.Only) || d.PDFBlock != "" {
		t.Fatalf("inherited records: %v, PDF %q", d.IDs, d.PDFBlock)
	}
	for _, block := range []string{d.Block, d.RetainedBlock} {
		for i, record := range sentLines(block) {
			line := record.line
			want, other := first, second
			if strings.Contains(line, "두 번째") || strings.Contains(line, "네 번째") {
				want, other = second, first
			}
			if !strings.Contains(line, "what: "+want) || strings.Contains(line, other) {
				t.Errorf("line %d has the wrong secret: %s", i, line)
			}
		}
		if strings.Count(block, "[for the lines below]") != 2 {
			t.Errorf("expected a separate header per secret:\n%s", block)
		}
	}
	req.Secret = 0
	tight := deliver()
	if !slices.Equal(tight.IDs, d.IDs) || tight.Chars != d.Chars || tight.SecretChars != 0 {
		t.Fatalf("annotation changed the general selection: %+v / %+v", tight, d)
	}
	extra := 0
	for i, line := range d.Lines {
		extra += lineSize(line) - lineSize(tight.Lines[i])
	}
	if extra <= 0 || d.SecretChars != extra {
		t.Errorf("secret budget counted %d for %d extra characters", d.SecretChars, extra)
	}
	req.Secret = extra - 1
	partial := deliver()
	if !slices.Equal(partial.IDs, d.IDs) || partial.SecretChars > req.Secret ||
		strings.Count(partial.RetainedBlock, "번째 생각") != len(req.Only) {
		t.Fatalf("tight secret ceiling changed the records: %+v", partial)
	}
	req.Secret = 4000
	req.Only = map[string]bool{"M7": true}
	single := deliver()
	if len(single.IDs) != 1 || !single.SecretIDs["M7"] || single.Chars != 0 {
		t.Fatalf("the view should still be selected under its basis's ceiling: %+v", single)
	}
	line := single.Lines[0]
	if !strings.Contains(line, "hide from: 대상 (what: "+first+")") ||
		!strings.Contains(line, "hide from: 대상 (what: "+second+")") ||
		!strings.Contains(line, `based on B1 "`+second+`"`) || !strings.Contains(single.Block, line) ||
		lineSize(line) > single.SecretChars {
		t.Errorf("different secrets carried on one line, counted %d: %s", single.SecretChars, line)
	}
	req.Only = nil
	all := deliver()
	for _, block := range []string{all.Block, all.RetainedBlock} {
		if strings.Count(block, "[for the lines below]") != 2 {
			t.Errorf("expected both source groups in the block:\n%s", block)
		}
		for _, line := range strings.Split(block, "\n") {
			if strings.Contains(line, "[for the lines below]") && strings.Contains(line, "(what: ") {
				t.Errorf("the group includes its source, but its header repeats the body: %s", line)
			}
		}
	}
	for i, id := range []string{"M1", "M2"} {
		j := slices.Index(all.IDs, id)
		if j < 0 || strings.Count(all.Lines[j], mems[i].Body) != 1 {
			t.Errorf("source %s must state its secret once: %v", id, all.Lines)
		}
	}
}

func TestBoundaryKnowledgeTurns(t *testing.T) {
	names := map[string]string{"P": "민", "S": "윤"}
	r := renderedRecord{memory: &memory.KnownMemory{ID: "M1"}}
	secret := map[string]bool{"M1": true, "M2": true}
	unknown := memory.KnownAwareness{Target: "M1", Kind: "unaware", Turn: 2, Knows: memory.Knows{Who: "P"}}
	hidden := unknown
	hidden.Kind = "hide_from"
	known := unknown
	known.Kind, known.Turn = "knows", 5
	for _, origin := range []struct{ via, from, want string }{
		{"전언", "S", " (전언, from 윤)"}, {"", "S", " (from 윤)"}, {"직접", "", " (직접)"}, {"", "", ""},
	} {
		known.Via, known.From = origin.via, origin.from
		r.carried = []memory.KnownAwareness{known, hidden, unknown, known}
		label := namedBoundary(r, names, secret, nil)
		forms := []string{label.suffix}
		if origin.via == "" && origin.from == "" {
			forms = append(forms, label.compact, label.header)
		} else if label.header != "" {
			t.Errorf("qualified knowledge moved to a header: %s", label.header)
		}
		for _, shown := range forms {
			if !strings.Contains(shown, "unaware at T2: 민") || !strings.Contains(shown, "hide from: 민") ||
				strings.Count(shown, "knows at T5: 민"+origin.want) != 1 || strings.Contains(shown, "unaware: 민") ||
				(origin.from == "" && strings.Contains(shown, "from 윤")) {
				t.Errorf("stored via %q, from %q rendered as %q", origin.via, origin.from, shown)
			}
		}
	}
	for _, change := range []func(*memory.KnownAwareness){
		func(a *memory.KnownAwareness) { a.Turn = unknown.Turn },
		func(a *memory.KnownAwareness) { a.Turn = unknown.Turn - 1 },
		func(a *memory.KnownAwareness) { a.Target = "M2" },
		func(a *memory.KnownAwareness) { a.Who = "S" },
	} {
		other := known
		change(&other)
		r.carried = []memory.KnownAwareness{unknown, other}
		if shown := namedBoundary(r, names, secret, nil).suffix; strings.Contains(shown, " at T") {
			t.Errorf("unrelated or non-later knowledge was dated: %s", shown)
		}
	}
}

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
	var labels []boundaryLabel
	var at []int
	for i, r := range recs {
		m := memory.KnownMemory{ID: "M" + string(rune('1'+i)), Turn: i + 1, Memory: memory.Memory{Kind: "event", Body: r.body}}
		lines = append(lines, deliveryLine(m, names, "", r.aware))
		labels = append(labels, plainBoundary(r.aware, names))
		at = append(at, i)
	}
	got := groupLines(lines, labels, at)
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
	label := plainBoundary(short, names)
	got = groupLines(l2, []boundaryLabel{label, label}, []int{0, 1})
	if strings.Join(got, "\n") != strings.Join(l2, "\n") {
		t.Fatalf("a header longer than it saves was written:\n%s", strings.Join(got, "\n"))
	}
}
