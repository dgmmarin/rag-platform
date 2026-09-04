package retrieve

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// fakeRows implements rowScanner over a fixed set of Results, so scanResults can be
// tested without a database. Scan assigns into the 9 destinations hybridSQL projects.
type fakeRows struct {
	rows    []Result
	metas   []json.RawMessage // one per row; the jsonb column as raw bytes
	i       int
	scanErr error
	err     error
}

func (f *fakeRows) Next() bool { f.i++; return f.i <= len(f.rows) }
func (f *fakeRows) Err() error { return f.err }
func (f *fakeRows) Scan(dest ...any) error {
	if f.scanErr != nil {
		return f.scanErr
	}
	r := f.rows[f.i-1]
	*dest[0].(*string) = r.ChunkID
	*dest[1].(*string) = r.DocumentID
	*dest[2].(*string) = r.SourceID
	*dest[3].(*string) = r.Content
	*dest[4].(*string) = r.URI
	*dest[5].(*string) = r.Title
	*dest[6].(*[]string) = r.HeadingPath
	*dest[7].(*[]byte) = []byte(f.metas[f.i-1])
	*dest[8].(*float64) = r.Score
	return nil
}

func TestScanResultsMapsColumnsAndMetadata(t *testing.T) {
	want := []Result{
		{ChunkID: "c1", DocumentID: "d1", SourceID: "s1", Content: "hello",
			URI: "https://x/1", Title: "One", HeadingPath: []string{"A", "B"}, Score: 0.5},
		{ChunkID: "c2", DocumentID: "d2", SourceID: "s2", Content: "world",
			URI: "", Title: "", HeadingPath: nil, Score: 0.25},
	}
	got, err := scanResults(&fakeRows{rows: want, metas: []json.RawMessage{json.RawMessage(`{"team":"x"}`), nil}})
	if err != nil {
		t.Fatalf("scanResults: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2", len(got))
	}
	if got[0].ChunkID != "c1" || got[0].Title != "One" || got[0].Score != 0.5 || string(got[0].Metadata) != `{"team":"x"}` {
		t.Fatalf("row 0 mismapped: %+v", got[0])
	}
	// An empty jsonb column must leave Metadata nil, not an empty RawMessage.
	if got[1].Metadata != nil {
		t.Fatalf("row 1 Metadata = %q, want nil", got[1].Metadata)
	}
}

func TestScanResultsPropagatesErrors(t *testing.T) {
	scanBoom := errors.New("scan boom")
	if _, err := scanResults(&fakeRows{rows: []Result{{}}, metas: []json.RawMessage{nil}, scanErr: scanBoom}); !errors.Is(err, scanBoom) {
		t.Fatalf("scan error not propagated: %v", err)
	}
	rowsBoom := errors.New("rows boom")
	if _, err := scanResults(&fakeRows{err: rowsBoom}); !errors.Is(err, rowsBoom) {
		t.Fatalf("rows error not propagated: %v", err)
	}
}

// fakeTx is a pgx.Tx whose only real methods are the ones withEFSearchTx uses
// (Exec, Rollback); the rest are inherited from the embedded nil interface and
// would panic if the tested path ever called them (it does not).
type fakeTx struct {
	pgx.Tx
	execErr    error
	execedSQL  []string
	rolledBack bool
}

func (f *fakeTx) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	f.execedSQL = append(f.execedSQL, sql)
	return pgconn.CommandTag{}, f.execErr
}
func (f *fakeTx) Rollback(context.Context) error { f.rolledBack = true; return nil }

type fakeBeginner struct {
	tx       pgx.Tx
	beginErr error
}

func (b fakeBeginner) BeginRead(context.Context) (pgx.Tx, error) { return b.tx, b.beginErr }

