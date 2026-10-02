package vm

import (
	"fmt"
	"reflect"

	"github.com/expr-lang/expr/file"
	"github.com/expr-lang/expr/vm/runtime"
)

// RowError associates a runtime error with the row (0-based index of the
// original input) that produced it.
type RowError struct {
	Row   int
	Error error
}

func (e RowError) Unwrap() error { return e.Error }

// BatchResult holds the outcome of a columnar evaluation. Values has one entry
// per input row; a row that fails has a nil value there and is reported in
// Errors with RowError.Row equal to that row's original index.
type BatchResult struct {
	Values []any
	Errors []RowError
}

// Columns provides column-oriented input. Keys match variable names (for map
// envs) or dot-joined field paths (for struct envs). Each value is a slice of
// the common row length.
type Columns map[string]any

// RunBatch evaluates program once for every row using column-oriented input.
//
// It targets the common scalar subset of expressions: literals, variables and
// struct fields, arithmetic, comparisons, string/matches tests, casts, the
// unary minus/not, nil tests, the ?? operator, and short-circuiting && / || /
// ternary expressions. Boolean short-circuiting is applied independently to
// each row, so a row whose condition is already decided never evaluates the
// other branch (a division or a host call included).
//
// Programs containing unsupported constructs (function calls, loops,
// closures, arrays, maps, slicing, profiling, etc.) are executed by falling
// back to the standard scalar VM, one row at a time. Results, nil semantics
// and error messages therefore match Run exactly; errors are attached to the
// row index of the original input.
//
// For programs compiled with expr.Env against a struct type, use RunBatchStruct
// so field paths can be resolved during the fallback path.
func RunBatch(program *Program, columns Columns) (*BatchResult, error) {
	return runBatch(program, nil, columns)
}

// RunBatchStruct is RunBatch for struct envs. prototype is a value (or
// pointer) of the type passed to expr.Env; its fields supply type information
// for the scalar fallback, while per-row values come from columns keyed by
// dot-joined field paths.
func RunBatchStruct(program *Program, prototype any, columns Columns) (*BatchResult, error) {
	if prototype == nil {
		return nil, fmt.Errorf("RunBatchStruct requires a non-nil prototype env")
	}
	return runBatch(program, prototype, columns)
}

// RunBatch runs a batch on a reusable VM. Reusing the same *VM across batches
// with the same column slices also reuses cached []any views of typed columns,
// avoiding per-batch boxing. A *VM must not be used concurrently.
func (vm *VM) RunBatch(program *Program, columns Columns) (*BatchResult, error) {
	return vm.runBatch(program, nil, columns)
}

// RunBatchStruct is the struct-env counterpart of VM.RunBatch.
func (vm *VM) RunBatchStruct(program *Program, prototype any, columns Columns) (*BatchResult, error) {
	if prototype == nil {
		return nil, fmt.Errorf("RunBatchStruct requires a non-nil prototype env")
	}
	return vm.runBatch(program, prototype, columns)
}

func runBatch(program *Program, prototype any, columns Columns) (*BatchResult, error) {
	return (&VM{}).runBatch(program, prototype, columns)
}

func (vm *VM) runBatch(program *Program, prototype any, columns Columns) (*BatchResult, error) {
	if program == nil {
		return nil, fmt.Errorf("program is nil")
	}

	n, err := columnCount(columns)
	if err != nil {
		return nil, err
	}

	input := batchInput{
		n:       n,
		columns: columns,
		views:   vm.batchViews,
	}
	if prototype != nil {
		v := reflect.ValueOf(prototype)
		for v.Kind() == reflect.Ptr {
			v = v.Elem()
		}
		input.protoType = v.Type()
	}

	if input.views == nil {
		input.views = make(map[string][]any)
	} else {
		// Drop cached views for columns that were replaced since last call.
		for key := range input.views {
			cur, ok := columns[key]
			if !ok || cachedViewStale(cur, input.views[key]) {
				delete(input.views, key)
			}
		}
	}

	if !batchSupported(program) {
		return runBatchFallback(program, input)
	}

	b := &batchVM{
		input:  input,
		n:      n,
		values: make([]any, n),
	}
	result, err := b.execute(program)
	if err == nil {
		vm.batchViews = input.views
	}
	return result, err
}

// cachedViewStale reports whether a cached []any view no longer matches a
// supplied column. []any columns are never cached; typed columns are compared
// by length only (same slice means same backing values).
func cachedViewStale(col any, cached []any) bool {
	if _, ok := col.([]any); ok {
		return false
	}
	l, ok := columnLenOk(col)
	return !ok || l != len(cached)
}

func columnLenOk(col any) (int, bool) {
	switch v := col.(type) {
	case []any:
		return len(v), true
	case []int:
		return len(v), true
	case []int64:
		return len(v), true
	case []int8:
		return len(v), true
	case []int16:
		return len(v), true
	case []int32:
		return len(v), true
	case []uint:
		return len(v), true
	case []uint8:
		return len(v), true
	case []uint16:
		return len(v), true
	case []uint32:
		return len(v), true
	case []uint64:
		return len(v), true
	case []float32:
		return len(v), true
	case []float64:
		return len(v), true
	case []string:
		return len(v), true
	case []bool:
		return len(v), true
	}
	return 0, false
}

