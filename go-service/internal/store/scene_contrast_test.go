package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"archivecenter2/goservice/internal/memory"
)

// Only SQL is substituted. Selection, budgets, protection, contrast, basis
// regeneration and final rendering run through production DB.Deliver.
type contrastFixture struct {
	vectors      map[int64][]float32
	memories     []memory.Memory
	queries      []string
	missingTable bool
	vectorReads  int
	failRead     int // inject a contrast SQL failure on this read
	failRowRead  int // inject a row failure after one score on this read
	count, dim   int // benchmark vectors are generated row-by-row, not retained
}
type contrastConn struct{ f *contrastFixture }
type contrastConnector struct{ f *contrastFixture }

func (c contrastConnector) Connect(context.Context) (driver.Conn, error) {
	return contrastConn{c.f}, nil
}
func (c contrastConnector) Driver() driver.Driver { return contrastDriver{} }

type contrastDriver struct{}

func (contrastDriver) Open(string) (driver.Conn, error) {
	return nil, fmt.Errorf("use explicit fixture connector")
}
func (c contrastConn) Prepare(string) (driver.Stmt, error) {
	return nil, fmt.Errorf("unexpected Prepare")
}
func (c contrastConn) Close() error              { return nil }
func (c contrastConn) Begin() (driver.Tx, error) { return nil, fmt.Errorf("writes forbidden") }

type contrastRows struct {
	cols    []string
	rows    [][]driver.Value
	next    int
	ids     []int64
	fixture *contrastFixture
}

func (r *contrastRows) Columns() []string { return r.cols }
func (r *contrastRows) Close() error      { return nil }
func (r *contrastRows) Next(dest []driver.Value) error {
	if r.fixture != nil {
		if r.fixture.vectorReads == r.fixture.failRowRead && r.next == 1 {
			return fmt.Errorf("synthetic contrast row read failure")
		}
		if r.next == len(r.ids) {
			return io.EOF
		}
		id := r.ids[r.next]
		r.next++
		v := r.fixture.vectors[id]
		if r.fixture.count > 0 {
			v = make([]float32, r.fixture.dim)
			for i := range v {
				v[i] = float32(math.Sin(float64(id) * float64(i+1)))
			}
		}
		data := make([]byte, len(v)*4)
		for i, x := range v {
			binary.LittleEndian.PutUint32(data[4*i:], math.Float32bits(x))
		}
		dest[0], dest[1] = id, data
		return nil
	}
	if r.next == len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.next])
	r.next++
	return nil
}
func (c contrastConn) QueryContext(_ context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	f := c.f
	f.queries = append(f.queries, q)
	cols := strings.Split(strings.TrimSpace(strings.SplitN(strings.TrimPrefix(q, "SELECT "), " FROM ", 2)[0]), ",")
	r := &contrastRows{cols: cols}
	switch {
	case strings.HasPrefix(q, "SELECT m.id,e.vector"):
		f.vectorReads++
		if f.vectorReads == f.failRead {
			return nil, fmt.Errorf("synthetic contrast SQL read failure")
		}
		if f.missingTable {
			return nil, fmt.Errorf("table doesn't exist")
		}
		if len(args) < 2 || args[0].Value != "synthetic" {
			return nil, fmt.Errorf("unexpected vector args: %v", args)
		}
		r.fixture = f
		for _, a := range args[1:] {
			id, err := strconv.ParseInt(a.Value.(string), 10, 64)
			if err != nil {
				return nil, err
			}
			if _, ok := f.vectors[id]; ok || f.count > 0 {
				r.ids = append(r.ids, id)
			}
		}
	case strings.HasPrefix(q, "SELECT m.id, 1 - VEC_DISTANCE_COSINE"):
		r.cols = []string{"id", "similarity"}
		// Controlled external rank input isolates rerendering from ranking.
		for _, a := range args[2:] {
			id, _ := strconv.ParseInt(a.Value.(string), 10, 64)
			if _, ok := f.vectors[id]; ok {
				r.rows = append(r.rows, []driver.Value{id, float64(len(f.memories)) - float64(id)})
			}
		}
	case strings.HasPrefix(q, "SELECT job_id, source_key FROM gen_jobs WHERE state='done'"):
		if args[0].Value != "synthetic" {
			return nil, fmt.Errorf("unexpected source")
		}
		r.rows = [][]driver.Value{{int64(1), args[2].Value}}
	case strings.HasPrefix(q, "SELECT id, job_id, name, type, q FROM names"):
		r.rows = [][]driver.Value{{int64(1), int64(1), "대상", "person", ""}}
	case strings.HasPrefix(q, "SELECT id, job_id, detail, read_words FROM memories"):
		for i, m := range f.memories {
			b, _ := json.Marshal(m)
			r.rows = append(r.rows, []driver.Value{int64(i + 1), int64(1), string(b), nil})
		}
	case strings.HasPrefix(q, "SELECT g.job_id, g.turn_id, t.dates"):
		r.cols = []string{"job_id", "turn_id", "dates", "body_date", "elapsed_days", "state"}
		r.rows = [][]driver.Value{{int64(1), int64(1), nil, nil, nil, "done"}}
	case strings.HasPrefix(q, "SELECT job_id, target, kind, who,"):
		for i, m := range f.memories {
			for _, who := range m.HideFrom {
				r.rows = append(r.rows, []driver.Value{int64(1), fmt.Sprintf("M%d", i+1), "hide_from", who, "", "", "", "", "", ""})
			}
		}
	case strings.Contains(q, " FROM scene_marks "):
		if len(f.memories) > 0 && f.memories[0].Scene != "" {
			r.rows = [][]driver.Value{{int64(1), f.memories[0].Scene, "open", ""}}
		}
	case strings.Contains(q, " FROM name_relations "), strings.Contains(q, " FROM mentions "), strings.Contains(q, " FROM links "):
	case strings.HasPrefix(q, "SELECT id, hold FROM memories"):
		r.cols = []string{"id", "hold"} // C18 flags: none in these fixtures
	default:
		return nil, fmt.Errorf("unexpected SQL: %s", q)
	}
	return r, nil
}
func fixtureDB(t testing.TB, f *contrastFixture) *DB {
	t.Helper()
	conn := sql.OpenDB(contrastConnector{f})
	t.Cleanup(func() { conn.Close() })
	return &DB{sql: conn}
}
func contrastKnown(count int) memory.Known {
	var k memory.Known
	for i := 1; i <= count; i++ {
		k.Memories = append(k.Memories, memory.KnownMemory{ID: fmt.Sprintf("M%d", i)})
	}
	return k
}

