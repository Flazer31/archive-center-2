package main

import (
	"net/url"
	"strconv"
	"strings"
	"testing"

	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/observe"
	"archivecenter2/goservice/internal/pipeline"
	"archivecenter2/goservice/internal/store"
)

// C14 the memory screen by person: kind=person lists the current facts and
// views, a person's facts in the prompt's mode order and then their views,
// the people by name and what is about nobody last; a fact a later record
// replaced is counted in replaced, not listed, and still listed by the fact
// kind (marked replaced); events stay in their own kind.
func TestMemoryScreenByPerson(t *testing.T) {
	db := modelsDB(t)
	pipe := pipeline.New(db)
	rows := []string{memRow(0, "user", "u1", "", "하나"), memRow(1, "char", "a1", "g1", "첫째"), memRow(2, "user", "u2", "", "둘"), memRow(3, "char", "a2", "g2", "둘째")}
	if _, err := pipe.Process([]observe.Record{memEvent(1, "load", "K", rows...)}); err != nil {
		t.Fatal(err)
	}
	var srcs []memory.Source
	pipe.WithSession("chat:K", func(s *lifecycle.Session) {
		for _, tr := range s.ActiveTurns() {
			srcs = append(srcs, memory.SourceOf(s.Key, tr))
		}
	})
	save := func(src memory.Source, raw string) {
		j, err := db.JobFor(src)
		if err != nil {
			t.Fatal(err)
		}
		out, _ := memory.Parse(raw)
		if _, err := db.SaveGeneration(j, store.Generation{Request: "r", ReadItems: "[]", Response: raw, Output: out}); err != nil {
			t.Fatal(err)
		}
	}
	save(srcs[0], `{"entities":[{"id":"n1","name":"리나"},{"id":"n2","name":"도윤"}],"memories":[
	 {"id":"m1","kind":"fact","about":"n1","mode":"말투","body":"리나는 존댓말을 쓴다"},
	 {"id":"m2","kind":"fact","about":"n1","mode":"상태","body":"리나는 약사다"},
	 {"id":"m3","kind":"view","owner":"n2","about":"n1","aspect":"belief","body":"도윤은 리나를 믿는다"},
	 {"id":"m4","kind":"fact","mode":"세계 규칙","body":"마을에서는 밤에 문을 닫는다"},
	 {"id":"m5","kind":"event","body":"리나가 왔다"},
	 {"id":"m6","kind":"fact","about":"n2","mode":"관계","body":"도윤은 리나의 동생이다"}]}`)
	h := memoriesHandler(pipe, db, nil, nil)
	idOf := map[string]string{}
	for _, m := range listed(t, callMem(t, h, "GET", "/memory/list?chat_id=K", "")) {
		idOf[m["body"].(string)] = m["id"].(string)
	}
	old := idOf["리나는 약사다"]
	if old == "" {
		t.Fatalf("first turn not listed: %v", idOf)
	}
	save(srcs[1], `{"memories":[
	 {"id":"m1","kind":"fact","about":"N1","mode":"상태","body":"리나는 약국을 그만뒀다","was":"`+old+`"},
	 {"id":"m2","kind":"view","owner":"N1","about":"N2","aspect":"goal","body":"리나는 떠나려 한다"}]}`)

	a := callMem(t, h, "GET", "/memory/list?chat_id=K&kind=person", "")
	var got []string
	for _, m := range listed(t, a) {
		got = append(got, m["body"].(string))
	}
	// 도윤 before 리나 by name; each person's facts by mode (상태 before 말투
	// before 관계), then their views (도윤's view of 리나 is 도윤's); the world rule (about nobody) last
	want := []string{"도윤은 리나의 동생이다", "도윤은 리나를 믿는다", "리나는 약국을 그만뒀다", "리나는 존댓말을 쓴다", "리나는 떠나려 한다", "마을에서는 밤에 문을 닫는다"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("by person\n got %v\nwant %v", got, want)
	}
	// each item names whom it is filed under; the world rule nobody
	var people []string
	for _, m := range listed(t, a) {
		who, _ := m["person"].(string)
		people = append(people, who)
	}
	if strings.Join(people, "|") != "도윤|도윤|리나|리나|리나|" {
		t.Fatalf("person fields %q", people)
	}
	kinds := a.V["kinds"].(map[string]any)
	if a.V["replaced"].(float64) != 1 || kinds["person"].(float64) != 6 || a.V["total"].(float64) != 6 {
		t.Fatalf("counts replaced=%v person=%v total=%v", a.V["replaced"], kinds["person"], a.V["total"])
	}
	// the fact kind still lists the replaced value, marked
	found := false
	for _, m := range listed(t, callMem(t, h, "GET", "/memory/list?chat_id=K&kind=fact", "")) {
		if m["body"] == "리나는 약사다" {
			found = m["replaced"] == true
		}
	}
	if !found {
		t.Fatal("the replaced fact is not listed as replaced in the fact kind")
	}
	// people in the listed order with counts; one person with their parts
	// (a tab per person, parts folded)
	var who []string
	for _, p := range a.V["people"].([]any) {
		m := p.(map[string]any)
		who = append(who, m["name"].(string)+"="+strconv.Itoa(int(m["count"].(float64))))
	}
	if strings.Join(who, "|") != "도윤=2|리나=3|=1" {
		t.Fatalf("people %q", who)
	}
	one := callMem(t, h, "GET", "/memory/list?chat_id=K&kind=person&person="+url.QueryEscape(nameRef(t, a, "리나")), "")
	got = nil
	for _, m := range listed(t, one) {
		got = append(got, m["body"].(string))
	}
	var parts []string
	for _, p := range one.V["parts"].([]any) {
		m := p.(map[string]any)
		parts = append(parts, m["name"].(string)+"="+strconv.Itoa(int(m["count"].(float64))))
	}
	if strings.Join(got, "|") != "리나는 약국을 그만뒀다|리나는 존댓말을 쓴다|리나는 떠나려 한다" || strings.Join(parts, "|") != "상태=1|말투=1|주관 기억=1" || one.V["total"].(float64) != 3 || len(one.V["people"].([]any)) != 3 {
		t.Fatalf("one person: %v parts %v total %v", got, parts, one.V["total"])
	}
	if n := listed(t, callMem(t, h, "GET", "/memory/list?chat_id=K&kind=person&person=", "")); len(n) != 1 || n[0]["body"] != "마을에서는 밤에 문을 닫는다" {
		t.Fatalf("about nobody: %v", n)
	}
	// the search narrows the person view and its counts alike
	a = callMem(t, h, "GET", "/memory/list?chat_id=K&kind=person&q=도윤", "")
	if n := len(listed(t, a)); n != 3 || a.V["kinds"].(map[string]any)["person"].(float64) != 3 {
		t.Fatalf("search by person: %d listed, kinds %v", n, a.V["kinds"])
	}
}