type batchInput struct {
	n         int
	columns   Columns
	protoType reflect.Type

	// views caches []any materializations of typed columns for the lifetime of
	// a reusable VM, so repeated batches over the same columns do not re-box.
	views map[string][]any
}

type batchVM struct {
	input batchInput
	n     int

	// Expression stack of value columns. Which rows are currently flowing
	// through the interpreter is tracked globally by active/suspended/dead so
	// that short-circuited rows are invisible to OpPop and intermediate pushes.
	stack [][]any
	owned []bool // true for pooled working columns, false for read-only inputs
	sp    int
	vars  [][]any
	nils  []any

	active    mask // rows currently executing at the current ip
	suspended mask // rows parked on a taken jump (not executing)
	dead      mask // rows that produced an error

	// Rows parked by a taken jump, keyed by the bytecode index they land on.
	pending map[int][]parked

	// Pool of reusable value columns, indexed by capacity availability.
	pool [][]any

	values []any
	errors []RowError
}

type parked struct {
	col   []any
	m     mask
	owned bool
}

func (b *batchVM) execute(program *Program) (*BatchResult, error) {
	b.active = newMask(b.n, true)
	b.suspended = newMask(b.n, false)
	b.dead = newMask(b.n, false)

	for ip := 0; ip < len(program.Bytecode); ip++ {
		op := program.Bytecode[ip]
		arg := program.Arguments[ip]
		if err := b.exec(program, ip, op, arg); err != nil {
			return nil, b.bindError(program, ip, err)
		}

		// Rows parked at the next instruction merge after this instruction's
		// stack effects (e.g. an OpPop at the start of a branch body), so the
		// carried condition/value lands in the correct slot.
		next := ip + 1
		if ps := b.pending[next]; len(ps) > 0 {
			b.mergePending(ps)
			delete(b.pending, next)
		}
	}

	if b.sp > 0 {
		col := b.stack[b.sp-1]
		for i := 0; i < b.n; i++ {
			if b.active.get(i) {
				b.values[i] = col[i]
			}
		}
	}

	if len(b.errors) > 1 {
		sortRowErrors(b.errors)
	}
	return &BatchResult{Values: b.values, Errors: b.errors}, nil
}

func (b *batchVM) bindError(program *Program, ip int, cause error) error {
	var location file.Location
	if ip < len(program.locations) {
		location = program.locations[ip]
	}
	f := &file.Error{
		Location: location,
		Message:  cause.Error(),
	}
	f.Wrap(cause)
	return f.Bind(program.source)
}

// fail marks a row dead and records its error.
func (b *batchVM) fail(row int, err error) {
	b.errors = append(b.errors, RowError{Row: row, Error: err})
	b.active.clear(row)
	b.dead.set(row)
}

// allocCol returns a zeroed value column of row length, reusing a buffer from
// the pool when possible.
func (b *batchVM) allocCol() []any {
	for i, buf := range b.pool {
		if cap(buf) >= b.n {
			b.pool[i] = b.pool[len(b.pool)-1]
			b.pool = b.pool[:len(b.pool)-1]
			return buf[:b.n]
		}
	}
	return make([]any, b.n)
}

// releaseCol returns a column to the pool after clearing references.
func (b *batchVM) releaseCol(col []any) {
	if cap(col) < b.n {
		return
	}
	for i := range col {
		col[i] = nil
	}
	b.pool = append(b.pool, col)
}

// mergePending reactivates parked rows. Each parked group carries the value
// column that must occupy the current top slot at the merge point. The target
// slot is materialized as owned before parked (read-only input) values are
// written into it.
func (b *batchVM) mergePending(ps []parked) {
	if !b.owned[b.sp-1] {
		col := b.materialize(b.stack[b.sp-1])
		b.stack[b.sp-1] = col
		b.owned[b.sp-1] = true
	}
	top := b.stack[b.sp-1]
	for _, p := range ps {
		p.m.andNot(b.dead)
		b.active.or(p.m)
		b.suspended.andNot(p.m)
		for i := 0; i < b.n; i++ {
			if p.m.get(i) {
				top[i] = p.col[i]
			}
		}
		if p.owned {
			b.releaseCol(p.col)
		}
	}
}

func (b *batchVM) park(target int, col []any, m mask) {
	if b.pending == nil {
		b.pending = make(map[int][]parked)
	}
	cp := b.allocCol()
	for i := 0; i < b.n; i++ {
		if m.get(i) {
			cp[i] = col[i]
		}
	}
	b.pending[target] = append(b.pending[target], parked{col: cp, m: m, owned: true})
}

