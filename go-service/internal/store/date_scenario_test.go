package store

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"archivecenter2/goservice/internal/ingest"
	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
)

// A chat observed turn by turn the way the plugin sends it, each turn's
// memory made (the critic's answer written here), then read and delivered
// (dates are hard to test by playing). Every kind of date a turn may get:
// a numeric variable, a Korean variable, none (carried on), a Korean date
// in the reply, the critic's story_date, and a chat counted in Days until
// a date appears.
func TestDatesThroughAChat(t *testing.T) {
	db := testDB(t)
	type turn struct {
		variable, reply, critic string // critic: the answer's top-level fields
		want, shown             string // stored date text; shown on a delivered line
	}
	run := func(chat string, turns []turn) {
		t.Helper()
		s := (*lifecycle.Session)(nil)
		var rows []any
		var sources []memory.Source
		for i, tc := range turns {
			rows = append(rows,
				map[string]any{"i": 2 * i, "role": "user", "chat_id": fmt.Sprintf("u%d", i), "data": map[string]string{"text": fmt.Sprintf("입력 %d", i)}},
				map[string]any{"i": 2*i + 1, "role": "char", "chat_id": fmt.Sprintf("a%d", i), "data": map[string]string{"text": tc.reply}})
			c := map[string]any{"chat_id": chat, "rows": rows}
			if tc.variable != "" {
				c["scriptstate"] = map[string]any{"clock": map[string]string{"text": tc.variable}}
			}
			raw, _ := json.Marshal(map[string]any{"hook": "output", "event_chat": c})
			observed, err := ingest.FromEvent(raw, ingest.NewTextCache())
			if err != nil || len(observed) != 1 {
				t.Fatalf("%s turn %d: observe %v %v", chat, i+1, observed, err)
			}
			if s == nil {
				s = lifecycle.NewSession(observed[0].Snapshot.SessionKey)
			}
			s.Apply(observed[0].Snapshot)
			if err := db.SaveSession(s, s.TakeEvents(), s.TakeRenames()); err != nil {
				t.Fatal(err)
			}
			active := s.ActiveTurns()
			if len(active) != i+1 {
				t.Fatalf("%s turn %d: %d turns", chat, i+1, len(active))
			}
			src := memory.SourceOf(s.Key, active[i])
			sources = append(sources, src)
			job, err := db.JobFor(src)
			if err != nil {
				t.Fatal(err)
			}
			answer := fmt.Sprintf(`{%s"memories":[{"id":"m1","kind":"event","body":"리나의 일 %d"}]}`, tc.critic, i+1)
			out, notes := memory.Parse(answer)
			if _, err := db.SaveGeneration(job, Generation{Output: out, Response: answer, Notes: notes}); err != nil {
				t.Fatal(err)
			}
		}
		k, err := db.KnownFor(sources)
		if err != nil || len(k.Memories) != len(turns) {
			t.Fatalf("%s: known %+v %v", chat, k, err)
		}
		for i, m := range k.Memories {
			if m.Date != turns[i].want {
				t.Errorf("%s turn %d: stored date %q, want %q", chat, i+1, m.Date, turns[i].want)
			}
		}
		// the latest turn is the request's own: the earlier ones are delivered
		d, err := db.Deliver(memory.Own(s), DeliveryRequest{Input: "리나", Payload: []TextPrint{{Role: "user"}}, Chars: 10000})
		if err != nil {
			t.Fatal(err)
		}
		for i, tc := range turns[:len(turns)-1] {
			stamp := fmt.Sprintf("(T%d", i+1)
			if tc.shown != "" {
				stamp += ", " + tc.shown
			}
			stamp += ") event 리나의 일 " + fmt.Sprint(i+1)
			if !strings.Contains(d.Block, stamp) {
				t.Errorf("%s turn %d: delivered block lacks %q:\n%s", chat, i+1, stamp, d.Block)
			}
		}
	}
	run("dated", []turn{
		{"1423-05-15", "아침이다.", `"elapsed_days":0,`, "clock = 1423-05-15", "1423/05/15"},
		{"1423년 5월 16일 (화)", "다음 날이다.", `"elapsed_days":1,`, "clock = 1423/05/16", "1423/05/16"},
		{"", "또 하루가 갔다.", `"elapsed_days":1,`, "date = 1423/05/17", "1423/05/17"},
		{"", "달력에는 1423년 5월 20일이라고 적혀 있다.", `"elapsed_days":3,`, "body = 1423/05/20", "1423/05/20"},
		{"", "오월 스무하루, 비가 그쳤다.", `"elapsed_days":1,"story_date":"1423/05/21",`, "story = 1423/05/21", "1423/05/21"},
		{"", "이틀이 더 지났다.", `"elapsed_days":2,`, "date = 1423/05/23", "1423/05/23"},
	})
	run("counted", []turn{
		{"", "처음 만났다.", `"elapsed_days":0,`, "Day 0", "Day 0"},
		{"", "사흘이 지났다.", `"elapsed_days":3,`, "Day 3", "Day 3"},
		{"", "편지에는 1423/06/01이라고 쓰여 있었다.", `"elapsed_days":1,`, "body = 1423/06/01", "1423/06/01"},
		{"", "하루가 더 갔다.", `"elapsed_days":1,`, "date = 1423/06/02", "1423/06/02"},
	})
}
