package store

// memorySchema holds the generated memory (AC2-GEN-ITEMS-V2.md). Every row a
// generation job adds carries its job_id: a job belongs to one turn read at
// one source (input texts + response revisions), so when a reroll or an edit
// changes the turn's source, that job's contributions drop out of current
// reads and stay as history. Persistent ids are N<id> names, R<id> identity
// relations, M<id> memories, M<id>.<k> event effects, S<id> scenes and
// L<id> links.
var memorySchema = []string{
	`CREATE TABLE IF NOT EXISTS name_merges (
		id BIGINT AUTO_INCREMENT PRIMARY KEY,
		session_key VARCHAR(191) NOT NULL,
		job_id BIGINT NOT NULL,
		keep_ref VARCHAR(64) NOT NULL,
		merge_ref VARCHAR(64) NOT NULL,
		relation_id BIGINT NOT NULL,
		created_at DATETIME(6) NOT NULL,
		undone_at DATETIME(6) NULL,
		KEY by_session (session_key,id), KEY by_job (job_id)
	) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`,
	`CREATE TABLE IF NOT EXISTS name_ref_changes (
		id BIGINT AUTO_INCREMENT PRIMARY KEY,
		merge_id BIGINT NOT NULL,
		session_key VARCHAR(191) NOT NULL,
		job_id BIGINT NOT NULL,
		row_table VARCHAR(32) NOT NULL,
		row_id BIGINT NOT NULL,
		path VARCHAR(191) NOT NULL,
		origin_ref VARCHAR(500) NOT NULL,
		before_ref VARCHAR(500) NOT NULL,
		after_ref VARCHAR(500) NOT NULL,
		KEY by_row (row_table,row_id,path,id), KEY by_job (job_id),
		KEY by_merge (merge_id), KEY by_session (session_key)
	) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`,
	`CREATE TABLE IF NOT EXISTS gen_jobs (
		job_id BIGINT AUTO_INCREMENT PRIMARY KEY,
		session_key VARCHAR(191) NOT NULL,
		turn_id INT NOT NULL,
		source_key VARCHAR(64) NOT NULL,
		source LONGTEXT NOT NULL,          -- what the job read (memory.Source)
		state VARCHAR(16) NOT NULL,        -- requested | received | unreadable | done | failed
		attempts INT NOT NULL DEFAULT 0,   -- model calls started for this job
		next_at DATETIME(6) NULL,          -- after a failed call: no new call before this
		unread_answers LONGTEXT NULL,      -- JSON list of answers in which nothing could be read, as received
		model VARCHAR(191) NOT NULL DEFAULT '',
		request LONGTEXT NULL,             -- exact model input
		read_items LONGTEXT NULL,          -- stored items shown to the model
		response LONGTEXT NULL,            -- raw model output, kept as received
		elapsed_days BIGINT NULL,           -- turn delta; NULL for legacy generations
		notes LONGTEXT NULL,               -- parse and resolve diagnostics
		created_at DATETIME(6) NOT NULL,
		done_at DATETIME(6) NULL,
		UNIQUE KEY one_job_per_source (session_key, turn_id, source_key),
		KEY by_turn (session_key, turn_id)
	) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`,
	`ALTER TABLE gen_jobs ADD COLUMN IF NOT EXISTS elapsed_days BIGINT NULL`,
	`CREATE TABLE IF NOT EXISTS names (
		id BIGINT AUTO_INCREMENT PRIMARY KEY,
		session_key VARCHAR(191) NOT NULL,
		job_id BIGINT NOT NULL,
		name VARCHAR(500) NOT NULL,
		type VARCHAR(500) NOT NULL DEFAULT '',
		q LONGTEXT NOT NULL,
		KEY by_session (session_key, name(100)),
		KEY by_job (job_id)
	) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`,
	`CREATE TABLE IF NOT EXISTS name_relations (
		id BIGINT AUTO_INCREMENT PRIMARY KEY,
		session_key VARCHAR(191) NOT NULL,
		job_id BIGINT NOT NULL,
		name_ref VARCHAR(64) NOT NULL,     -- N<id>
		same_as VARCHAR(500) NOT NULL,     -- N<id>, or the unresolved text
		how VARCHAR(500) NOT NULL DEFAULT '',
		q LONGTEXT NOT NULL,
		KEY by_job (job_id)
	) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`,
	`CREATE TABLE IF NOT EXISTS memories (
		id BIGINT AUTO_INCREMENT PRIMARY KEY,
		session_key VARCHAR(191) NOT NULL,
		job_id BIGINT NOT NULL,
		kind VARCHAR(16) NOT NULL,
		body LONGTEXT NOT NULL,
		q LONGTEXT NOT NULL,
		imp DOUBLE NULL,                   -- NULL = not judged (differs from 0)
		about VARCHAR(500) NOT NULL DEFAULT '',
		slot VARCHAR(500) NOT NULL DEFAULT '',
		mode VARCHAR(500) NOT NULL DEFAULT '',
		status VARCHAR(500) NOT NULL DEFAULT '',
		owner VARCHAR(500) NOT NULL DEFAULT '',
		aspect VARCHAR(500) NOT NULL DEFAULT '',
		scene VARCHAR(500) NOT NULL DEFAULT '',
		detail LONGTEXT NOT NULL,          -- the whole item with persistent ids (lossless)
		doc_hash CHAR(64) NOT NULL DEFAULT '',
		KEY by_session (session_key, id),
		KEY by_doc (doc_hash),
		KEY by_job (job_id)
	) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`,
	`CREATE TABLE IF NOT EXISTS memory_was (
		memory_id BIGINT NOT NULL,
		ord INT NOT NULL,
		target VARCHAR(500) NOT NULL,      -- M<id>, M<id>.<k>, or the unresolved text
		PRIMARY KEY (memory_id, ord)
	) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`,
	`CREATE TABLE IF NOT EXISTS effects (
		memory_id BIGINT NOT NULL,
		k INT NOT NULL,                    -- M<memory_id>.<k>
		about VARCHAR(500) NOT NULL DEFAULT '',
		slot VARCHAR(500) NOT NULL DEFAULT '',
		before_value LONGTEXT NOT NULL,
		value LONGTEXT NOT NULL,
		delta VARCHAR(191) NOT NULL DEFAULT '',
		was TEXT NOT NULL,                 -- JSON list of earlier records this effect replaced
		status VARCHAR(500) NOT NULL DEFAULT '',
		PRIMARY KEY (memory_id, k)
	) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`,
	`CREATE TABLE IF NOT EXISTS mentions (
		id BIGINT AUTO_INCREMENT PRIMARY KEY,
		session_key VARCHAR(191) NOT NULL,
		job_id BIGINT NOT NULL,
		target VARCHAR(500) NOT NULL,      -- the re-mentioned item
		q LONGTEXT NOT NULL,
		KEY by_target (session_key, target(64)),
		KEY by_job (job_id)
	) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`,
	`CREATE TABLE IF NOT EXISTS awareness (
		id BIGINT AUTO_INCREMENT PRIMARY KEY,
		session_key VARCHAR(191) NOT NULL,
		job_id BIGINT NOT NULL,
		target VARCHAR(500) NOT NULL,      -- memory, name, relation or link it applies to
		kind VARCHAR(16) NOT NULL,         -- knows | unaware | hide_from | allow_to
		who VARCHAR(500) NOT NULL,
		via VARCHAR(500) NOT NULL DEFAULT '',
		from_who VARCHAR(500) NOT NULL DEFAULT '',
		extent VARCHAR(500) NOT NULL DEFAULT '',
		part LONGTEXT NOT NULL,
		ref VARCHAR(500) NOT NULL DEFAULT '',
		q LONGTEXT NOT NULL,
		KEY by_target (session_key, target(64)),
		KEY by_job (job_id)
	) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`,
	`CREATE TABLE IF NOT EXISTS scenes (
		id BIGINT AUTO_INCREMENT PRIMARY KEY,
		session_key VARCHAR(191) NOT NULL,
		job_id BIGINT NOT NULL,            -- the job that opened it
		KEY by_job (job_id)
	) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`,
	`CREATE TABLE IF NOT EXISTS scene_marks (
		id BIGINT AUTO_INCREMENT PRIMARY KEY,
		session_key VARCHAR(191) NOT NULL,
		job_id BIGINT NOT NULL,
		scene VARCHAR(500) NOT NULL,       -- S<id>, or the unresolved text
		change_kind VARCHAR(16) NOT NULL,  -- open | continue | close
		q LONGTEXT NOT NULL,
		KEY by_job (job_id)
	) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`,
	`CREATE TABLE IF NOT EXISTS links (
		id BIGINT AUTO_INCREMENT PRIMARY KEY,
		session_key VARCHAR(191) NOT NULL,
		job_id BIGINT NOT NULL,
		from_ref VARCHAR(500) NOT NULL,
		rel VARCHAR(500) NOT NULL,
		to_ref VARCHAR(500) NOT NULL,
		q LONGTEXT NOT NULL,
		KEY by_job (job_id)
	) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`,
	// Direct-node management reads use endpoint PK cursors, independently
	// of generation job. Existing rows acquire the same lookup indexes.
	`CREATE INDEX IF NOT EXISTS detail_from ON links (from_ref(64), id)`,
	`CREATE INDEX IF NOT EXISTS detail_to ON links (to_ref(64), id)`,
	`ALTER TABLE memories ADD COLUMN IF NOT EXISTS read_words LONGTEXT NULL`,
	// C18 pin / suppress (memory_hold.go): pin | mute | "" (none)
	`ALTER TABLE memories ADD COLUMN IF NOT EXISTS hold VARCHAR(8) NOT NULL DEFAULT ''`,
	`CREATE INDEX IF NOT EXISTS detail_target ON awareness (target(64), id)`,
	// document vectors by model and document text (embeddings.go): each
	// model's dimension, the vectors in a VECTOR(<dim>) table per dimension.
	// The same text is embedded once; a vector stays when its memory leaves
	// current reads (search filters by current jobs). Losing them loses no
	// memory.
	`CREATE TABLE IF NOT EXISTS embedding_models (
		model VARCHAR(191) PRIMARY KEY,
		dim INT NOT NULL
	) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`,
	// history turns the user asked to make (a range from /memory/backfill),
	// until each is made; kept across a restart
	// one row per memory delivery (the request's input, the choice and every
	// candidate considered), kept so a delivery can be checked after a
	// restart; the latest deliveryTracesKept per chat are kept
	`CREATE TABLE IF NOT EXISTS delivery_traces (
		id BIGINT AUTO_INCREMENT PRIMARY KEY,
		session_key VARCHAR(191) NOT NULL,
		at DATETIME(6) NOT NULL,
		summary LONGTEXT NOT NULL,         -- JSON: input, window, ceilings, used, warning
		block LONGTEXT NOT NULL,           -- the delivered text as sent
		trace LONGTEXT NOT NULL,           -- JSON list of TraceRow
		KEY by_session (session_key, id)
	) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`,
	`CREATE TABLE IF NOT EXISTS gen_grants (
		session_key VARCHAR(191) NOT NULL,
		turn_id INT NOT NULL,
		created_at DATETIME(6) NOT NULL,
		PRIMARY KEY (session_key, turn_id)
	) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`,
	// which persistent id each new item of a job's parsed answer got (the
	// array and the item's place in it), so a later re-read of the same
	// stored answer can give a changed item its existing id
	`CREATE TABLE IF NOT EXISTS gen_items (
		job_id BIGINT NOT NULL,
		arr VARCHAR(16) NOT NULL,          -- entities | relations | memories | scenes | links
		pos INT NOT NULL,                  -- index in the parsed array
		temp_id LONGTEXT NOT NULL,         -- the id the model wrote, as written ('' if none)
		item_id VARCHAR(64) NOT NULL,      -- the persistent id issued
		PRIMARY KEY (job_id, arr, pos)
	) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`,
	`CREATE INDEX IF NOT EXISTS by_item ON gen_items (item_id)`,
	// Model-written values used to stop at 32 (or 64) characters, and a longer
	// one failed the turn's whole save (11-05 tester report). Existing
	// databases widen on start; the full item stays in detail as before.
	`ALTER TABLE names MODIFY type VARCHAR(500) NOT NULL DEFAULT ''`,
	`ALTER TABLE name_relations MODIFY how VARCHAR(500) NOT NULL DEFAULT ''`,
	`ALTER TABLE memories MODIFY mode VARCHAR(500) NOT NULL DEFAULT '', MODIFY status VARCHAR(500) NOT NULL DEFAULT '', MODIFY aspect VARCHAR(500) NOT NULL DEFAULT '', MODIFY scene VARCHAR(500) NOT NULL DEFAULT ''`,
	`ALTER TABLE effects MODIFY status VARCHAR(500) NOT NULL DEFAULT ''`,
	`ALTER TABLE awareness MODIFY via VARCHAR(500) NOT NULL DEFAULT '', MODIFY extent VARCHAR(500) NOT NULL DEFAULT ''`,
	`ALTER TABLE scene_marks MODIFY scene VARCHAR(500) NOT NULL`,
	`ALTER TABLE links MODIFY rel VARCHAR(500) NOT NULL`,
}

func init() { schema = append(schema, memorySchema...) }
