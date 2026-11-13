package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"archivecenter2/goservice/internal/memory"
)

// PreviewCount distinguishes a failed read from an observed zero. SQL errors
// are deliberately not returned to management clients (they can contain data).
type PreviewCount struct {
	Count  *int64 `json:"count"`
	Status string `json:"status"`
}

type PreviewName struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

type NameMergePreview struct {
	Keep     PreviewName             `json:"keep"`
	Merge    PreviewName             `json:"merge"`
	Counts   map[string]PreviewCount `json:"counts"`
	Warnings []string                `json:"warnings"`
}

// These are the reference slots rewritten by resolver.memory, including slots
// not indexed by MemoryRefs. JSON containment is applied only to typed slots,
// never body, q, slot, terms or arbitrary text that happens to contain an ID.
// A basis naming several sources keeps them joined ("N1,N2"): each one is
// found too. The one id is bound once.
const mergeMemoryRefs = `EXISTS (SELECT 1 FROM (SELECT ? AS r) q WHERE JSON_CONTAINS(JSON_EXTRACT(detail,
	'$.ref','$.at','$.scene','$.about','$.owner','$.to','$.was','$.seen_by',
	'$.roles.actor','$.roles.target','$.roles.object','$.roles.place','$.roles.speaker','$.roles.listener',
	'$.effects[*].about','$.effects[*].was','$.basis.from','$.basis.ref',
	'$.unaware','$.hide_from','$.allow_to','$.knows[*].who','$.knows[*].from','$.knows[*].ref'), JSON_QUOTE(q.r))=1
	OR FIND_IN_SET(q.r, REPLACE(JSON_UNQUOTE(JSON_EXTRACT(detail,'$.basis.from')),' ',''))>0)`

func previewValue(n int64) PreviewCount { return PreviewCount{Count: &n, Status: "available"} }

