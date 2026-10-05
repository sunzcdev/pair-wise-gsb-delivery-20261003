package toml

import (
	"fmt"
	"reflect"
	"strconv"
)

// LayoutStep is one element of the key path handed to a LayoutFunc.
//
// A step selects either a table/map key or an array element:
//
//   - For a key step, Key holds the key and Index is -1.
//   - For an array-element step, Index holds the zero-based position and Key
//     is empty.
//
// The path of a value is the sequence of keys and array indices walked from
// the document root to that value. For example, the second element of the
// "servers" array of tables and its "host" field have the paths
// [{servers} {1}] and [{servers} {1} {host}] respectively.
type LayoutStep struct {
	// Key is the table or map key selected by the step. It is empty when
	// Index >= 0.
	Key string
	// Index is the zero-based array position selected by the step. It is -1
	// for a key step.
	Index int
}

// IsKey reports whether the step selects a table or map key.
func (s LayoutStep) IsKey() bool { return s.Index < 0 }

// IsIndex reports whether the step selects an array element.
func (s LayoutStep) IsIndex() bool { return s.Index >= 0 }

// Layout is the layout decision a LayoutFunc returns for a single value.
//
// The fields only select the physical placement of the value; they never
// change the encoded value. Each field is a tri-state choice:
//
//   - nil leaves the choice to a lower-priority source (a global Encoder
//     option);
//   - a non-nil value pins the axis, overriding the global option.
//
// Fields that do not apply to the kind of value at the path (for example
// Inline on an integer, or Inline on an array of tables rendered with
// headers) are silently ignored.
type Layout struct {
	// Inline selects between a header table and an inline table.
	//
	//   - nil: use the global SetTablesInline option.
	//   - true: render the table as an inline table ({...}), or an array of
	//     tables as an inline array of inline tables.
	//   - false: render the table with a [table] (or [[array table]])
	//     header, overriding a global SetTablesInline(true).
	Inline *bool
	// ArrayMultiline selects between a single-line and a one-element-per-line
	// array.
	//
	//   - nil: use the global SetArraysMultiline option.
	//   - true: emit one element per line.
	//   - false: keep the array on a single line, overriding a global
	//     SetArraysMultiline(true).
	ArrayMultiline *bool
}

// InlineTable returns a Layout that renders a table inline ({...}).
func InlineTable() Layout { v := true; return Layout{Inline: &v} }

// HeaderTable returns a Layout that renders a table with a [table] header.
func HeaderTable() Layout { v := false; return Layout{Inline: &v} }

// MultilineArray returns a Layout that emits one array element per line.
func MultilineArray() Layout { v := true; return Layout{ArrayMultiline: &v} }

// SinglelineArray returns a Layout that keeps an array on a single line.
func SinglelineArray() Layout { v := false; return Layout{ArrayMultiline: &v} }

// LayoutFunc chooses the layout of a value while encoding.
//
// path is the key path of the value, starting with an empty (non-nil) slice
// for the document root. The callback is invoked once for every table and
// array value, including map entries that cannot carry a struct tag. It is
// not invoked for scalar values.
//
// The callback decides presentation only. It must not alter the value being
// encoded; the document produced by the encoder re-decodes to exactly the
// input data. Returning a non-nil error aborts encoding; the error is wrapped
// with the path it was returned for.
//
// The path slice is a fresh copy for every invocation. A callback that keeps
// it is free to do so: later invocations and the rest of the encode never
// mutate the retained slice.
//
// Precedence, from strongest to weakest:
//
//  1. struct tags: the "inline" and "multiline" tags always win for the axis
//     they set;
//  2. this callback;
//  3. the global Encoder options (SetTablesInline, SetArraysMultiline).
type LayoutFunc func(path []LayoutStep) (Layout, error)

// cbLayout bits record, per entry or value, the layout the LayoutFunc
// selected. They are packed into the spare byte valueOptions already leaves
// next to rawShape so that enabling the (opt-in) callback does not grow an
// entry or perturb the default encode path.
const (
	// cbInlineAxis is set when the callback pinned the table axis; cbInline
	// then selects inline (set) versus a header table (clear).
	cbInlineAxis uint8 = 1 << iota
	// cbInline asks for an inline table or inline array of tables; only
	// meaningful with cbInlineAxis.
	cbInline
	// cbArrayAxis is set when the callback pinned the array axis;
	// cbArrayMultiline then selects multiline (set) versus single line
	// (clear).
	cbArrayAxis
	// cbArrayMultiline asks for one array element per line.
	cbArrayMultiline
)

// applyRootLayout consults the callback for the document root. The root is
// always a table, so asking to render it inline has no valid representation
// and is reported as an invalid layout.
func (e *encoderState) applyRootLayout() error {
	if e.layoutFunc == nil {
		return nil
	}
	path := append([]LayoutStep(nil), e.path...)
	layout, err := e.layoutFunc(path)
	if err != nil {
		return fmt.Errorf("toml: layout error at %s: %w", e.layoutPathString(path), err)
	}
	if layout.Inline != nil && *layout.Inline {
		return e.invalidLayout(path, "the document root cannot be an inline table")
	}
	return nil
}

// resolveLayout invokes the callback for the value at the current path and
// records its decision on opts. Struct tags take precedence over the
// callback, so an axis already pinned by a tag is left untouched.
func (e *encoderState) resolveLayout(opts *valueOptions) error {
	if e.layoutFunc == nil {
		return nil
	}
	path := append([]LayoutStep(nil), e.path...)
	layout, err := e.layoutFunc(path)
	if err != nil {
		return fmt.Errorf("toml: layout error at %s: %w", e.layoutPathString(path), err)
	}
	if !opts.inline {
		switch {
		case layout.Inline != nil && *layout.Inline:
			opts.cbLayout |= cbInlineAxis | cbInline
		case layout.Inline != nil:
			opts.cbLayout |= cbInlineAxis
		}
	}
	if !opts.multiline {
		switch {
		case layout.ArrayMultiline != nil && *layout.ArrayMultiline:
			opts.cbLayout |= cbArrayAxis | cbArrayMultiline
		case layout.ArrayMultiline != nil:
			opts.cbLayout |= cbArrayAxis
		}
	}
	return nil
}

