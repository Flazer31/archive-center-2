package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
)

// scrubDropped removes the bodies a reroll or an edit dropped from the
// chat's raw observations and from its generation jobs, so no copy of an
// earlier reply stays. A text the chat still holds anywhere (an input, a
// current reply, an identical line elsewhere) is never removed. In an
// observation only the text goes (its length and fingerprint stay, so the
// plugin is asked to resend a text it needs); in a job source the
// response's data becomes empty, and that job's copies go too (scrubJob:
// 1.4, user "앞으로 생기는 것만 지우기"). A text another chat still uses (a
// branch reading this chat's turn, a chat holding the same row, an
// attached past) is left in the jobs: they read it.
func scrubDropped(tx *sql.Tx, s *lifecycle.Session, dropped map[string][]string) error {
	if len(dropped) == 0 {
		return nil
	}
	held := s.Held()
	// the turn of each row (a row keeps its turn when its key is upgraded)
	turnOf := map[string]int{}
	for _, tn := range s.Turns {
		for _, m := range tn.Members {
			turnOf[m.Key] = tn.ID
		}
		for _, r := range tn.Responses {
			turnOf[r.Key] = tn.ID
		}
	}
	gone := map[string]map[int]bool{} // text -> the turns of the rows it was dropped from
	for key, texts := range dropped {
		for _, t := range texts {
			if t != "" && !held[t] {
				if gone[t] == nil {
					gone[t] = map[int]bool{}
				}
				if id, ok := turnOf[key]; ok {
					gone[t][id] = true
				}
			}
		}
	}
	plainHeld := map[string]bool{}
	for t := range held {
		plainHeld[memory.PlainText(t)] = true
	}
	// a chat without id shares its observations' chat id ("") with other
	// such chats: only its job sources are cleared
	chat, ok := strings.CutPrefix(s.Key, "chat:")
	for t, turns := range gone {
		if ok {
			if err := scrubObservations(tx, chat, t, held); err != nil {
				return err
			}
		}
		// by text, not row key: a source written before the row got its chat
		// id names it by its old key
		// a text the chat holds rewritten with markup only reads the same:
		// its job's request is the new revision's too (SameRequestAnswer)
		if plainHeld[memory.PlainText(t)] {
			continue
		}
		if used, err := usedElsewhere(tx, s.Key, t); err != nil {
			return err
		} else if used {
			continue
		}
		if err := scrubSources(tx, s.Key, turns, t, heldTexts(held)); err != nil {
			return err
		}
	}
	return nil
}

// fragment is the longest run of a text that stored JSON writes the same
// whatever wrote it (the plugin's JSON.stringify, Go's encoder): no quote,
// backslash, <, >, &, control character or line/paragraph separator.
func fragment(text string) string {
	best, start := "", -1
	cut := func(end int) {
		if start >= 0 && end-start > len(best) {
			best = text[start:end]
		}
		start = -1
	}
	for i, r := range text {
		if r == '"' || r == '\\' || r == '<' || r == '>' || r == '&' || r == ' ' || r == ' ' || unicode.IsControl(r) {
			cut(i)
			continue
		}
		if start < 0 {
			start = i
		}
	}
	cut(len(text))
	return best
}

type foundRow struct {
	id   int64
	body string
}

func findRows(tx *sql.Tx, q string, args ...any) ([]foundRow, error) {
	rows, err := tx.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var all []foundRow
	for rows.Next() {
		var f foundRow
		if err := rows.Scan(&f.id, &f.body); err != nil {
			return nil, err
		}
		all = append(all, f)
	}
	return all, rows.Err()
}

