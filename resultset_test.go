package sqlx

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// A scripted fake driver implementing driver.RowsNextResultSet lets these
// tests exercise ScanResultSets without an external database.

var (
	errFakeRow        = errors.New("fake driver: boom while iterating rows")
	errFakeTransition = errors.New("fake driver: boom while advancing result set")
	errHandler        = errors.New("handler failed")
)

type fakeSet struct {
	columns []string
	rows    [][]driver.Value
	// rowErr is returned after all scripted rows (instead of io.EOF).
	rowErr error
	// block makes Next block until the query context is canceled.
	block bool
}

type fakeScript struct {
	sets []fakeSet
	// transitionErrs maps the index of the set being left (0 based) to an
	// error returned by the driver's NextResultSet.
	transitionErrs map[int]error
}

var (
	fakeMu      sync.Mutex
	fakeScripts = map[string]*fakeScript{}
	fakeLast    *fakeRows
)

type fakeDriver struct{}

func (fakeDriver) Open(name string) (driver.Conn, error) {
	fakeMu.Lock()
	script, ok := fakeScripts[name]
	fakeMu.Unlock()
	if !ok {
		return nil, fmt.Errorf("no fake script registered for %q", name)
	}
	return &fakeConn{script: script}, nil
}

type fakeConn struct{ script *fakeScript }

func (c *fakeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("not implemented")
}

func (c *fakeConn) Close() error { return nil }
func (c *fakeConn) Begin() (driver.Tx, error) {
	return nil, errors.New("not implemented")
}

func (c *fakeConn) QueryContext(ctx context.Context, _ string, _ []driver.NamedValue) (driver.Rows, error) {
	fr := &fakeRows{script: c.script, ctx: ctx}
	fakeMu.Lock()
	fakeLast = fr
	fakeMu.Unlock()
	return fr, nil
}

type fakeRows struct {
	script      *fakeScript
	ctx         context.Context
	set         int
	row         int
	nextCalls   []int
	closedCount int
	mu          sync.Mutex
}

func (r *fakeRows) Columns() []string {
	if len(r.nextCalls) == 0 {
		r.nextCalls = make([]int, len(r.script.sets))
	}
	return r.script.sets[r.set].columns
}

func (r *fakeRows) Close() error {
	r.mu.Lock()
	r.closedCount++
	r.mu.Unlock()
	return nil
}

func (r *fakeRows) Next(dest []driver.Value) error {
	set := &r.script.sets[r.set]
	if set.block {
		// Emulate a driver whose Next respects context cancellation without
		// holding the connection lock while blocked (which would deadlock with
		// database/sql's context close path).
		select {
		case <-r.ctx.Done():
			return r.ctx.Err()
		case <-time.After(2 * time.Second):
			return errors.New("fake driver: block was not canceled in time")
		}
	}
	if err := r.ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	r.nextCalls[r.set]++
	r.mu.Unlock()
	if r.row >= len(set.rows) {
		if set.rowErr != nil {
			return set.rowErr
		}
		return io.EOF
	}
	copy(dest, set.rows[r.row])
	r.row++
	return nil
}

func (r *fakeRows) HasNextResultSet() bool {
	return r.set < len(r.script.sets)-1
}

func (r *fakeRows) NextResultSet() error {
	if err, ok := r.script.transitionErrs[r.set]; ok {
		return err
	}
	if r.set >= len(r.script.sets)-1 {
		return io.EOF
	}
	r.set++
	r.row = 0
	return nil
}

func (r *fakeRows) nextCallsForSet(set int) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if set >= len(r.nextCalls) {
		return 0
	}
	return r.nextCalls[set]
}

func (r *fakeRows) closed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closedCount == 1
}

func (r *fakeRows) waitClosed(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if r.closed() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("rows cursor was not closed")
}

func init() {
	sql.Register("sqlx-fake-resultsets", fakeDriver{})
}

type fakeFixture struct {
	db   *DB
	rows *Rows
	fake *fakeRows
}

