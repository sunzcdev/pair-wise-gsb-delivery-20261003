package sqlx

// Chunked slice expansion support for named queries.
//
// NamedExec expands a slice of structs/maps into a single multi-row INSERT,
// but drivers cap the number of bindvars a single statement may carry.
// NamedExecChunks expands the same slice in groups of rows, running one
// statement per group while honouring a caller supplied bindvar budget.

import (
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/jmoiron/sqlx/reflectx"
)

// ErrUnsupportedChunkQuery is returned when a query cannot be split into
// chunks.  Only a single, reliably identifiable VALUES row template is
// supported; queries with several VALUES lists, pre-expanded row lists, a
// VALUES clause without a bindvar, or with no VALUES row template at all are
// rejected rather than guessed at.
var ErrUnsupportedChunkQuery = errors.New("sqlx: query does not contain a single splittable VALUES row template")

// ErrChunkRowTooLarge is returned when a single input row expands to more
// bindvars than the per-statement limit, making the row impossible to insert.
var ErrChunkRowTooLarge = errors.New("sqlx: a single row requires more bindvars than the per-statement limit")

// ChunkRange is the half-open interval [Start, End) of input elements that a
// statement covers.
type ChunkRange struct {
	Start int
	End   int
}

// Len returns the number of input elements the range covers.
func (r ChunkRange) Len() int {
	return r.End - r.Start
}

// ChunkExecResult reports how a chunked execution went.  Both ranges are
// half-open indices into the input slice.
//
// Executed covers the elements of every statement that succeeded.  When a
// statement fails, Failed covers the elements of the failing statement and
// execution stops; when everything succeeds Failed is the zero range.
type ChunkExecResult struct {
	Executed ChunkRange
	Failed   ChunkRange
}

// chunkTemplate describes a question-bound query with a single VALUES row
// template.  open and close delimit the "(...)" row template, and perRow
// marks the question marks that live inside it.  Prefix and suffix bindvars
// are statement-level parameters bound once per statement.
type chunkTemplate struct {
	bound  string
	names  []string
	open   int
	close  int
	perRow []bool
}

// parseChunkTemplate compiles the named query with QUESTION bindvars and
// locates its single VALUES row template.  A query is accepted only when
// exactly one VALUES (...) template containing at least one bindvar can be
// reliably identified.  String and comment content is ignored so that the
// word VALUES appearing there is not mistaken for the clause.
func parseChunkTemplate(query string) (chunkTemplate, error) {
	bound, names, err := compileNamedQuery([]byte(query), QUESTION)
	if err != nil {
		return chunkTemplate{}, err
	}

	type paren struct{ open, close int }
	var templates []paren
	var bindvars []int
	// Values keywords at or before insideUntil are part of a template body
	// and must not be treated as new clauses.
	insideUntil := -1

	for i := 0; i < len(bound); {
		switch {
		// line comment: -- to end of line
		case bound[i] == '-' && i+1 < len(bound) && bound[i+1] == '-':
			i = skipToLineEnd(bound, i)
		// MySQL line comment
		case bound[i] == '#':
			i = skipToLineEnd(bound, i)
		// block comment
		case bound[i] == '/' && i+1 < len(bound) && bound[i+1] == '*':
			if j := strings.Index(bound[i+2:], "*/"); j < 0 {
				i = len(bound)
			} else {
				i += 2 + j + 2
			}
		// single quoted string, honoring '' and backslash escapes
		case bound[i] == '\'':
			i = skipSingleQuoted(bound, i)
		// double quoted identifier
		case bound[i] == '"':
			i = skipDoubleQuoted(bound, i)
		// back quoted identifier (MySQL)
		case bound[i] == '`':
			i++
			for i < len(bound) && bound[i] != '`' {
				i++
			}
			if i < len(bound) {
				i++
			}
		// dollar quoted string (Postgres), e.g. $tag$ ... $tag$
		case bound[i] == '$':
			i = skipDollarQuoted(bound, i)
		default:
			if bound[i] == '?' {
				bindvars = append(bindvars, i)
				i++
			} else if i > insideUntil && isValuesKeywordAt(bound, i) {
				if open, close, ok := valuesRowParen(bound, i+len("VALUES")); ok {
					templates = append(templates, paren{open: open, close: close})
					// Keep scanning the body to collect its bindvars, but do
					// not treat a VALUES token inside it as a second clause.
					insideUntil = close
					i++
					continue
				}
			}
			i++
		}
	}

	if len(templates) != 1 {
		return chunkTemplate{}, ErrUnsupportedChunkQuery
	}

	tpl := templates[0]

	// A comma following the template means the statement already lists more
	// rows; refuse rather than silently appending to them.
	for k := tpl.close + 1; k < len(bound); k++ {
		if bound[k] == ',' {
			return chunkTemplate{}, ErrUnsupportedChunkQuery
		}
		if !isSpaceByte(bound[k]) {
			break
		}
	}

	// bindvars lists every real question mark in query order; its order
	// matches t.names as produced by compileNamedQuery.
	perRow := make([]bool, len(bindvars))
	rowCount := 0
	for n, off := range bindvars {
		if off > tpl.open && off < tpl.close {
			perRow[n] = true
			rowCount++
		}
	}

	// The row template must contribute at least one bindvar, otherwise there
	// is nothing to repeat per row.
	if rowCount == 0 || len(bindvars) != len(names) {
		return chunkTemplate{}, ErrUnsupportedChunkQuery
	}

	return chunkTemplate{
		bound:  bound,
		names:  names,
		open:   tpl.open,
		close:  tpl.close,
		perRow: perRow,
	}, nil
}