func scrubObservations(tx *sql.Tx, chat, text string, current map[string]bool) error {
	all, err := findRows(tx, `SELECT id, event FROM raw_observations WHERE (chat_id=? OR chat_id2=?) AND INSTR(event, ?)>0`, chat, chat, fragment(text))
	if err != nil {
		return err
	}
	// the input and afterRequest hooks carry one text and no chat (1.4: the
	// earlier reply stayed in them): only that text field is cleared, when it
	// shows the dropped body. Which chat such a row came from is not known, so
	// a text another chat's observations also hold is left
	bare, err := findRows(tx, `SELECT id, event FROM raw_observations WHERE chat_id IS NULL AND chat_id2 IS NULL AND hook IN ('input','afterRequest') AND INSTR(event, ?)>0`, fragment(text))
	if err != nil {
		return err
	}
	if len(bare) > 0 {
		var elsewhere int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM raw_observations WHERE ((chat_id IS NOT NULL AND chat_id<>?) OR (chat_id2 IS NOT NULL AND chat_id2<>?)) AND INSTR(event, ?)>0`, chat, chat, fragment(text)).Scan(&elsewhere); err != nil {
			return err
		}
		if elsewhere > 0 {
			bare = nil
		}
	}
	for _, f := range bare {
		var ev map[string]json.RawMessage
		if json.Unmarshal([]byte(f.body), &ev) != nil {
			continue
		}
		out, did, err := dropText(ev["content"], text, current)
		if err != nil {
			return err
		}
		if !did {
			continue
		}
		ev["content"] = out
		b, err := json.Marshal(ev)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE raw_observations SET event=? WHERE id=?`, string(b), f.id); err != nil {
			return err
		}
	}
	for _, f := range all {
		var ev map[string]json.RawMessage
		if json.Unmarshal([]byte(f.body), &ev) != nil {
			continue
		}
		changed := false
		for _, part := range []string{"stored_chat", "event_chat", "reread_chat"} {
			raw, ok := ev[part]
			if !ok {
				continue
			}
			out, did, err := scrubChat(raw, chat, text, current)
			if err != nil {
				return err
			}
			if did {
				ev[part], changed = out, true
			}
		}
		if !changed {
			continue
		}
		b, err := json.Marshal(ev)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE raw_observations SET event=? WHERE id=?`, string(b), f.id); err != nil {
			return err
		}
	}
	return nil
}

// dropText removes the text of one stored text field when it holds the
// dropped body; the field's other entries (length, fingerprint) stay.
func dropText(raw json.RawMessage, text string, current map[string]bool) (json.RawMessage, bool, error) {
	var field map[string]json.RawMessage
	if json.Unmarshal(raw, &field) != nil {
		return raw, false, nil
	}
	var stored *string
	json.Unmarshal(field["text"], &stored)
	if stored == nil || !lifecycle.ShowsDropped(*stored, text, current) {
		return raw, false, nil
	}
	delete(field, "text")
	b, err := json.Marshal(field)
	return b, err == nil, err
}

// scrubChat removes the dropped body from one chat of an event: the rows'
// texts and the script state records (translations kept beside the rows).
func scrubChat(raw json.RawMessage, chat, text string, current map[string]bool) (json.RawMessage, bool, error) {
	var c map[string]json.RawMessage
	if json.Unmarshal(raw, &c) != nil {
		return raw, false, nil
	}
	var id *string
	json.Unmarshal(c["chat_id"], &id)
	own := ""
	if id != nil {
		own = *id
	}
	if own != chat {
		return raw, false, nil
	}
	changed := false
	var rows []map[string]json.RawMessage
	if json.Unmarshal(c["rows"], &rows) == nil {
		rowsChanged := false
		for _, r := range rows {
			out, did, err := dropText(r["data"], text, current)
			if err != nil {
				return raw, false, err
			}
			if did {
				r["data"], rowsChanged = out, true
			}
		}
		if rowsChanged {
			b, err := json.Marshal(rows)
			if err != nil {
				return raw, false, err
			}
			c["rows"], changed = b, true
		}
	}
	var vars map[string]json.RawMessage
	if json.Unmarshal(c["scriptstate"], &vars) == nil {
		varsChanged := false
		for k, v := range vars {
			out, did, err := dropText(v, text, current)
			if err != nil {
				return raw, false, err
			}
			if did {
				vars[k], varsChanged = out, true
			}
		}
		if varsChanged {
			b, err := json.Marshal(vars)
			if err != nil {
				return raw, false, err
			}
			c["scriptstate"], changed = b, true
		}
	}
	if !changed {
		return raw, false, nil
	}
	out, err := json.Marshal(c)
	return out, err == nil, err
}

func scrubSources(tx *sql.Tx, sessionKey string, turns map[int]bool, text string, keep []string) error {
	// a job that read the same text with other markup (an earlier revision
	// of a row only restyled, kept then for the request it shares) read it
	// too: the jobs of the rows' turns are looked at as well as the jobs
	// whose source holds the text (a source keeps the key it was read with)
	plain := memory.PlainText(text)
	q := `SELECT job_id, source FROM gen_jobs WHERE session_key=? AND (INSTR(source, ?)>0 OR INSTR(source, ?)>0`
	args := []any{sessionKey, fragment(text), fragment(plain)}
	for id := range turns {
		q += ` OR turn_id=?`
		args = append(args, id)
	}
	all, err := findRows(tx, q+`)`, args...)
	if err != nil {
		return err
	}
	for _, f := range all {
		var src memory.Source
		if json.Unmarshal([]byte(f.body), &src) != nil {
			continue
		}
		// a restyled text another chat still uses keeps its job
		shared := false
		for _, r := range src.Responses {
			if r.Data != "" && r.Data != text && memory.PlainText(r.Data) == plain {
				if used, err := usedElsewhere(tx, sessionKey, r.Data); err != nil {
					return err
				} else if used {
					shared = true
				}
			}
		}
		if shared {
			continue
		}
		changed := false
		for i, r := range src.Responses {
			if r.Data != "" && (r.Data == text || memory.PlainText(r.Data) == plain) {
				src.Responses[i].Data, changed = "", true
			}
		}
		if !changed {
			continue
		}
		b, err := json.Marshal(src)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE gen_jobs SET source=? WHERE job_id=?`, string(b), f.id); err != nil {
			return err
		}
		if err := scrubJob(tx, f.id, keep); err != nil {
			return err
		}
	}
	return nil
}