func newFakeFixture(t *testing.T, script *fakeScript) *fakeFixture {
	t.Helper()
	name := fmt.Sprintf("script-%p", script)
	fakeMu.Lock()
	fakeScripts[name] = script
	fakeLast = nil
	fakeMu.Unlock()

	sqldb, err := sql.Open("sqlx-fake-resultsets", name)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db := NewDb(sqldb, "sqlx-fake-resultsets")

	sqlRows, err := sqldb.Query(name)
	if err != nil {
		t.Fatalf("query: %v", err)
	}

	fakeMu.Lock()
	fr := fakeLast
	fakeMu.Unlock()

	t.Cleanup(func() {
		sqldb.Close()
		delete(fakeScripts, name)
	})

	return &fakeFixture{
		db:   db,
		rows: &Rows{Rows: sqlRows, Mapper: mapper()},
		fake: fr,
	}
}

func newFakeFixtureCtx(t *testing.T, script *fakeScript, ctx context.Context) *fakeFixture {
	t.Helper()
	name := fmt.Sprintf("script-ctx-%p", script)
	fakeMu.Lock()
	fakeScripts[name] = script
	fakeLast = nil
	fakeMu.Unlock()

	sqldb, err := sql.Open("sqlx-fake-resultsets", name)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	sqlRows, err := sqldb.QueryContext(ctx, name)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	fakeMu.Lock()
	fr := fakeLast
	fakeMu.Unlock()

	t.Cleanup(func() {
		sqldb.Close()
		delete(fakeScripts, name)
	})

	return &fakeFixture{
		db:   NewDb(sqldb, "sqlx-fake-resultsets"),
		rows: &Rows{Rows: sqlRows, Mapper: mapper()},
		fake: fr,
	}
}

type statRow struct {
	TotalOrders int `db:"total_orders"`
	TotalAmount int `db:"total_amount"`
}

type orderLine struct {
	ID    int    `db:"id"`
	Email string `db:"email"`
	Price int    `db:"price"`
}

type errorRow struct {
	Code    int    `db:"code"`
	Message string `db:"message"`
}

// Three result sets: stats (1 row), order lines (2 rows), errors (1 row).
func happyScript() *fakeScript {
	return &fakeScript{
		sets: []fakeSet{
			{
				columns: []string{"total_orders", "total_amount"},
				rows:    [][]driver.Value{{int64(2), int64(250)}},
			},
			{
				columns: []string{"id", "email", "price"},
				rows: [][]driver.Value{
					{int64(10), "a@example.com", int64(100)},
					{int64(11), "b@example.com", int64(150)},
				},
			},
			{
				columns: []string{"code", "message"},
				rows:    [][]driver.Value{{int64(7), "stock low"}},
			},
		},
	}
}

func TestScanResultSets_HappyPath(t *testing.T) {
	f := newFakeFixture(t, happyScript())

	var stats []statRow
	var lines []orderLine
	var errs []errorRow

	err := ScanResultSets(f.rows, []ResultSetSpec{
		{Dest: &stats},
		{Dest: &lines},
		{Dest: &errs},
	}, TailError)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(stats) != 1 || stats[0].TotalOrders != 2 || stats[0].TotalAmount != 250 {
		t.Fatalf("bad stats: %+v", stats)
	}
	if len(lines) != 2 || lines[0].ID != 10 || lines[1].Email != "b@example.com" || lines[1].Price != 150 {
		t.Fatalf("bad lines: %+v", lines)
	}
	if len(errs) != 1 || errs[0].Code != 7 || errs[0].Message != "stock low" {
		t.Fatalf("bad errors: %+v", errs)
	}
	if !f.fake.closed() {
		t.Fatal("cursor was not released")
	}
}

