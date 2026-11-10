// Package embed indexes stored memories as vectors. MariaDB keeps the
// memory; a vector is a derived copy: an indexing failure loses no memory,
// and indexing again never makes memory again.
package embed

import (
	"context"
	"strings"

	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/provider"
	"archivecenter2/goservice/internal/store"
)

// Embedder is the vector provider. A contextualized model
// (provider.Contextual) embeds each turn's memories as one group, with the
// turn's input and response as their context.
type Embedder interface {
	Embed(ctx context.Context, texts []string, inputType string) (provider.Embedding, error)
	EmbedGroups(ctx context.Context, groups [][]string, inputType string) (provider.GroupEmbedding, error)
}

// Store is the part of store.DB indexing uses.
type Store interface {
	KnownOf(jobs []int64) (memory.Known, error)
	NamesByID(ids []string) (map[string]string, error)
	SetDocHashes(hashes map[string]string) error
	JobSources(jobs []int64) (map[int64]memory.Source, error)
	HaveEmbeddings(model string, hashes []string) (map[string]bool, error)
	SaveEmbeddings(model string, hashes []string, vectors [][]float32) error
}

// Report counts one indexing run.
type Report struct {
	Memories int // memories of the jobs with a text to embed
	Reused   int // memories whose text already had a vector
	Embedded int // distinct texts sent
	Requests int
	Tokens   int
}

// Index embeds the memories of the given jobs whose text has no vector of
// the model yet, batch texts per request (a contextualized model: a turn per
// request, indexGroups). On a provider failure it returns
// what it stored so far; the rest is found missing on a later run.
func Index(ctx context.Context, st Store, e Embedder, model string, jobs []int64, batch int) (Report, error) {
	var r Report
	k, names, err := prepareKnown(st, jobs)
	if err != nil {
		return r, err
	}
	if provider.Contextual(model) {
		return indexGroups(ctx, st, e, model, k, names, jobs, r)
	}
	docHashes, hashes, text := prepareFlat(k, names)
	r.Memories = len(hashes)
	if err := st.SetDocHashes(docHashes); err != nil {
		return r, err
	}
	have, err := st.HaveEmbeddings(model, hashes)
	if err != nil {
		return r, err
	}
	var todo []string
	seen := map[string]bool{}
	for _, h := range hashes {
		switch {
		case have[h]:
			r.Reused++
		case !seen[h]:
			seen[h] = true
			todo = append(todo, h)
		}
	}
	for start := 0; start < len(todo); start += batch {
		part := todo[start:min(start+batch, len(todo))]
		texts := make([]string, len(part))
		for i, h := range part {
			texts[i] = text[h]
		}
		r.Requests++
		res, err := e.Embed(ctx, texts, "document")
		if err != nil {
			return r, err
		}
		r.Tokens += res.Tokens
		if err := st.SaveEmbeddings(model, part, res.Vectors); err != nil {
			return r, err
		}
		r.Embedded += len(part)
	}
	return r, nil
}

// indexGroups embeds, for a contextualized model, each turn with a memory
// that has no vector yet: the turn's input and response, then its memories,
// as one group in one request (as AC1 does). A memory's vector depends on
// the whole group, so its key is the group's content and its own text: a
// changed memory of the turn gives every memory of it a new key.
func indexGroups(ctx context.Context, st Store, e Embedder, model string, k memory.Known, names map[string]string, jobs []int64, r Report) (Report, error) {
	order, groups, docHashes, hashes, err := prepareGroups(st, k, names, jobs)
	if err != nil {
		return r, err
	}
	r.Memories = len(hashes)
	if err := st.SetDocHashes(docHashes); err != nil {
		return r, err
	}
	have, err := st.HaveEmbeddings(model, hashes)
	if err != nil {
		return r, err
	}
	for _, j := range order {
		g := groups[j]
		send := false
		for _, h := range g.keys {
			if have[h] {
				r.Reused++
			} else {
				send = true
			}
		}
		if !send {
			continue
		}
		r.Requests++
		res, err := e.EmbedGroups(ctx, [][]string{g.chunks}, "document")
		if err != nil {
			return r, err
		}
		r.Tokens += res.Tokens
		var keys []string
		var vectors [][]float32
		for i, h := range g.keys {
			if have[h] {
				continue
			}
			have[h] = true
			keys = append(keys, h)
			vectors = append(vectors, res.Vectors[0][g.context+i])
		}
		if err := st.SaveEmbeddings(model, keys, vectors); err != nil {
			return r, err
		}
		r.Embedded += len(keys)
	}
	return r, nil
}

