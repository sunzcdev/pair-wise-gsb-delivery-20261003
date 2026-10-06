package sqlx

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
)

// graphModel* types are the destination graph.

type graphAdjustment struct {
	Seq    int    `db:"adj_seq"`
	Kind   string `db:"adj_kind"`
	Reason string `db:"adj_reason"`
}

type graphItem struct {
	ID          int64
	Name        string `db:"item_name"`
	Qty         int    `db:"item_qty"`
	Adjustments []*graphAdjustment
}

type graphTag struct {
	ID    int64  `db:"tag_id"`
	Label string `db:"tag_label"`
}

type graphOrder struct {
	ID    int64  `db:"id"`
	Code  string `db:"code"`
	Note  string `db:"note"`
	Items []*graphItem
	Tags  []graphTag
}

type valueItem struct {
	ID          int64
	Qty         int `db:"item_qty"`
	Adjustments []graphAdjustment
}

type valueOrder struct {
	ID    int64  `db:"id"`
	Code  string `db:"code"`
	Items []valueItem
}

func orderRelation() *Relation {
	return &Relation{
		Keys:  []string{"ID", "Code"},
		Model: graphOrder{},
		Relations: map[string]*Relation{
			"Items": {
				Prefix: "item_",
				Keys:   []string{"ID"},
				Model:  graphItem{},
				Relations: map[string]*Relation{
					"Adjustments": {
						Prefix: "adj_",
						Keys:   []string{"Seq", "Kind"},
						Model:  graphAdjustment{},
					},
				},
			},
			"Tags": {
				Prefix: "tag_",
				Keys:   []string{"ID"},
				Model:  graphTag{},
			},
		},
	}
}

// --- A self-contained database/sql driver that replays canned rows ---

var registerFakeOnce sync.Once

type graphFixture struct {
	columns []string
	rows    [][]driver.Value
}

var (
	fakeMu      sync.Mutex
	fakeDSNSeq  int
	fakeSources = map[string]*graphFixture{}
)

type graphFakeDriver struct{}

func (graphFakeDriver) Open(dsn string) (driver.Conn, error) {
	fakeMu.Lock()
	defer fakeMu.Unlock()
	fixture, ok := fakeSources[dsn]
	if !ok {
		return nil, fmt.Errorf("sqlxfake: unknown fixture %q", dsn)
	}
	return &graphFakeConn{fixture: fixture}, nil
}

type graphFakeConn struct{ fixture *graphFixture }

func (c *graphFakeConn) Prepare(string) (driver.Stmt, error) {
	return &graphFakeStmt{fixture: c.fixture}, nil
}
func (c *graphFakeConn) Close() error { return nil }
func (c *graphFakeConn) Begin() (driver.Tx, error) {
	return nil, errors.New("sqlxfake: tx unsupported")
}

type graphFakeStmt struct{ fixture *graphFixture }

func (s *graphFakeStmt) Close() error  { return nil }
func (s *graphFakeStmt) NumInput() int { return -1 }
func (s *graphFakeStmt) Exec([]driver.Value) (driver.Result, error) {
	return nil, errors.New("sqlxfake: exec unsupported")
}
func (s *graphFakeStmt) Query([]driver.Value) (driver.Rows, error) {
	return &graphFakeRows{fixture: s.fixture}, nil
}

type graphFakeRows struct {
	fixture *graphFixture
	pos     int
}

func (r *graphFakeRows) Columns() []string { return r.fixture.columns }
func (r *graphFakeRows) Close() error      { return nil }
func (r *graphFakeRows) Next(dest []driver.Value) error {
	if r.pos >= len(r.fixture.rows) {
		return io.EOF
	}
	for i := range dest {
		dest[i] = r.fixture.rows[r.pos][i]
	}
	r.pos++
	return nil
}

