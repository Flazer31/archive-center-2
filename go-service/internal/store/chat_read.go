package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

var ErrChatNotFound = errors.New("chat_not_found")

// sqlSourceQuote spells encoding/json's string encoding on the server. Only
// source-key projection uses it: no session bodies cross the compare boundary.
// In particular JSON_QUOTE's uppercase control escapes differ from Go's.
func sqlSourceQuote(value string) string {
	value = "REPLACE(" + value + ",CHAR(92),CONCAT(CHAR(92),CHAR(92)))"
	value = "REPLACE(" + value + ",CHAR(34),CONCAT(CHAR(92),CHAR(34)))"
	for i := 0; i < 32; i++ {
		esc := fmt.Sprintf("u%04x", i)
		if s, ok := map[int]string{8: "b", 9: "t", 10: "n", 12: "f", 13: "r"}[i]; ok {
			esc = s
		}
		value = fmt.Sprintf("REPLACE(%s,CHAR(%d),CONCAT(CHAR(92),'%s'))", value, i, esc)
	}
	for _, r := range []rune{'<', '>', '&', '\u2028', '\u2029'} {
		value = fmt.Sprintf("REPLACE(%s,'%c',CONCAT(CHAR(92),'u%04x'))", value, r, r)
	}
	return "CONCAT(CHAR(34)," + value + ",CHAR(34))"
}

// Same ordered {Session,Turn,Members:[{Data}],Responses:[{Seq,Data}]} hash as
// memory.Source.Key. Row keys, generation IDs and dates deliberately stay out.
func sqlSourceKey(session, turn, members, responses string) string {
	return "LEFT(SHA2(CONCAT('{\"Session\":'," + sqlSourceQuote(session) +
		",',\"Turn\":'," + turn + ",',\"Members\":'," + members +
		",',\"Responses\":'," + responses + ",'}'),256),32)"
}

func sqlLocalSourceKey(whole bool) string {
	members := `(SELECT COALESCE(CONCAT('[',GROUP_CONCAT(CONCAT('{"Data":',` + sqlSourceQuote("m.data") + `,'}') ORDER BY m.ord SEPARATOR ','),']'),'null') FROM turn_members m WHERE m.session_key=t.session_key AND m.turn_id=t.turn_id AND m.state='active')`
	off := " AND NOT r.off"
	if whole {
		off = ""
	}
	read := `(v.seq IS NOT NULL AND (r.generation_id='' OR v.generation_id='' OR v.generation_id=r.generation_id))`
	responses := `(SELECT COALESCE(CONCAT('[',GROUP_CONCAT(CONCAT('{"Seq":',IF(` + read + `,v.seq,0),',"Data":',` + sqlSourceQuote("IF("+read+",v.data,'')") + `,'}') ORDER BY r.ord SEPARATOR ','),']'),'null') FROM response_rows r LEFT JOIN revisions v ON v.session_key=r.session_key AND v.row_key=r.row_key AND v.seq=(SELECT MAX(x.seq) FROM revisions x WHERE x.session_key=r.session_key AND x.row_key=r.row_key) WHERE r.session_key=t.session_key AND r.turn_id=t.turn_id AND r.state='active'` + off + `)`
	return sqlSourceKey("t.session_key", "t.turn_id", members, responses)
}

func sqlCopySourceKey() string {
	members := `(SELECT COALESCE(CONCAT('[',GROUP_CONCAT(CONCAT('{"Data":',` + sqlSourceQuote("m.data") + `,'}') ORDER BY m.ord SEPARATOR ','),']'),'null') FROM JSON_TABLE(c.source,'$.members[*]' COLUMNS(ord FOR ORDINALITY,data LONGTEXT PATH '$.data')) m)`
	responses := `(SELECT COALESCE(CONCAT('[',GROUP_CONCAT(CONCAT('{"Seq":',r.seq,',"Data":',` + sqlSourceQuote("r.data") + `,'}') ORDER BY r.ord SEPARATOR ','),']'),'null') FROM JSON_TABLE(c.source,'$.responses[*]' COLUMNS(ord FOR ORDINALITY,seq INT PATH '$.seq',data LONGTEXT PATH '$.data')) r)`
	return sqlSourceKey("JSON_UNQUOTE(JSON_EXTRACT(c.source,'$.session'))", "JSON_EXTRACT(c.source,'$.turn')", members, responses)
}

// Only metadata survives this projection. It mirrors memory.Own and turnJobs:
// active non-greeting/on local turns, a still-recorded whole copy with no off
// reply, then the exact parent's done job, or the child's own done job.
// SET STATEMENT is connection-local and automatically restored; without it
// GROUP_CONCAT's default ceiling can truncate a single long source's hash.
func chatReadCTE() string {
	return `SET STATEMENT group_concat_max_len=4294967295 FOR WITH local_sources AS (
	 SELECT t.turn_id,` + sqlLocalSourceKey(false) + ` AS own_source,
	 CASE WHEN b.copies_chat=b.parent_chat AND b.copies_message=b.parent_message AND b.copies_chat<>''
	 AND c.own_key=` + sqlLocalSourceKey(true) + `
	 AND NOT EXISTS(SELECT 1 FROM response_rows r WHERE r.session_key=t.session_key AND r.turn_id=t.turn_id AND r.state='active' AND r.off)
	 THEN ` + sqlCopySourceKey() + ` END AS parent_source,
	 JSON_UNQUOTE(JSON_EXTRACT(c.source,'$.session')) AS parent_session,
	 JSON_EXTRACT(c.source,'$.turn') AS parent_turn
	 FROM logical_turns t LEFT JOIN session_branch b ON b.session_key=t.session_key
	 LEFT JOIN branch_copies c ON c.session_key=t.session_key AND c.turn_id=t.turn_id
	 WHERE t.session_key=? AND NOT t.greeting AND NOT t.inactive AND NOT t.off
	), scope AS (
	 SELECT s.turn_id,COALESCE(p.job_id,o.job_id) AS job_id
	 FROM local_sources s LEFT JOIN gen_jobs p ON p.session_key=s.parent_session AND p.turn_id=s.parent_turn AND p.source_key=s.parent_source AND p.state='done'
	 LEFT JOIN gen_jobs o ON o.session_key=? AND o.turn_id=s.turn_id AND o.source_key=s.own_source AND o.state='done'
	) `
}

type chatRead struct {
	tx   *sql.Tx
	chat string
}

func (d *DB) beginChatRead(ctx context.Context, chat string) (*chatRead, error) {
	tx, err := d.sql.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	var exists int
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM logical_turns WHERE session_key=?) OR EXISTS(SELECT 1 FROM chat_labels WHERE session_key=?)`, chat, chat).Scan(&exists)
	if err != nil || exists == 0 {
		tx.Rollback()
		if err == nil {
			err = ErrChatNotFound
		}
		return nil, err
	}
	return &chatRead{tx: tx, chat: chat}, nil
}

// Small PK/job mappings only, shared by comparison and export. Unlike
// LoadSession/ChatMemories this never materializes a session's original text.
func (r *chatRead) jobs() ([]int64, map[int64]int, error) {
	ids := []int64{}
	local := map[int64]int{}
	err := eachOn(r.tx, chatReadCTE()+`SELECT job_id,turn_id FROM scope WHERE job_id IS NOT NULL ORDER BY turn_id`, []any{r.chat, r.chat}, func(rows *sql.Rows) error {
		var id int64
		var turn int
		if err := rows.Scan(&id, &turn); err != nil {
			return err
		}
		if _, ok := local[id]; !ok {
			ids = append(ids, id)
		}
		local[id] = turn
		return nil
	})
	return ids, local, err
}
