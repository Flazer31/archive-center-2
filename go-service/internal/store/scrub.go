package store

import (
	"database/sql"
	"encoding/json"
	"strings"
	"unicode"

	"archivecenter2/goservice/internal/lifecycle"
	"archivecenter2/goservice/internal/memory"
)

// scrubDropped removes the bodies a reroll or an edit dropped from the
// chat's raw observations and from its generation jobs' sources, so no copy
// of an earlier reply stays. A text the chat still holds anywhere (an input,
// a current reply, an identical line elsewhere) is never removed. In an
// observation only the text goes (its length and fingerprint stay, so the
// plugin is asked to resend a text it needs); in a job source the
// response's data becomes empty.
func scrubDropped(tx *sql.Tx, s *lifecycle.Session, dropped map[string][]string) error {
	if len(dropped) == 0 {
		return nil
	}
	held := s.Held()
	gone := map[string]bool{}
	for _, texts := range dropped {
		for _, t := range texts {
			if t != "" && !held[t] {
				gone[t] = true
			}
		}
	}
	// a chat without id shares its observations' chat id ("") with other
	// such chats: only its job sources are cleared
	chat, ok := strings.CutPrefix(s.Key, "chat:")
	for t := range gone {
		if ok {
			if err := scrubObservations(tx, chat, t, held); err != nil {
				return err
			}
		}
		// by text, not row key: a source written before the row got its chat
		// id names it by its old key
		if err := scrubSources(tx, s.Key, t); err != nil {
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

func scrubSources(tx *sql.Tx, sessionKey, text string) error {
	all, err := findRows(tx, `SELECT job_id, source FROM gen_jobs WHERE session_key=? AND INSTR(source, ?)>0`, sessionKey, fragment(text))
	if err != nil {
		return err
	}
	for _, f := range all {
		var src memory.Source
		if json.Unmarshal([]byte(f.body), &src) != nil {
			continue
		}
		changed := false
		for i, r := range src.Responses {
			if r.Data == text {
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
	}
	return nil
}