// Documents uses the indexer's exact document keys without writes or provider
// calls, for maintenance target counts and invalidating the selected vectors.
func Documents(st Store, model string, jobs []int64) (map[string]string, error) {
	k, names, err := prepareKnown(st, jobs)
	if err != nil {
		return nil, err
	}
	if provider.Contextual(model) {
		_, _, docs, _, err := prepareGroups(st, k, names, jobs)
		return docs, err
	}
	docs, _, _ := prepareFlat(k, names)
	return docs, nil
}

func prepareKnown(st Store, jobs []int64) (memory.Known, map[string]string, error) {
	k, err := st.KnownOf(jobs)
	if err != nil {
		return memory.Known{}, nil, err
	}
	// the names the memories use, whatever turn wrote them, so a memory
	// gives the same text however many jobs are indexed together
	var refs []string
	for _, m := range k.Memories {
		refs = append(refs, memory.NameRefs(m.Memory)...)
	}
	names, err := st.NamesByID(refs)
	if err != nil {
		return memory.Known{}, nil, err
	}
	return k, names, nil
}
func prepareFlat(k memory.Known, names map[string]string) (map[string]string, []string, map[string]string) {
	var hashes []string
	text := map[string]string{}
	docHashes := map[string]string{}
	for _, m := range k.Memories {
		t := memory.DocText(m, names)
		if strings.TrimSpace(t) == "" {
			continue // nothing to embed (a record with no content of its own)
		}
		h := store.TextHash(t)
		docHashes[m.ID] = h
		hashes = append(hashes, h)
		text[h] = t
	}
	return docHashes, hashes, text
}

type documentGroup struct {
	chunks  []string
	context int      // the chunks before the memories
	ids     []string // the memory of each chunk after them
	keys    []string // each memory's key
}

func prepareGroups(st Store, k memory.Known, names map[string]string, jobs []int64) ([]int64, map[int64]*documentGroup, map[string]string, []string, error) {
	sources, err := st.JobSources(jobs)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	var order []int64
	groups := map[int64]*documentGroup{}
	for _, m := range k.Memories {
		t := memory.DocText(m, names)
		if strings.TrimSpace(t) == "" {
			continue // nothing to embed (a record with no content of its own)
		}
		g := groups[m.Job]
		if g == nil {
			g = &documentGroup{}
			var in, out []string
			for _, x := range sources[m.Job].Members {
				in = append(in, x.Data)
			}
			for _, x := range sources[m.Job].Responses {
				out = append(out, x.Data)
			}
			for _, c := range []string{strings.Join(in, "\n"), strings.Join(out, "\n")} {
				if strings.TrimSpace(c) != "" {
					g.chunks = append(g.chunks, c)
				}
			}
			g.context = len(g.chunks)
			groups[m.Job] = g
			order = append(order, m.Job)
		}
		g.chunks = append(g.chunks, t)
		g.ids = append(g.ids, m.ID)
	}
	for _, j := range order {
		g := groups[j]
		whole := store.TextHash(strings.Join(g.chunks, "\x00"))
		for _, c := range g.chunks[g.context:] {
			g.keys = append(g.keys, store.TextHash(whole+"\x00"+c))
		}
	}
	docHashes := map[string]string{}
	var hashes []string
	for _, j := range order {
		g := groups[j]
		for i, id := range g.ids {
			docHashes[id] = g.keys[i]
			hashes = append(hashes, g.keys[i])
		}
	}
	return order, groups, docHashes, hashes, nil
}