// PreviewMergeNames is advisory only. It neither enters a pipeline session nor
// changes the merge contract. Global IDs mean child-owned references move too;
// equal spelling in an unrelated chat does not. Only the two supplied IDs and
// their stored kinds are exposed: no name surfaces, quotes, relationship
// endpoints, chat IDs or memory bodies (including protected ones) leave here.
func (d *DB) PreviewMergeNames(chat, keep, merge string) (NameMergePreview, error) {
	out := NameMergePreview{Keep: PreviewName{ID: keep}, Merge: PreviewName{ID: merge}, Counts: map[string]PreviewCount{}, Warnings: []string{}}
	for _, n := range []*PreviewName{&out.Keep, &out.Merge} {
		id, ok := detailID(n.ID, 'N')
		if !ok {
			return out, ErrNameNotFound
		}
		err := d.sql.QueryRow(`SELECT type FROM names WHERE id=? AND session_key=?`, id, chat).Scan(&n.Type)
		if err == sql.ErrNoRows {
			return out, ErrNameNotFound
		}
		if err != nil {
			return out, err
		}
	}
	count := func(key, query string, args ...any) {
		var n int64
		if keep == merge {
			out.Counts[key] = previewValue(0)
			return
		}
		if err := d.sql.QueryRow(query, args...).Scan(&n); err != nil {
			out.Counts[key] = PreviewCount{Status: "unavailable"}
		} else {
			out.Counts[key] = previewValue(n)
		}
	}
	for _, kind := range []string{"event", "fact", "view"} {
		count("memories_"+kind, `SELECT COUNT(*) FROM memories WHERE kind=? AND `+mergeMemoryRefs, kind, merge)
	}
	count("memories", `SELECT COUNT(*) FROM memories WHERE `+mergeMemoryRefs, merge)
	count("effects", `SELECT COUNT(*) FROM memories m JOIN JSON_TABLE(m.detail, '$.effects[*]' COLUMNS (value JSON PATH '$')) e WHERE JSON_CONTAINS(JSON_EXTRACT(e.value,'$.about','$.was'),JSON_QUOTE(?))=1`, merge)
	count("links", `SELECT COUNT(*) FROM links WHERE from_ref=? OR to_ref=?`, merge, merge)
	count("awareness", `SELECT COUNT(*) FROM awareness WHERE target=? OR who=? OR from_who=? OR ref=?`, merge, merge, merge, merge)
	count("relations", `SELECT COUNT(*) FROM name_relations WHERE name_ref=? OR same_as=?`, merge, merge)
	count("user_aliases", `SELECT COUNT(*) FROM name_relations WHERE how='user_alias' AND (name_ref=? OR same_as=?)`, merge, merge)
	if keep == merge {
		out.Counts["user_aliases_added"] = previewValue(0)
	} else {
		out.Counts["user_aliases_added"] = previewValue(1)
	}
	count("mentions", `SELECT COUNT(*) FROM mentions WHERE target=?`, merge)
	count("memory_was", `SELECT COUNT(*) FROM memory_was WHERE target=?`, merge)
	count("search_refs", `SELECT COUNT(*) FROM search_refs WHERE ref=?`, merge)
	count("search_words", `SELECT COUNT(*) FROM search_words WHERE item=?`, merge)
	// The same row/job can appear in several mirrors; UNION removes duplicates.
	scope := `SELECT session_key,job_id FROM memories WHERE ` + mergeMemoryRefs + `
		UNION SELECT session_key,job_id FROM name_relations WHERE name_ref=? OR same_as=?
		UNION SELECT session_key,job_id FROM mentions WHERE target=?
		UNION SELECT session_key,job_id FROM awareness WHERE target=? OR who=? OR from_who=? OR ref=?
		UNION SELECT session_key,job_id FROM links WHERE from_ref=? OR to_ref=?
		UNION SELECT m.session_key,m.job_id FROM memory_was w JOIN memories m ON m.id=w.memory_id WHERE w.target=?
		UNION SELECT session_key,job_id FROM search_refs WHERE ref=?
		UNION SELECT session_key,job_id FROM search_words WHERE item=?`
	args := []any{merge, merge, merge, merge, merge, merge, merge, merge, merge, merge, merge, merge, merge}
	count("sessions", `SELECT COUNT(DISTINCT session_key) FROM (`+scope+`) affected`, args...)
	count("jobs", `SELECT COUNT(DISTINCT job_id) FROM (`+scope+`) affected`, args...)
	count("other_sessions", `SELECT COUNT(DISTINCT session_key) FROM (`+scope+`) affected WHERE session_key<>?`, append(append([]any{}, args...), chat)...)
	count("other_jobs", `SELECT COUNT(DISTINCT job_id) FROM (`+scope+`) affected WHERE session_key<>?`, append(append([]any{}, args...), chat)...)
	// Warnings disclose no surfaces or identity assertions, including disguise.
	count("same_name_others", `SELECT COUNT(*) FROM names n JOIN names a ON a.id=? JOIN names b ON b.id=? WHERE n.session_key=? AND n.id<>a.id AND n.id<>b.id AND (n.name=a.name OR n.name=b.name)`, strings.TrimPrefix(keep, "N"), strings.TrimPrefix(merge, "N"), chat)
	count("same_name_pair", `SELECT COUNT(*) FROM names a JOIN names b ON b.id=? WHERE a.id=? AND a.name=b.name`, strings.TrimPrefix(merge, "N"), strings.TrimPrefix(keep, "N"))
	if v := out.Counts["same_name_others"]; v.Count != nil && *v.Count > 0 {
		out.Warnings = append(out.Warnings, "same_name_other_targets")
	}
	if v := out.Counts["same_name_pair"]; v.Count != nil && *v.Count > 0 {
		out.Warnings = append(out.Warnings, "same_name_distinct_ids")
	}
	if keep != merge && out.Keep.Type != "" && out.Merge.Type != "" && out.Keep.Type != out.Merge.Type {
		out.Warnings = append(out.Warnings, "different_types")
	}
	count("protected_names", `SELECT COUNT(DISTINCT n.id) FROM names n WHERE n.id IN (?,?) AND EXISTS (SELECT 1 FROM awareness a WHERE a.kind IN ('unaware','hide_from') AND (a.target=CONCAT('N',n.id) OR a.target IN (SELECT CONCAT('R',r.id) FROM name_relations r WHERE r.name_ref=CONCAT('N',n.id) OR r.same_as=CONCAT('N',n.id))))`, strings.TrimPrefix(keep, "N"), strings.TrimPrefix(merge, "N"))
	if v := out.Counts["protected_names"]; v.Count != nil && *v.Count > 0 {
		out.Warnings = append(out.Warnings, "protected_name_change")
	}
	for _, key := range []string{"documents_changed", "embedding_jobs", "flat_documents", "contextual_documents"} {
		out.Counts[key] = PreviewCount{Status: "unavailable"}
	}
	if keep == merge {
		for _, key := range []string{"documents_changed", "embedding_jobs", "flat_documents", "contextual_documents"} {
			out.Counts[key] = previewValue(0)
		}
	} else if docs, jobs, err := d.mergePreviewDocuments(merge, keep); err == nil {
		out.Counts["documents_changed"], out.Counts["flat_documents"] = previewValue(docs), previewValue(docs)
		out.Counts["embedding_jobs"] = previewValue(int64(len(jobs)))
		var total int64
		ok := true
		for start := 0; start < len(jobs); start += 250 {
			in, params := inJobs(jobs[start:min(start+250, len(jobs))])
			var n int64
			if err := d.sql.QueryRow(`SELECT COUNT(*) FROM memories WHERE job_id IN `+in, params...).Scan(&n); err != nil {
				ok = false
				break
			}
			total += n
		}
		if ok {
			out.Counts["contextual_documents"] = previewValue(total)
		}
	}
	return out, nil
}

