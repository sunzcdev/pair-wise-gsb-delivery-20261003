package sqlx

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
)

// chunkDriver is a minimal in-memory database/sql driver used to exercise
// chunked execution without a real database or CGO.  Each Exec records the
// rendered query and its arg count, and statements carrying more than
// maxArgs or the nth execution (rejectNth > 0) are rejected.
type chunkDriver struct {
	name      string
	maxArgs   int
	rejectNth int64
	execs     int64
	records   *[]chunkExec
}

type chunkExec struct {
	query string
	args  []driver.Value
}

func (d *chunkDriver) Open(name string) (driver.Conn, error) {
	return &chunkConn{drv: d}, nil
}

type chunkConn struct {
	drv *chunkDriver
}

func (c *chunkConn) Prepare(query string) (driver.Stmt, error) {
	return &chunkStmt{conn: c, query: query}, nil
}
func (c *chunkConn) Close() error              { return nil }
func (c *chunkConn) Begin() (driver.Tx, error) { return chunkTx{}, nil }

var _ driver.ExecerContext = (*chunkConn)(nil)

func (c *chunkConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	return c.drv.exec(query, namedToValues(args))
}

type chunkStmt struct {
	conn  *chunkConn
	query string
}

var _ driver.StmtExecContext = (*chunkStmt)(nil)

func (s *chunkStmt) NumInput() int { return -1 }
func (s *chunkStmt) Close() error  { return nil }

func (s *chunkStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	return s.conn.drv.exec(s.query, namedToValues(args))
}

func (s *chunkStmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.conn.drv.exec(s.query, args)
}

func (s *chunkStmt) Query(args []driver.Value) (driver.Rows, error) {
	return nil, errors.New("not implemented")
}

type chunkTx struct{}

func (chunkTx) Commit() error   { return nil }
func (chunkTx) Rollback() error { return nil }

func namedToValues(args []driver.NamedValue) []driver.Value {
	out := make([]driver.Value, len(args))
	for i, a := range args {
		out[i] = a.Value
	}
	return out
}

var errChunkRejected = errors.New("chunkDriver: statement rejected")

func (d *chunkDriver) exec(query string, args []driver.Value) (driver.Result, error) {
	n := atomic.AddInt64(&d.execs, 1)
	if d.maxArgs > 0 && len(args) > d.maxArgs {
		return nil, fmt.Errorf("%w: %d args exceeds limit %d", errChunkRejected, len(args), d.maxArgs)
	}
	if d.rejectNth > 0 && n == d.rejectNth {
		return nil, fmt.Errorf("%w: execution #%d forced failure", errChunkRejected, n)
	}
	*d.records = append(*d.records, chunkExec{query: query, args: append([]driver.Value(nil), args...)})
	return driver.RowsAffected(int64(len(args))), nil
}

var chunkDriverSeq int64

// newChunkDB registers a fresh driver and opens an *sqlx.DB for it.
func newChunkDB(t *testing.T, bindType int, maxArgs int, rejectNth int64) (*DB, *[]chunkExec) {
	t.Helper()
	n := atomic.AddInt64(&chunkDriverSeq, 1)
	name := fmt.Sprintf("chunktest-%d", n)
	records := &[]chunkExec{}
	drv := &chunkDriver{name: name, maxArgs: maxArgs, rejectNth: rejectNth, records: records}
	sql.Register(name, drv)
	db, err := Open(name, "")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	BindDriver(name, bindType)
	db.driverName = name
	t.Cleanup(func() { _ = db.Close() })
	return db, records
}

type chunkPerson struct {
	FirstName string `db:"first_name"`
	LastName  string `db:"last_name"`
}

func makeChunkPeople(n int) []chunkPerson {
	people := make([]chunkPerson, n)
	for i := range people {
		people[i] = chunkPerson{
			FirstName: fmt.Sprintf("first%d", i),
			LastName:  fmt.Sprintf("last%d", i),
		}
	}
	return people
}