// usedElsewhere says another chat still uses a text: a row of its own, the
// recorded parent source a branch reads, or a past attached to it.
func usedElsewhere(tx *sql.Tx, sessionKey, text string) (bool, error) {
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM revisions WHERE session_key<>? AND data=?`, sessionKey, text).Scan(&n); err != nil || n > 0 {
		return n > 0, err
	}
	frag := fragment(text)
	for _, q := range []string{
		`SELECT source FROM branch_copies WHERE session_key<>? AND INSTR(source, ?)>0`,
		`SELECT source FROM session_past WHERE session_key<>? AND INSTR(source, ?)>0`,
		`SELECT own_source FROM session_past WHERE session_key<>? AND INSTR(COALESCE(own_source, ''), ?)>0`,
	} {
		stored, err := findTexts(tx, q, sessionKey, frag)
		if err != nil {
			return false, err
		}
		for _, r := range stored {
			if sourceHolds(r, text) {
				return true, nil
			}
		}
	}
	return false, nil
}

// sourceHolds says a stored source (JSON) has the text as a row's data; an
// unreadable one counts when it holds the text at all.
func sourceHolds(stored, text string) bool {
	var src memory.Source
	if json.Unmarshal([]byte(stored), &src) != nil {
		return strings.Contains(stored, text)
	}
	for _, m := range src.Members {
		if m.Data == text {
			return true
		}
	}
	for _, r := range src.Responses {
		if r.Data == text {
			return true
		}
	}
	return false
}

// heldTexts are the texts a chat holds, as stored and as read (markup out).
func heldTexts(held map[string]bool) []string {
	var out []string
	for t := range held {
		out = append(out, t, memory.PlainText(t))
	}
	return out
}

// quoteTables hold a job's quotes (q) of the turn it read.
var quoteTables = []string{"names", "name_relations", "memories", "mentions", "awareness", "scene_marks", "links"}

// scrubJob clears what a job of a dropped reply holds of it: its request
// and answer keep only their length and fingerprint, and each quote (q) no
// text in keep (what the chat holds, as stored and as read) contains is
// emptied, in the columns, in a memory's stored item and in the search words
// made from it. Such a job read a turn no chat has any more.
func scrubJob(tx *sql.Tx, job int64, keep []string) error {
	var request, response sql.NullString
	if err := tx.QueryRow(`SELECT request, response FROM gen_jobs WHERE job_id=?`, job).Scan(&request, &response); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE gen_jobs SET request=?, response=? WHERE job_id=?`, removedText(request), removedText(response), job); err != nil {
		return err
	}
	// answers kept unreadable before a later one was stored
	var unread sql.NullString
	if err := tx.QueryRow(`SELECT unread_answers FROM gen_jobs WHERE job_id=?`, job).Scan(&unread); err != nil {
		return err
	}
	if unread.Valid && unread.String != "" {
		var answers []string
		if json.Unmarshal([]byte(unread.String), &answers) == nil {
			for i, a := range answers {
				answers[i] = removedString(a)
			}
			b, err := json.Marshal(answers)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE gen_jobs SET unread_answers=? WHERE job_id=?`, string(b), job); err != nil {
				return err
			}
		}
	}
	kept := func(q string) bool {
		for _, t := range keep {
			if strings.Contains(t, q) {
				return true
			}
		}
		return false
	}
	for _, table := range quoteTables {
		quotes, err := findTexts(tx, `SELECT DISTINCT q FROM `+table+` WHERE job_id=? AND q<>''`, job)
		if err != nil {
			return err
		}
		for _, q := range quotes {
			if kept(q) {
				continue
			}
			if _, err := tx.Exec(`UPDATE `+table+` SET q='' WHERE job_id=? AND q=?`, job, q); err != nil {
				return err
			}
		}
	}
	return scrubJobIndex(tx, job, kept)
}

// scrubJobIndex brings a memory's stored item and the quote search words in
// line with the quotes left: every quote in a memory's stored item (its own,
// and those under it, as who knows) goes as the columns' did, an emptied
// memory quote leaves its words, and each mentioned target's words are made
// again from the quotes left.
func scrubJobIndex(tx *sql.Tx, job int64, kept func(string) bool) error {
	rows, err := tx.Query(`SELECT id, q, detail FROM memories WHERE job_id=?`, job)
	if err != nil {
		return err
	}
	type item struct {
		id        int64
		q, detail string
	}
	var items []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.id, &it.q, &it.detail); err != nil {
			rows.Close()
			return err
		}
		items = append(items, it)
	}
	rows.Close()
	for _, it := range items {
		var v any
		if json.Unmarshal([]byte(it.detail), &v) == nil && dropQuotes(v, kept) {
			b, err := json.Marshal(v)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE memories SET detail=? WHERE id=?`, string(b), it.id); err != nil {
				return err
			}
		}
		if it.q == "" {
			if _, err := tx.Exec(`DELETE FROM search_words WHERE job_id=? AND item=? AND field='q'`, job, "M"+strconv.FormatInt(it.id, 10)); err != nil {
				return err
			}
		}
	}
	targets, err := findTexts(tx, `SELECT DISTINCT target FROM mentions WHERE job_id=?`, job)
	if err != nil {
		return err
	}
	for _, target := range targets {
		var session string
		if err := tx.QueryRow(`SELECT session_key FROM mentions WHERE job_id=? AND target=? LIMIT 1`, job, target).Scan(&session); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM search_words WHERE job_id=? AND item=? AND field='mention'`, job, target); err != nil {
			return err
		}
		quotes, err := findTexts(tx, `SELECT q FROM mentions WHERE job_id=? AND target=? AND q<>''`, job, target)
		if err != nil {
			return err
		}
		for _, q := range quotes {
			if err := indexMention(tx, session, job, target, q); err != nil {
				return err
			}
		}
	}
	return nil
}

// dropQuotes empties every "q" in a stored item that kept does not keep,
// however deep; it says whether one went.
func dropQuotes(v any, kept func(string) bool) bool {
	changed := false
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if q, ok := e.(string); ok && k == "q" && q != "" && !kept(q) {
				x[k], changed = "", true
				continue
			}
			if dropQuotes(e, kept) {
				changed = true
			}
		}
	case []any:
		for _, e := range x {
			if dropQuotes(e, kept) {
				changed = true
			}
		}
	}
	return changed
}

// removedText is what stays of a dropped reply's job's request or answer.
func removedText(s sql.NullString) any {
	if !s.Valid {
		return s
	}
	return removedString(s.String)
}

// removedString is what stays of one such text: its length and fingerprint
// ("" and an already removed text stay as they are).
func removedString(s string) string {
	if s == "" || strings.HasPrefix(s, removedMark) {
		return s
	}
	p := textPrint(s)
	return fmt.Sprintf("%s%d chars, %s)", removedMark, p.Len, p.H)
}

const removedMark = "(removed with the dropped reply: "

// droppedSource says a job's stored source lost a text it read: a reroll or
// an edit dropped it (scrubDropped empties it; a source a job read is never
// otherwise blank).
func droppedSource(src memory.Source) bool {
	for _, r := range src.Responses {
		if r.Data == "" {
			return true
		}
	}
	return false
}

// heldBy are the texts a stored chat holds (its rows and their revisions),
// as stored and as read: what a late answer for a dropped reply may keep.
func heldBy(tx *sql.Tx, sessionKey string) ([]string, error) {
	var out []string
	for _, q := range []string{`SELECT data FROM turn_members WHERE session_key=?`, `SELECT data FROM revisions WHERE session_key=?`} {
		texts, err := findTexts(tx, q, sessionKey)
		if err != nil {
			return nil, err
		}
		for _, t := range texts {
			out = append(out, t, memory.PlainText(t))
		}
	}
	return out, nil
}

func findTexts(tx *sql.Tx, query string, args ...any) ([]string, error) {
	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