// The first result set is empty but still occupies index 0; rows of the
// later sets must land in their own destinations.
func TestScanResultSets_EmptyFirstSet(t *testing.T) {
	script := &fakeScript{
		sets: []fakeSet{
			{columns: []string{"total_orders", "total_amount"}},
			{
				columns: []string{"id", "email", "price"},
				rows:    [][]driver.Value{{int64(10), "a@example.com", int64(100)}},
			},
		},
	}
	f := newFakeFixture(t, script)

	var stats []statRow
	var lines []orderLine
	err := ScanResultSets(f.rows, []ResultSetSpec{
		{Dest: &stats},
		{Dest: &lines},
	}, TailError)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(stats) != 0 {
		t.Fatalf("expected empty stats, got %+v", stats)
	}
	if len(lines) != 1 || lines[0].ID != 10 || lines[0].Email != "a@example.com" {
		t.Fatalf("bad lines: %+v", lines)
	}
}

// An empty middle set still consumes a spec, so the following set is not
// scanned into the wrong destination.
func TestScanResultSets_EmptyMiddleSet(t *testing.T) {
	script := &fakeScript{
		sets: []fakeSet{
			{columns: []string{"total_orders", "total_amount"}, rows: [][]driver.Value{{int64(1), int64(5)}}},
			{columns: []string{"id", "email", "price"}},
			{columns: []string{"code", "message"}, rows: [][]driver.Value{{int64(9), "x"}}},
		},
	}
	f := newFakeFixture(t, script)

	var stats []statRow
	var lines []orderLine
	var errs []errorRow
	err := ScanResultSets(f.rows, []ResultSetSpec{
		{Dest: &stats},
		{Dest: &lines},
		{Dest: &errs},
	}, TailError)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(stats) != 1 || len(lines) != 0 || len(errs) != 1 || errs[0].Code != 9 {
		t.Fatalf("unexpected data: stats=%+v lines=%+v errs=%+v", stats, lines, errs)
	}
}

// A driver error while iterating rows ends consumption, surfaces via the
// returned error and releases the cursor.
func TestScanResultSets_RowError(t *testing.T) {
	script := &fakeScript{
		sets: []fakeSet{
			{columns: []string{"total_orders", "total_amount"}, rows: [][]driver.Value{{int64(1), int64(5)}}},
			{
				columns: []string{"id", "email", "price"},
				rows:    [][]driver.Value{{int64(10), "a@example.com", int64(100)}},
				rowErr:  errFakeRow,
			},
		},
	}
	f := newFakeFixture(t, script)

	err := ScanResultSets(f.rows, []ResultSetSpec{
		{Skip: true},
		{Skip: true},
	}, TailError)
	if !errors.Is(err, errFakeRow) {
		t.Fatalf("expected errFakeRow, got %v", err)
	}
	f.fake.waitClosed(t)
}

// An error returned by the driver while advancing between result sets is
// reported instead of being confused with a clean end.
func TestScanResultSets_TransitionError(t *testing.T) {
	script := &fakeScript{
		sets: []fakeSet{
			{columns: []string{"total_orders", "total_amount"}},
			{columns: []string{"id", "email", "price"}},
		},
		transitionErrs: map[int]error{0: errFakeTransition},
	}
	f := newFakeFixture(t, script)

	err := ScanResultSets(f.rows, []ResultSetSpec{
		{Skip: true},
		{Skip: true},
	}, TailError)
	if !errors.Is(err, errFakeTransition) {
		t.Fatalf("expected errFakeTransition, got %v", err)
	}
	f.fake.waitClosed(t)
}

// Fewer result sets than specs is always an error.
func TestScanResultSets_MissingResultSets(t *testing.T) {
	script := &fakeScript{
		sets: []fakeSet{
			{columns: []string{"total_orders", "total_amount"}},
		},
	}
	f := newFakeFixture(t, script)

	err := ScanResultSets(f.rows, []ResultSetSpec{
		{Skip: true},
		{Skip: true},
	}, TailIgnore)
	if err == nil || !strings.Contains(err.Error(), "expected 2 result sets") {
		t.Fatalf("expected missing-result-sets error, got %v", err)
	}
	f.fake.waitClosed(t)
}