// Exactly one statement at the limit: 2 bindvars/row, limit 4 => 2 rows fill
// it exactly, with a remainder forming a second, smaller statement.
func TestNamedExecChunksExact(t *testing.T) {
	db, records := newChunkDB(t, QUESTION, 4, 0)
	people := makeChunkPeople(5)

	res, err := db.NamedExecChunks(
		"INSERT INTO person (first_name, last_name) VALUES (:first_name, :last_name)",
		people, 4)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Executed != (ChunkRange{0, 5}) || res.Failed != (ChunkRange{}) {
		t.Fatalf("unexpected result: %+v", res)
	}
	if len(*records) != 3 {
		t.Fatalf("expected 3 statements (2+2+1), got %d", len(*records))
	}
	for i, rec := range *records {
		want := 4
		if i == 2 {
			want = 2
		}
		if len(rec.args) != want {
			t.Fatalf("statement %d args = %d, want %d", i, len(rec.args), want)
		}
	}

	// exact fill: a number of rows divisible by the per-chunk size
	db2, records2 := newChunkDB(t, QUESTION, 4, 0)
	res2, err := db2.NamedExecChunks(
		"INSERT INTO person (first_name, last_name) VALUES (:first_name, :last_name)",
		makeChunkPeople(4), 4)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res2.Executed != (ChunkRange{0, 4}) {
		t.Fatalf("unexpected result: %+v", res2)
	}
	if len(*records2) != 2 {
		t.Fatalf("expected 2 statements, got %d", len(*records2))
	}
	if (*records2)[0].query != "INSERT INTO person (first_name, last_name) VALUES (?, ?), (?, ?)" {
		t.Fatalf("unexpected query: %q", (*records2)[0].query)
	}
}

// A single row already over the limit must fail without executing anything.
func TestNamedExecChunksSingleRowTooLarge(t *testing.T) {
	db, records := newChunkDB(t, QUESTION, 4, 0)

	// 3 bindvars inside VALUES per row + 1 fixed outside = 4 names; a limit of
	// 3 cannot hold even one row.
	query := `WITH x AS (SELECT :tenant AS t)
		INSERT INTO person (first_name, last_name, tenant)
		VALUES (:first_name, :last_name, :tenant)`

	_, err := db.NamedExecChunks(query, makeChunkPeople(3), 3)
	if !errors.Is(err, ErrChunkRowTooLarge) {
		t.Fatalf("expected ErrChunkRowTooLarge, got %v", err)
	}
	if len(*records) != 0 {
		t.Fatalf("expected no statements executed, got %d", len(*records))
	}
}

// An empty slice succeeds with no statements and a zero result.
func TestNamedExecChunksEmpty(t *testing.T) {
	db, records := newChunkDB(t, QUESTION, 4, 0)

	res, err := db.NamedExecChunks(
		"INSERT INTO person (first_name, last_name) VALUES (:first_name, :last_name)",
		[]chunkPerson{}, 4)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Executed != (ChunkRange{}) || res.Failed != (ChunkRange{}) {
		t.Fatalf("expected zero result, got %+v", res)
	}
	if len(*records) != 0 {
		t.Fatalf("expected no statements, got %d", len(*records))
	}
}

// The driver rejects the second block: first block succeeds, second fails and
// is reported, execution stops.
func TestNamedExecChunksSecondBlockRejected(t *testing.T) {
	db, records := newChunkDB(t, QUESTION, 0, 2)
	people := makeChunkPeople(6)

	res, err := db.NamedExecChunks(
		"INSERT INTO person (first_name, last_name) VALUES (:first_name, :last_name)",
		people, 4)
	if !errors.Is(err, errChunkRejected) {
		t.Fatalf("expected driver rejection, got %v", err)
	}
	if res.Executed != (ChunkRange{0, 2}) {
		t.Fatalf("expected executed [0,2), got %+v", res.Executed)
	}
	if res.Failed != (ChunkRange{2, 4}) {
		t.Fatalf("expected failed [2,4), got %+v", res.Failed)
	}
	if len(*records) != 1 {
		t.Fatalf("expected only the first block to run, got %d", len(*records))
	}
}

