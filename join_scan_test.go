package sqlx

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"reflect"
	"strings"
	"sync"
	"testing"
)

type joinOrder struct {
	TenantID int    `db:"tenant_id"`
	OrderID  int    `db:"order_id"`
	Number   string `db:"order_number"`
	Items    []joinItem
	Labels   []joinLabel
}

type joinItem struct {
	TenantID int    `db:"tenant_id"`
	OrderID  int    `db:"order_id"`
	ItemID   int    `db:"item_id"`
	Name     string `db:"item_name"`
	Options  []joinOption
}

type joinLabel struct {
	TenantID int    `db:"tenant_id"`
	OrderID  int    `db:"order_id"`
	LabelID  int    `db:"label_id"`
	Name     string `db:"label_name"`
}

type joinOption struct {
	TenantID int    `db:"tenant_id"`
	OrderID  int    `db:"order_id"`
	ItemID   int    `db:"item_id"`
	OptionID int    `db:"option_id"`
	Name     string `db:"option_name"`
}

func orderJoinMap() *JoinMap {
	return &JoinMap{
		Prefix: "order_",
		Keys:   []string{"tenant_id", "order_id"},
		Relations: []*JoinMap{
			{
				Field:  "Items",
				Prefix: "item_",
				Keys:   []string{"tenant_id", "order_id", "item_id"},
				Relations: []*JoinMap{
					{
						Field:  "Options",
						Prefix: "option_",
						Keys:   []string{"tenant_id", "order_id", "item_id", "option_id"},
					},
				},
			},
			{
				Field:  "Labels",
				Prefix: "label_",
				Keys:   []string{"tenant_id", "order_id", "label_id"},
			},
		},
	}
}

func expectedJoinOrders() []joinOrder {
	return []joinOrder{
		{
			TenantID: 7,
			OrderID:  1,
			Number:   "O-1",
			Items: []joinItem{
				{
					TenantID: 7,
					OrderID:  1,
					ItemID:   10,
					Name:     "item-10",
					Options: []joinOption{
						{TenantID: 7, OrderID: 1, ItemID: 10, OptionID: 100, Name: "option-100"},
						{TenantID: 7, OrderID: 1, ItemID: 10, OptionID: 101, Name: "option-101"},
					},
				},
				{
					TenantID: 7,
					OrderID:  1,
					ItemID:   11,
					Name:     "item-11",
					Options: []joinOption{
						{TenantID: 7, OrderID: 1, ItemID: 11, OptionID: 110, Name: "option-110"},
					},
				},
			},
			Labels: []joinLabel{
				{TenantID: 7, OrderID: 1, LabelID: 20, Name: "paid"},
				{TenantID: 7, OrderID: 1, LabelID: 21, Name: "urgent"},
			},
		},
		{
			TenantID: 7,
			OrderID:  2,
			Number:   "O-2",
			Items:    []joinItem{},
			Labels:   []joinLabel{},
		},
	}
}

func TestJoinScanAssemblesIndependentOfRowOrder(t *testing.T) {
	columns, rows := orderJoinRows()
	original := cloneJoinRowset(columns, rows)

	var byOrder []joinOrder
	db, cleanup := openJoinRowsDB(t, "ordered", columns, rows)
	defer cleanup()
	sqlRows, err := db.Query("SELECT joined")
	if err != nil {
		t.Fatal(err)
	}
	if err := JoinScan(sqlRows, &byOrder, orderJoinMap()); err != nil {
		t.Fatal(err)
	}
	if err := sqlRows.Close(); err != nil {
		t.Fatal(err)
	}

	shuffledRows := shuffleJoinRows(original, 42)
	var shuffled []joinOrder
	db, cleanup = openJoinRowsDB(t, "shuffled", columns, shuffledRows)
	defer cleanup()
	sqlRows, err = db.Query("SELECT joined")
	if err != nil {
		t.Fatal(err)
	}
	if err := JoinScan(sqlRows, &shuffled, orderJoinMap()); err != nil {
		t.Fatal(err)
	}
	if err := sqlRows.Close(); err != nil {
		t.Fatal(err)
	}

	expected := expectedJoinOrders()
	if !reflect.DeepEqual(byOrder, expected) {
		t.Fatalf("ordered result mismatch\nexpected: %#v\nactual:   %#v", expected, byOrder)
	}
	if !reflect.DeepEqual(shuffled, expected) {
		t.Fatalf("shuffled result mismatch\nexpected: %#v\nactual:   %#v", expected, shuffled)
	}

	joinedRows := flattenJoinRowCount(byOrder)
	if joinedRows != len(original) {
		t.Fatalf("joined row count = %d, assembled row count = %d", len(original), joinedRows)
	}
}

