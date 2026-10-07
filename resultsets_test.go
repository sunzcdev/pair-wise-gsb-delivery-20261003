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
)

// fakeResultSet describes one result set returned by the fake driver.
type fakeResultSet struct {
	cols []string
	rows [][]driver.Value
	// err, when non-nil, is returned by Next after all rows are consumed,
	// simulating a driver error in the middle of a result set stream.
	err error
}

// fakeScript is a scripted sequence of result sets for one fake connection.
type fakeScript struct {
	sets   []fakeResultSet
	closed bool
}

var (
	fakeScriptsMu sync.Mutex
	fakeScripts   = map[string]*fakeScript{}
)

func registerFakeScript(sets ...fakeResultSet) (*fakeScript, string) {
	s := &fakeScript{sets: sets}
	name := fmt.Sprintf("script-%p", s)
	fakeScriptsMu.Lock()
	fakeScripts[name] = s
	fakeScriptsMu.Unlock()
	return s, name
}

type fakeMultiDriver struct{}

func (fakeMultiDriver) Open(name string) (driver.Conn, error) {
	fakeScriptsMu.Lock()
	defer fakeScriptsMu.Unlock()
	s, ok := fakeScripts[name]
	if !ok {
		return nil, fmt.Errorf("no fake script registered for %q", name)
	}
	return &fakeMultiConn{script: s}, nil
}

type fakeMultiConn struct {
	script *fakeScript
}

func (c *fakeMultiConn) Prepare(query string) (driver.Stmt, error) {
	return nil, errors.New("fake driver does not support Prepare")
}

func (c *fakeMultiConn) Close() error { return nil }

func (c *fakeMultiConn) Begin() (driver.Tx, error) {
	return nil, errors.New("fake driver does not support transactions")
}

func (c *fakeMultiConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &fakeMultiRows{script: c.script}, nil
}

// fakeMultiRows implements driver.Rows and driver.RowsNextResultSet.
type fakeMultiRows struct {
	script *fakeScript
	set    int
	row    int
}

func (r *fakeMultiRows) Columns() []string {
	return r.script.sets[r.set].cols
}

func (r *fakeMultiRows) Close() error {
	r.script.closed = true
	return nil
}

func (r *fakeMultiRows) Next(dest []driver.Value) error {
	s := &r.script.sets[r.set]
	if r.row >= len(s.rows) {
		if s.err != nil {
			return s.err
		}
		return io.EOF
	}
	copy(dest, s.rows[r.row])
	r.row++
	return nil
}

func (r *fakeMultiRows) HasNextResultSet() bool {
	return r.set+1 < len(r.script.sets)
}

func (r *fakeMultiRows) NextResultSet() error {
	if !r.HasNextResultSet() {
		return io.EOF
	}
	r.set++
	r.row = 0
	return nil
}

func init() {
	sql.Register("sqlx-fake-multi", fakeMultiDriver{})
}

func openFakeMultiDB(t *testing.T, sets ...fakeResultSet) (*DB, *fakeScript) {
	t.Helper()
	script, name := registerFakeScript(sets...)
	db, err := Open("sqlx-fake-multi", name)
	if err != nil {
		t.Fatalf("open fake db: %v", err)
	}
	t.Cleanup(func() {
		db.Close()
		fakeScriptsMu.Lock()
		delete(fakeScripts, name)
		fakeScriptsMu.Unlock()
	})
	return db, script
}

type fakeStats struct {
	Total int `db:"total"`
}

type fakeOrderLine struct {
	ID  int    `db:"id"`
	SKU string `db:"sku"`
}

