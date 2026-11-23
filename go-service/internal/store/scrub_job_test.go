package store

import (
	"strings"
	"testing"
	"time"

	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
)

// 1.4 (user "앞으로 생기는 것만 지우기", redesigned after review 10): the job
// made from a reply an edit or a reroll dropped keeps no copy of it.

type scrubFixture struct {
	t  *testing.T
	db *DB
	s  *lifecycle.Session
}

func newScrubFixture(t *testing.T, key string) *scrubFixture {
	return &scrubFixture{t: t, db: testDB(t), s: lifecycle.NewSession(key)}
}

func (f *scrubFixture) show(reply string) {
	f.t.Helper()
	snap := lsnap(lifecycle.SourceOutputEvent, lrow("u1", "user", "", "반지를 어디 뒀어?"), lrow("a1", "char", "g1", reply), lrow("u2", "user", "", "다음"))
	snap.SessionKey = f.s.Key
	f.s.Apply(snap)
	if err := f.db.SaveSession(f.s, f.s.TakeEvents(), f.s.TakeRenames()); err != nil {
		f.t.Fatal(err)
	}
}

// the job's request, answer and memory quotes, as stored and as read
func (f *scrubFixture) job(id int64) (request, response string, column, read map[string]string, words int) {
	f.t.Helper()
	if err := f.db.sql.QueryRow(`SELECT COALESCE(request,''), COALESCE(response,'') FROM gen_jobs WHERE job_id=?`, id).Scan(&request, &response); err != nil {
		f.t.Fatal(err)
	}
	column, read = map[string]string{}, map[string]string{}
	rows, err := f.db.sql.Query(`SELECT body, q FROM memories WHERE job_id=?`, id)
	if err != nil {
		f.t.Fatal(err)
	}
	for rows.Next() {
		var b, q string
		rows.Scan(&b, &q)
		column[b] = q
	}
	rows.Close()
	it, err := f.db.ItemsOf([]int64{id})
	if err != nil {
		f.t.Fatal(err)
	}
	for _, m := range it.Memories {
		read[m.Body] = m.Q
	}
	if err := f.db.sql.QueryRow(`SELECT COUNT(*) FROM search_words WHERE job_id=? AND field='q' AND word LIKE '%은반지%'`, id).Scan(&words); err != nil {
		f.t.Fatal(err)
	}
	return
}

const scrubAnswer = `{"memories":[
 {"id":"m1","kind":"event","body":"리나가 은반지를 숨김","q":"은반지를 상자에 숨겼다"},
 {"id":"m2","kind":"event","body":"리나가 문을 닫음","q":"문을 닫았다"},
 {"id":"m3","kind":"event","body":"반지 위치를 물음","q":"반지를 어디 뒀어"}]}`

// An edit drops the reply: its job's request and answer keep only their
// length and fingerprint; a quote of the dropped text goes from the column,
// from the stored item as read and from the search words; a quote the chat
// still holds (the input; a sentence the new reply keeps, with markup in
// it now) stays. Another job is untouched.
func TestDroppedReplyLeavesNoCopyInItsJob(t *testing.T) {
	f := newScrubFixture(t, "chat:A")
	first := "리나는 <b>은반지</b>를 상자에 숨겼다. 그리고 문을 닫았다."
	f.show(first)
	j, _, _ := a24Save(t, f.db, turnSource(f.s, 0), scrubAnswer)
	other, _, _ := a24Save(t, f.db, turnSource(f.s, 1), `{"memories":[{"id":"m1","kind":"event","body":"다음","q":"다음"}]}`)
	if _, err := f.db.sql.Exec(`UPDATE gen_jobs SET request=? WHERE job_id=?`, "REPLY: "+memory.PlainText(first), j.ID); err != nil {
		t.Fatal(err)
	}
	f.show("리나는 대답하지 않았다. 그리고 <i>문</i>을 닫았다.")

	req, resp, column, read, words := f.job(j.ID)
	for _, v := range []string{req, resp} {
		if strings.Contains(v, "은반지") || !strings.HasPrefix(v, removedMark) {
			t.Fatalf("the dropped reply's job still holds it: %q", v)
		}
	}
	if column["리나가 은반지를 숨김"] != "" || read["리나가 은반지를 숨김"] != "" || words != 0 {
		t.Fatalf("a quote of the dropped reply stayed: column %v read %v words %d", column, read, words)
	}
	if column["리나가 문을 닫음"] != "문을 닫았다" || read["리나가 문을 닫음"] != "문을 닫았다" || column["반지 위치를 물음"] != "반지를 어디 뒀어" {
		t.Fatalf("a quote the chat still holds (as read) was emptied: column %v read %v", column, read)
	}
	if oreq, _, ocol, _, _ := f.job(other.ID); oreq != "synthetic request" || ocol["다음"] != "다음" {
		t.Fatalf("another job changed: %q %v", oreq, ocol)
	}
}