func TestJoinScanMergesRepeatedEntityReferences(t *testing.T) {
	type sharedJoinItem struct {
		TenantID int    `db:"tenant_id"`
		ItemID   int    `db:"item_id"`
		Name     string `db:"item_name"`
	}
	type pointerOrder struct {
		TenantID int               `db:"tenant_id"`
		OrderID  int               `db:"order_id"`
		Number   string            `db:"order_number"`
		Items    []*sharedJoinItem `db:"-"`
	}

	columns := []string{"order_tenant_id", "order_order_id", "order_order_number", "item_tenant_id", "item_item_id", "item_item_name"}
	rows := [][]driver.Value{
		{int64(7), int64(1), "O-1", int64(7), int64(10), "shared-item"},
		{int64(7), int64(2), "O-2", int64(7), int64(10), "shared-item"},
	}
	db, cleanup := openJoinRowsDB(t, "pointers", columns, rows)
	defer cleanup()
	sqlRows, err := db.Query("SELECT joined")
	if err != nil {
		t.Fatal(err)
	}
	defer sqlRows.Close()

	var orders []*pointerOrder
	mapping := &JoinMap{
		Prefix: "order_",
		Keys:   []string{"tenant_id", "order_id"},
		Relations: []*JoinMap{
			{Field: "Items", Prefix: "item_", Keys: []string{"tenant_id", "item_id"}},
		},
	}
	if err := JoinScan(sqlRows, &orders, mapping); err != nil {
		t.Fatal(err)
	}
	if len(orders) != 2 || len(orders[0].Items) != 1 || len(orders[1].Items) != 1 {
		t.Fatalf("unexpected assembled collections: %+v", orders)
	}
	if orders[0].Items[0] != orders[1].Items[0] {
		t.Fatal("shared item was not merged into one reference")
	}
}

func TestJoinScanSupportsScannerCompositeKeyAndNullJoin(t *testing.T) {
	type nullItem struct {
		OrderID sql.NullInt64 `db:"order_id"`
		ItemID  sql.NullInt64 `db:"item_id"`
		Name    string        `db:"item_name"`
	}
	type nullOrder struct {
		OrderID int        `db:"order_id"`
		Number  string     `db:"order_number"`
		Items   []nullItem `db:"-"`
	}

	columns := []string{"order_order_id", "order_order_number", "item_order_id", "item_item_id", "item_item_name"}
	rows := [][]driver.Value{
		{int64(1), "O-1", int64(1), int64(10), "item-10"},
		{int64(2), "O-2", nil, nil, nil},
	}
	db, cleanup := openJoinRowsDB(t, "null-key", columns, rows)
	defer cleanup()
	sqlRows, err := db.Query("SELECT joined")
	if err != nil {
		t.Fatal(err)
	}
	defer sqlRows.Close()

	var orders []nullOrder
	mapping := &JoinMap{
		Prefix: "order_",
		Keys:   []string{"order_id"},
		Relations: []*JoinMap{
			{Field: "Items", Prefix: "item_", Keys: []string{"order_id", "item_id"}},
		},
	}
	if err := JoinScan(sqlRows, &orders, mapping); err != nil {
		t.Fatal(err)
	}

	expected := []nullOrder{
		{OrderID: 1, Number: "O-1", Items: []nullItem{{
			OrderID: sql.NullInt64{Int64: 1, Valid: true},
			ItemID:  sql.NullInt64{Int64: 10, Valid: true},
			Name:    "item-10",
		}}},
		{OrderID: 2, Number: "O-2", Items: []nullItem{}},
	}
	if !reflect.DeepEqual(orders, expected) {
		t.Fatalf("expected %#v, got %#v", expected, orders)
	}
}

func TestJoinScanReportsConflictAndDoesNotWriteDestination(t *testing.T) {
	columns, rows := orderJoinRows()
	rows = append(cloneJoinRowset(columns, rows), []driver.Value{
		int64(7), int64(1), "changed",
		nil, nil, nil, nil,
		nil, nil, nil, nil,
		nil, nil, nil, nil, nil,
	})

	db, cleanup := openJoinRowsDB(t, "conflict", columns, rows)
	defer cleanup()
	sqlRows, err := db.Query("SELECT joined")
	if err != nil {
		t.Fatal(err)
	}
	defer sqlRows.Close()

	sentinel := []joinOrder{{OrderID: -99, Number: "keep"}}
	err = JoinScan(sqlRows, &sentinel, orderJoinMap())
	if err == nil {
		t.Fatal("expected a conflict error")
	}
	conflict, ok := err.(*JoinConflictError)
	if !ok {
		t.Fatalf("expected *JoinConflictError, got %T: %v", err, err)
	}
	if conflict.Row != len(rows) || conflict.PreviousRow != 1 || conflict.Column != "order_order_number" {
		t.Fatalf("unexpected conflict location: %+v", conflict)
	}
	if !reflect.DeepEqual(sentinel, []joinOrder{{OrderID: -99, Number: "keep"}}) {
		t.Fatalf("destination was modified after conflict: %#v", sentinel)
	}
}

