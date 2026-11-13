package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// TextHash names a document text for vector reuse.
func TextHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// HaveEmbeddings reports which of the text hashes already have a vector of
// the model.
func (d *DB) HaveEmbeddings(model string, hashes []string) (map[string]bool, error) {
	have := map[string]bool{}
	if len(hashes) == 0 {
		return have, nil
	}
	dim, err := d.embeddingDimension(model)
	if err != nil || dim == 0 {
		return have, err
	}
	for start := 0; start < len(hashes); start += 500 {
		part := hashes[start:min(start+500, len(hashes))]
		args := []any{model}
		for _, h := range part {
			args = append(args, h)
		}
		q := fmt.Sprintf(`SELECT text_hash FROM embedding_vectors_%d WHERE model=? AND text_hash IN (`, dim) + strings.TrimSuffix(strings.Repeat("?,", len(part)), ",") + `)`
		err := d.each(q, args, func(rows *sql.Rows) error {
			var h string
			if err := rows.Scan(&h); err != nil {
				return err
			}
			have[h] = true
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return have, nil
}

// SaveEmbeddings stores vectors of the model by text hash (a vector already
// stored is kept).
func (d *DB) SaveEmbeddings(model string, hashes []string, vectors [][]float32) error {
	if len(hashes) != len(vectors) {
		return fmt.Errorf("embedding hashes/vectors length mismatch: %d/%d", len(hashes), len(vectors))
	}
	if len(hashes) == 0 {
		return nil
	}
	dim := len(vectors[0])
	if dim == 0 {
		return fmt.Errorf("embedding model %q: empty vector", model)
	}
	for _, v := range vectors {
		if len(v) != dim {
			return fmt.Errorf("embedding model %q: mixed vector dimensions %d and %d", model, dim, len(v))
		}
	}
	storedDim, err := d.embeddingDimension(model)
	if err != nil {
		return err
	}
	if storedDim != 0 && storedDim != dim {
		return fmt.Errorf("embedding model %q: dimension %d does not match stored dimension %d", model, dim, storedDim)
	}
	// DDL commits implicitly in MariaDB, so make the dimension's table before
	// starting the transaction that registers the model and stores its vectors.
	table := fmt.Sprintf("embedding_vectors_%d", dim)
	if _, err := d.sql.Exec(fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
		model VARCHAR(191) NOT NULL,
		text_hash CHAR(64) NOT NULL,
		`+"`vector`"+` MEDIUMBLOB NOT NULL,
		created_at DATETIME(6) NOT NULL,
		PRIMARY KEY (model, text_hash)
	) DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`, table)); err != nil {
		return err
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// The model row serializes first saves too: concurrent calls cannot
	// register different dimensions for the same model.
	if _, err := tx.Exec(`INSERT INTO embedding_models (model, dim) VALUES (?,?) ON DUPLICATE KEY UPDATE model=model`, model, dim); err != nil {
		return err
	}
	if err := tx.QueryRow(`SELECT dim FROM embedding_models WHERE model=?`, model).Scan(&storedDim); err != nil {
		return err
	}
	if storedDim != dim {
		return fmt.Errorf("embedding model %q: dimension %d does not match stored dimension %d", model, dim, storedDim)
	}
	now := time.Now().UTC()
	for i, h := range hashes {
		b := make([]byte, 4*len(vectors[i]))
		for j, f := range vectors[i] {
			binary.LittleEndian.PutUint32(b[4*j:], math.Float32bits(f))
		}
		// Keep prepared-statement bytes binary rather than converting them
		// through the connection's character set (tables made before 1.1 hold
		// them in a VECTOR column, which validates the length).
		if _, err := tx.Exec(`INSERT INTO `+table+" (model, text_hash, `vector`, created_at) VALUES (?,?,CAST(? AS BINARY),?) ON DUPLICATE KEY UPDATE text_hash=text_hash", model, h, b, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Embedding reads one stored vector.
func (d *DB) Embedding(model, hash string) ([]float32, bool, error) {
	dim, err := d.embeddingDimension(model)
	if err != nil || dim == 0 {
		return nil, false, err
	}
	var b []byte
	err = d.sql.QueryRow(fmt.Sprintf("SELECT `vector` FROM embedding_vectors_%d WHERE model=? AND text_hash=?", dim), model, hash).Scan(&b)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return v, true, nil
}

func (d *DB) embeddingDimension(model string) (int, error) {
	var dim int
	err := d.sql.QueryRow(`SELECT dim FROM embedding_models WHERE model=?`, model).Scan(&dim)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return dim, err
}

// SetDocHashes connects M<id> memories to their current document text before
// calling a vector provider. These derived links do not change memory content.
func (d *DB) SetDocHashes(hashes map[string]string) error {
	if len(hashes) == 0 {
		return nil
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for id, hash := range hashes {
		n, err := strconv.ParseInt(strings.TrimPrefix(id, "M"), 10, 64)
		if err != nil {
			return fmt.Errorf("memory document id %q: %w", id, err)
		}
		if _, err := tx.Exec(`UPDATE memories SET doc_hash=? WHERE id=? AND doc_hash<>?`, hash, n, hash); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// VectorsWaiting counts the memories indexing has given a text whose vector
// of the model is not stored yet (a failed or not yet run index; after a
// model change, all of them until they are indexed again).
func (d *DB) VectorsWaiting(model string) (int, error) {
	var n int
	dim, err := d.embeddingDimension(model)
	if err != nil {
		return 0, err
	}
	if dim == 0 {
		err = d.sql.QueryRow(`SELECT COUNT(*) FROM memories WHERE doc_hash<>''`).Scan(&n)
		return n, err
	}
	err = d.sql.QueryRow(fmt.Sprintf(`SELECT COUNT(*) FROM memories m LEFT JOIN embedding_vectors_%d e ON e.model=? AND e.text_hash=m.doc_hash WHERE m.doc_hash<>'' AND e.text_hash IS NULL`, dim), model).Scan(&n)
	return n, err
}