func TestWithEFSearchTxTunesGUCsThenRunsFnAndRollsBack(t *testing.T) {
	tx := &fakeTx{}
	ran := false
	err := withEFSearchTx(context.Background(), fakeBeginner{tx: tx}, 40, func(_ context.Context, got pgx.Tx) error {
		ran = true
		if got != tx {
			t.Fatal("fn received a different tx")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("withEFSearchTx: %v", err)
	}
	if !ran {
		t.Fatal("fn was not run")
	}
	if !tx.rolledBack {
		t.Fatal("read-only tx was not rolled back")
	}
	// The GUC-tuning statement must set all three knobs.
	if len(tx.execedSQL) != 1 {
		t.Fatalf("expected 1 tuning exec, got %d: %v", len(tx.execedSQL), tx.execedSQL)
	}
	for _, guc := range []string{"hnsw.ef_search", "hnsw.iterative_scan", "plan_cache_mode"} {
		if !strings.Contains(tx.execedSQL[0], guc) {
			t.Fatalf("tuning statement missing %s: %q", guc, tx.execedSQL[0])
		}
	}
}

func TestWithEFSearchTxSurfacesBeginAndTuneErrors(t *testing.T) {
	beginBoom := errors.New("begin boom")
	if err := withEFSearchTx(context.Background(), fakeBeginner{beginErr: beginBoom}, 40,
		func(context.Context, pgx.Tx) error { return nil }); !errors.Is(err, beginBoom) {
		t.Fatalf("begin error not surfaced: %v", err)
	}
	tuneBoom := errors.New("tune boom")
	tx := &fakeTx{execErr: tuneBoom}
	called := false
	err := withEFSearchTx(context.Background(), fakeBeginner{tx: tx}, 40,
		func(context.Context, pgx.Tx) error { called = true; return nil })
	if !errors.Is(err, tuneBoom) {
		t.Fatalf("tune error not surfaced: %v", err)
	}
	if called {
		t.Fatal("fn ran despite a GUC-tuning failure")
	}
	if !tx.rolledBack {
		t.Fatal("tx not rolled back after tune failure")
	}
}

// planRows is a single-column rowScanner (the shape EXPLAIN returns) for readPlan.
type planRows struct {
	lines []string
	i     int
	err   error
}

func (p *planRows) Next() bool { p.i++; return p.i <= len(p.lines) }
func (p *planRows) Err() error { return p.err }
func (p *planRows) Scan(dest ...any) error {
	*dest[0].(*string) = p.lines[p.i-1]
	return nil
}

func TestReadPlanJoinsLines(t *testing.T) {
	got, err := readPlan(&planRows{lines: []string{"Limit", "  Sort", "    Index Scan using chunks_embedding_idx"}})
	if err != nil {
		t.Fatalf("readPlan: %v", err)
	}
	want := "Limit\n  Sort\n    Index Scan using chunks_embedding_idx\n"
	if got != want {
		t.Fatalf("readPlan = %q, want %q", got, want)
	}
	boom := errors.New("plan boom")
	if _, err := readPlan(&planRows{err: boom}); !errors.Is(err, boom) {
		t.Fatalf("readPlan error not propagated: %v", err)
	}
}

// The embedding guard must fire before any DB access, so a nil *tenant.DB is safe.
func TestEmptyEmbeddingRejectedBeforeDB(t *testing.T) {
	ctx := context.Background()
	for name, call := range map[string]func() error{
		"Retrieve":       func() error { _, e := Retrieve(ctx, nil, Params{QueryText: "q"}); return e },
		"Explain":        func() error { _, e := Explain(ctx, nil, Params{QueryText: "q"}); return e },
		"ExplainIndexed": func() error { _, e := ExplainIndexed(ctx, nil, Params{QueryText: "q"}); return e },
	} {
		if err := call(); !errors.Is(err, ErrNoEmbedding) {
			t.Fatalf("%s with empty embedding: want ErrNoEmbedding, got %v", name, err)
		}
	}
}

// RRFScore is the canonical Go statement of the reciprocal-rank-fusion formula
// the SPEC-06 §2 SQL computes (sum(1.0/(60+r))). It is the oracle the e2e test
// compares the SQL's score against, so both agree by construction.
func TestRRFScoreMatchesSpecFormula(t *testing.T) {
	const k = 60.0
	cases := []struct {
		name  string
		ranks []int
		want  float64
	}{
		{"top of one list", []int{1}, 1.0 / (k + 1)},
		{"top of both lists", []int{1, 1}, 1.0/(k+1) + 1.0/(k+1)},
		{"mixed ranks", []int{1, 5}, 1.0/(k+1) + 1.0/(k+5)},
		{"no lists", nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RRFScore(tc.ranks...)
			if math.Abs(got-tc.want) > 1e-12 {
				t.Fatalf("RRFScore(%v) = %v, want %v", tc.ranks, got, tc.want)
			}
		})
	}
}

// A higher-ranked chunk (appearing near the top of both lists) must fuse to a
// strictly higher score than one appearing only deep in one list.
func TestRRFScoreRanksHybridWinnerHighest(t *testing.T) {
	both := RRFScore(1, 2)
	vectorOnlyDeep := RRFScore(40)
	if both <= vectorOnlyDeep {
		t.Fatalf("hybrid winner %v should beat single-list deep hit %v", both, vectorOnlyDeep)
	}
}

func TestEFSearchIsMaxOf40AndKVector(t *testing.T) {
	cases := []struct{ kVector, want int }{
		{0, 40}, {10, 40}, {40, 40}, {41, 41}, {200, 200}, {-5, 40},
	}
	for _, tc := range cases {
		if got := efSearch(tc.kVector); got != tc.want {
			t.Fatalf("efSearch(%d) = %d, want %d", tc.kVector, got, tc.want)
		}
	}
}

// likePrefixPattern must neutralise LIKE metacharacters in the user-supplied
// prefix so a client can never inject a wildcard, then append a trailing % so the
// pattern is an anchored prefix match. The escape character is backslash (the SQL
// uses `like $n escape '\'`).
func TestLikePrefixPatternEscapesMetacharacters(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://docs.acme.com/", `https://docs.acme.com/%`},
		{"a%b", `a\%b%`},
		{"a_b", `a\_b%`},
		{`a\b`, `a\\b%`},
		{"100%_x", `100\%\_x%`},
		{"", `%`},
	}
	for _, tc := range cases {
		if got := likePrefixPattern(tc.in); got != tc.want {
			t.Fatalf("likePrefixPattern(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestWithDefaultsAppliesADR0007Values(t *testing.T) {
	got := Params{Embedding: []float32{0.1}}.withDefaults()
	if got.KVector != 40 || got.KText != 40 || got.K != 8 {
		t.Fatalf("defaults = kVector %d kText %d k %d, want 40/40/8", got.KVector, got.KText, got.K)
	}
	// Explicit values are preserved.
	set := Params{Embedding: []float32{0.1}, KVector: 100, KText: 50, K: 12}.withDefaults()
	if set.KVector != 100 || set.KText != 50 || set.K != 12 {
		t.Fatalf("explicit values overwritten: %+v", set)
	}
}

// buildArgs must lay the query parameters out in the exact $1..$10 order the
// hybrid SQL binds, and an absent filter must become a NULL bind (nil) so its
// `$n is null or ...` guard turns it into a no-op — never an empty array/string
// that would silently exclude every row.
func TestBuildArgsOrderAndNoOpFilters(t *testing.T) {
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	p := Params{
		Embedding: []float32{0.5, -0.25},
		QueryText: "reset the X200",
		KVector:   40, KText: 40, K: 8,
		Filters: Filters{
			SourceIDs:    []string{"11111111-1111-1111-1111-111111111111"},
			URIPrefix:    "https://docs.acme.com/",
			DateFrom:     &from,
			DateTo:       &to,
			MetadataTags: json.RawMessage(`{"team":"support"}`),
		},
	}
	args := buildArgs(p)
	if len(args) != 10 {
		t.Fatalf("len(args) = %d, want 10", len(args))
	}
	if args[0] != "[0.5,-0.25]" {
		t.Fatalf("args[0] embedding literal = %v", args[0])
	}
	src, ok := args[1].([]string)
	if !ok || len(src) != 1 || src[0] != "11111111-1111-1111-1111-111111111111" {
		t.Fatalf("args[1] source_ids = %v", args[1])
	}
	if args[2] != 40 || args[3] != "reset the X200" || args[4] != 40 || args[5] != 8 {
		t.Fatalf("args[2..5] = %v %v %v %v", args[2], args[3], args[4], args[5])
	}
	if args[6] != `https://docs.acme.com/%` {
		t.Fatalf("args[6] uri pattern = %v", args[6])
	}
	if args[7] != from || args[8] != to {
		t.Fatalf("args[7..8] dates = %v %v", args[7], args[8])
	}
	if args[9] != `{"team":"support"}` {
		t.Fatalf("args[9] metadata = %v", args[9])
	}
}

func TestBuildArgsAbsentFiltersAreNil(t *testing.T) {
	args := buildArgs(Params{Embedding: []float32{1}, QueryText: "q", KVector: 40, KText: 40, K: 8})
	for _, i := range []int{1, 6, 7, 8, 9} {
		if args[i] != nil {
			t.Fatalf("args[%d] = %v, want nil (no-op filter)", i, args[i])
		}
	}
}

// An empty source list must be nil, not an empty []string: `= any('{}')` matches
// nothing, which would silently return zero rows instead of "no source filter".
func TestBuildArgsEmptySourceListIsNil(t *testing.T) {
	args := buildArgs(Params{Embedding: []float32{1}, QueryText: "q", Filters: Filters{SourceIDs: []string{}}}.withDefaults())
	if args[1] != nil {
		t.Fatalf("args[1] empty source list = %v, want nil", args[1])
	}
}