func openGraphRows(t *testing.T, columns []string, rows [][]driver.Value) *sql.Rows {
	t.Helper()
	registerFakeOnce.Do(func() { sql.Register("sqlxfake", graphFakeDriver{}) })

	fakeMu.Lock()
	fakeDSNSeq++
	dsn := fmt.Sprintf("fixture-%d", fakeDSNSeq)
	fakeSources[dsn] = &graphFixture{columns: columns, rows: rows}
	fakeMu.Unlock()

	db, err := sql.Open("sqlxfake", dsn)
	if err != nil {
		t.Fatalf("open fake db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	r, err := db.Query("select")
	if err != nil {
		t.Fatalf("fake query: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

// alignedRows turns column-indexed maps into driver rows aligned to columns.
func alignedRows(columns []string, values []map[string]interface{}) [][]driver.Value {
	out := make([][]driver.Value, len(values))
	for i, m := range values {
		row := make([]driver.Value, len(columns))
		for j, col := range columns {
			if v, ok := m[col]; ok {
				row[j] = v
			}
		}
		out[i] = row
	}
	return out
}

func reverseStrings(in []string) []string {
	out := append([]string(nil), in...)
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func reverseRows(in [][]driver.Value) [][]driver.Value {
	out := make([][]driver.Value, len(in))
	for i := range in {
		out[i] = in[len(in)-1-i]
	}
	return out
}

// permuteRows reorders the values inside every row to follow columnOrder.
func permuteRows(columns, columnOrder []string, rows [][]driver.Value) [][]driver.Value {
	idx := make([]int, len(columns))
	for i, col := range columnOrder {
		for j, c := range columns {
			if c == col {
				idx[i] = j
			}
		}
	}
	out := make([][]driver.Value, len(rows))
	for i, row := range rows {
		nr := make([]driver.Value, len(row))
		for j, k := range idx {
			nr[j] = row[k]
		}
		out[i] = nr
	}
	return out
}

var graphColumns = []string{
	"id", "code", "note",
	"item_id", "item_name", "item_qty",
	"adj_seq", "adj_kind", "adj_reason",
	"tag_id", "tag_label",
}

// graphFixtureRows returns rows for two orders, both with items and tags,
// with a multi-level (item -> adjustment) nesting and composite keys on the
// root (id+code) and adjustment (seq+kind) levels.
func graphFixtureRows() []map[string]interface{} {
	nilItem := map[string]interface{}{
		"item_id": nil, "item_name": nil, "item_qty": nil,
		"adj_seq": nil, "adj_kind": nil, "adj_reason": nil,
	}
	nilAdj := map[string]interface{}{
		"adj_seq": nil, "adj_kind": nil, "adj_reason": nil,
	}
	nilTag := map[string]interface{}{"tag_id": nil, "tag_label": nil}
	return []map[string]interface{}{
		// Order 1, item 10, adjustment (1,rebate), tag 100
		{"id": int64(1), "code": "A", "note": "first",
			"item_id": int64(10), "item_name": "widget", "item_qty": int64(2),
			"adj_seq": int64(1), "adj_kind": "rebate", "adj_reason": "promo",
			"tag_id": int64(100), "tag_label": "urgent"},
		// Same order+item+adjustment, tag 101: parent entities repeat.
		{"id": int64(1), "code": "A", "note": "first",
			"item_id": int64(10), "item_name": "widget", "item_qty": int64(2),
			"adj_seq": int64(1), "adj_kind": "rebate", "adj_reason": "promo",
			"tag_id": int64(101), "tag_label": "paid"},
		// Same order+item, second adjustment, tag 100 repeats.
		{"id": int64(1), "code": "A", "note": "first",
			"item_id": int64(10), "item_name": "widget", "item_qty": int64(2),
			"adj_seq": int64(2), "adj_kind": "tax", "adj_reason": "vat",
			"tag_id": int64(100), "tag_label": "urgent"},
		// Order 1, item 11, no adjustment, tag 100: fully NULL adj keys.
		mergeRows(
			map[string]interface{}{"id": int64(1), "code": "A", "note": "first",
				"item_id": int64(11), "item_name": "gizmo", "item_qty": int64(1),
				"tag_id": int64(100), "tag_label": "urgent"},
			nilAdj),
		// Order 1 with no items and no tags at all: both outer joins NULL.
		mergeRows(map[string]interface{}{"id": int64(1), "code": "A", "note": "first"}, nilItem, nilTag),
		// Order 2, item 20, its own tag.
		{"id": int64(2), "code": "B", "note": "second",
			"item_id": int64(20), "item_name": "bolt", "item_qty": int64(5),
			"adj_seq": int64(1), "adj_kind": "tax", "adj_reason": "vat",
			"tag_id": int64(200), "tag_label": "bulk"},
	}
}

func mergeRows(maps ...map[string]interface{}) map[string]interface{} {
	out := map[string]interface{}{}
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

func expectedGraph() []*graphOrder {
	return []*graphOrder{
		{
			ID: 1, Code: "A", Note: "first",
			Items: []*graphItem{
				{
					ID: 10, Name: "widget", Qty: 2,
					Adjustments: []*graphAdjustment{
						{Seq: 1, Kind: "rebate", Reason: "promo"},
						{Seq: 2, Kind: "tax", Reason: "vat"},
					},
				},
				{ID: 11, Name: "gizmo", Qty: 1},
			},
			Tags: []graphTag{{ID: 100, Label: "urgent"}, {ID: 101, Label: "paid"}},
		},
		{
			ID: 2, Code: "B", Note: "second",
			Items: []*graphItem{
				{ID: 20, Name: "bolt", Qty: 5, Adjustments: []*graphAdjustment{
					{Seq: 1, Kind: "tax", Reason: "vat"},
				}},
			},
			Tags: []graphTag{{ID: 200, Label: "bulk"}},
		},
	}
}

func sortGraph(orders []*graphOrder) {
	sort.Slice(orders, func(i, j int) bool { return orders[i].ID < orders[j].ID })
	for _, o := range orders {
		sort.Slice(o.Items, func(i, j int) bool { return o.Items[i].ID < o.Items[j].ID })
		for _, it := range o.Items {
			sort.Slice(it.Adjustments, func(i, j int) bool {
				if it.Adjustments[i].Seq != it.Adjustments[j].Seq {
					return it.Adjustments[i].Seq < it.Adjustments[j].Seq
				}
				return it.Adjustments[i].Kind < it.Adjustments[j].Kind
			})
		}
		sort.Slice(o.Tags, func(i, j int) bool { return o.Tags[i].ID < o.Tags[j].ID })
	}
}

func assertGraph(t *testing.T, got []*graphOrder) {
	t.Helper()
	sortGraph(got)
	want := expectedGraph()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("assembled graph mismatch:\n got: %+v\nwant: %+v", dumpGraph(got), dumpGraph(want))
	}
}

func dumpGraph(orders []*graphOrder) string {
	var b strings.Builder
	for _, o := range orders {
		fmt.Fprintf(&b, "Order{ID:%d Code:%q Note:%q Items:[", o.ID, o.Code, o.Note)
		for _, it := range o.Items {
			fmt.Fprintf(&b, "Item{ID:%d Name:%q Qty:%d Adj:[", it.ID, it.Name, it.Qty)
			for _, a := range it.Adjustments {
				fmt.Fprintf(&b, "{Seq:%d Kind:%q Reason:%q}", a.Seq, a.Kind, a.Reason)
			}
			b.WriteString("]} ")
		}
		b.WriteString("] Tags:[")
		for _, tg := range o.Tags {
			fmt.Fprintf(&b, "{ID:%d Label:%q}", tg.ID, tg.Label)
		}
		b.WriteString("]} ")
	}
	return b.String()
}

func TestScanGraphAssemblesTwoOneToMany(t *testing.T) {
	rows := openGraphRows(t, graphColumns, alignedRows(graphColumns, graphFixtureRows()))

	var got []*graphOrder
	if err := ScanGraph(rows, &got, orderRelation()); err != nil {
		t.Fatalf("ScanGraph: %v", err)
	}
	assertGraph(t, got)
}

func TestScanGraphIndependentOfRowAndColumnOrder(t *testing.T) {
	data := alignedRows(graphColumns, graphFixtureRows())
	shuffledCols := reverseStrings(graphColumns)
	shuffled := permuteRows(graphColumns, shuffledCols, reverseRows(data))
	rows := openGraphRows(t, shuffledCols, shuffled)

	var got []*graphOrder
	if err := ScanGraph(rows, &got, orderRelation()); err != nil {
		t.Fatalf("ScanGraph: %v", err)
	}
	assertGraph(t, got)
}

func TestScanGraphValueSlicesAndMultiLevelNesting(t *testing.T) {
	cols := []string{"id", "code", "item_id", "item_qty", "adj_seq", "adj_kind", "adj_reason"}
	values := []map[string]interface{}{
		{"id": int64(1), "code": "A", "item_id": int64(10), "item_qty": int64(2),
			"adj_seq": int64(1), "adj_kind": "rebate", "adj_reason": "promo"},
		{"id": int64(1), "code": "A", "item_id": int64(10), "item_qty": int64(2),
			"adj_seq": int64(2), "adj_kind": "tax", "adj_reason": "vat"},
		{"id": int64(1), "code": "A", "item_id": int64(11), "item_qty": int64(1),
			"adj_seq": nil, "adj_kind": nil, "adj_reason": nil},
	}
	rows := openGraphRows(t, cols, alignedRows(cols, values))

	root := &Relation{
		Keys:  []string{"ID", "Code"},
		Model: valueOrder{},
		Relations: map[string]*Relation{
			"Items": {
				Prefix: "item_",
				Keys:   []string{"ID"},
				Model:  valueItem{},
				Relations: map[string]*Relation{
					"Adjustments": {Prefix: "adj_", Keys: []string{"Seq", "Kind"}, Model: graphAdjustment{}},
				},
			},
		},
	}

	var got []valueOrder
	if err := ScanGraph(rows, &got, root); err != nil {
		t.Fatalf("ScanGraph: %v", err)
	}
	want := []valueOrder{
		{ID: 1, Code: "A", Items: []valueItem{
			{ID: 10, Qty: 2, Adjustments: []graphAdjustment{
				{Seq: 1, Kind: "rebate", Reason: "promo"},
				{Seq: 2, Kind: "tax", Reason: "vat"},
			}},
			{ID: 11, Qty: 1},
		}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("value-slice graph mismatch:\n got: %+v\nwant: %+v", got, want)
	}
}

func TestScanGraphEmptyResultLeavesEmptySlice(t *testing.T) {
	rows := openGraphRows(t, graphColumns, nil)
	got := []*graphOrder{{ID: 99}}
	if err := ScanGraph(rows, &got, orderRelation()); err != nil {
		t.Fatalf("ScanGraph: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty slice, got %+v", got)
	}
}

func TestScanGraphConflictReportsRowAndField(t *testing.T) {
	values := graphFixtureRows()
	bad := mergeRows(values[0], map[string]interface{}{"note": "CONFLICT"})
	data := append(alignedRows(graphColumns, values[:1]), alignedRows(graphColumns, []map[string]interface{}{bad})...)
	rows := openGraphRows(t, graphColumns, data)

	var got []*graphOrder
	err := ScanGraph(rows, &got, orderRelation())
	if err == nil {
		t.Fatal("expected conflict error, got nil")
	}
	if !strings.Contains(err.Error(), "row 2") || !strings.Contains(err.Error(), "Note") {
		t.Fatalf("error should name row 2 and field Note, got: %v", err)
	}
}

func TestScanGraphPartiallyNullCompositeKeyRejected(t *testing.T) {
	values := []map[string]interface{}{
		{"id": int64(1), "code": "A", "note": "first",
			"item_id": int64(10), "item_name": "widget", "item_qty": int64(2),
			"adj_seq": int64(1), "adj_kind": nil, "adj_reason": "promo",
			"tag_id": nil, "tag_label": nil},
	}
	rows := openGraphRows(t, graphColumns, alignedRows(graphColumns, values))

	var got []*graphOrder
	err := ScanGraph(rows, &got, orderRelation())
	if err == nil || !strings.Contains(err.Error(), "partially NULL") {
		t.Fatalf("expected partially NULL key error, got: %v", err)
	}
}

func TestScanGraphCycleRejectedBeforeScanning(t *testing.T) {
	type cycleNode struct {
		ID       int64 `db:"n_id"`
		Children []*cycleNode
	}
	root := &Relation{Keys: []string{"ID"}, Model: cycleNode{}}
	child := &Relation{Prefix: "n_", Keys: []string{"ID"}, Model: cycleNode{}}
	root.Relations = map[string]*Relation{"Children": child}
	child.Relations = map[string]*Relation{"Children": root}

	rows := openGraphRows(t, graphColumns, alignedRows(graphColumns, graphFixtureRows()))
	var got []*cycleNode
	err := ScanGraph(rows, &got, root)
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("expected cycle error before scanning, got: %v", err)
	}
}

func TestScanGraphFailureDoesNotWriteDest(t *testing.T) {
	values := graphFixtureRows()
	bad := mergeRows(values[0], map[string]interface{}{"note": "CONFLICT"})
	data := append(alignedRows(graphColumns, values[:1]), alignedRows(graphColumns, []map[string]interface{}{bad})...)
	rows := openGraphRows(t, graphColumns, data)

	sentinel := []*graphOrder{{ID: 42, Code: "KEEP"}}
	got := append([]*graphOrder(nil), sentinel...)
	if err := ScanGraph(rows, &got, orderRelation()); err == nil {
		t.Fatal("expected error")
	}
	if !reflect.DeepEqual(got, sentinel) {
		t.Fatalf("dest mutated on failure:\n got: %+v\nwant: %+v", got, sentinel)
	}
}

func TestScanGraphSingleStructDest(t *testing.T) {
	values := []map[string]interface{}{
		{"id": int64(1), "code": "A", "note": "solo",
			"item_id": int64(10), "item_name": "widget", "item_qty": int64(2),
			"adj_seq": nil, "adj_kind": nil, "adj_reason": nil,
			"tag_id": int64(100), "tag_label": "urgent"},
	}
	rows := openGraphRows(t, graphColumns, alignedRows(graphColumns, values))

	var got graphOrder
	if err := ScanGraph(rows, &got, orderRelation()); err != nil {
		t.Fatalf("ScanGraph: %v", err)
	}
	if got.ID != 1 || got.Code != "A" || got.Note != "solo" {
		t.Fatalf("root mismatch: %+v", got)
	}
	if len(got.Items) != 1 || got.Items[0].ID != 10 {
		t.Fatalf("items mismatch: %+v", got.Items)
	}
	if len(got.Tags) != 1 || got.Tags[0].ID != 100 {
		t.Fatalf("tags mismatch: %+v", got.Tags)
	}
}

func TestScanGraphMergesSharedEntityReference(t *testing.T) {
	type order struct {
		ID   int64 `db:"id"`
		Tags []*graphTag
	}
	cols := []string{"id", "tag_id", "tag_label"}
	values := []map[string]interface{}{
		{"id": int64(1), "tag_id": int64(7), "tag_label": "shared"},
		{"id": int64(2), "tag_id": int64(7), "tag_label": "shared"},
	}
	root := &Relation{
		Keys:  []string{"ID"},
		Model: order{},
		Relations: map[string]*Relation{
			"Tags": {Prefix: "tag_", Keys: []string{"ID"}, Model: graphTag{}},
		},
	}
	rows := openGraphRows(t, cols, alignedRows(cols, values))

	var got []*order
	if err := ScanGraph(rows, &got, root); err != nil {
		t.Fatalf("ScanGraph: %v", err)
	}
	if len(got) != 2 || len(got[0].Tags) != 1 || len(got[1].Tags) != 1 {
		t.Fatalf("unexpected assembly: %+v", got)
	}
	if got[0].Tags[0] != got[1].Tags[0] {
		t.Fatal("tag with identical key should be merged into a single shared reference")
	}
}
