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
	mean := make([]float64, dim)
	ids := make([]string, len(k.Memories))
	for i, m := range k.Memories {
		ids[i] = m.ID
	}
	visit := func(use func(string, []float64)) error { return d.unitVectors(req.Model, dim, ids, use) }
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

// unitVectors streams the stored vectors of ids as unit vectors, in pages
// of 500. A missing vector table has the same meaning as in similarities
// (no stored vectors of this dimension yet); a vector of another size, zero
// or not finite is skipped. Two streamed passes avoid retaining a
// dimension-sized vector per memory.
func (d *DB) unitVectors(model string, dim int, ids []string, use func(string, []float64)) error {
	vector := make([]float64, dim)
	for start := 0; start < len(ids); start += 500 {
		part := ids[start:min(start+500, len(ids))]
		args := []any{model}
		for _, id := range part {
			args = append(args, strings.TrimPrefix(id, "M"))
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
				continue
			}
			var length float64
			for i := range vector {
				vector[i] = float64(math.Float32frombits(binary.LittleEndian.Uint32(data[4*i:])))
				length += vector[i] * vector[i]
			}
			if length == 0 || math.IsNaN(length) || math.IsInf(length, 0) {
				continue
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

// contrastAgainst scores ids as sceneContrast does, against the center of a
// population whose unit vectors sum to sum (count of them), so a population
// already read once (1.2: the general order's records, read with their
// similarity) is not read twice more.
func (d *DB) contrastAgainst(req DeliveryRequest, sum []float64, count int, ids []string) (map[string]float64, error) {
	out := map[string]float64{}
	dim := len(req.Query)
	var norm float64
	for _, v := range req.Query {
		norm += float64(v) * float64(v)
	}
	if norm == 0 || count <= 1 || len(sum) != dim {
		return out, nil
	}
	norm = math.Sqrt(norm)
	mean, q := make([]float64, dim), make([]float64, dim)
	for i := range q {
		mean[i] = sum[i] / float64(count)
		q[i] = float64(req.Query[i])/norm - mean[i]
	}
	err := d.unitVectors(req.Model, dim, ids, func(id string, v []float64) {
		var score float64
		for i := range v {
			score += (v[i] - mean[i]) * q[i]
		}
		out[id] = score
	})
	return out, err
}

// similaritiesWithSum is similarities and, in the same read, the sum and
// count of the unit vectors (as sceneContrast reads them) of the records
// that have one. Both are computed from the stored values in the same order
// as those two, so the numbers are theirs.
func (d *DB) similaritiesWithSum(model string, query []float32, ids []string) (map[string]float64, []float64, int, error) {
	dim := len(query)
	sim, sum, count := map[string]float64{}, make([]float64, dim), 0
	q := make([]float64, dim)
	var qnorm float64
	for i, v := range query {
		q[i] = float64(v)
		if math.IsNaN(q[i]) || math.IsInf(q[i], 0) {
			return nil, nil, 0, fmt.Errorf("query vector is not finite")
		}
		qnorm += q[i] * q[i]
	}
	if qnorm == 0 {
		return sim, sum, 0, nil
	}
	qnorm = math.Sqrt(qnorm)
	raw := make([]float64, dim)
	for start := 0; start < len(ids); start += 500 {
		part := ids[start:min(start+500, len(ids))]
		args := []any{model}
		for _, id := range part {
			args = append(args, strings.TrimPrefix(id, "M"))
		}
		// the statement similarities reads with: this is its read
		stmt := fmt.Sprintf("SELECT m.id, e.`vector` FROM memories m JOIN embedding_vectors_%d e ON e.model=? AND e.text_hash=m.doc_hash WHERE m.id IN (", dim) +
			strings.TrimSuffix(strings.Repeat("?,", len(part)), ",") + ")"
		rows, err := d.sql.Query(stmt, args...)
		if err != nil {
			if strings.Contains(err.Error(), "doesn't exist") {
				return sim, sum, count, nil
			}
			return nil, nil, 0, err
		}
		for rows.Next() {
			var id int64
			var data sql.RawBytes
			if err := rows.Scan(&id, &data); err != nil {
				rows.Close()
				return nil, nil, 0, err
			}
			if len(data) != dim*4 {
				continue
			}
			var dot, norm float64
			for i := 0; i < dim; i++ {
				raw[i] = float64(math.Float32frombits(binary.LittleEndian.Uint32(data[4*i:])))
				dot += raw[i] * q[i]
				norm += raw[i] * raw[i]
			}
			key := "M" + strconv.FormatInt(id, 10)
			if s := dot / (math.Sqrt(norm) * qnorm); norm != 0 && !math.IsNaN(s) && !math.IsInf(s, 0) {
				sim[key] = s
			}
			if norm == 0 || math.IsNaN(norm) || math.IsInf(norm, 0) {
				continue
			}
			length := math.Sqrt(norm)
			for i := range sum {
				sum[i] += raw[i] / length
			}
			count++
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, nil, 0, err
		}
	}
	return sim, sum, count, nil
}
