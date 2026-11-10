package embed

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"reflect"
	"regexp"
	"testing"

	"archivecenter2/goservice/internal/memory"
	"archivecenter2/goservice/internal/provider"
	"archivecenter2/goservice/internal/store"
)

type docHashFailingProvider struct {
	t       *testing.T
	db      *sql.DB
	want    map[string]string
	failure error
	calls   int
}

func (p *docHashFailingProvider) Embed(_ context.Context, texts []string, inputType string) (provider.Embedding, error) {
	p.calls++
	if inputType != "document" || len(texts) != 2 {
		p.t.Fatalf("unexpected embedding request: %q %v", inputType, texts)
	}
	// Read via a separate SQL connection while the provider is being called:
	// links must already be committed, including later batches and reused text.
	p.check()
	return provider.Embedding{}, p.failure
}

func (p *docHashFailingProvider) EmbedGroups(_ context.Context, groups [][]string, inputType string) (provider.GroupEmbedding, error) {
	p.t.Fatalf("unexpected contextualized request: %q %v", inputType, groups)
	return provider.GroupEmbedding{}, nil
}

func (p *docHashFailingProvider) check() {
	p.t.Helper()
	rows, err := p.db.Query(`SELECT CONCAT('M',id), doc_hash FROM memories ORDER BY id`)
	if err != nil {
		p.t.Fatal(err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var id, hash string
		if err := rows.Scan(&id, &hash); err != nil {
			p.t.Fatal(err)
		}
		want, ok := p.want[id]
		if !ok || hash != want {
			p.t.Fatalf("document hash for %s: got %q, want %q", id, hash, want)
		}
		count++
	}
	if err := rows.Err(); err != nil || count != len(p.want) {
		p.t.Fatalf("document rows %d/%d: %v", count, len(p.want), err)
	}
}

func TestIndexDocHashesSurviveProviderFailure(t *testing.T) {
	db := testDB(t)
	job := saveTurn(t, db, `{"entities":[{"id":"n1","name":"한얼"}],"memories":[{"id":"m1","kind":"fact","about":"n1","body":"문을 잠갔다"},{"id":"m2","kind":"event","body":"주막에 갔다"},{"id":"m3","kind":"event","body":"주막에 갔다"}]}`)
	k, err := db.KnownOf([]int64{job})
	if err != nil {
		t.Fatal(err)
	}
	names, err := db.NamesByID([]string{k.Names[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{}
	for _, m := range k.Memories {
		want[m.ID] = store.TextHash(memory.DocText(m, names))
	}
	name := "ac2_test_embed_" + regexp.MustCompile(`\W`).ReplaceAllString(t.Name(), "_")
	inspect, err := sql.Open("mysql", os.Getenv("AC2_TEST_MARIADB_DSN")+name)
	if err != nil {
		t.Fatal(err)
	}
	defer inspect.Close()
	failure := errors.New("provider offline")
	p := &docHashFailingProvider{t: t, db: inspect, want: want, failure: failure}
	r, err := Index(context.Background(), db, p, "m", []int64{job}, 8)
	if !errors.Is(err, failure) || r.Memories != len(want) || r.Embedded != 0 || p.calls != 1 {
		t.Fatalf("failed run: %+v, %v, provider calls %d", r, err, p.calls)
	}
	p.check()
	after, err := db.KnownOf([]int64{job})
	if err != nil || !reflect.DeepEqual(after, k) {
		t.Fatalf("provider failure changed canonical memory: %v", err)
	}
}