// Bindvars outside VALUES and repeated names in one row are counted and
// ordered correctly; the rendered query keeps the driver's bindvar format.
func TestNamedExecChunksOutsideAndRepeated(t *testing.T) {
	db, records := newChunkDB(t, DOLLAR, 0, 0)

	rows := []map[string]interface{}{
		{"first_name": "f0", "last_name": "l0", "tenant": "t0"},
		{"first_name": "f1", "last_name": "l1", "tenant": "t1"},
		{"first_name": "f2", "last_name": "l2", "tenant": "t2"},
		{"first_name": "f3", "last_name": "l3", "tenant": "t3"},
	}

	// :tenant is a fixed bindvar both before (prefix) and after (suffix) the
	// VALUES list, and it also appears inside each row.  :last_name repeats
	// inside the row.  Per row there are 4 bindvars; 2 fixed bindvars sit
	// outside the template.
	query := "INSERT INTO person (first_name, last_name, dup, tenant) " +
		"SELECT :tenant AS tenant UNION ALL " +
		"VALUES (:first_name, :last_name, :last_name, :tenant) WHERE :tenant <> ''"

	// 10 bindvars for 2 rows (1 prefix + 8 row + 1 suffix); limit 10 fits 2
	// rows exactly.
	res, err := NamedExecChunks(db, query, rows, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Executed != (ChunkRange{0, 4}) {
		t.Fatalf("unexpected result: %+v", res)
	}
	if len(*records) != 2 {
		t.Fatalf("expected 2 statements, got %d", len(*records))
	}

	first := (*records)[0]
	wantQuery := "INSERT INTO person (first_name, last_name, dup, tenant) " +
		"SELECT $1 AS tenant UNION ALL " +
		"VALUES ($2, $3, $4, $5), ($6, $7, $8, $9) WHERE $10 <> ''"
	if first.query != wantQuery {
		t.Fatalf("query mismatch:\n got: %q\nwant: %q", first.query, wantQuery)
	}

	// Args follow query position: prefix :tenant, each row's four template
	// values, then suffix :tenant.
	wantArgs := []driver.Value{"t0", "f0", "l0", "l0", "t0", "f1", "l1", "l1", "t1", "t0"}
	if len(first.args) != len(wantArgs) {
		t.Fatalf("arg count = %d, want %d", len(first.args), len(wantArgs))
	}
	for i := range wantArgs {
		if first.args[i] != wantArgs[i] {
			t.Fatalf("arg %d = %v, want %v", i, first.args[i], wantArgs[i])
		}
	}

	// Second statement: fixed prefix is taken from that chunk's first row.
	second := (*records)[1]
	wantArgs2 := []driver.Value{"t2", "f2", "l2", "l2", "t2", "f3", "l3", "l3", "t3", "t2"}
	for i := range wantArgs2 {
		if second.args[i] != wantArgs2[i] {
			t.Fatalf("second arg %d = %v, want %v", i, second.args[i], wantArgs2[i])
		}
	}
}

// Suffix (statement-level) bindvars are bound once from the first chunk row
// and ordered after every per-row bindvar.
func TestNamedExecChunksSuffix(t *testing.T) {
	db, records := newChunkDB(t, QUESTION, 0, 0)
	rows := []map[string]interface{}{
		{"a": "a0", "b": "b0", "note": "n0"},
		{"a": "a1", "b": "b1", "note": "n1"},
		{"a": "a2", "b": "b2", "note": "n2"},
	}

	query := "INSERT INTO t (a, b) VALUES (:a, :b) RETURNING :note"

	// limit 5 => 1 fixed + 2*rows <= 5 => 2 rows per statement
	res, err := db.NamedExecChunks(query, rows, 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Executed != (ChunkRange{0, 3}) {
		t.Fatalf("unexpected result: %+v", res)
	}

	first := (*records)[0]
	if first.query != "INSERT INTO t (a, b) VALUES (?, ?), (?, ?) RETURNING ?" {
		t.Fatalf("unexpected query: %q", first.query)
	}
	want := []driver.Value{"a0", "b0", "a1", "b1", "n0"}
	if len(first.args) != len(want) {
		t.Fatalf("arg count = %d, want %d", len(first.args), len(want))
	}
	for i := range want {
		if first.args[i] != want[i] {
			t.Fatalf("arg %d = %v, want %v", i, first.args[i], want[i])
		}
	}
}

// Inside a transaction the method never commits or rolls back; after a forced
// failure the caller may still roll the transaction back.
func TestNamedExecChunksTx(t *testing.T) {
	db, records := newChunkDB(t, QUESTION, 0, 2)
	people := makeChunkPeople(6)

	tx, err := db.Beginx()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}

	res, err := tx.NamedExecChunks(
		"INSERT INTO person (first_name, last_name) VALUES (:first_name, :last_name)",
		people, 4)
	if !errors.Is(err, errChunkRejected) {
		t.Fatalf("expected driver rejection, got %v", err)
	}
	if res.Executed != (ChunkRange{0, 2}) || res.Failed != (ChunkRange{2, 4}) {
		t.Fatalf("unexpected result: %+v", res)
	}

	// Transaction is still usable / under caller control.
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if len(*records) != 1 {
		t.Fatalf("expected 1 executed block, got %d", len(*records))
	}

	// Success path inside a txn leaves committing to the caller.
	db2, records2 := newChunkDB(t, QUESTION, 0, 0)
	tx2, err := db2.Beginx()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	res2, err := tx2.NamedExecChunks(
		"INSERT INTO person (first_name, last_name) VALUES (:first_name, :last_name)",
		makeChunkPeople(3), 4)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res2.Executed != (ChunkRange{0, 3}) || res2.Failed != (ChunkRange{}) {
		t.Fatalf("unexpected result: %+v", res2)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if len(*records2) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(*records2))
	}
}