// Extra result sets are reported under TailError, and drained under
// TailIgnore; the choice is always the caller's.
func TestScanResultSets_ExtraResultSets(t *testing.T) {
	scriptWithTail := func() *fakeScript {
		return &fakeScript{
			sets: []fakeSet{
				{columns: []string{"total_orders", "total_amount"}},
				{columns: []string{"id", "email", "price"}},
				{
					columns: []string{"code", "message"},
					rows:    [][]driver.Value{{int64(3), "trailing"}},
				},
			},
		}
	}

	f := newFakeFixture(t, scriptWithTail())
	err := ScanResultSets(f.rows, []ResultSetSpec{{Skip: true}, {Skip: true}}, TailError)
	if err == nil || !strings.Contains(err.Error(), "unexpected extra result set at index 2") {
		t.Fatalf("expected strict-tail error, got %v", err)
	}
	f.fake.waitClosed(t)

	f = newFakeFixture(t, scriptWithTail())
	err = ScanResultSets(f.rows, []ResultSetSpec{{Skip: true}, {Skip: true}}, TailIgnore)
	if err != nil {
		t.Fatalf("TailIgnore should drain the tail, got %v", err)
	}
	if !f.fake.closed() {
		t.Fatal("cursor was not released")
	}
}

type itemRow struct {
	ID    int `db:"id"`
	Price int `db:"price"`
}

// Column names repeat but their order changes; the mapping must be rebuilt per
// result set rather than reused from the first set.
func TestScanResultSets_RebuildsMappingOnColumnOrderChange(t *testing.T) {
	script := &fakeScript{
		sets: []fakeSet{
			{columns: []string{"id", "price"}, rows: [][]driver.Value{{int64(1), int64(100)}}},
			{columns: []string{"price", "id"}, rows: [][]driver.Value{{int64(200), int64(2)}}},
		},
	}
	f := newFakeFixture(t, script)

	var first, second []itemRow
	err := ScanResultSets(f.rows, []ResultSetSpec{
		{Handler: func(r *Rows) error {
			var row itemRow
			if err := r.StructScan(&row); err != nil {
				return err
			}
			first = append(first, row)
			return nil
		}},
		{Handler: func(r *Rows) error {
			var row itemRow
			if err := r.StructScan(&row); err != nil {
				return err
			}
			second = append(second, row)
			return nil
		}},
	}, TailError)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(first) != 1 || first[0].ID != 1 || first[0].Price != 100 {
		t.Fatalf("bad first set: %+v", first)
	}
	if len(second) != 1 || second[0].ID != 2 || second[0].Price != 200 {
		t.Fatalf("mapping was not rebuilt: %+v", second)
	}
}

// A handler may StructScan each result set into a different struct type on the
// same *Rows; the cached mapping from a prior set must not leak.
func TestScanResultSets_HandlerDifferentStructTypes(t *testing.T) {
	script := happyScript()
	f := newFakeFixture(t, script)

	var gotStats []statRow
	var gotLines []orderLine
	var gotErrs []errorRow

	err := ScanResultSets(f.rows, []ResultSetSpec{
		{Handler: func(r *Rows) error {
			var s statRow
			if err := r.StructScan(&s); err != nil {
				return err
			}
			gotStats = append(gotStats, s)
			return nil
		}},
		{Handler: func(r *Rows) error {
			var l orderLine
			if err := r.StructScan(&l); err != nil {
				return err
			}
			gotLines = append(gotLines, l)
			return nil
		}},
		{Handler: func(r *Rows) error {
			var e errorRow
			if err := r.StructScan(&e); err != nil {
				return err
			}
			gotErrs = append(gotErrs, e)
			return nil
		}},
	}, TailError)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(gotStats) != 1 || gotStats[0].TotalAmount != 250 {
		t.Fatalf("bad stats: %+v", gotStats)
	}
	if len(gotLines) != 2 || gotLines[1].Price != 150 {
		t.Fatalf("bad lines: %+v", gotLines)
	}
	if len(gotErrs) != 1 || gotErrs[0].Message != "stock low" {
		t.Fatalf("bad errors: %+v", gotErrs)
	}
}

