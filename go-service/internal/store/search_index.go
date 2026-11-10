package store

import (
	"database/sql"
	"encoding/json"
	"strings"

	"archivecenter2/goservice/internal/memory"
)

// searchSchema holds what the reading range looks up instead of loading a
// whole chat: the words and the references of every stored item, by the job
// that wrote it (a read keeps its own jobs), and reverse lookups on the
// tables records point through. All of it is derived from what SaveGeneration
// stores; losing it loses no memory. Applied after memorySchema (file order).
var searchSchema = []string{
	`CREATE TABLE IF NOT EXISTS search_words (
		session_key VARCHAR(191) NOT NULL,
		job_id BIGINT NOT NULL,
		item VARCHAR(500) NOT NULL,        -- M<id>, or the target of a mention (as long as mentions.target)
		field VARCHAR(16) NOT NULL,        -- body | q | slot | ... | mention
		word LONGTEXT NOT NULL,            -- whole, as long as the text it comes from
		KEY by_word (session_key, word(100), job_id),
		KEY by_item (session_key, item(64)),
		KEY by_job (job_id, field)
	) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`,
	`CREATE TABLE IF NOT EXISTS search_refs (
		session_key VARCHAR(191) NOT NULL,
		job_id BIGINT NOT NULL,
		item VARCHAR(64) NOT NULL,         -- M<id>
		field VARCHAR(16) NOT NULL,        -- about | owner | actor | effect | ...
		ref LONGTEXT NOT NULL,             -- N<id>, M<id>, M<id>.<k>, R<id>, or the unresolved text, whole
		KEY by_ref (session_key, ref(100), job_id),
		KEY by_item (session_key, item),
		KEY by_job (job_id)
	) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`,
	`CREATE INDEX IF NOT EXISTS by_from ON links (session_key, from_ref(64))`,
	`CREATE INDEX IF NOT EXISTS by_to ON links (session_key, to_ref(64))`,
	`CREATE INDEX IF NOT EXISTS by_target ON memory_was (target(64))`,
	`CREATE INDEX IF NOT EXISTS by_about ON effects (about(64))`,
}

func init() { schema = append(schema, searchSchema...) }

// indexMemory stores the words and references of a saved memory.
func indexMemory(tx *sql.Tx, session string, job int64, item string, m memory.Memory) error {
	words := memory.MemoryWords(m)
	body := []string{}
	for _, e := range words {
		if e.Field == "body" {
			body = append(body, e.Value)
		}
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE memories SET read_words=? WHERE id=? AND job_id=?`, string(encoded), strings.TrimPrefix(item, "M"), job); err != nil {
		return err
	}
	if err := insertEntries(tx, "search_words", "word", session, job, item, words); err != nil {
		return err
	}
	return insertEntries(tx, "search_refs", "ref", session, job, item, memory.MemoryRefs(m))
}

// indexMention stores the words of a re-mention's quote under the item it
// mentions.
func indexMention(tx *sql.Tx, session string, job int64, target, q string) error {
	var es []memory.IndexEntry
	for _, w := range memory.Words(q) {
		es = append(es, memory.IndexEntry{Field: "mention", Value: w})
	}
	return insertEntries(tx, "search_words", "word", session, job, target, es)
}

func insertEntries(tx *sql.Tx, table, col, session string, job int64, item string, es []memory.IndexEntry) error {
	for start := 0; start < len(es); start += 200 {
		part := es[start:min(start+200, len(es))]
		var args []any
		for _, e := range part {
			args = append(args, session, job, item, e.Field, e.Value)
		}
		q := `INSERT INTO ` + table + ` (session_key, job_id, item, field, ` + col + `) VALUES ` + strings.TrimSuffix(strings.Repeat("(?,?,?,?,?),", len(part)), ",")
		if _, err := tx.Exec(q, args...); err != nil {
			return err
		}
	}
	return nil
}