// A person whose name looks like a record (M12) is still that person (#2);
// a fact with no mode or one outside the list stays before the person's
// views (#3); a replaced view is counted in replaced too (#1).
func TestMemoryScreenByPersonReview13(t *testing.T) {
	db := modelsDB(t)
	pipe := pipeline.New(db)
	rows := []string{memRow(0, "user", "u1", "", "하나"), memRow(1, "char", "a1", "g1", "첫째"), memRow(2, "user", "u2", "", "둘"), memRow(3, "char", "a2", "g2", "둘째")}
	if _, err := pipe.Process([]observe.Record{memEvent(1, "load", "K", rows...)}); err != nil {
		t.Fatal(err)
	}
	var srcs []memory.Source
	pipe.WithSession("chat:K", func(s *lifecycle.Session) {
		for _, tr := range s.ActiveTurns() {
			srcs = append(srcs, memory.SourceOf(s.Key, tr))
		}
	})
	save := func(src memory.Source, raw string) {
		j, err := db.JobFor(src)
		if err != nil {
			t.Fatal(err)
		}
		out, _ := memory.Parse(raw)
		if _, err := db.SaveGeneration(j, store.Generation{Request: "r", ReadItems: "[]", Response: raw, Output: out}); err != nil {
			t.Fatal(err)
		}
	}
	save(srcs[0], `{"entities":[{"id":"n1","name":"M12","type":"person"}],"memories":[
	 {"id":"m1","kind":"view","owner":"n1","aspect":"belief","body":"M12는 비가 올 거라 믿는다"},
	 {"id":"m2","kind":"fact","about":"n1","body":"M12는 키가 크다"},
	 {"id":"m3","kind":"fact","about":"n1","mode":"직업","body":"M12는 목수다"},
	 {"id":"m4","kind":"fact","about":"n1","mode":"상태","body":"M12는 피곤하다"}]}`)
	h := memoriesHandler(pipe, db, nil, nil)
	old := ""
	for _, m := range listed(t, callMem(t, h, "GET", "/memory/list?chat_id=K&kind=view", "")) {
		old = m["id"].(string)
	}
	save(srcs[1], `{"memories":[{"id":"m1","kind":"view","owner":"N1","aspect":"belief","body":"M12는 이제 비가 그칠 거라 믿는다","was":"`+old+`"}]}`)
	a := callMem(t, h, "GET", "/memory/list?chat_id=K&kind=person", "")
	var got, people []string
	for _, m := range listed(t, a) {
		got = append(got, m["body"].(string))
		who, _ := m["person"].(string)
		people = append(people, who)
	}
	// in the fact tab's mode order: listed modes, then no mode, then others
	want := []string{"M12는 피곤하다", "M12는 키가 크다", "M12는 목수다", "M12는 이제 비가 그칠 거라 믿는다"}
	if strings.Join(got, "|") != strings.Join(want, "|") || strings.Join(people, "|") != "M12|M12|M12|M12" {
		t.Fatalf("by person\n got %v %q\nwant %v", got, people, want)
	}
	if a.V["replaced"].(float64) != 1 {
		t.Fatalf("a replaced view is counted: %v", a.V["replaced"])
	}
}