func TestParseChunkTemplateUnsupported(t *testing.T) {
	ok := []string{
		"INSERT INTO t (a, b) VALUES (:a, :b)",
		"insert into t(a) values(:a)",
		"WITH x AS (SELECT 1) INSERT INTO t (a) VALUES (:a)",
		"INSERT INTO t (a, b) VALUES (:a, NOW())",
		// a VALUES token hidden in a string literal is ignored, the real one is used
		"INSERT INTO t (a) VALUES (:a) /* 'VALUES (7)' decoy */",
	}
	for _, q := range ok {
		if _, err := parseChunkTemplate(q); err != nil {
			t.Errorf("expected supported query %q, got %v", q, err)
		}
	}

	bad := []string{
		"SELECT * FROM t WHERE a = :a",                                             // no VALUES
		"INSERT INTO t (a) VALUES (:a) VALUES (:a)",                                // two lists
		"WITH x AS (VALUES (:z)) INSERT INTO t (a) VALUES (:a)",                    // VALUES in CTE
		"INSERT INTO t (a, b) VALUES (:a, :b),(:a, :b)",                            // pre-expanded rows
		"INSERT INTO t (a) VALUES (1)",                                             // no bindvar
		"INSERT INTO t (a, b) VALUES (:a, :b) ON DUPLICATE KEY UPDATE a=VALUES(a)", // second VALUES
		"SELECT 'VALUES (:x)' AS s",                                                // VALUES only in string
		"INSERT INTO t (a) VALUES (:a",                                             // unbalanced parens
	}
	for _, q := range bad {
		if _, err := parseChunkTemplate(q); !errors.Is(err, ErrUnsupportedChunkQuery) {
			t.Errorf("expected ErrUnsupportedChunkQuery for %q, got %v", q, err)
		}
	}
}

func TestParseChunkTemplateBindFormats(t *testing.T) {
	base := "INSERT INTO t (a, b) VALUES (:a, :b)"

	cases := map[int]string{
		QUESTION: "INSERT INTO t (a, b) VALUES (?, ?), (?, ?)",
		DOLLAR:   "INSERT INTO t (a, b) VALUES ($1, $2), ($3, $4)",
		AT:       "INSERT INTO t (a, b) VALUES (@p1, @p2), (@p3, @p4)",
		NAMED:    "INSERT INTO t (a, b) VALUES (:arg1, :arg2), (:arg3, :arg4)",
	}

	for bindType, wantQ := range cases {
		tpl, err := parseChunkTemplate(base)
		if err != nil {
			t.Fatalf("bindType %d: %v", bindType, err)
		}
		rows := [][]interface{}{{1, 2}, {3, 4}}
		q, args := tpl.build(rows, 0, 2, bindType)
		if q != wantQ {
			t.Errorf("bindType %d query = %q, want %q", bindType, q, wantQ)
		}
		if len(args) != 4 {
			t.Errorf("bindType %d args = %v", bindType, args)
		}
	}
}

func TestNamedExecChunksInvalidArgs(t *testing.T) {
	db, _ := newChunkDB(t, QUESTION, 0, 0)

	if _, err := db.NamedExecChunks(
		"INSERT INTO t (a) VALUES (:a)", makeChunkPeople(1), 0); err == nil {
		t.Fatal("expected error for maxParams <= 0")
	}

	if _, err := db.NamedExecChunks(
		"INSERT INTO t (a) VALUES (:a)", chunkPerson{FirstName: "x"}, 10); err == nil {
		t.Fatal("expected error for non-slice arg")
	}

	// Missing map key surfaces during binding; first block is reported failed.
	res, err := db.NamedExecChunks(
		"INSERT INTO t (a, b) VALUES (:a, :b)",
		[]map[string]interface{}{{"a": 1}, {"a": 2, "b": 3}}, 10)
	if err == nil {
		t.Fatal("expected bind error, got nil")
	}
	if res.Failed != (ChunkRange{0, 2}) || res.Executed != (ChunkRange{}) {
		t.Fatalf("unexpected result: %+v", res)
	}
}