func skipToLineEnd(s string, i int) int {
	if j := strings.IndexByte(s[i:], '\n'); j < 0 {
		return len(s)
	} else {
		return i + j + 1
	}
}

func skipSingleQuoted(s string, i int) int {
	i++
	for i < len(s) {
		switch s[i] {
		case '\\':
			i += 2
			continue
		case '\'':
			if i+1 < len(s) && s[i+1] == '\'' {
				i += 2
				continue
			}
			return i + 1
		}
		i++
	}
	return i
}

func skipDoubleQuoted(s string, i int) int {
	i++
	for i < len(s) {
		if s[i] == '"' {
			if i+1 < len(s) && s[i+1] == '"' {
				i += 2
				continue
			}
			return i + 1
		}
		i++
	}
	return i
}

func skipDollarQuoted(s string, i int) int {
	tagEnd := i + 1
	for tagEnd < len(s) && tagEnd-i <= 32 && isIdentByte(s[tagEnd]) {
		tagEnd++
	}
	if tagEnd >= len(s) || s[tagEnd] != '$' {
		return i + 1
	}
	tag := s[i : tagEnd+1]
	if j := strings.Index(s[tagEnd+1:], tag); j < 0 {
		return len(s)
	} else {
		return tagEnd + 1 + j + len(tag)
	}
}

// isValuesKeywordAt reports whether s[pos:] begins with a word-boundary
// delimited, case-insensitive VALUES token.
func isValuesKeywordAt(s string, pos int) bool {
	const word = "VALUES"
	if pos+len(word) > len(s) {
		return false
	}
	for k := 0; k < len(word); k++ {
		c := s[pos+k]
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		if c != word[k] {
			return false
		}
	}
	if pos > 0 && isIdentByte(s[pos-1]) {
		return false
	}
	if after := pos + len(word); after < len(s) && isIdentByte(s[after]) {
		return false
	}
	return true
}

// valuesRowParen, given the byte immediately after VALUES, finds the opening
// parenthesis and its matching closing parenthesis.  Parentheses nested in
// function calls such as NOW() are balanced.
func valuesRowParen(s string, from int) (open, close int, ok bool) {
	i := from
	for i < len(s) && isSpaceByte(s[i]) {
		i++
	}
	if i >= len(s) || s[i] != '(' {
		return 0, 0, false
	}
	open = i
	depth := 0
	for ; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return open, i, true
			}
		}
	}
	return 0, 0, false
}

func isSpaceByte(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\r', '\v', '\f':
		return true
	}
	return false
}