// A branch still reads the parent's turn (its recorded copy holds the
// reply): the parent's edit leaves the job as it was.
func TestDroppedReplyABranchReadsStays(t *testing.T) {
	f := newScrubFixture(t, "chat:A")
	first := "리나는 은반지를 상자에 숨겼다. 그리고 문을 닫았다."
	f.show(first)
	j, _, _ := a24Save(t, f.db, turnSource(f.s, 0), scrubAnswer)
	src := `{"Session":"chat:A","Turn":1,"Members":[{"Key":"c:u1","Data":"반지를 어디 뒀어?"}],"Responses":[{"Key":"c:a1","Seq":1,"Data":"` + first + `"}]}`
	if _, err := f.db.sql.Exec(`INSERT INTO branch_copies (session_key, turn_id, own_key, source) VALUES (?,?,?,?)`, "chat:B", 1, "k", src); err != nil {
		t.Fatal(err)
	}
	f.show("리나는 대답하지 않았다.")
	req, resp, column, _, _ := f.job(j.ID)
	if req != "synthetic request" || !strings.Contains(resp, "은반지") || column["리나가 은반지를 숨김"] != "은반지를 상자에 숨겼다" {
		t.Fatalf("a job a branch reads was changed: %q %q %v", req, resp, column)
	}
}

// The answer comes after the edit (the call was under way): what it stores
// keeps no copy of the dropped reply either.
func TestLateAnswerForADroppedReplyKeepsNoCopy(t *testing.T) {
	f := newScrubFixture(t, "chat:A")
	first := "리나는 은반지를 상자에 숨겼다. 그리고 문을 닫았다."
	f.show(first)
	j, err := f.db.JobFor(turnSource(f.s, 0)) // the call starts
	if err != nil {
		t.Fatal(err)
	}
	f.show("리나는 대답하지 않았다.") // the edit while it runs
	if _, err := f.db.SaveGeneration(j, Generation{Request: "REPLY: " + first, ReadItems: "[]", Response: scrubAnswer, Output: parse(t, scrubAnswer)}); err != nil {
		t.Fatal(err)
	}
	req, resp, column, read, words := f.job(j.ID)
	if strings.Contains(req, "은반지") || strings.Contains(resp, "은반지") || column["리나가 은반지를 숨김"] != "" || read["리나가 은반지를 숨김"] != "" || words != 0 {
		t.Fatalf("a late answer kept a copy: %q %q %v %v %d", req, resp, column, read, words)
	}
	if column["반지 위치를 물음"] != "반지를 어디 뒀어" {
		t.Fatalf("the input's quote was emptied: %v", column)
	}
}

// A reply restyled (markup only) keeps its job, then a later edit changes
// what it says: the first job, which read the unstyled text, goes too.
func TestRestyledThenEditedReplyLeavesNoCopy(t *testing.T) {
	f := newScrubFixture(t, "chat:A")
	first := "리나는 은반지를 상자에 숨겼다. 그리고 문을 닫았다."
	f.show(first)
	j1, _, _ := a24Save(t, f.db, turnSource(f.s, 0), scrubAnswer)
	f.show("리나는 <b>은반지</b>를 상자에 숨겼다. 그리고 문을 닫았다.")
	if req, _, col, _, _ := f.job(j1.ID); req != "synthetic request" || col["리나가 은반지를 숨김"] == "" {
		t.Fatalf("a restyle must leave the job: %q %v", req, col)
	}
	f.show("리나는 대답하지 않았다.")
	req, resp, column, read, _ := f.job(j1.ID)
	if !strings.HasPrefix(req, removedMark) || !strings.HasPrefix(resp, removedMark) || column["리나가 은반지를 숨김"] != "" || read["리나가 은반지를 숨김"] != "" {
		t.Fatalf("the first job kept a copy after the edit: %q %q %v %v", req, resp, column, read)
	}
}