// resolveEntryLayouts resolves the callback for the table- and array-valued
// entries of a regular table, before the two encoding passes route them as
// key-values or sub-tables. Scalar entries never invoke the callback.
func (e *encoderState) resolveEntryLayouts(entries []entry) error {
	if e.layoutFunc == nil {
		return nil
	}
	for i := range entries {
		if !e.entryHasLayout(entries[i].value) {
			continue
		}
		e.pushKey(entries[i].key)
		err := e.resolveLayout(&entries[i].options)
		e.popPath()
		if err != nil {
			return err
		}
	}
	return nil
}

// entryHasLayout reports whether the callback applies to the value: a table,
// an array of tables, or a plain array. Scalars are excluded.
func (e *encoderState) entryHasLayout(v reflect.Value) bool {
	rv, ok := resolve(v)
	if !ok {
		return false
	}
	if rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array {
		return true
	}
	return !isValueKind(rv)
}

// resolveInlineEntries resolves the callback for the members of an inline
// table. Requesting a [table] header for such a member is checked separately
// by validateInlineTable, since a header cannot live inside braces.
func (e *encoderState) resolveInlineEntries(entries []entry) error {
	if e.layoutFunc == nil {
		return nil
	}
	for i := range entries {
		ent := &entries[i]
		if !e.entryHasLayout(ent.value) {
			continue
		}
		e.pushKey(ent.key)
		err := e.resolveLayout(&ent.options)
		e.popPath()
		if err != nil {
			return err
		}
	}
	return nil
}

// validateInlineTable rejects a table-like member that asks for a [table]
// header, which cannot appear inside braces.
func (e *encoderState) validateInlineTable(entries []entry) error {
	if e.layoutFunc == nil {
		return nil
	}
	for i := range entries {
		ent := &entries[i]
		if !e.entryHasLayout(ent.value) {
			continue
		}
		e.pushKey(ent.key)
		if ent.options.callbackAskedTable() &&
			(e.isTableLike(ent.value) || e.isArrayOfTables(ent.value)) {
			err := e.invalidLayout(e.path, "a table inside an inline table cannot use a [table] header")
			e.popPath()
			return err
		}
		e.popPath()
	}
	return nil
}

// arrayElementLayout resolves the callback for one element of a plain (inline)
// array. It must be called with the element's index already pushed onto the
// path, so nested callbacks see the full path. Scalar elements never invoke
// the callback; a table element is rendered as an inline table, and asking for
// a [table] header in a value position is invalid. A nested array can ask for
// its own multiline layout via ArrayMultiline.
func (e *encoderState) arrayElementLayout(v reflect.Value) (valueOptions, error) {
	var opts valueOptions
	if e.layoutFunc == nil {
		return opts, nil
	}
	if !e.entryHasLayout(v) {
		return opts, nil
	}
	err := e.resolveLayout(&opts)
	if err == nil && opts.callbackAskedTable() && e.isTableLike(v) {
		err = e.invalidLayout(e.path, "a table element of an array cannot use a [table] header; use Inline or an array of tables")
	}
	return opts, err
}

// entryForcedInline reports whether a table entry is rendered as a key-value
// (inline) rather than as a header: the global option, the struct tag, or the
// callback can each force it.
func (e *encoderState) entryForcedInline(ent *entry) bool {
	if ent.options.inline {
		return true
	}
	if ent.options.cbLayout&cbInlineAxis != 0 {
		return ent.options.cbLayout&cbInline != 0
	}
	return e.tablesInline
}

// arrayMultiline reports whether an array must be emitted with one element per
// line, honoring the tag, the callback, and the global option in that order.
func (opts valueOptions) arrayMultiline(global bool) bool {
	if opts.multiline {
		return true
	}
	if opts.cbLayout&cbArrayAxis != 0 {
		return opts.cbLayout&cbArrayMultiline != 0
	}
	return global
}

// callbackAskedTable reports whether the callback explicitly requested a
// header table for the entry.
func (opts valueOptions) callbackAskedTable() bool {
	return opts.cbLayout&cbInlineAxis != 0 && opts.cbLayout&cbInline == 0
}

func (e *encoderState) pushKey(key string) {
	e.path = append(e.path, LayoutStep{Key: key, Index: -1})
}

func (e *encoderState) pushIndex(index int) {
	e.path = append(e.path, LayoutStep{Index: index})
}

func (e *encoderState) popPath() {
	e.path = e.path[:len(e.path)-1]
}

// invalidLayout builds an error tagged with the offending path.
func (e *encoderState) invalidLayout(path []LayoutStep, message string) error {
	return fmt.Errorf("toml: invalid layout at %s: %s", e.layoutPathString(path), message)
}

// layoutPathString renders a path for error messages, quoting keys as needed.
func (e *encoderState) layoutPathString(path []LayoutStep) string {
	if len(path) == 0 {
		return "<root>"
	}
	b := make([]byte, 0, 32)
	for i, step := range path {
		if step.Index >= 0 {
			b = append(b, '[')
			b = strconv.AppendInt(b, int64(step.Index), 10)
			b = append(b, ']')
			continue
		}
		if i > 0 {
			b = append(b, '.')
		}
		b = e.appendKey(b, step.Key)
	}
	return string(b)
}
