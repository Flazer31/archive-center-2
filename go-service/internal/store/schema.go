// Package store keeps AC2 data in MariaDB (canonical truth storage).
package store

// schema is applied on open; every statement is idempotent.
var schema = []string{
	`CREATE TABLE IF NOT EXISTS raw_observations (
		id BIGINT AUTO_INCREMENT PRIMARY KEY,
		received_at DATETIME(6) NOT NULL,
		receive_seq BIGINT NOT NULL,
		plugin_version VARCHAR(32) NOT NULL DEFAULT '',
		boot_id VARCHAR(64) NULL,
		seq BIGINT NULL,
		hook VARCHAR(32) NOT NULL DEFAULT '',
		chat_id VARCHAR(191) NULL, -- plugin chat id of the event's chat ('' without id), NULL without chat
		chat_id2 VARCHAR(191) NULL, -- a second chat in the same event (output re-read of another chat)
		event LONGTEXT NOT NULL,
		UNIQUE KEY plugin_order (boot_id, seq),
		KEY by_chat (chat_id, id),
		KEY by_chat2 (chat_id2, id)
	) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`,
	`CREATE TABLE IF NOT EXISTS logical_turns (
		session_key VARCHAR(191) NOT NULL,
		turn_id INT NOT NULL,
		greeting BOOL NOT NULL,
		confirmed BOOL NOT NULL,
		inactive BOOL NOT NULL,
		off BOOL NOT NULL DEFAULT FALSE,  -- a row of the turn is disabled in the host
		dates TEXT NULL,                   -- date variables last seen (JSON object)
		PRIMARY KEY (session_key, turn_id)
	) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`,
	`CREATE TABLE IF NOT EXISTS turn_members (
		session_key VARCHAR(191) NOT NULL,
		member_key VARCHAR(191) NOT NULL,
		turn_id INT NOT NULL,
		ord INT NOT NULL,
		data LONGTEXT NOT NULL,
		state VARCHAR(16) NOT NULL,
		PRIMARY KEY (session_key, member_key),
		KEY by_turn (session_key, turn_id, ord)
	) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`,
	`CREATE TABLE IF NOT EXISTS response_rows (
		session_key VARCHAR(191) NOT NULL,
		row_key VARCHAR(191) NOT NULL,
		turn_id INT NOT NULL,
		ord INT NOT NULL,
		state VARCHAR(16) NOT NULL,
		generation_id VARCHAR(191) NOT NULL DEFAULT '',
		awaiting_replacement BOOL NOT NULL DEFAULT FALSE,
		off BOOL NOT NULL DEFAULT FALSE, -- the reply is disabled in the host
		PRIMARY KEY (session_key, row_key),
		KEY by_turn (session_key, turn_id, ord)
	) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`,
	`CREATE TABLE IF NOT EXISTS revisions (
		session_key VARCHAR(191) NOT NULL,
		row_key VARCHAR(191) NOT NULL,
		seq INT NOT NULL,
		data LONGTEXT NOT NULL,
		generation_id VARCHAR(191) NOT NULL,
		source VARCHAR(32) NOT NULL,
		confirmed BOOL NOT NULL,
		PRIMARY KEY (session_key, row_key, seq)
	) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`,
	`CREATE TABLE IF NOT EXISTS session_branch (
		session_key VARCHAR(191) NOT NULL PRIMARY KEY,
		parent_chat VARCHAR(191) NOT NULL,
		parent_message VARCHAR(191) NOT NULL,
		markers TEXT NULL, -- branch markers seen (JSON [[chat, message], ...])
		copies_chat VARCHAR(191) NULL, -- the parent the copies were recorded against
		copies_message VARCHAR(191) NULL
	) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`,
	// C22: a stored chat attached as a chat's earlier story, frozen when
	// attached (lifecycle.Session.Past); not part of host observation.
	`CREATE TABLE IF NOT EXISTS session_past (
		session_key VARCHAR(191) NOT NULL,
		pos INT NOT NULL,
		from_chat VARCHAR(191) NOT NULL, -- the chat it was attached from
		source LONGTEXT NOT NULL,        -- the source the turn reads (JSON)
		own_source LONGTEXT NULL,        -- that chat's own source of a copy it made (JSON)
		source_chat VARCHAR(191) NOT NULL, -- the chat whose memory source reads
		own_chat VARCHAR(191) NULL,        -- the chat whose memory own_source reads
		PRIMARY KEY (session_key, pos),
		KEY session_past_source (source_chat),
		KEY session_past_own (own_chat)
	) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`,
	`CREATE TABLE IF NOT EXISTS branch_copies (
		session_key VARCHAR(191) NOT NULL,
		turn_id INT NOT NULL,
		own_key VARCHAR(64) NOT NULL, -- the branch turn's source key when recorded
		source LONGTEXT NOT NULL,     -- the parent source it reads (JSON)
		PRIMARY KEY (session_key, turn_id)
	) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`,
	`CREATE TABLE IF NOT EXISTS session_progress (
		session_key VARCHAR(191) NOT NULL,
		boot_id VARCHAR(64) NOT NULL,
		seq BIGINT NOT NULL,
		PRIMARY KEY (session_key, boot_id)
	) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`,
	`CREATE TABLE IF NOT EXISTS lifecycle_events (
		id BIGINT AUTO_INCREMENT PRIMARY KEY,
		session_key VARCHAR(191) NOT NULL,
		turn_id INT NOT NULL,
		kind VARCHAR(64) NOT NULL,
		detail TEXT NOT NULL,
		KEY by_session (session_key, id)
	) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`,
	// rows cut away from the front of the host chat (/cut, /del) are kept;
	// the mark survives a restart (review 22 A-5)
	`ALTER TABLE turn_members ADD COLUMN IF NOT EXISTS cut BOOL NOT NULL DEFAULT FALSE`,
	`ALTER TABLE response_rows ADD COLUMN IF NOT EXISTS cut BOOL NOT NULL DEFAULT FALSE`,
}
