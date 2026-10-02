package lua

import (
	"errors"
	"fmt"
)

// ErrTableFrozen is returned when an LTable mutator (such as RawSet or
// Append) is invoked directly from Go against a read-only (frozen) table.
//
// Writes performed through an LState (for example Lua assignments, rawset or
// table.insert) raise regular Lua errors instead of this Go error.
var ErrTableFrozen = errors.New("attempt to modify a read-only table")

// Freeze marks this table read-only. A shallow freeze only affects this
// table: values reachable through it are left untouched.
//
// Once frozen, every mutator on this table fails: Lua assignments, rawset,
// table.insert, table.remove, table.sort, replacing the metatable and the
// LState RawSet family of functions. Reads, RawGet, pairs/ipairs and other
// traversals keep working normally.
//
// Freezing an already frozen table is a no-op. To freeze the whole table
// graph use FreezeRecursively.
func (tb *LTable) Freeze() {
	tb.frozen = true
}

// IsFrozen reports whether this table has been marked read-only by Freeze or
// FreezeRecursively.
func (tb *LTable) IsFrozen() bool {
	return tb.frozen
}

// FreezeRecursively marks this table and every table reachable from it
// read-only.
//
// Traversal rules:
//   - Tables used as keys or values are followed and frozen.
//   - nil, booleans, numbers, strings, functions and channels (LChannel) are
//     treated as opaque leaves and do not stop the freeze.
//   - Userdata (*LUserData) and threads (*LState) cannot be made read-only;
//     reaching one, either as a key or as a value, aborts the operation with
//     an ErrUnfreezableValue error.
//   - Metatables attached to reachable tables are traversed when they are
//     tables.
//
// Cycles and tables shared by several parts of the graph are supported.
//
// The whole operation is atomic: if any reachable value is unsupported, the
// call returns an error without freezing a single table, so the graph is
// never left partially frozen. Freezing an already fully frozen graph is a
// no-op and returns nil.
func (tb *LTable) FreezeRecursively() error {
	visited := make(map[*LTable]bool)
	if err := collectFrozenTables(tb, visited); err != nil {
		return err
	}
	for target := range visited {
		target.Freeze()
	}
	return nil
}

// ErrUnfreezableValue reports a value that FreezeRecursively cannot freeze.
type ErrUnfreezableValue struct {
	// Type is the Lua type name of the unsupported value.
	Type string
}

func (err *ErrUnfreezableValue) Error() string {
	return fmt.Sprintf("cannot freeze table graph: value of type %s cannot be made read-only", err.Type)
}

// Is reports true for other *ErrUnfreezableValue errors, regardless of the
// concrete type, so errors.Is(err, &ErrUnfreezableValue{}) works.
func (err *ErrUnfreezableValue) Is(target error) bool {
	_, ok := target.(*ErrUnfreezableValue)
	return ok
}

func collectFrozenTables(tb *LTable, visited map[*LTable]bool) error {
	if tb == nil || visited[tb] {
		return nil
	}
	visited[tb] = true

	if mt, ok := tb.Metatable.(*LTable); ok {
		if err := collectFrozenTables(mt, visited); err != nil {
			return err
		}
	}

	var err error
	tb.ForEach(func(key, value LValue) {
		if err != nil {
			return
		}
		if err = checkFreezable(key); err != nil {
			return
		}
		if err = checkFreezable(value); err != nil {
			return
		}
		if child, ok := key.(*LTable); ok {
			err = collectFrozenTables(child, visited)
			if err != nil {
				return
			}
		}
		if child, ok := value.(*LTable); ok {
			err = collectFrozenTables(child, visited)
		}
	})
	return err
}

// checkFreezable returns ErrUnfreezableValue for mutable values whose
// contents cannot be protected by freezing.
func checkFreezable(value LValue) error {
	switch value.(type) {
	case *LUserData:
		return &ErrUnfreezableValue{Type: LTUserData.String()}
	case *LState:
		return &ErrUnfreezableValue{Type: LTThread.String()}
	default:
		return nil
	}
}

// checkWritable panics with ErrTableFrozen when a direct Go mutator is used
// against a frozen table. It is the last line of defense; operations that
// run inside an LState raise Lua errors before reaching this point.
func (tb *LTable) checkWritable() {
	if tb.frozen {
		panic(ErrTableFrozen)
	}
}
