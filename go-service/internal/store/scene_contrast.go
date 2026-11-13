package store

import (
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"strings"

	"archivecenter2/goservice/internal/memory"
)

// sceneContrast centers unit query/document vectors on the pre-request corpus.
// Two streamed passes avoid retaining a dimension-sized vector per memory.
// Missing vectors have no score and keep the baseline delivery decision.
func (d *DB) sceneContrast(k memory.Known, req DeliveryRequest) (map[string]float64, error) {
	out := map[string]float64{}
	dim := len(req.Query)
	var norm float64
	for _, v := range req.Query {
		norm += float64(v) * float64(v)
	}
	if norm == 0 || len(k.Memories) <= 1 {
		return out, nil
	}
	norm = math.Sqrt(norm)
	mean, vector := make([]float64, dim), make([]float64, dim)
	// A missing vector table has the same meaning as in similarities: there
	// are no stored vectors of this dimension yet.
	visit := func(use func(string, []float64)) error {
		for start := 0; start < len(k.Memories); start += 500 {
			part := k.Memories[start:min(start+500, len(k.Memories))]
			args := []any{req.Model}
			for _, m := range part {
				args = append(args, strings.TrimPrefix(m.ID, "M"))
			}
			stmt := fmt.Sprintf("SELECT m.id,e.vector FROM memories m JOIN embedding_vectors_%d e ON e.model=? AND e.text_hash=m.doc_hash WHERE m.id IN (", dim) + strings.TrimSuffix(strings.Repeat("?,", len(part)), ",") + ")"
			rows, err := d.sql.Query(stmt, args...)
			if err != nil {
				if strings.Contains(err.Error(), "doesn't exist") {
					return nil
				}
				return err
			}
			for rows.Next() {
				var id int64
				var data sql.RawBytes
				if err := rows.Scan(&id, &data); err != nil {
					rows.Close()
					return err
				}
				if len(data) != dim*4 {
					continue // a stored vector of another size is not compared (as in similarities)
				}
				var length float64
				for i := range vector {
					vector[i] = float64(math.Float32frombits(binary.LittleEndian.Uint32(data[4*i:])))
					length += vector[i] * vector[i]
				}
				if length == 0 || math.IsNaN(length) || math.IsInf(length, 0) {
					continue // nor a zero or non-finite one: the others keep their contrast
				}
				length = math.Sqrt(length)
				for i := range vector {
					vector[i] /= length
				}
				use("M"+strconv.FormatInt(id, 10), vector)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
		}
		return nil
	}
	count := 0
	if err := visit(func(_ string, v []float64) {
		count++
		for i := range mean {
			mean[i] += v[i]
		}
	}); err != nil {
		return nil, err
	}
	if count <= 1 {
		return out, nil
	}
	q := make([]float64, dim)
	for i := range q {
		mean[i] /= float64(count)
		q[i] = float64(req.Query[i])/norm - mean[i]
	}
	err := visit(func(id string, v []float64) {
		var score float64
		for i := range v {
			score += (v[i] - mean[i]) * q[i]
		}
		out[id] = score
	})
	return out, err
}
