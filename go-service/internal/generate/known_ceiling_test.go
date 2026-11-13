package generate

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/provider"
)

// lenaModel writes one long fact about 리나 a turn: the first turn names her,
// later turns use her id from KNOWN. It records the prompts.
type lenaModel struct {
	mu      sync.Mutex
	prompts []string
}

var lenaID = regexp.MustCompile(`\[(N\d+)\] 리나`)

func (f *lenaModel) Chat(ctx context.Context, prompt string) (provider.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prompts = append(f.prompts, prompt)
	n := len(f.prompts)
	body := fmt.Sprintf("리나 사실%d %s", n, strings.Repeat("가", 300))
	if m := lenaID.FindStringSubmatch(prompt); m != nil {
		return provider.Result{Content: fmt.Sprintf(`{"memories":[{"id":"m1","kind":"fact","about":"%s","body":"%s"}]}`, m[1], body)}, nil
	}
	return provider.Result{Content: fmt.Sprintf(`{"entities":[{"id":"n1","name":"리나","type":"person"}],"memories":[{"id":"m1","kind":"fact","about":"n1","body":"%s"}]}`, body)}, nil
}

// The critic setting known_chars bounds a generation call's whole request (a
// tester set 3,000 and the request did not change, Beta 0.0.1 had left it
// unapplied; since 1.0.8 the ceiling is on the whole request). The stored
// memory gets what the instructions and the turn's text leave: over the
// ceiling whole records are left out, the oldest named fact first, and under
// the same ceiling a longer reply leaves less; with none (0) the turn reads
// them all.
func TestKnownCharsBoundsTheGenerationPrompt(t *testing.T) {
	third := func(t *testing.T, ceiling int, longer string) string {
		db := testDB(t)
		m := &lenaModel{}
		w := NewWorker(db, m, "fake", time.Minute)
		s := lifecycle.NewSession("chat:A")
		var rows []lifecycle.Row
		for i := 1; i <= 3; i++ {
			reply := fmt.Sprintf("리나가 %d번째로 답한다", i)
			if i == 3 {
				w.SetKnownMax(ceiling)
				reply += longer
			}
			rows = append(rows, row(fmt.Sprintf("u%d", i), "user", "", fmt.Sprintf("리나에게 %d번째로 묻는다", i)), row(fmt.Sprintf("a%d", i), "char", fmt.Sprintf("g%d", i), reply))
			s.Apply(snap(lifecycle.SourceOutputEvent, rows...))
			goOn(s, rows...)
			run(w, s)
		}
		if len(m.prompts) != 3 {
			t.Fatalf("calls %d", len(m.prompts))
		}
		return m.prompts[2]
	}
	known := func(p string) string { return p[strings.Index(p, "\nKNOWN:\n"):strings.Index(p, "\nTHIS TURN")] }
	size := func(p string) int { return len([]rune(p)) }
	long := " " + strings.Repeat("나", 200) // less than one fact (about 310 chars)
	var all, longAll, bounded, roomy, roomyLong string
	t.Run("none", func(t *testing.T) { all = third(t, 0, ""); longAll = third(t, 0, long) })
	if all == "" {
		t.Skip("no database")
	}
	t.Run("ceiling", func(t *testing.T) {
		bounded = third(t, size(all)-100, "")
		roomy = third(t, size(all), "")
		roomyLong = third(t, size(all), long)
	})
	if !strings.Contains(known(all), "리나 사실1") || !strings.Contains(known(all), "리나 사실2") || !strings.Contains(known(longAll), "리나 사실1") {
		t.Fatalf("with no ceiling both facts are read:\n%s", known(all))
	}
	// 100 chars under the whole request: the oldest named fact goes, the
	// newer one and the name stay, and the request is within the ceiling
	if b := known(bounded); strings.Contains(b, "리나 사실1") || !strings.Contains(b, "리나 사실2") || !strings.Contains(b, "리나 (person)") || size(bounded) > size(all)-100 {
		t.Fatalf("bounded request %d chars (ceiling %d):\n%s", size(bounded), size(all)-100, b)
	}
	// at the request's own size nothing goes; the same ceiling with a reply
	// 201 chars longer leaves the stored memory less, and the oldest fact goes
	if !strings.Contains(known(roomy), "리나 사실1") || size(roomy) != size(all) {
		t.Fatalf("at its own size the request is unchanged: %d vs %d", size(roomy), size(all))
	}
	if strings.Contains(known(roomyLong), "리나 사실1") || !strings.Contains(known(roomyLong), "리나 사실2") || size(roomyLong) > size(all) {
		t.Fatalf("a longer reply leaves less for the stored memory: %d chars (ceiling %d):\n%s", size(roomyLong), size(all), known(roomyLong))
	}
}
