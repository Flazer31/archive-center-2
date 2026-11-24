package store

import (
	"archivecenter2/goservice/internal/memory"
	"slices"
	"strings"
	"testing"
)

func assertTransportLines(t *testing.T, d Delivery) {
	t.Helper()
	if len(d.Lines) != len(d.IDs) {
		t.Fatal("fixture lines/IDs differ")
	}
	ordinary, secret := []string{}, []string{}
	for i, line := range d.Lines {
		if d.SecretIDs[d.IDs[i]] {
			secret = append(secret, line)
		} else {
			ordinary = append(ordinary, line)
		}
	}
	// each lane sends every selected line of its own once and nothing else;
	// a line under a shared header carries that header's awareness (1.3)
	sameRecords(t, "general", d.PDFBlock, append(append([]string{}, d.Names...), ordinary...))
	sameRecords(t, "secret", d.RetainedBlock, append(append([]string{}, d.SecretNames...), secret...))
}

type sentRecord struct {
	line, groupBodies string
}

// A grouped line gets its header's awareness and its group's displayed bodies.
func sentLines(block string) []sentRecord {
	if block == "" {
		return nil
	}
	var out []sentRecord
	lines := strings.Split(strings.TrimSuffix(block, "\n"), "\n")[1:]
	for len(lines) > 0 {
		line := lines[0]
		lines = lines[1:]
		header, grouped := strings.CutPrefix(line, "- [for the lines below] ")
		if !grouped {
			out = append(out, sentRecord{line: line})
			continue
		}
		n := 0
		for n < len(lines) && strings.HasPrefix(lines[n], "  - ") {
			n++
		}
		bodies := strings.Join(lines[:n], "\n")
		for _, member := range lines[:n] {
			out = append(out, sentRecord{member[2:] + " | " + header, bodies})
		}
		lines = lines[n:]
	}
	return out
}

// awarenessCut splits a line at its awareness: the text before it and the
// people it names.
func awarenessCut(l string) (string, []string) {
	cut := len(l)
	for _, k := range []string{" | knows: ", " | unaware: ", " | hide from: ", " | unaware, hide from: ", " | allow to: ", " | knows at T", " | unaware at T"} {
		if i := strings.Index(l, k); i >= 0 && i < cut {
			cut = i
		}
	}
	var who []string
	for _, part := range strings.Split(l[cut:], " | ") {
		if people, subject, ok := strings.Cut(part, " (what: "); ok {
			who = append(who, "what: "+subject)
			part = people
		}
		if _, rest, ok := strings.Cut(part, ": "); ok {
			for _, w := range strings.Split(rest, ", ") {
				who = append(who, strings.TrimSpace(w))
			}
		}
	}
	return l[:cut], who
}

// sameRecords: the block sends exactly the wanted lines (in any order), each
// with the same text and every person its awareness names.
func sameRecords(t *testing.T, lane, block string, want []string) {
	t.Helper()
	got := sentLines(block)
	if len(got) != len(want) {
		t.Fatalf("%s lane sends %d lines, selected %d:\n%s", lane, len(got), len(want), block)
	}
	used := make([]bool, len(got))
	for _, w := range want {
		wb, ww := awarenessCut(w)
		found := false
		for j, g := range got {
			gb, gw := awarenessCut(g.line)
			if used[j] || gb != wb {
				continue
			}
			for _, x := range ww {
				if slices.Contains(gw, x) {
					continue
				}
				// A shared header can leave its subject to the records under it.
				// Every expected person still has to appear in the awareness.
				if subject, ok := strings.CutPrefix(x, "what: "); ok && strings.Contains(g.groupBodies, strings.TrimSuffix(subject, ")")) {
					continue
				}
				t.Fatalf("%s lane lost %q on %q", lane, x, g.line)
			}
			used[j], found = true, true
			break
		}
		if !found {
			t.Fatalf("%s lane is missing %q:\n%s", lane, w, block)
		}
	}
}

func TestDeliverTransportLanesVerbatim(t *testing.T) {
	db, s, _ := deliverChat(t)
	for _, budgets := range [][2]int{{2000, 4000}, {2000, 0}, {0, 4000}} {
		d, err := db.Deliver(memory.ScopeOf(s), DeliveryRequest{Input: "리나에게 묻는다", Payload: printsOf(turnSource(s, 3)), Chars: budgets[0], Secret: budgets[1]})
		if err != nil {
			t.Fatal(err)
		}
		if d.Block == "" {
			t.Fatal("fixture selected no records")
		}
		assertTransportLines(t, d)
		if d.Chars > budgets[0] || d.SecretChars > budgets[1] {
			t.Fatal("ceiling changed")
		}
		if budgets[0] == 0 && d.PDFBlock != "" {
			t.Fatal("secret-only contains PDF lines")
		}
		if budgets[1] == 0 && d.RetainedBlock != "" {
			t.Fatal("ordinary-only contains retained lines")
		}
	}
}

func TestDeliverTransportSharedBasisAcrossLanes(t *testing.T) {
	db := testDB(t)
	s := chatOf(t, [][2]string{{"리나가 소문을 들었다", "들음"}, {"걸었다", "길"}}, "서진이 배신자라고?")
	a24Save(t, db, turnSource(s, 0), `{"entities":[{"id":"n1","name":"리나"},{"id":"n2","name":"서진"},{"id":"n3","name":"도윤"}],"memories":[{"id":"m0","kind":"event","body":"도윤이 서진의 배신을 말했다"},{"id":"m1","kind":"view","owner":"n1","about":"n2","aspect":"belief","body":"서진이 배신자라고 믿는다","basis":{"via":"report","ref":"m0"}},{"id":"m2","kind":"view","owner":"n3","about":"n2","aspect":"emotion","body":"서진이 배신자라 분노한다","basis":{"via":"inference","ref":"m0"},"hide_from":["n1"]}]}`)
	d, err := db.Deliver(memory.ScopeOf(s), DeliveryRequest{Input: "서진이 배신자라고?", Payload: printsOf(turnSource(s, 1)), Chars: 2000, Secret: 4000})
	if err != nil {
		t.Fatal(err)
	}
	assertTransportLines(t, d)
	if d.PDFBlock == "" || d.RetainedBlock == "" {
		t.Fatalf("must cover mixed lanes: %+v", d)
	}
	union := d.PDFBlock + d.RetainedBlock
	if strings.Count(d.PDFBlock, `[text B1="도윤이 서진의 배신을 말했다"]`) != 1 || strings.Count(union, "based on B1") != 2 || strings.Count(union, "도윤이 서진의 배신을 말했다") != 1 || strings.Contains(union, `based on B1 "`) {
		t.Fatal("shared basis definition/ref changed", union)
	}
	if !strings.Contains(d.RetainedBlock, "hide from: 리나") || strings.Contains(d.PDFBlock, "분노한다") {
		t.Fatal("awareness/lane protection lost")
	}
}