func TestJoinScanRejectsMappingCycleBeforeScanning(t *testing.T) {
	cyclic := &JoinMap{Prefix: "root_", Keys: []string{"order_id"}}
	cyclic.Relations = []*JoinMap{cyclic}

	sentinel := []joinOrder{{OrderID: -99}}
	err := JoinScan(nil, &sentinel, cyclic)
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("expected cycle error, got %v", err)
	}
	if !reflect.DeepEqual(sentinel, []joinOrder{{OrderID: -99}}) {
		t.Fatalf("destination was modified after cycle error: %#v", sentinel)
	}
}

func TestJoinScanDoesNotWriteDestinationAfterScanError(t *testing.T) {
	columns, rows := orderJoinRows()
	rows = append(cloneJoinRowset(columns, rows), []driver.Value{
		"bad", int64(3), "O-3",
		nil, nil, nil, nil,
		nil, nil, nil, nil,
		nil, nil, nil, nil, nil,
	})

	db, cleanup := openJoinRowsDB(t, "invalid", columns, rows)
	defer cleanup()
	sqlRows, err := db.Query("SELECT joined")
	if err != nil {
		t.Fatal(err)
	}
	defer sqlRows.Close()

	sentinel := []joinOrder{{OrderID: -99, Number: "keep"}}
	if err := JoinScan(sqlRows, &sentinel, orderJoinMap()); err == nil {
		t.Fatal("expected key conversion error")
	}
	if !reflect.DeepEqual(sentinel, []joinOrder{{OrderID: -99, Number: "keep"}}) {
		t.Fatalf("destination was modified after scan error: %#v", sentinel)
	}
}

func flattenJoinRowCount(orders []joinOrder) int {
	count := 0
	for _, order := range orders {
		optionRows := 0
		itemRows := len(order.Items)
		if itemRows == 0 {
			itemRows = 1
		}
		for _, item := range order.Items {
			options := len(item.Options)
			if options == 0 {
				options = 1
			}
			optionRows += options
		}
		if optionRows == 0 {
			optionRows = 1
		}
		labelRows := len(order.Labels)
		if labelRows == 0 {
			labelRows = 1
		}
		count += optionRows * labelRows
	}
	return count
}

func orderJoinRows() ([]string, [][]driver.Value) {
	columns := []string{
		"order_tenant_id", "order_order_id", "order_order_number",
		"item_tenant_id", "item_order_id", "item_item_id", "item_item_name",
		"label_tenant_id", "label_order_id", "label_label_id", "label_label_name",
		"option_tenant_id", "option_order_id", "option_item_id", "option_option_id", "option_option_name",
	}
	orders := []struct {
		order  joinOrder
		items  []joinItem
		labels []joinLabel
	}{
		{
			order: joinOrder{TenantID: 7, OrderID: 1, Number: "O-1"},
			items: []joinItem{
				{TenantID: 7, OrderID: 1, ItemID: 10, Name: "item-10"},
				{TenantID: 7, OrderID: 1, ItemID: 11, Name: "item-11"},
			},
			labels: []joinLabel{
				{TenantID: 7, OrderID: 1, LabelID: 20, Name: "paid"},
				{TenantID: 7, OrderID: 1, LabelID: 21, Name: "urgent"},
			},
		},
		{
			order:  joinOrder{TenantID: 7, OrderID: 2, Number: "O-2"},
			items:  nil,
			labels: nil,
		},
	}
	options := map[int][]joinOption{
		10: {
			{TenantID: 7, OrderID: 1, ItemID: 10, OptionID: 100, Name: "option-100"},
			{TenantID: 7, OrderID: 1, ItemID: 10, OptionID: 101, Name: "option-101"},
		},
		11: {
			{TenantID: 7, OrderID: 1, ItemID: 11, OptionID: 110, Name: "option-110"},
		},
	}

	rows := make([][]driver.Value, 0)
	for _, orderRow := range orders {
		items := orderRow.items
		if items == nil {
			items = []joinItem{{}}
		}
		labels := orderRow.labels
		if labels == nil {
			labels = []joinLabel{{}}
		}
		for _, item := range items {
			itemOptions := options[item.ItemID]
			if itemOptions == nil {
				itemOptions = []joinOption{{}}
			}
			for _, option := range itemOptions {
				for _, label := range labels {
					rows = append(rows, []driver.Value{
						orderRow.order.TenantID, orderRow.order.OrderID, orderRow.order.Number,
						nullableJoinInt(item.TenantID), nullableJoinInt(item.OrderID), nullableJoinInt(item.ItemID), nullableJoinString(item.Name),
						nullableJoinInt(label.TenantID), nullableJoinInt(label.OrderID), nullableJoinInt(label.LabelID), nullableJoinString(label.Name),
						nullableJoinInt(option.TenantID), nullableJoinInt(option.OrderID), nullableJoinInt(option.ItemID), nullableJoinInt(option.OptionID), nullableJoinString(option.Name),
					})
				}
			}
		}
	}
	return columns, rows
}