func TestSelectResultSets(t *testing.T) {
	db, script := openFakeMultiDB(t,
		fakeResultSet{
			cols: []string{"total"},
			rows: [][]driver.Value{{int64(3)}},
		},
		fakeResultSet{
			cols: []string{"id", "sku"},
			rows: [][]driver.Value{
				{int64(1), "a"},
				{int64(2), "b"},
			},
		},
		fakeResultSet{
			cols: []string{"code", "message"},
			rows: [][]driver.Value{
				{int64(42), "discarded"},
			},
		},
	)

	var stats []fakeStats
	var lines []fakeOrderLine
	err := db.SelectResultSets(ResultSetsStrict, []ResultSet{
		ScanResultSet(&stats),
		ScanResultSet(&lines),
		SkipResultSet(),
	}, "call proc")
	if err != nil {
		t.Fatalf("SelectResultSets: %v", err)
	}
	if len(stats) != 1 || stats[0].Total != 3 {
		t.Errorf("unexpected stats: %+v", stats)
	}
	if len(lines) != 2 || lines[0].ID != 1 || lines[0].SKU != "a" || lines[1].ID != 2 || lines[1].SKU != "b" {
		t.Errorf("unexpected lines: %+v", lines)
	}
	if !script.closed {
		t.Error("rows were not closed")
	}
}

func TestSelectResultSetsEmptyFirstSet(t *testing.T) {
	db, _ := openFakeMultiDB(t,
		// an empty result set still occupies a position in the spec list
		fakeResultSet{cols: []string{"total"}},
		fakeResultSet{
			cols: []string{"id", "sku"},
			rows: [][]driver.Value{{int64(7), "x"}},
		},
	)

	var stats []fakeStats
	var lines []fakeOrderLine
	err := db.SelectResultSets(ResultSetsStrict, []ResultSet{
		ScanResultSet(&stats),
		ScanResultSet(&lines),
	}, "call proc")
	if err != nil {
		t.Fatalf("SelectResultSets: %v", err)
	}
	if len(stats) != 0 {
		t.Errorf("expected no stats, got %+v", stats)
	}
	if len(lines) != 1 || lines[0].ID != 7 {
		t.Errorf("unexpected lines: %+v", lines)
	}
}

func TestSelectResultSetsRemapsColumns(t *testing.T) {
	// both result sets use the same column names but with different order
	// and types; each set must get a freshly built mapping.
	db, _ := openFakeMultiDB(t,
		fakeResultSet{
			cols: []string{"a", "b"},
			rows: [][]driver.Value{{int64(1), "one"}},
		},
		fakeResultSet{
			cols: []string{"b", "a"},
			rows: [][]driver.Value{{"two", int64(2)}},
		},
	)

	type first struct {
		A int    `db:"a"`
		B string `db:"b"`
	}
	type second struct {
		A int    `db:"a"`
		B string `db:"b"`
	}

	var gotFirst, gotSecond []interface{}
	err := db.SelectResultSets(ResultSetsStrict, []ResultSet{
		EachResultSetRow(func(r *Rows) error {
			var f first
			if err := r.StructScan(&f); err != nil {
				return err
			}
			gotFirst = append(gotFirst, f)
			return nil
		}),
		EachResultSetRow(func(r *Rows) error {
			var s second
			if err := r.StructScan(&s); err != nil {
				return err
			}
			gotSecond = append(gotSecond, s)
			return nil
		}),
	}, "call proc")
	if err != nil {
		t.Fatalf("SelectResultSets: %v", err)
	}
	if len(gotFirst) != 1 || gotFirst[0] != (first{A: 1, B: "one"}) {
		t.Errorf("unexpected first set: %+v", gotFirst)
	}
	if len(gotSecond) != 1 || gotSecond[0] != (second{A: 2, B: "two"}) {
		t.Errorf("unexpected second set: %+v", gotSecond)
	}
}

func TestSelectResultSetsStreamError(t *testing.T) {
	streamErr := errors.New("stream broke")
	db, script := openFakeMultiDB(t,
		fakeResultSet{
			cols: []string{"total"},
			rows: [][]driver.Value{{int64(1)}},
		},
		fakeResultSet{
			cols: []string{"id", "sku"},
			rows: [][]driver.Value{{int64(1), "a"}},
			err:  streamErr,
		},
	)

	var stats []fakeStats
	var lines []fakeOrderLine
	err := db.SelectResultSets(ResultSetsStrict, []ResultSet{
		ScanResultSet(&stats),
		ScanResultSet(&lines),
	}, "call proc")
	if !errors.Is(err, streamErr) {
		t.Fatalf("expected stream error, got %v", err)
	}
	if !script.closed {
		t.Error("rows were not closed after stream error")
	}
}