// Quotes under a memory (who knows, and how they heard) and answers kept
// unreadable before one was stored go with the dropped reply too.
func TestDroppedReplyLeavesNoNestedOrUnreadCopy(t *testing.T) {
	f := newScrubFixture(t, "chat:A")
	first := "리나는 은반지를 상자에 숨겼다. 그리고 문을 닫았다."
	f.show(first)
	j, err := f.db.JobFor(turnSource(f.s, 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.KeepUnreadable(j, "m", "REPLY: "+first, "[]", "깨진 답: "+first, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	raw := `{"entities":[{"id":"n1","name":"도윤","type":"person"}],"memories":[
	 {"id":"m1","kind":"event","body":"리나가 은반지를 숨김","q":"반지를 어디 뒀어","knows":[{"who":"n1","q":"은반지를 상자에 숨겼다"},{"who":"n1","q":"반지를 어디 뒀어"}]}]}`
	j, _ = f.db.JobFor(turnSource(f.s, 0))
	if _, err := f.db.SaveGeneration(j, Generation{Request: "REPLY: " + first, ReadItems: "[]", Response: raw, Output: parse(t, raw)}); err != nil {
		t.Fatal(err)
	}
	f.show("리나는 대답하지 않았다.")
	it, err := f.db.ItemsOf([]int64{j.ID})
	if err != nil {
		t.Fatal(err)
	}
	var quotes []string
	for _, m := range it.Memories {
		for _, k := range m.Knows {
			quotes = append(quotes, k.Q)
		}
	}
	if len(quotes) != 2 || quotes[0] != "" || quotes[1] != "반지를 어디 뒀어" {
		t.Fatalf("who-knows quotes as read: %q (want the dropped one emptied, the input's kept)", quotes)
	}
	unread, err := f.db.UnreadAnswers(j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(unread) != 1 || strings.Contains(unread[0], "은반지") || !strings.HasPrefix(unread[0], removedMark) {
		t.Fatalf("an unreadable answer kept a copy: %q", unread)
	}
}

// An unreadable answer is kept, an edit drops the reply while the retry is
// under way, and the retry's answer is stored late: the job is cleared twice
// without failing, and nothing of the reply stays.
func TestDroppedReplyClearedTwiceKeepsNoCopy(t *testing.T) {
	f := newScrubFixture(t, "chat:A")
	first := "리나는 은반지를 상자에 숨겼다. 그리고 문을 닫았다."
	f.show(first)
	j, err := f.db.JobFor(turnSource(f.s, 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.KeepUnreadable(j, "m", "REPLY: "+first, "[]", "깨진 답: "+first, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	f.show("리나는 대답하지 않았다.") // the first clearing
	// the retry under way stores into the job it started with
	if _, err := f.db.SaveGeneration(j, Generation{Request: "REPLY: " + first, ReadItems: "[]", Response: scrubAnswer, Output: parse(t, scrubAnswer)}); err != nil {
		t.Fatal(err)
	}
	unread, err := f.db.UnreadAnswers(j.ID)
	if err != nil {
		t.Fatal(err)
	}
	req, _, column, _, _ := f.job(j.ID)
	if len(unread) != 1 || !strings.HasPrefix(unread[0], removedMark) || strings.Contains(req, "은반지") || column["리나가 은반지를 숨김"] != "" {
		t.Fatalf("after two clearings: unread %q request %q quotes %v", unread, req, column)
	}
}

// A branch keeps the first text; the parent restyles it, then changes what
// it says: the job the branch reads stays.
func TestRestyledThenEditedReplyABranchReadsStays(t *testing.T) {
	f := newScrubFixture(t, "chat:A")
	first := "리나는 은반지를 상자에 숨겼다. 그리고 문을 닫았다."
	f.show(first)
	j1, _, _ := a24Save(t, f.db, turnSource(f.s, 0), scrubAnswer)
	src := `{"session":"chat:A","turn":1,"members":[{"key":"c:u1","data":"반지를 어디 뒀어?"}],"responses":[{"key":"c:a1","seq":1,"data":"` + first + `"}]}`
	if _, err := f.db.sql.Exec(`INSERT INTO branch_copies (session_key, turn_id, own_key, source) VALUES (?,?,?,?)`, "chat:B", 1, "k", src); err != nil {
		t.Fatal(err)
	}
	f.show("리나는 <b>은반지</b>를 상자에 숨겼다. 그리고 문을 닫았다.")
	f.show("리나는 대답하지 않았다.")
	req, resp, column, _, _ := f.job(j1.ID)
	if req != "synthetic request" || !strings.Contains(resp, "은반지") || column["리나가 은반지를 숨김"] != "은반지를 상자에 숨겼다" {
		t.Fatalf("a job a branch reads was changed: %q %q %v", req, resp, column)
	}
}

// Both texts carry markup at other places (no common run of the stored
// texts): the first job is still found, by the row it read.
func TestRestyledTwiceThenEditedReplyLeavesNoCopy(t *testing.T) {
	f := newScrubFixture(t, "chat:A")
	f.show("리나는 <b>은반지</b>를 상자에 숨겼다.")
	j1, _, _ := a24Save(t, f.db, turnSource(f.s, 0), scrubAnswer)
	f.show("<i>리나는 은반지를 상자에 숨겼다.</i>")
	f.show("리나는 대답하지 않았다.")
	req, resp, column, _, _ := f.job(j1.ID)
	if !strings.HasPrefix(req, removedMark) || !strings.HasPrefix(resp, removedMark) || column["리나가 은반지를 숨김"] != "" {
		t.Fatalf("the first job kept a copy: %q %q %v", req, resp, column)
	}
}

// The reply first comes without a chat id (its job reads it under an
// interim key), then gets one (the key is upgraded), is restyled with
// markup at other places, and then changes: the first job is still found,
// by its turn.
func TestRestyledReplyWithUpgradedKeyLeavesNoCopy(t *testing.T) {
	f := newScrubFixture(t, "chat:A")
	save := func(reply, chat string) {
		t.Helper()
		snap := lsnap(lifecycle.SourceOutputEvent, lrow("u1", "user", "", "반지를 어디 뒀어?"), lrow(chat, "char", "g1", reply), lrow("u2", "user", "", "다음"))
		snap.SessionKey = f.s.Key
		f.s.Apply(snap)
		if err := f.db.SaveSession(f.s, f.s.TakeEvents(), f.s.TakeRenames()); err != nil {
			t.Fatal(err)
		}
	}
	first := "리나는 <b>은반지</b>를 상자에 숨겼다."
	save(first, "")
	j1, _, _ := a24Save(t, f.db, turnSource(f.s, 0), scrubAnswer)
	save(first, "a1") // the same reply gets its chat id
	save("<i>리나는 은반지를 상자에 숨겼다.</i>", "a1")
	save("리나는 대답하지 않았다.", "a1")
	req, resp, column, _, _ := f.job(j1.ID)
	if !strings.HasPrefix(req, removedMark) || !strings.HasPrefix(resp, removedMark) || column["리나가 은반지를 숨김"] != "" {
		t.Fatalf("the first job kept a copy: %q %q %v", req, resp, column)
	}
}

// Two turns hold the same reply text, both restyled, then both changed in
// one snapshot: both jobs go.
func TestSameTextDroppedFromTwoRowsLeavesNoCopy(t *testing.T) {
	f := newScrubFixture(t, "chat:A")
	save := func(r1, r2 string) {
		t.Helper()
		snap := lsnap(lifecycle.SourceOutputEvent, lrow("u1", "user", "", "반지를 어디 뒀어?"), lrow("a1", "char", "g1", r1),
			lrow("u2", "user", "", "그래서?"), lrow("a2", "char", "g2", r2), lrow("u3", "user", "", "다음"))
		snap.SessionKey = f.s.Key
		f.s.Apply(snap)
		if err := f.db.SaveSession(f.s, f.s.TakeEvents(), f.s.TakeRenames()); err != nil {
			t.Fatal(err)
		}
	}
	first := "리나는 <b>은반지</b>를 상자에 숨겼다."
	save(first, first)
	j1, _, _ := a24Save(t, f.db, turnSource(f.s, 0), scrubAnswer)
	j2, _, _ := a24Save(t, f.db, turnSource(f.s, 1), scrubAnswer)
	restyled := "<i>리나는 은반지를 상자에 숨겼다.</i>"
	save(restyled, restyled)
	save("리나는 대답하지 않았다.", "리나는 웃기만 했다.")
	for _, j := range []int64{j1.ID, j2.ID} {
		req, _, column, _, _ := f.job(j)
		if !strings.HasPrefix(req, removedMark) || column["리나가 은반지를 숨김"] != "" {
			t.Fatalf("job %d kept a copy: %q %v", j, req, column)
		}
	}
}
