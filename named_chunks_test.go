package sqlx

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// chunkMockDriver is a minimal driver used to drive the chunking logic
// without a real database.  Its DSN configures:
//
//   - limit=n      reject statements that bind more than n parameters
//   - failafter=n  reject the (n+1)-th statement, then succeed again
//   - bind=k       bindvar produced after registration (dollar|question|named)
type chunkMockDriver struct {
	mu        sync.Mutex
	limit     int
	failAfter int
	attempts  int
	began     int
	committed int
	rolled    int
}

func parseChunkDSN(dsn string) (limit, failAfter int) {
	for _, field := range splitFields(dsn) {
		if value, ok := trimPrefixField(field, "limit="); ok {
			limit, _ = strconv.Atoi(value)
		}
		if value, ok := trimPrefixField(field, "failafter="); ok {
			failAfter, _ = strconv.Atoi(value)
		}
	}
	return limit, failAfter
}

func splitFields(s string) []string {
	var fields []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ';' {
			fields = append(fields, s[start:i])
			start = i + 1
		}
	}
	return fields
}

func trimPrefixField(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && s[:len(prefix)] == prefix {
		return s[len(prefix):], true
	}
	return "", false
}

func (d *chunkMockDriver) OpenConnector(dsn string) (driver.Connector, error) {
	limit, failAfter := parseChunkDSN(dsn)
	return &chunkMockConnector{driver: d, limit: limit, failAfter: failAfter}, nil
}

func (d *chunkMockDriver) Open(dsn string) (driver.Conn, error) {
	c, err := d.OpenConnector(dsn)
	if err != nil {
		return nil, err
	}
	return c.Connect(context.Background())
}

type chunkMockConnector struct {
	driver    *chunkMockDriver
	limit     int
	failAfter int
	mu        sync.Mutex
	queries   []string
	argCounts []int
	argValues [][]driver.Value
}

func (c *chunkMockConnector) Connect(_ context.Context) (driver.Conn, error) {
	return &chunkMockConn{connector: c}, nil
}

func (c *chunkMockConnector) Driver() driver.Driver { return c.driver }

func (c *chunkMockConnector) check(query string, args []driver.NamedValue) error {
	values := make([]driver.Value, len(args))
	for i, arg := range args {
		values[i] = arg.Value
	}
	c.mu.Lock()
	c.queries = append(c.queries, query)
	c.argCounts = append(c.argCounts, len(args))
	c.argValues = append(c.argValues, values)
	c.mu.Unlock()

	c.driver.mu.Lock()
	defer c.driver.mu.Unlock()
	c.driver.attempts++
	if c.failAfter > 0 && c.driver.attempts == c.failAfter+1 {
		return errors.New("chunkMockDriver: statement rejected by test driver")
	}
	if c.limit > 0 && len(args) > c.limit {
		return fmt.Errorf("chunkMockDriver: %d parameters exceed limit %d", len(args), c.limit)
	}
	return nil
}

type chunkMockConn struct {
	connector *chunkMockConnector
	inTx      bool
}

func (c *chunkMockConn) Prepare(query string) (driver.Stmt, error) {
	return &chunkMockStmt{connector: c.connector}, nil
}

func (c *chunkMockConn) Close() error { return nil }

func (c *chunkMockConn) Begin() (driver.Tx, error) {
	c.inTx = true
	c.connector.driver.mu.Lock()
	c.connector.driver.began++
	c.connector.driver.mu.Unlock()
	return &chunkMockTx{conn: c}, nil
}

func (c *chunkMockConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	return c.Prepare(query)
}

func (c *chunkMockConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if err := c.connector.check(query, args); err != nil {
		return nil, err
	}
	return chunkMockResult(1), nil
}

func (c *chunkMockConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return c.Begin()
}

func (c *chunkMockConn) Ping(context.Context) error { return nil }

var _ driver.ExecerContext = (*chunkMockConn)(nil)
var _ driver.ConnPrepareContext = (*chunkMockConn)(nil)
var _ driver.ConnBeginTx = (*chunkMockConn)(nil)
var _ driver.Pinger = (*chunkMockConn)(nil)

type chunkMockStmt struct {
	connector *chunkMockConnector
}