// A thing keeps its record and its values carry on. The by-name view gives
// each name its type and how many changed values it has; a name with
// changes only has a tab before nobody's; person=<name> lists its changes
// oldest first: a replaced fact of it, and each effect of an event about it
// (before, value). An effect on a record (an obligation fulfilled) is no
// name's.
func TestMemoryScreenByNameChanges(t *testing.T) {
	db := modelsDB(t)
	pipe := pipeline.New(db)
	rows := []string{memRow(0, "user", "u1", "", "하나"), memRow(1, "char", "a1", "g1", "첫째"), memRow(2, "user", "u2", "", "둘"), memRow(3, "char", "a2", "g2", "둘째")}
	if _, err := pipe.Process([]observe.Record{memEvent(1, "load", "K", rows...)}); err != nil {
		t.Fatal(err)
	}
	var srcs []memory.Source
	pipe.WithSession("chat:K", func(s *lifecycle.Session) {
		for _, tr := range s.ActiveTurns() {
			srcs = append(srcs, memory.SourceOf(s.Key, tr))
		}
	})
	save := func(src memory.Source, raw string) {
		j, err := db.JobFor(src)
		if err != nil {
			t.Fatal(err)
		}
		out, _ := memory.Parse(raw)
		if _, err := db.SaveGeneration(j, store.Generation{Request: "r", ReadItems: "[]", Response: raw, Output: out}); err != nil {
			t.Fatal(err)
		}
	}
	save(srcs[0], `{"entities":[{"id":"n1","name":"성검","type":"thing"},{"id":"n2","name":"아린","type":"person"}],"memories":[
	 {"id":"m1","kind":"fact","about":"n1","mode":"상태","slot":"상태","body":"성검은 빛난다"},
	 {"id":"m2","kind":"fact","mode":"의무","body":"아린은 성검을 신전에 돌려줘야 한다"},
	 {"id":"m3","kind":"event","body":"아린이 성검을 뽑았다","effects":[{"about":"n2","slot":"소지품","before":"없음","value":"성검"}]}]}`)
	h := memoriesHandler(pipe, db, nil, nil)
	idOf := map[string]string{}
	for _, m := range listed(t, callMem(t, h, "GET", "/memory/list?chat_id=K", "")) {
		idOf[m["body"].(string)] = m["id"].(string)
	}
	old, duty := idOf["성검은 빛난다"], idOf["아린은 성검을 신전에 돌려줘야 한다"]
	if old == "" || duty == "" {
		t.Fatalf("first turn not listed: %v", idOf)
	}
	save(srcs[1], `{"memories":[
	 {"id":"m1","kind":"fact","about":"N1","mode":"상태","slot":"상태","body":"성검에 금이 갔다","was":"`+old+`"},
	 {"id":"m2","kind":"event","body":"성검이 마왕의 검과 부딪쳤다","effects":[{"about":"N1","slot":"상태","before":"빛남","value":"금 감"}]},
	 {"id":"m3","kind":"event","body":"아린이 성검을 신전에 돌려주었다","effects":[{"about":"`+duty+`","status":"fulfilled"},{"about":"N2","slot":"소지품","before":"성검","value":"없음"}]}]}`)

	a := callMem(t, h, "GET", "/memory/list?chat_id=K&kind=person", "")
	var who []string
	for _, p := range a.V["people"].([]any) {
		m := p.(map[string]any)
		who = append(who, m["name"].(string)+":"+m["type"].(string)+"="+strconv.Itoa(int(m["count"].(float64)))+"/"+strconv.Itoa(int(m["changes"].(float64))))
	}
	// 성검: its current fact, the replaced fact and one effect; 아린: no fact,
	// two effects, a tab before nobody's (the duty); the duty's fulfilment is no name's
	if strings.Join(who, "|") != "성검:thing=1/2|아린:person=0/2|:=1/0" {
		t.Fatalf("people %q", who)
	}
	changesOf := func(name string) []string {
		one := callMem(t, h, "GET", "/memory/list?chat_id=K&kind=person&person="+url.QueryEscape(nameRef(t, a, name)), "")
		var out []string
		for _, c := range one.V["changes"].([]any) {
			m := c.(map[string]any)
			before, _ := m["before"].(string)
			ev, _ := m["event"].(string)
			out = append(out, strconv.Itoa(int(m["turn"].(float64)))+" "+m["slot"].(string)+" "+before+">"+m["value"].(string)+" "+ev+" "+strconv.FormatBool(m["replaced"] == true))
		}
		return out
	}
	if got := strings.Join(changesOf("성검"), "|"); got != "1 상태 >성검은 빛난다  true|2 상태 빛남>금 감 성검이 마왕의 검과 부딪쳤다 false" {
		t.Fatalf("성검 changes %q", got)
	}
	if got := strings.Join(changesOf("아린"), "|"); got != "1 소지품 없음>성검 아린이 성검을 뽑았다 false|2 소지품 성검>없음 아린이 성검을 신전에 돌려주었다 false" {
		t.Fatalf("아린 changes %q", got)
	}
	if n := listed(t, callMem(t, h, "GET", "/memory/list?chat_id=K&kind=person&person="+url.QueryEscape(nameRef(t, a, "아린")), "")); len(n) != 0 {
		t.Fatalf("아린 has no current fact or view: %v", n)
	}
}

