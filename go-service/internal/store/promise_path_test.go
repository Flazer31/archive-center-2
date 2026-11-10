package store

import (
	"fmt"
	"strings"
	"testing"

	"archivecenter2/goservice/internal/memory"
)

// Synthetic model output: this tests persistence and delivery, not whether a
// model notices the promise. Facts precede their source event, as the generation
// instructions request. The full path must resolve those forward references.
func TestBaselinePromiseHistoryAndClosedCurrentFacts(t *testing.T) {
	db := testDB(t)
	s := chatOf(t, [][2]string{
		{"약속", "리나는 도윤에게 수로 지도를 그려 주겠다고 했다."},
		{"밤", "두 사람은 잠들었다."},
		{"아침", "아침을 먹었다."},
	}, "리나가 도윤에게 약속한 지도는?")
	_, items, notes := a24Save(t, db, turnSource(s, 0), `{"entities":[{"id":"n1","name":"리나"},{"id":"n2","name":"도윤"}],"memories":[
 {"id":"m1","kind":"fact","mode":"의무","about":"n1","to":"n2","body":"리나는 도윤에게 수로 지도를 그려 주기로 약속했다.","status":"active","at":"m4","q":"수로 지도를 그려 주겠다고 했다"},
 {"id":"m2","kind":"view","owner":"n2","about":"n1","aspect":"goal","body":"도윤은 리나가 자신의 옆자리에 앉기를 원하지만 리나는 응답하지 않았다."},
 {"id":"m3","kind":"fact","mode":"경계","about":"n1","body":"리나는 허락 없이 자신의 머리카락을 만지지 말라고 했다.","status":"active"},
 {"id":"m4","kind":"event","body":"리나가 도윤에게 수로 지도를 그려 주겠다고 말했다.","roles":{"actor":["n1"],"listener":["n2"]},"outcome":"success"}]}`)
	if len(notes) != 0 {
		t.Fatalf("forward references produced diagnostics: %v", notes)
	}
	promise, event := items.Memories[0], items.Memories[3]
	if promise.At != event.ID || promise.About != items.Names[0].ID || len(promise.To) != 1 || promise.To[0] != items.Names[1].ID {
		t.Fatalf("promise lost its event or parties: %+v", promise)
	}
	req := DeliveryRequest{Input: "리나가 도윤에게 약속한 지도는?", Payload: printsOf(turnSource(s, 2)), Chars: 2000, Secret: 4000}
	before, err := db.Deliver(memory.ScopeOf(s), req)
	if err != nil {
		t.Fatal(err)
	}
	if !has(before.IDs, promise.ID) || !strings.Contains(before.Block, "수로 지도") {
		t.Fatalf("open promise not delivered: %s", before.Block)
	}
	// A later model answer closes the existing promise; it does not invent a
	// replacement obligation. Store it in turn 2, before the held turn 3.
	_, done, notes := a24Save(t, db, turnSource(s, 1), fmt.Sprintf(`{"memories":[{"id":"m1","kind":"event","body":"리나가 도윤에게 완성한 수로 지도를 건넸다.","roles":{"actor":[%q],"target":[%q]},"outcome":"success","effects":[{"about":%q,"status":"fulfilled"}]}]}`, items.Names[0].ID, items.Names[1].ID, promise.ID))
	if len(notes) != 0 {
		t.Fatalf("completion diagnostics: %v", notes)
	}
	// Ask about the completed action: the original promise question is not
	// guaranteed to recall an event via this selector's lexical path.
	req.Input = "리나가 도윤에게 완성한 수로 지도를 건넸다. 그 지도를 펼쳐 본다."
	after, err := db.Deliver(memory.ScopeOf(s), req)
	if err != nil {
		t.Fatal(err)
	}
	// Oct-27 delivery may quote the original promise alongside completion;
	// current-fact state must still recognize the explicit closing effect.
	if !has(after.IDs, promise.ID) || !has(after.IDs, done.Memories[0].ID) {
		t.Fatalf("baseline promise history or completion missing: %s", after.Block)
	}
	known, err := db.CurrentKnown(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range memory.CurrentFacts(known) {
		if m.ID == promise.ID {
			t.Fatal("closed promise remained a current obligation")
		}
	}
	if !has(after.IDs, items.Memories[2].ID) {
		t.Fatalf("unrelated personal boundary disappeared: %s", after.Block)
	}
	var obligations int
	if err := db.sql.QueryRow("SELECT COUNT(*) FROM memories WHERE mode='의무'").Scan(&obligations); err != nil {
		t.Fatal(err)
	}
	if obligations != 1 {
		t.Fatalf("completion duplicated or deleted obligation history: %d", obligations)
	}
}

// A ref is evidence of a re-mention, not permission for Go to infer a new
// intention from its quote. This isolates why a model's over-broad duplicate
// decision cannot preserve a new plan merely by pointing to an old motive.
func TestMentionQuoteDoesNotBecomeANewPlan(t *testing.T) {
	db := testDB(t)
	s := chatOf(t, [][2]string{{"독학", "리나는 기술학교에서 배우는 편이 효율적이라고 여겼다."}, {"결심", "리나는 내일 기술학교에서 용접 기초를 배우겠다고 했다."}}, "리나는 내일 무엇을 할까?")
	j1, old, _ := a24Save(t, db, turnSource(s, 0), `{"entities":[{"id":"n1","name":"리나"}],"memories":[{"id":"m1","kind":"view","owner":"n1","about":"n1","aspect":"motive","body":"리나는 기술학교에서 배우는 편이 효율적이라고 여겼다."}]}`)
	quote := "내일 기술학교에서 용접 기초를 배우겠다고 했다"
	j2, items, notes := a24Save(t, db, turnSource(s, 1), fmt.Sprintf(`{"memories":[{"ref":%q,"q":%q}]}`, old.Memories[0].ID, quote))
	if len(notes) != 0 || len(items.Memories) != 0 {
		t.Fatalf("reference was turned into inferred memory: notes=%v items=%v", notes, items.Memories)
	}
	var stored string
	if err := db.sql.QueryRow("SELECT q FROM mentions WHERE job_id=?", j2.ID).Scan(&stored); err != nil || stored != quote {
		t.Fatalf("source quote not retained: %q %v", stored, err)
	}
	k, err := db.KnownOf([]int64{j1.ID, j2.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(k.Memories) != 1 || k.Memories[0].Aspect != "motive" {
		t.Fatalf("unexpected canonical memories: %+v", k.Memories)
	}
	// Even when the old record is selected, its later mention does not
	// supply the new plan to the generation renderer.
	text := memory.RenderKnown(memory.Selection{Memories: k.Memories}, memory.Superseded(k))
	if !strings.Contains(text, "효율적") || strings.Contains(text, "용접 기초") {
		t.Fatalf("reference treated as a new semantic record: %s", text)
	}
}

// The corrected generation contract keeps a new scheduled intention alongside
// the older motive. This checks that storage/reading do not merge them again;
// it does not claim that a model follows the generation instructions.
func TestNewPlanCoexistsWithOldMotive(t *testing.T) {
	db := testDB(t)
	s := chatOf(t, [][2]string{{"独学", "リナは学校で学ぶほうが効率的だと思った。"}, {"決意", "リナは明日学校で溶接の基礎を学ぶと決めた。"}}, "明日の予定は？")
	j1, old, _ := a24Save(t, db, turnSource(s, 0), `{"entities":[{"id":"n1","name":"リナ"}],"memories":[{"id":"m1","kind":"view","owner":"n1","about":"n1","aspect":"motive","body":"学校で学ぶほうが効率的だと思う。"}]}`)
	j2, fresh, notes := a24Save(t, db, turnSource(s, 1), fmt.Sprintf(`{"memories":[{"id":"m1","kind":"view","owner":%q,"about":%q,"aspect":"goal","body":"明日学校で溶接の基礎を学ぶ。","q":"明日学校で溶接の基礎を学ぶと決めた"}]}`, old.Names[0].ID, old.Names[0].ID))
	if len(notes) != 0 || len(fresh.Memories) != 1 {
		t.Fatalf("new plan was not stored: notes=%v memories=%v", notes, fresh.Memories)
	}
	k, err := db.KnownOf([]int64{j1.ID, j2.ID})
	if err != nil {
		t.Fatal(err)
	}
	closed := memory.Superseded(k)
	if len(k.Memories) != 2 || closed[old.Memories[0].ID] || closed[fresh.Memories[0].ID] {
		t.Fatalf("same-topic memories replaced one another: memories=%v superseded=%v", k.Memories, closed)
	}
	text := memory.RenderKnown(memory.Selection{Memories: k.Memories}, closed)
	if !strings.Contains(text, "view/motive") || !strings.Contains(text, "view/goal") || !strings.Contains(text, "明日学校で溶接の基礎") {
		t.Fatalf("new intention was lost when preparing later generation: %s", text)
	}
}