func columnCount(columns Columns) (int, error) {
	n := -1
	for name, col := range columns {
		l, err := columnLen(col)
		if err != nil {
			return 0, fmt.Errorf("batch column %q: %w", name, err)
		}
		if n == -1 {
			n = l
		} else if l != n {
			return 0, fmt.Errorf("batch columns have different lengths: %q has %d rows but expected %d", name, l, n)
		}
	}
	if n < 0 {
		n = 0
	}
	return n, nil
}

func columnLen(col any) (int, error) {
	if col == nil {
		return 0, fmt.Errorf("column is nil; pass a slice, e.g. []any{} for an empty batch")
	}
	switch v := col.(type) {
	case []any:
		return len(v), nil
	case []int:
		return len(v), nil
	case []int8:
		return len(v), nil
	case []int16:
		return len(v), nil
	case []int32:
		return len(v), nil
	case []int64:
		return len(v), nil
	case []uint:
		return len(v), nil
	case []uint8:
		return len(v), nil
	case []uint16:
		return len(v), nil
	case []uint32:
		return len(v), nil
	case []uint64:
		return len(v), nil
	case []float32:
		return len(v), nil
	case []float64:
		return len(v), nil
	case []string:
		return len(v), nil
	case []bool:
		return len(v), nil
	default:
		return 0, fmt.Errorf("unsupported column type %T", col)
	}
}

func columnAt(col any, i int) any {
	switch v := col.(type) {
	case []any:
		return v[i]
	case []int:
		return v[i]
	case []int8:
		return v[i]
	case []int16:
		return v[i]
	case []int32:
		return v[i]
	case []int64:
		return v[i]
	case []uint:
		return v[i]
	case []uint8:
		return v[i]
	case []uint16:
		return v[i]
	case []uint32:
		return v[i]
	case []uint64:
		return v[i]
	case []float32:
		return v[i]
	case []float64:
		return v[i]
	case []string:
		return v[i]
	case []bool:
		return v[i]
	}
	return nil
}

// anyView returns an []any view of a typed input column. Non-nil only for
// slice types. The result is not cached inside columns; callers cache it.
func anyView(col any) []any {
	switch v := col.(type) {
	case nil:
		return nil
	case []any:
		return v
	case []int:
		out := make([]any, len(v))
		for i := range v {
			out[i] = v[i]
		}
		return out
	case []int8:
		out := make([]any, len(v))
		for i := range v {
			out[i] = v[i]
		}
		return out
	case []int16:
		out := make([]any, len(v))
		for i := range v {
			out[i] = v[i]
		}
		return out
	case []int32:
		out := make([]any, len(v))
		for i := range v {
			out[i] = v[i]
		}
		return out
	case []int64:
		out := make([]any, len(v))
		for i := range v {
			out[i] = v[i]
		}
		return out
	case []uint:
		out := make([]any, len(v))
		for i := range v {
			out[i] = v[i]
		}
		return out
	case []uint8:
		out := make([]any, len(v))
		for i := range v {
			out[i] = v[i]
		}
		return out
	case []uint16:
		out := make([]any, len(v))
		for i := range v {
			out[i] = v[i]
		}
		return out
	case []uint32:
		out := make([]any, len(v))
		for i := range v {
			out[i] = v[i]
		}
		return out
	case []uint64:
		out := make([]any, len(v))
		for i := range v {
			out[i] = v[i]
		}
		return out
	case []float32:
		out := make([]any, len(v))
		for i := range v {
			out[i] = v[i]
		}
		return out
	case []float64:
		out := make([]any, len(v))
		for i := range v {
			out[i] = v[i]
		}
		return out
	case []string:
		out := make([]any, len(v))
		for i := range v {
			out[i] = v[i]
		}
		return out
	case []bool:
		out := make([]any, len(v))
		for i := range v {
			out[i] = v[i]
		}
		return out
	}
	return nil
}

// inputColumn resolves a named input column to an []any view, caching
// materialized views back into the columns map. Missing columns yield nil
// (matching the scalar VM's nil value for absent map keys).
func (b *batchVM) inputColumn(key string) []any {
	raw, ok := b.input.columns[key]
	if !ok {
		return nil
	}
	if v, ok := raw.([]any); ok {
		return v
	}
	if view, ok := b.input.views[key]; ok {
		return view
	}
	view := anyView(raw)
	if view != nil {
		b.input.views[key] = view
	}
	return view
}

func (b *batchVM) fieldColumn(program *Program, arg int) []any {
	field := program.Constants[arg].(*runtime.Field)
	return b.inputColumn(joinPath(field.Path))
}

func joinPath(path []string) string {
	out := ""
	for i, p := range path {
		if i > 0 {
			out += "."
		}
		out += p
	}
	return out
}

func sortRowErrors(errs []RowError) {
	for i := 1; i < len(errs); i++ {
		for j := i; j > 0 && errs[j-1].Row > errs[j].Row; j-- {
			errs[j-1], errs[j] = errs[j], errs[j-1]
		}
	}
}