// nameRef is the stored reference of the one tab named name in a by-name
// answer (person= asks for a tab by it).
func nameRef(t *testing.T, a memAnswer, name string) string {
	t.Helper()
	ref, n := "", 0
	for _, p := range a.V["people"].([]any) {
		m := p.(map[string]any)
		if m["name"] == name {
			ref, n = m["ref"].(string), n+1
		}
	}
	if n != 1 || ref == "" {
		t.Fatalf("%d tabs named %q (ref %q)", n, name, ref)
	}
	return ref
}

// A search for a name or a value only an effect holds keeps that change
// (A1); a name that reads like a record (M1) keeps an effect on the record
// M1 out of its changes (A2); two names that read the same are two tabs,
// each with its own type, facts and changes (A3).
func TestMemoryScreenByNameReview1109(t *testing.T) {
	db := modelsDB(t)
	pipe := pipeline.New(db)
	rows := []string{memRow(0, "user", "u1", "", "하나"), memRow(1, "char", "a1", "g1", "첫째"), memRow(2, "user", "u2", "", "둘"), memRow(3, "char", "a2", "g2", "둘째")}
	if _, err := pipe.Process([]observe.Record{memEvent(1, "load", "K", rows...)}); err != nil {
		t.Fatal(err)
	}
	var srcs []memory.Source
	pipe.WithSession("chat:K", func(s *lifecycle.Session) {
		for _, tr := range s.ActiveTurns() {
			srcs = append(srcs, memory.SourceOf(s.Key, tr))
		}
	})
	save := func(src memory.Source, raw string) {
		j, err := db.JobFor(src)
		if err != nil {
			t.Fatal(err)
		}
		out, _ := memory.Parse(raw)
		if _, err := db.SaveGeneration(j, store.Generation{Request: "r", ReadItems: "[]", Response: raw, Output: out}); err != nil {
			t.Fatal(err)
		}
	}
	save(srcs[0], `{"entities":[{"id":"n1","name":"성검","type":"thing"},{"id":"n2","name":"M1","type":"person"},{"id":"n3","name":"별","type":"person"},{"id":"n4","name":"별","type":"thing"}],"memories":[
	 {"id":"m1","kind":"fact","mode":"의무","body":"반납 의무"},
	 {"id":"m2","kind":"fact","about":"n1","mode":"상태","body":"아직 멀쩡하다"},
	 {"id":"m3","kind":"event","body":"검이 부딪쳤다","effects":[{"about":"n1","slot":"상태","before":"빛남","value":"금 감"}]},
	 {"id":"m4","kind":"fact","about":"n3","body":"소녀다"},
	 {"id":"m5","kind":"fact","about":"n4","body":"칼이다"},
	 {"id":"m6","kind":"event","body":"둘의 상태가 달라졌다","effects":[{"about":"n3","slot":"상태","before":"슬픔","value":"기쁨"},{"about":"n4","slot":"상태","before":"온전","value":"깨짐"}]}]}`)
	h := memoriesHandler(pipe, db, nil, nil)
	duty := ""
	for _, m := range listed(t, callMem(t, h, "GET", "/memory/list?chat_id=K", "")) {
		if m["body"] == "반납 의무" {
			duty = m["id"].(string)
		}
	}
	// the name M1 is the stored name n2; give it a fact so it has a tab
	save(srcs[1], `{"memories":[{"id":"m1","kind":"event","body":"이름 대상과 의무를 함께 바꾼다","effects":[{"about":"N2","slot":"소지품","before":"없음","value":"검"},{"about":"`+duty+`","status":"fulfilled"}]}]}`)
	a := callMem(t, h, "GET", "/memory/list?chat_id=K&kind=person", "")
	var who []string
	for _, p := range a.V["people"].([]any) {
		m := p.(map[string]any)
		who = append(who, m["name"].(string)+":"+m["type"].(string)+"="+strconv.Itoa(int(m["count"].(float64)))+"/"+strconv.Itoa(int(m["changes"].(float64))))
	}
	if strings.Join(who, "|") != "별:person=1/1|별:thing=1/1|성검:thing=1/1|M1:person=0/1|:=1/0" {
		t.Fatalf("people %q (A2: the record M1's fulfilment is no name's; A3: two 별 tabs)", who)
	}
	var stars []string
	for _, p := range a.V["people"].([]any) {
		if m := p.(map[string]any); m["name"] == "별" {
			one := callMem(t, h, "GET", "/memory/list?chat_id=K&kind=person&person="+url.QueryEscape(m["ref"].(string)), "")
			rows := listed(t, one)
			c := one.V["changes"].([]any)
			if len(rows) != 1 || len(c) != 1 {
				t.Fatalf("one 별: %v %v", rows, c)
			}
			stars = append(stars, m["type"].(string)+" "+rows[0]["body"].(string)+" "+c[0].(map[string]any)["value"].(string))
		}
	}
	if strings.Join(stars, "|") != "person 소녀다 기쁨|thing 칼이다 깨짐" {
		t.Fatalf("two 별 tabs %q", stars)
	}
	m1 := callMem(t, h, "GET", "/memory/list?chat_id=K&kind=person&person=N2", "")
	if c := m1.V["changes"].([]any); len(c) != 1 || c[0].(map[string]any)["value"] != "검" {
		t.Fatalf("M1 changes %v", c)
	}
	sword := nameRef(t, a, "성검")
	for _, q := range []string{"", "성검", "금 감"} {
		one := callMem(t, h, "GET", "/memory/list?chat_id=K&kind=person&person="+url.QueryEscape(sword)+"&q="+url.QueryEscape(q), "")
		if c := one.V["changes"].([]any); len(c) != 1 {
			t.Fatalf("q=%q: the effect's change is kept, got %v", q, c)
		}
	}
	if s := callMem(t, h, "GET", "/memory/list?chat_id=K&kind=person&q="+url.QueryEscape("금 감"), ""); !strings.Contains(s.Raw, `"name":"성검"`) {
		t.Fatalf("a value only an effect holds keeps its name's tab: %s", s.Raw)
	}
}