// A handler returning an error aborts consumption and closes the cursor.
func TestScanResultSets_HandlerError(t *testing.T) {
	f := newFakeFixture(t, happyScript())

	err := ScanResultSets(f.rows, []ResultSetSpec{
		{Handler: func(*Rows) error { return errHandler }},
		{Skip: true},
		{Skip: true},
	}, TailError)
	if !errors.Is(err, errHandler) {
		t.Fatalf("expected handler error, got %v", err)
	}
	f.fake.waitClosed(t)
}

// Skip still drains the result set (and observes Rows.Err) so later sets
// remain usable.
func TestScanResultSets_SkipDrainsRows(t *testing.T) {
	script := &fakeScript{
		sets: []fakeSet{
			{
				columns: []string{"id", "email", "price"},
				rows: [][]driver.Value{
					{int64(10), "a@example.com", int64(100)},
					{int64(11), "b@example.com", int64(150)},
				},
			},
			{columns: []string{"code", "message"}, rows: [][]driver.Value{{int64(1), "ok"}}},
		},
	}
	f := newFakeFixture(t, script)

	var errs []errorRow
	err := ScanResultSets(f.rows, []ResultSetSpec{
		{Skip: true},
		{Dest: &errs},
	}, TailError)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(errs) != 1 || errs[0].Code != 1 {
		t.Fatalf("second set was not reached correctly: %+v", errs)
	}
	if f.fake.nextCallsForSet(0) != 3 {
		t.Fatalf("skipped set was not drained: %d Next calls", f.fake.nextCallsForSet(0))
	}
}

// Cancellation ends consumption and releases the cursor.
func TestScanResultSetsContext_Canceled(t *testing.T) {
	script := &fakeScript{
		sets: []fakeSet{
			{columns: []string{"id", "email", "price"}, block: true},
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	f := newFakeFixtureCtx(t, script, ctx)

	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	err := ScanResultSetsContext(ctx, f.rows, []ResultSetSpec{{Skip: true}}, TailError)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	f.fake.waitClosed(t)
}

// The context entry works end to end on a normal scripted statement.
func TestScanResultSetsContext_HappyPath(t *testing.T) {
	f := newFakeFixtureCtx(t, happyScript(), context.Background())

	var stats []statRow
	err := ScanResultSetsContext(context.Background(), f.rows, []ResultSetSpec{
		{Dest: &stats},
		{Skip: true},
		{Skip: true},
	}, TailError)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(stats) != 1 || stats[0].TotalOrders != 2 {
		t.Fatalf("bad stats: %+v", stats)
	}
	if !f.fake.closed() {
		t.Fatal("cursor was not released")
	}
}

func TestScanResultSets_InvalidSpecs(t *testing.T) {
	f := newFakeFixture(t, happyScript())

	if err := ScanResultSets(f.rows, nil, TailError); err == nil {
		t.Fatal("expected error for empty specs")
	}
	f.fake.waitClosed(t)

	f = newFakeFixture(t, happyScript())
	var lines []orderLine
	err := ScanResultSets(f.rows, []ResultSetSpec{{Dest: &lines, Skip: true}}, TailError)
	if !errors.Is(err, ErrInvalidResultSetSpec) {
		t.Fatalf("expected ErrInvalidResultSetSpec, got %v", err)
	}
	f.fake.waitClosed(t)

	f = newFakeFixture(t, happyScript())
	err = ScanResultSets(f.rows, []ResultSetSpec{{}}, TailError)
	if !errors.Is(err, ErrInvalidResultSetSpec) {
		t.Fatalf("expected ErrInvalidResultSetSpec for empty spec, got %v", err)
	}
	f.fake.waitClosed(t)
}