func (s *chunkMockStmt) Close() error  { return nil }
func (s *chunkMockStmt) NumInput() int { return -1 }
func (s *chunkMockStmt) Exec(args []driver.Value) (driver.Result, error) {
	return nil, errors.New("unexpected value Exec path")
}
func (s *chunkMockStmt) Query(args []driver.Value) (driver.Rows, error) {
	return nil, errors.New("unexpected Query")
}

type chunkMockResult int64

func (r chunkMockResult) LastInsertId() (int64, error) { return int64(r), nil }
func (r chunkMockResult) RowsAffected() (int64, error) { return int64(r), nil }

type chunkMockTx struct {
	conn *chunkMockConn
}

func (t *chunkMockTx) Commit() error {
	t.conn.connector.driver.mu.Lock()
	t.conn.connector.driver.committed++
	t.conn.connector.driver.mu.Unlock()
	t.conn.inTx = false
	return nil
}

func (t *chunkMockTx) Rollback() error {
	t.conn.connector.driver.mu.Lock()
	t.conn.connector.driver.rolled++
	t.conn.connector.driver.mu.Unlock()
	t.conn.inTx = false
	return nil
}

var (
	_ driver.Driver        = (*chunkMockDriver)(nil)
	_ io.Closer            = (*chunkMockConn)(nil)
	_ driver.Result        = chunkMockResult(0)
	_ driver.Stmt          = (*chunkMockStmt)(nil)
	_ driver.Tx            = (*chunkMockTx)(nil)
	_ driver.DriverContext = (*chunkMockDriver)(nil)
)

type chunkPerson struct {
	FirstName string `db:"first_name"`
	LastName  string `db:"last_name"`
	Email     string
}

func chunkPeople(n int) []chunkPerson {
	people := make([]chunkPerson, n)
	for i := range people {
		people[i] = chunkPerson{
			FirstName: fmt.Sprintf("first-%d", i),
			LastName:  fmt.Sprintf("last-%d", i),
			Email:     fmt.Sprintf("user-%d@example.com", i),
		}
	}
	return people
}

func openChunkDBWithConnector(t *testing.T, driverName, dsn string) (*DB, *chunkMockDriver, *chunkMockConnector) {
	d := &chunkMockDriver{}
	sql.Register(driverName, d)
	limit, failAfter := parseChunkDSN(dsn)
	connector := &chunkMockConnector{driver: d, limit: limit, failAfter: failAfter}
	db := &DB{DB: sql.OpenDB(connector), driverName: driverName, Mapper: mapper()}
	t.Cleanup(func() { db.Close() })
	return db, d, connector
}

func countBindvar(query, kind string) int {
	switch kind {
	case "question":
		n := 0
		for _, r := range query {
			if r == '?' {
				n++
			}
		}
		return n
	case "dollar":
		n := 0
		for i := 0; i < len(query); i++ {
			if query[i] == '$' {
				n++
			}
		}
		return n
	case "named":
		n := 0
		for i := 0; i < len(query); i++ {
			if query[i] == ':' {
				n++
			}
		}
		return n
	case "at":
		n := 0
		for i := 0; i+1 < len(query); i++ {
			if query[i] == '@' && query[i+1] == 'p' {
				n++
			}
		}
		return n
	}
	return 0
}