func TestSceneContrastDirectionsAndAbsentVectors(t *testing.T) {
	f := &contrastFixture{vectors: map[int64][]float32{1: {1, 0}, 2: {0, 1}}}
	db := fixtureDB(t, f)
	req := DeliveryRequest{Model: "synthetic", Query: []float32{1, 0}}
	scores, err := db.sceneContrast(contrastKnown(3), req)
	if err != nil {
		t.Fatal(err)
	}
	if scores["M1"] <= 0 || scores["M2"] >= 0 {
		t.Fatalf("directions: %v", scores)
	}
	if _, ok := scores["M3"]; ok {
		t.Fatal("missing vector was scored")
	}
	for _, tc := range []struct {
		name    string
		query   []float32
		vectors map[int64][]float32
		missing bool
	}{
		{"no_query", nil, f.vectors, false}, {"zero_query", []float32{0, 0}, f.vectors, false},
		{"no_vectors", req.Query, nil, false}, {"one_vector", req.Query, map[int64][]float32{1: {1, 0}}, false},
		{"no_table", req.Query, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &contrastFixture{vectors: tc.vectors, missingTable: tc.missing}
			got, err := fixtureDB(t, f).sceneContrast(contrastKnown(3), DeliveryRequest{Model: req.Model, Query: tc.query})
			if err != nil || len(got) != 0 {
				t.Fatalf("want baseline passthrough: %v %v", got, err)
			}
		})
	}
}

func syntheticScope() []memory.ScopedTurn {
	return []memory.ScopedTurn{{Confirmed: true, Source: memory.Source{Session: "synthetic", Turn: 1, Members: []memory.SourceMember{{Data: "old"}}, Responses: []memory.SourceResponse{{Data: "answer"}}}}}
}
func syntheticRequest() DeliveryRequest {
	return DeliveryRequest{Input: "대상", Model: "synthetic", Query: []float32{1, 0}, Payload: []TextPrint{{Role: "user"}}, Chars: 2000, Secret: 4000}
}

func TestContrastNoVectorDeliveryIsBaseline(t *testing.T) {
	mems := []memory.Memory{{Kind: "fact", About: "N1", Mode: "상태", Body: "남겨야 할 첫 기록"}, {Kind: "fact", About: "N1", Mode: "성향", Body: "남겨야 할 둘째 기록"}}
	for _, vectors := range []map[int64][]float32{nil, {1: {1, 0}}} {
		f := &contrastFixture{memories: mems, vectors: vectors}
		db := fixtureDB(t, f)
		req := syntheticRequest()
		got, err := db.Deliver(syntheticScope(), req)
		if err != nil {
			t.Fatal(err)
		}
		req.Query = nil
		want, err := db.Deliver(syntheticScope(), req)
		if err != nil {
			t.Fatal(err)
		}
		// Ranking metadata may describe a single stored vector; content may not change.
		if got.Block != want.Block || !reflect.DeepEqual(got.IDs, want.IDs) || got.Chars != want.Chars {
			t.Fatalf("not baseline: %+v / %+v", got, want)
		}
	}
}