func TestSelectResultSetsMissingResultSet(t *testing.T) {
	for _, mode := range []ResultSetMode{ResultSetsStrict, ResultSetsIgnoreExtra} {
		db, script := openFakeMultiDB(t,
			fakeResultSet{cols: []string{"total"}},
		)

		var stats, more []fakeStats
		err := db.SelectResultSets(mode, []ResultSet{
			ScanResultSet(&stats),
			ScanResultSet(&more),
		}, "call proc")
		if err == nil || !strings.Contains(err.Error(), "expected 2 result sets") {
			t.Errorf("mode %d: expected missing result set error, got %v", mode, err)
		}
		if !script.closed {
			t.Errorf("mode %d: rows were not closed", mode)
		}
	}
}

func TestSelectResultSetsExtraResultSet(t *testing.T) {
	sets := []fakeResultSet{
		{cols: []string{"total"}},
		{cols: []string{"total"}},
	}

	// strict mode reports the extra result set
	db, script := openFakeMultiDB(t, sets...)
	var stats []fakeStats
	err := db.SelectResultSets(ResultSetsStrict, []ResultSet{
		ScanResultSet(&stats),
	}, "call proc")
	if err == nil || !strings.Contains(err.Error(), "more result sets") {
		t.Errorf("strict: expected extra result set error, got %v", err)
	}
	if !script.closed {
		t.Error("strict: rows were not closed")
	}

	// ignore-extra mode discards the trailing result set
	db, script = openFakeMultiDB(t, sets...)
	stats = nil
	err = db.SelectResultSets(ResultSetsIgnoreExtra, []ResultSet{
		ScanResultSet(&stats),
	}, "call proc")
	if err != nil {
		t.Errorf("ignore-extra: unexpected error %v", err)
	}
	if !script.closed {
		t.Error("ignore-extra: rows were not closed")
	}
}

func TestSelectResultSetsCallbackError(t *testing.T) {
	callbackErr := errors.New("callback failed")
	db, script := openFakeMultiDB(t,
		fakeResultSet{
			cols: []string{"total"},
			rows: [][]driver.Value{{int64(1)}, {int64(2)}},
		},
		fakeResultSet{cols: []string{"total"}},
	)

	calls := 0
	err := db.SelectResultSets(ResultSetsStrict, []ResultSet{
		EachResultSetRow(func(r *Rows) error {
			calls++
			return callbackErr
		}),
		SkipResultSet(),
	}, "call proc")
	if !errors.Is(err, callbackErr) {
		t.Fatalf("expected callback error, got %v", err)
	}
	if calls != 1 {
		t.Errorf("expected consumption to stop after callback error, got %d calls", calls)
	}
	if !script.closed {
		t.Error("rows were not closed after callback error")
	}
}

func TestSelectResultSetsContextCancel(t *testing.T) {
	db, script := openFakeMultiDB(t,
		fakeResultSet{
			cols: []string{"total"},
			rows: [][]driver.Value{{int64(1)}},
		},
		fakeResultSet{cols: []string{"total"}},
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := db.SelectResultSetsContext(ctx, ResultSetsStrict, []ResultSet{
		EachResultSetRow(func(r *Rows) error {
			cancel()
			return nil
		}),
		SkipResultSet(),
	}, "call proc")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if !script.closed {
		t.Error("rows were not closed after cancellation")
	}
}

func TestSelectResultSetsInvalidSpec(t *testing.T) {
	db, script := openFakeMultiDB(t,
		fakeResultSet{cols: []string{"total"}},
	)

	var stats []fakeStats
	err := db.SelectResultSets(ResultSetsStrict, []ResultSet{
		ScanResultSet(&stats),
		{}, // no Dest, RowFunc or Skip: not a valid way to skip a set
	}, "call proc")
	if err == nil || !strings.Contains(err.Error(), "result set 1") {
		t.Fatalf("expected invalid spec error, got %v", err)
	}
	if script.closed {
		t.Error("query should not have been executed with an invalid spec")
	}
}