func TestChunkPlanParsing(t *testing.T) {
	type tc struct {
		name    string
		query   string
		err     bool
		prefixN []string
		rowN    []string
		suffixN []string
		prefixQ string
		rowQ    string
		suffixQ string
	}
	cases := []tc{
		{
			name:    "simple insert",
			query:   `INSERT INTO person (first_name, last_name, email) VALUES (:first_name, :last_name, :email)`,
			rowN:    []string{"first_name", "last_name", "email"},
			prefixQ: `INSERT INTO person (first_name, last_name, email) VALUES (`,
			rowQ:    `?, ?, ?`,
			suffixQ: `)`,
		},
		{
			name:    "params outside values",
			query:   `INSERT INTO person (a, b, c) VALUES (:a, :b) ON CONFLICT (a) DO UPDATE SET b = :b, c = :c`,
			prefixN: nil,
			rowN:    []string{"a", "b"},
			suffixN: []string{"b", "c"},
			prefixQ: `INSERT INTO person (a, b, c) VALUES (`,
			rowQ:    `?, ?`,
			suffixQ: `) ON CONFLICT (a) DO UPDATE SET b = ?, c = ?`,
		},
		{
			name:    "prefix placeholder",
			query:   `WITH x AS (SELECT :tenant AS t) INSERT INTO person (a,b) VALUES (:a,:b)`,
			prefixN: []string{"tenant"},
			rowN:    []string{"a", "b"},
			suffixN: nil,
			prefixQ: `WITH x AS (SELECT ? AS t) INSERT INTO person (a,b) VALUES (`,
			rowQ:    `?,?`,
			suffixQ: `)`,
		},
		{
			name:    "repeated name within row",
			query:   `INSERT INTO person (a, b) VALUES (:a, :a)`,
			rowN:    []string{"a", "a"},
			prefixQ: `INSERT INTO person (a, b) VALUES (`,
			rowQ:    `?, ?`,
			suffixQ: `)`,
		},
		{
			name:  "no values",
			query: `SELECT * FROM person WHERE first_name = :first_name`,
			err:   true,
		},
		{
			name:  "two rows already present",
			query: `INSERT INTO person (a,b) VALUES (:a,:b),(:a,:b)`,
			err:   true,
		},
		{
			name:  "no named placeholder in row",
			query: `INSERT INTO person (a) VALUES (1)`,
			err:   true,
		},
		{
			name:  "unbalanced parens",
			query: `INSERT INTO person (a,b) VALUES (:a,:b`,
			err:   true,
		},
		{
			name:  "values inside an identifier",
			query: `INSERT INTO table_values (a,b) VALUES (:a,:b)`,
			rowN:  []string{"a", "b"},
		},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			plan, err := buildChunkPlan(test.query)
			if test.err {
				if err != ErrUnsupportedChunkQuery {
					t.Fatalf("expected ErrUnsupportedChunkQuery, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if test.prefixN != nil || len(plan.prefixN) > 0 {
				if len(plan.prefixN) != len(test.prefixN) {
					t.Fatalf("prefixN = %v, want %v", plan.prefixN, test.prefixN)
				}
				for i := range test.prefixN {
					if plan.prefixN[i] != test.prefixN[i] {
						t.Fatalf("prefixN[%d] = %q, want %q", i, plan.prefixN[i], test.prefixN[i])
					}
				}
			}
			if len(plan.rowN) != len(test.rowN) {
				t.Fatalf("rowN = %v, want %v", plan.rowN, test.rowN)
			}
			for i := range test.rowN {
				if plan.rowN[i] != test.rowN[i] {
					t.Fatalf("rowN[%d] = %q, want %q", i, plan.rowN[i], test.rowN[i])
				}
			}
			if test.suffixN != nil || len(plan.suffixN) > 0 {
				if len(plan.suffixN) != len(test.suffixN) {
					t.Fatalf("suffixN = %v, want %v", plan.suffixN, test.suffixN)
				}
				for i := range test.suffixN {
					if plan.suffixN[i] != test.suffixN[i] {
						t.Fatalf("suffixN[%d] = %q, want %q", i, plan.suffixN[i], test.suffixN[i])
					}
				}
			}
			if test.prefixQ != "" && plan.prefixQ != test.prefixQ {
				t.Errorf("prefixQ = %q, want %q", plan.prefixQ, test.prefixQ)
			}
			if test.rowQ != "" && plan.rowQ != test.rowQ {
				t.Errorf("rowQ = %q, want %q", plan.rowQ, test.rowQ)
			}
			if test.suffixQ != "" && plan.suffixQ != test.suffixQ {
				t.Errorf("suffixQ = %q, want %q", plan.suffixQ, test.suffixQ)
			}
		})
	}
}