func TestContrastErrorDeliveryIsBaseline(t *testing.T) {
	mems := []memory.Memory{
		{Kind: "fact", About: "N1", Mode: "상태", Body: "첫 기록", Awareness: memory.Awareness{HideFrom: memory.Who{"N1"}}},
		{Kind: "fact", About: "N1", Mode: "성향", Body: "둘째 기록"},
	}
	for _, tc := range []struct {
		name        string
		failRead    int
		failRowRead int
		vectors     map[int64][]float32
	}{
		{"first_pass_sql", 1, 0, map[int64][]float32{1: {0, 1}, 2: {1, 0}}},
		{"second_pass_sql", 2, 0, map[int64][]float32{1: {0, 1}, 2: {1, 0}}},
		{"second_pass_partial_scores", 0, 2, map[int64][]float32{1: {0, 1}, 2: {1, 0}}},
		{"dimension_mismatch", 0, 0, map[int64][]float32{1: {1}, 2: {1, 0}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &contrastFixture{memories: mems, vectors: tc.vectors, missingTable: true}
			db := fixtureDB(t, f)
			req := syntheticRequest()
			// Only the optional contrast read is absent. Baseline ranking,
			// protection, both ceilings and rendering still run in Deliver.
			want, err := db.Deliver(syntheticScope(), req)
			if err != nil || len(want.IDs) != len(mems) || want.Block == "" || want.SecretChars == 0 {
				t.Fatalf("incomplete baseline: %+v %v", want, err)
			}
			f.missingTable, f.vectorReads = false, 0
			f.failRead, f.failRowRead = tc.failRead, tc.failRowRead
			got, err := db.Deliver(syntheticScope(), req)
			if err != nil {
				t.Fatalf("contrast error aborted delivery: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("contrast error changed baseline: %+v / %+v", got, want)
			}
			if f.vectorReads == 0 || (tc.failRead > 0 && f.vectorReads != tc.failRead) || (tc.failRowRead > 0 && f.vectorReads != tc.failRowRead) {
				t.Fatalf("contrast error was not exercised: reads=%d", f.vectorReads)
			}
		})
	}
}

func TestContrastOversizedRetainedRowContinues(t *testing.T) {
	// A secret view defines a long public basis. Once that view is removed,
	// another view must quote the basis from its smaller public lane.
	mems := []memory.Memory{
		{Kind: "view", Owner: "N1", Body: "첫 생각", Basis: &memory.Basis{Ref: "M4"}, Awareness: memory.Awareness{HideFrom: memory.Who{"N1"}}},
		{Kind: "view", Owner: "N1", Body: "둘째 생각", Basis: &memory.Basis{Ref: "M4"}},
		{Kind: "fact", About: "N1", Mode: "상태", Body: "끝까지 전달할 기록"},
		{Kind: "event", Body: strings.Repeat("긴 근거 ", 100)},
	}
	for i := range mems {
		mems[i].Scene = "S1"
	}
	f := &contrastFixture{memories: mems, vectors: map[int64][]float32{1: {0, 1}, 2: {1, 0}, 3: {1, 0}, 4: {0, 1}}}
	db := fixtureDB(t, f)
	req := syntheticRequest()
	// Derive the lane ceiling from the exact production rendering of the
	// shared-reference view and final fact, not a fitted budget constant.
	view := memory.KnownMemory{ID: "M2", Turn: 1, Memory: mems[1]}
	fact := memory.KnownMemory{ID: "M3", Turn: 1, Memory: mems[2]}
	charge := func(m memory.KnownMemory, basis string) int {
		return recordSize(memory.RenderKnown(memory.Selection{Memories: []memory.KnownMemory{m}}, nil), deliveryLine(m, map[string]string{"N1": "대상"}, basis, nil))
	}
	req.Chars = charge(view, "based on B1") + charge(fact, "")
	baselineReq := req
	baselineReq.Query = nil
	baseline, err := db.Deliver(syntheticScope(), baselineReq)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"M1", "M2", "M3"} {
		if !has(baseline.IDs, id) {
			t.Fatalf("baseline did not exercise rerender: %+v", baseline)
		}
	}
	got, err := db.Deliver(syntheticScope(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !has(got.IDs, "M3") || has(got.IDs, "M1") || has(got.IDs, "M2") {
		t.Fatalf("skip only expanded row: %+v", got)
	}
	if strings.Contains(got.Block, "B1") || got.Chars > req.Chars || got.SecretChars > req.Secret {
		t.Fatalf("broken marks or budgets: %+v", got)
	}
	found := false
	for _, tr := range got.Trace {
		if tr.ID == "M2" && tr.Result != "over_ceiling" {
			t.Fatalf("retained failure not observed: %+v", tr)
		}
		if tr.ID == "M1" && tr.Result == "contrast_removed" {
			found = true
		}
	}
	if !found {
		t.Fatal("basis definition was not removed by contrast")
	}
}

func BenchmarkSceneContrast(b *testing.B) {
	for _, count := range []int{300, 3000} {
		b.Run(strconv.Itoa(count), func(b *testing.B) {
			f := &contrastFixture{count: count, dim: 1024}
			db := fixtureDB(b, f)
			k := contrastKnown(count)
			q := make([]float32, f.dim)
			q[0] = 1
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				f.queries = nil
				if _, err := db.sceneContrast(k, DeliveryRequest{Model: "synthetic", Query: q}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
