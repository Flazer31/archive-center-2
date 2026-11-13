package memory

import (
	"strings"
	"testing"
)

// A word is counted once per turn it is in, also inside a longer word, and
// the counts decide recall as counting each turn's text would.
func TestTurnCountsReadEachTextOnce(t *testing.T) {
	n := map[string]int{}
	countTurns([]string{"주막", "흑요석", "단검"}, []string{
		"주막에 갔다. 주막은 붐볐다", // twice in one turn: once
		"큰주막에서 흑요석을 보았다",  // inside a longer word
		"단검",
		"아무 일 없음",
	}, n)
	if n["주막"] != 2 || n["흑요석"] != 1 || n["단검"] != 1 {
		t.Fatalf("counts %v", n)
	}
	k := Known{Memories: []KnownMemory{{ID: "M1", Turn: 1, Memory: Memory{Kind: "event", Body: "주막 흑요석 단검"}}}}
	// 흑요석 (in 1 earlier turn + this) and 단검 (1 + this): 1/2 + 1/2 = 1
	if sel := Select(k, "흑요석 단검을 꺼냈다", []string{"큰주막에서 흑요석을 보았다", "단검"}); len(sel.Memories) != 1 {
		t.Fatalf("not recalled: %+v", sel.Memories)
	}
	// 주막 is in every earlier turn: 1/3 alone
	if sel := Select(k, "주막에 들렀다", []string{"주막에 갔다", "큰주막에서"}); len(sel.Memories) != 0 {
		t.Fatalf("recalled by a word of every turn: %+v", sel.Memories)
	}
}

// The counts are those of searching each word in each text, also for words
// that are prefixes or suffixes of each other and texts that repeat one
// letter (each text is read once whatever it repeats).
func TestTurnCountsMatchSearchingEachWord(t *testing.T) {
	words := []string{"aaaa", "aa", "ab", "bab", "주막", "큰주막", "막에", "𝄞음표", "é"}
	texts := []string{
		strings.Repeat("a", 5000),
		"babab ab",
		"큰주막에서 막에 𝄞음표",
		"주막",
		"e\u0301 é",
		"",
	}
	n := map[string]int{}
	countTurns(words, texts, n)
	for _, w := range words {
		want := 0
		for _, x := range texts {
			if strings.Contains(x, w) {
				want++
			}
		}
		if n[w] != want {
			t.Errorf("%q: %d texts, searching finds %d", w, n[w], want)
		}
	}
}