func nullableJoinInt(value int) driver.Value {
	if value == 0 {
		return nil
	}
	return int64(value)
}

func nullableJoinString(value string) driver.Value {
	if value == "" {
		return nil
	}
	return value
}

func cloneJoinRowset(columns []string, rows [][]driver.Value) [][]driver.Value {
	clone := make([][]driver.Value, len(rows))
	for i := range rows {
		clone[i] = append([]driver.Value(nil), rows[i]...)
	}
	return clone
}

func shuffleJoinRows(rows [][]driver.Value, seed int64) [][]driver.Value {
	shuffled := make([][]driver.Value, len(rows))
	perm := rand.New(rand.NewSource(seed)).Perm(len(rows))
	for i, j := range perm {
		shuffled[i] = append([]driver.Value(nil), rows[j]...)
	}
	return shuffled
}

var joinDriverMu sync.Mutex
var joinDriverRowsets = map[string]joinRowset{}
var joinDriverSequence int

type joinRowset struct {
	columns []string
	rows    [][]driver.Value
}

type joinResultDriver struct{}

func (joinResultDriver) Open(name string) (driver.Conn, error) {
	joinDriverMu.Lock()
	rowset, ok := joinDriverRowsets[name]
	joinDriverMu.Unlock()
	if !ok {
		return nil, fmt.Errorf("join test rowset %q not found", name)
	}
	return &joinResultConn{rowset: rowset}, nil
}

type joinResultConn struct {
	rowset joinRowset
}

func (c *joinResultConn) Prepare(query string) (driver.Stmt, error) {
	return nil, errors.New("Prepare is not supported")
}

func (c *joinResultConn) Close() error {
	return nil
}

func (c *joinResultConn) Begin() (driver.Tx, error) {
	return nil, errors.New("Begin is not supported")
}

func (c *joinResultConn) Query(query string, args []driver.Value) (driver.Rows, error) {
	return c.newRows(), nil
}

func (c *joinResultConn) newRows() *joinResultRows {
	rows := make([][]driver.Value, len(c.rowset.rows))
	for i := range c.rowset.rows {
		rows[i] = append([]driver.Value(nil), c.rowset.rows[i]...)
	}
	return &joinResultRows{columns: append([]string(nil), c.rowset.columns...), rows: rows}
}

type joinResultRows struct {
	columns []string
	rows    [][]driver.Value
	index   int
}

func (r *joinResultRows) Columns() []string {
	return r.columns
}

func (r *joinResultRows) Close() error {
	return nil
}

func (r *joinResultRows) Next(dest []driver.Value) error {
	if r.index >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.index])
	r.index++
	return nil
}

func registerJoinRowset(t *testing.T, columns []string, rows [][]driver.Value) (string, func()) {
	t.Helper()
	joinDriverMu.Lock()
	joinDriverSequence++
	name := fmt.Sprintf("join-rowset-%d", joinDriverSequence)
	joinDriverRowsets[name] = joinRowset{columns: columns, rows: rows}
	joinDriverMu.Unlock()
	return name, func() {
		joinDriverMu.Lock()
		delete(joinDriverRowsets, name)
		joinDriverMu.Unlock()
	}
}

func openJoinRowsDB(t *testing.T, namePrefix string, columns []string, rows [][]driver.Value) (*sql.DB, func()) {
	t.Helper()
	driverName := "sqlx-join-" + namePrefix
	registerJoinDriverOnce(driverName)
	name, cleanupRowset := registerJoinRowset(t, columns, rows)
	db, err := sql.Open(driverName, name)
	if err != nil {
		t.Fatal(err)
	}
	return db, func() {
		db.Close()
		cleanupRowset()
	}
}

var joinDriverOnce sync.Map

func registerJoinDriverOnce(name string) {
	_, loaded := joinDriverOnce.LoadOrStore(name, struct{}{})
	if !loaded {
		sql.Register(name, joinResultDriver{})
	}
}