// DocText, rather than a changed-field count, defines the embedding range.
// Only matching rows are read by PK cursor, one page at a time; all other
// counts are SQL aggregates. Retain job IDs, never a chat's hydrated bodies.
// This is a potential key-change range, not cache misses or provider calls.
func (d *DB) mergePreviewDocuments(from, to string) (int64, []int64, error) {
	var docs, after int64
	var jobs []int64
	seen := map[int64]bool{}
	r := resolver{ids: map[string]string{from: to}}
	for {
		rows, err := d.sql.Query(`SELECT id,job_id,detail FROM memories WHERE id>? AND `+mergeMemoryRefs+` ORDER BY id LIMIT 100`, after, from)
		if err != nil {
			return 0, nil, err
		}
		type row struct {
			id, job int64
			m       memory.Memory
		}
		page := []row{}
		for rows.Next() {
			var v row
			var raw string
			if err = rows.Scan(&v.id, &v.job, &raw); err == nil {
				err = json.Unmarshal([]byte(raw), &v.m)
			}
			if err != nil {
				rows.Close()
				return 0, nil, err
			}
			page = append(page, v)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return 0, nil, err
		}
		if len(page) == 0 {
			break
		}
		ids := []int64{}
		unique := map[int64]bool{}
		for _, v := range page {
			for _, ref := range append(memory.NameRefs(v.m), to) {
				if n, ok := detailID(ref, 'N'); ok && !unique[n] {
					unique[n] = true
					ids = append(ids, n)
				}
			}
		}
		names := map[string]string{}
		for start := 0; start < len(ids); start += 250 {
			in, args := inJobs(ids[start:min(start+250, len(ids))])
			if err = d.each(`SELECT id,name FROM names WHERE id IN `+in, args, func(rows *sql.Rows) error {
				var id int64
				var label string
				if err := rows.Scan(&id, &label); err != nil {
					return err
				}
				names[fmt.Sprintf("N%d", id)] = label
				return nil
			}); err != nil {
				return 0, nil, err
			}
		}
		for _, v := range page {
			if memory.DocText(memory.KnownMemory{Memory: v.m}, names) != memory.DocText(memory.KnownMemory{Memory: r.memory(v.m)}, names) {
				docs++
				if !seen[v.job] {
					seen[v.job] = true
					jobs = append(jobs, v.job)
				}
			}
			after = v.id
		}
	}
	return docs, jobs, nil
}