func TestChunkRowsPerChunk(t *testing.T) {
	plan, err := buildChunkPlan(`INSERT INTO person (a,b) VALUES (:a, :b)`)
	if err != nil {
		t.Fatal(err)
	}
	if rows, err := plan.rowsPerChunk(8); err != nil || rows != 4 {
		t.Fatalf("rows=%d err=%v, want 4", rows, err)
	}
	if rows, err := plan.rowsPerChunk(1); err != ErrRowExceedsParamLimit {
		t.Fatalf("rows=%d err=%v, want ErrRowExceedsParamLimit", rows, err)
	}

	// :x lives before the VALUES opening parenthesis (prefix), :y after it
	// (suffix): fixed params = 2, per row = 1.
	plan2, err := buildChunkPlan(`SELECT :x FROM dual INSERT INTO person (a) VALUES (:a) RETURNING :y`)
	if err != nil {
		t.Fatal(err)
	}
	// fixed params = 2, per row = 1; limit 4 -> 2 rows
	if rows, err := plan2.rowsPerChunk(4); err != nil || rows != 2 {
		t.Fatalf("rows=%d err=%v, want 2", rows, err)
	}
	// limit 3 is exactly the fixed cost + one row, so exactly one fits
	if rows, err := plan2.rowsPerChunk(3); err != nil || rows != 1 {
		t.Fatalf("rows=%d err=%v, want 1", rows, err)
	}
	if _, err := plan2.rowsPerChunk(1); err != ErrRowExceedsParamLimit {
		t.Fatalf("err=%v, want ErrRowExceedsParamLimit", err)
	}
}

func TestNamedExecChunksExactFit(t *testing.T) {
	// Three placeholders per row; a limit of 9 fits exactly 3 rows per chunk.
	db, _, conn := openChunkDBWithConnector(t, "chunkmock-exact", "limit=9")
	people := chunkPeople(6)

	query := `INSERT INTO person (first_name, last_name, email) VALUES (:first_name, :last_name, :email)`
	res, err := db.NamedExecChunks(query, people, 9)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Successful != (InputRange{0, 6}) {
		t.Fatalf("successful = %+v, want [0,6)", res.Successful)
	}
	if res.Failed != (InputRange{0, 0}) {
		t.Fatalf("failed = %+v, want zero", res.Failed)
	}

	conn.mu.Lock()
	defer conn.mu.Unlock()
	if len(conn.queries) != 2 {
		t.Fatalf("executed %d statements, want 2", len(conn.queries))
	}
	for i, q := range conn.queries {
		if got := countBindvar(q, "question"); got != 9 {
			t.Fatalf("statement %d bound %d params, want 9: %s", i, got, q)
		}
		if conn.argCounts[i] != 9 {
			t.Fatalf("statement %d passed %d args, want 9", i, conn.argCounts[i])
		}
	}
}

func TestNamedExecChunksSingleRowOversized(t *testing.T) {
	db, _, conn := openChunkDBWithConnector(t, "chunkmock-oversize", "limit=100")
	people := chunkPeople(5)

	query := `INSERT INTO person (first_name, last_name, email) VALUES (:first_name, :last_name, :email)`
	res, err := db.NamedExecChunks(query, people, 2)
	if err != ErrRowExceedsParamLimit {
		t.Fatalf("err = %v, want ErrRowExceedsParamLimit", err)
	}
	if res.Successful != (InputRange{0, 0}) || res.Failed != (InputRange{0, 0}) {
		t.Fatalf("expected no work, got %+v", res)
	}
	conn.mu.Lock()
	defer conn.mu.Unlock()
	if len(conn.queries) != 0 {
		t.Fatalf("executed %d statements, want 0", len(conn.queries))
	}
}