func isIdentByte(b byte) bool {
	return b == '_' ||
		(b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9')
}

// perRowCount is the number of bindvars repeated per row.
func (t chunkTemplate) perRowCount() int {
	n := 0
	for _, inside := range t.perRow {
		if inside {
			n++
		}
	}
	return n
}

// bindvarsFor returns the number of bindvars a statement carrying n rows uses.
func (t chunkTemplate) bindvarsFor(n int) int {
	p := t.perRowCount()
	return len(t.names) - p + p*n
}

// build renders and binds the statement for rows [start, end).
//
// rows[r] holds the fully resolved argument list for input row r, in named
// order.  The query is split into a prefix, the row template and a suffix.
// Fixed bindvars (in prefix and suffix) are bound once from the first row at
// their original positions; per-row bindvars are bound once per row.  Fixed
// and per-row bindvars are interleaved in query order so positional drivers
// such as postgres number them correctly.  The rendered question-bound
// statement is rebound to bindType.
func (t chunkTemplate) build(rows [][]interface{}, start, end int, bindType int) (string, []interface{}) {
	out := make([]interface{}, 0, t.bindvarsFor(end-start))

	// Offsets in t.bound of every question mark, in query order.  qIndex maps
	// a question mark's position to its index in t.names (and therefore to the
	// per-element arg index produced by bindAnyArgs).
	offsets := make([]int, 0, len(t.names))
	for off := 0; off < len(t.bound); off++ {
		if t.bound[off] == '?' {
			offsets = append(offsets, off)
		}
	}

	var builder strings.Builder
	builder.Grow(len(t.bound) + (t.close-t.open+2)*(end-start))

	cursor := 0

	// emitFixed writes the fixed (non-row) bindvars whose question marks fall
	// before limit, preserving query order.
	emitFixed := func(limit int) {
		for idx, off := range offsets {
			if t.perRow[idx] || off >= limit {
				continue
			}
			builder.WriteString(t.bound[cursor : off+1])
			cursor = off + 1
			out = append(out, rows[start][idx])
		}
	}

	// Prefix fixed bindvars, up to the opening parenthesis.
	emitFixed(t.open)
	builder.WriteString(t.bound[cursor:t.open])
	cursor = t.open

	// One parenthesised copy of the template inner content per row; copies are
	// separated by ", ".
	for r := start; r < end; r++ {
		if r > start {
			builder.WriteString(", ")
		}
		builder.WriteByte('(')
		rowCursor := t.open + 1
		for idx, off := range offsets {
			if !t.perRow[idx] {
				continue
			}
			builder.WriteString(t.bound[rowCursor : off+1])
			rowCursor = off + 1
			out = append(out, rows[r][idx])
		}
		builder.WriteString(t.bound[rowCursor:t.close])
		builder.WriteByte(')')
	}
	cursor = t.close + 1

	// Suffix fixed bindvars, from the closing parenthesis to the end.
	for idx, off := range offsets {
		if t.perRow[idx] || off <= t.close {
			continue
		}
		builder.WriteString(t.bound[cursor : off+1])
		cursor = off + 1
		out = append(out, rows[start][idx])
	}
	builder.WriteString(t.bound[cursor:])

	return Rebind(bindType, builder.String()), out
}

// sliceValueForChunks rejects non-slice inputs, including []byte.
func sliceValueForChunks(arg interface{}) (reflect.Value, error) {
	v, ok := asSliceForIn(arg)
	if !ok {
		return reflect.Value{}, fmt.Errorf("sqlx: chunked NamedExec requires a slice argument, got %T", arg)
	}
	return v, nil
}

// namedExecChunks contains the chunking logic shared by the package level
// NamedExecChunks and the DB/Tx methods.
func namedExecChunks(e Execer, bindType int, m *reflectx.Mapper, query string, arg interface{}, maxParams int) (ChunkExecResult, error) {
	if maxParams <= 0 {
		return ChunkExecResult{}, fmt.Errorf("sqlx: maxParams must be greater than zero, got %d", maxParams)
	}

	tpl, err := parseChunkTemplate(query)
	if err != nil {
		return ChunkExecResult{}, err
	}

	value, err := sliceValueForChunks(arg)
	if err != nil {
		return ChunkExecResult{}, err
	}

	total := value.Len()
	if total == 0 {
		return ChunkExecResult{}, nil
	}

	perRow := tpl.perRowCount()
	fixed := len(tpl.names) - perRow

	// Even a single row cannot fit the budget.
	if fixed+perRow > maxParams {
		return ChunkExecResult{}, ErrChunkRowTooLarge
	}

	// Largest number of rows that fit: fixed + perRow*rows <= maxParams.
	rowsPerChunk := (maxParams - fixed) / perRow

	start := 0
	for start < total {
		end := start + rowsPerChunk
		if end > total {
			end = total
		}

		failed := ChunkExecResult{
			Executed: ChunkRange{Start: 0, End: start},
			Failed:   ChunkRange{Start: start, End: end},
		}

		rows := make([][]interface{}, 0, end-start)
		for r := start; r < end; r++ {
			args, err := bindAnyArgs(tpl.names, value.Index(r).Interface(), m)
			if err != nil {
				return failed, err
			}
			rows = append(rows, args)
		}

		q, args := tpl.build(rows, 0, len(rows), bindType)
		if _, err := e.Exec(q, args...); err != nil {
			return failed, err
		}

		start = end
	}

	return ChunkExecResult{Executed: ChunkRange{Start: 0, End: total}}, nil
}