func TestNamedExecChunksEmpty(t *testing.T) {
	db, _, conn := openChunkDBWithConnector(t, "chunkmock-empty", "")
	res, err := db.NamedExecChunks(
		`INSERT INTO person (first_name) VALUES (:first_name)`,
		[]chunkPerson{},
		5,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Successful != (InputRange{0, 0}) || res.Failed != (InputRange{0, 0}) {
		t.Fatalf("empty result = %+v", res)
	}
	conn.mu.Lock()
	defer conn.mu.Unlock()
	if len(conn.queries) != 0 {
		t.Fatalf("executed %d statements for empty input, want 0", len(conn.queries))
	}
}

func TestNamedExecChunksSecondChunkRejected(t *testing.T) {
	// 3 params per row; 4 rows per chunk (limit 12).  Ten rows make three
	// chunks and failafter=1 rejects the second one.
	db, drv, conn := openChunkDBWithConnector(t, "chunkmock-failsecond", "limit=12;failafter=1")
	people := chunkPeople(10)

	query := `INSERT INTO person (first_name, last_name, email) VALUES (:first_name, :last_name, :email)`
	res, err := db.NamedExecChunks(query, people, 12)
	if err == nil {
		t.Fatal("expected an error from the rejecting second chunk")
	}
	if res.Successful != (InputRange{0, 4}) {
		t.Fatalf("successful = %+v, want [0,4)", res.Successful)
	}
	if res.Failed != (InputRange{4, 8}) {
		t.Fatalf("failed = %+v, want [4,8)", res.Failed)
	}

	conn.mu.Lock()
	queries := len(conn.queries)
	conn.mu.Unlock()
	if queries != 2 {
		t.Fatalf("executed %d statements, want 2", queries)
	}
	drv.mu.Lock()
	defer drv.mu.Unlock()
	if drv.committed != 0 || drv.began != 0 {
		t.Fatalf("DB mode must not manage transactions: began=%d committed=%d", drv.began, drv.committed)
	}
}

func TestNamedExecChunksTxNotCommitted(t *testing.T) {
	db, drv, conn := openChunkDBWithConnector(t, "chunkmock-tx", "limit=9")
	people := chunkPeople(6)

	tx, err := db.Beginx()
	if err != nil {
		t.Fatal(err)
	}
	query := `INSERT INTO person (first_name, last_name, email) VALUES (:first_name, :last_name, :email)`
	res, err := tx.NamedExecChunks(query, people, 9)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Successful != (InputRange{0, 6}) {
		t.Fatalf("successful = %+v, want [0,6)", res.Successful)
	}

	drv.mu.Lock()
	began, committed := drv.began, drv.committed
	drv.mu.Unlock()
	if began != 1 {
		t.Fatalf("began = %d, want 1", began)
	}
	if committed != 0 {
		t.Fatalf("sqlx committed the caller transaction, commits=%d", committed)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	drv.mu.Lock()
	committed = drv.committed
	drv.mu.Unlock()
	if committed != 1 {
		t.Fatalf("committed after caller commit = %d, want 1", committed)
	}

	conn.mu.Lock()
	defer conn.mu.Unlock()
	if len(conn.queries) != 2 {
		t.Fatalf("executed %d statements, want 2", len(conn.queries))
	}
}

func TestNamedExecChunksParamsOutsideValues(t *testing.T) {
	db, _, conn := openChunkDBWithConnector(t, "chunkmock-fixed", "limit=100")
	rows := make([]map[string]interface{}, 4)
	for i := range rows {
		rows[i] = map[string]interface{}{
			"a": fmt.Sprintf("a-%d", i),
			"b": fmt.Sprintf("b-%d", i),
		}
	}

	query := `INSERT INTO person (a,b,c) VALUES (:a,:b) ON CONFLICT (a) DO UPDATE SET b=:b,c=:c`
	fixed := map[string]interface{}{
		"a": "fixed-a",
		"b": "fixed-b",
		"c": "fixed-c",
	}
	res, err := db.NamedExecChunks(query, rows, 100, fixed)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Successful != (InputRange{0, 4}) {
		t.Fatalf("successful = %+v, want [0,4)", res.Successful)
	}
	conn.mu.Lock()
	defer conn.mu.Unlock()
	if len(conn.queries) != 1 {
		t.Fatalf("executed %d statements, want 1", len(conn.queries))
	}
	// per chunk: 0 prefix + 4 rows * 2 + 2 suffix = 10 parameters
	if conn.argCounts[0] != 10 {
		t.Fatalf("args = %d, want 10", conn.argCounts[0])
	}
	if got := countBindvar(conn.queries[0], "question"); got != 10 {
		t.Fatalf("query bound %d params, want 10: %s", got, conn.queries[0])
	}
	// the suffix placeholders are bound from the fixed argument, in order
	if conn.argValues[0][8] != "fixed-b" || conn.argValues[0][9] != "fixed-c" {
		t.Fatalf("suffix args = %v, %v, want fixed-b, fixed-c", conn.argValues[0][8], conn.argValues[0][9])
	}
}

func TestNamedExecChunksRebindsPlaceholderFormat(t *testing.T) {
	for _, test := range []struct {
		driverName string
		bindType   int
		kind       string
	}{
		{"chunkmock-dollar", DOLLAR, "dollar"},
		{"chunkmock-at", AT, "at"},
	} {
		t.Run(test.driverName, func(t *testing.T) {
			BindDriver(test.driverName, test.bindType)
			db, _, conn := openChunkDBWithConnector(t, test.driverName, "limit=6")
			people := chunkPeople(4)

			query := `INSERT INTO person (first_name, last_name, email) VALUES (:first_name, :last_name, :email)`
			res, err := db.NamedExecChunks(query, people, 6)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.Successful != (InputRange{0, 4}) {
				t.Fatalf("successful = %+v, want [0,4)", res.Successful)
			}
			conn.mu.Lock()
			defer conn.mu.Unlock()
			if len(conn.queries) != 2 {
				t.Fatalf("executed %d statements, want 2", len(conn.queries))
			}
			for i, q := range conn.queries {
				if strings.Contains(q, "?") {
					t.Fatalf("statement %d still contains '?': %s", i, q)
				}
				if got := countBindvar(q, test.kind); got != 6 {
					t.Fatalf("statement %d bound %d %s params, want 6: %s", i, got, test.kind, q)
				}
			}
		})
	}
}

func TestNamedExecChunksInputOrder(t *testing.T) {
	// Three placeholders per row with a limit of 6 forces two rows into the
	// first chunk and one into the second; recorded values must still stream in
	// the original input order. Map rows are exercised here as well.
	db, _, conn := openChunkDBWithConnector(t, "chunkmock-order", "limit=6")
	people := chunkPeople(3)
	rowMaps := make([]map[string]interface{}, len(people))
	for i, p := range people {
		rowMaps[i] = map[string]interface{}{
			"first_name": p.FirstName,
			"last_name":  p.LastName,
			"email":      p.Email,
		}
	}
	res, err := db.NamedExecChunks(
		`INSERT INTO person (first_name,last_name,email) VALUES (:first_name,:last_name,:email)`,
		rowMaps,
		6,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Successful != (InputRange{0, 3}) {
		t.Fatalf("successful = %+v, want [0,3)", res.Successful)
	}

	conn.mu.Lock()
	defer conn.mu.Unlock()
	if len(conn.argValues) != 2 {
		t.Fatalf("executed %d statements, want 2", len(conn.argValues))
	}
	want := []driver.Value{
		"first-0", "last-0", "user-0@example.com",
		"first-1", "last-1", "user-1@example.com",
		"first-2", "last-2", "user-2@example.com",
	}
	var got []driver.Value
	for _, vals := range conn.argValues {
		got = append(got, vals...)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d values, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("value %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestNamedExecChunksUnsupported(t *testing.T) {
	db, _, _ := openChunkDBWithConnector(t, "chunkmock-unsupported", "")
	cases := map[string]string{
		"select":        `SELECT * FROM person WHERE first_name = :first_name`,
		"multiple rows": `INSERT INTO person (a,b) VALUES (:a,:b),(:a,:b)`,
		"literal only":  `INSERT INTO person (a) VALUES (1)`,
		"unbalanced":    `INSERT INTO person (a,b) VALUES (:a,:b`,
	}
	for name, query := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := db.NamedExecChunks(query, chunkPeople(1), 10)
			if err != ErrUnsupportedChunkQuery {
				t.Fatalf("err = %v, want ErrUnsupportedChunkQuery", err)
			}
		})
	}
}

func TestNamedExecChunksBadInputs(t *testing.T) {
	db, _, _ := openChunkDBWithConnector(t, "chunkmock-badinput", "")
	people := chunkPeople(1)
	query := `INSERT INTO person (first_name) VALUES (:first_name)`

	if _, err := db.NamedExecChunks(query, people, 0); err == nil {
		t.Fatal("expected error for non-positive maxParams")
	}
	if _, err := db.NamedExecChunks(query, people, 5, nil, nil); err == nil {
		t.Fatal("expected error for two fixed arguments")
	}
	if _, err := db.NamedExecChunks(query, chunkPerson{}, 5); err == nil {
		t.Fatal("expected error for non-slice argument")
	}
}

func TestNamedExecChunksContextSecondChunkRejected(t *testing.T) {
	db, _, _ := openChunkDBWithConnector(t, "chunkmock-ctx", "limit=6;failafter=1")
	people := chunkPeople(6)
	res, err := NamedExecChunksContext(context.Background(), db,
		`INSERT INTO person (first_name,last_name,email) VALUES (:first_name,:last_name,:email)`,
		people, 6)
	if err == nil {
		t.Fatal("expected rejection error")
	}
	if res.Successful != (InputRange{0, 2}) || res.Failed != (InputRange{2, 4}) {
		t.Fatalf("result = %+v, want successful [0,2) failed [2,4)", res)
	}
}
