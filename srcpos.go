package gojq

import "weak"

// srcRange is a half-open byte range [start, end) in a source query.
// It is used to attribute runtime errors to source locations. The zero value
// marks a range that is not available (for example built-in functions or
// synthetic terms produced during compilation).
type srcRange struct {
	start int
	end   int
}

func (r srcRange) valid() bool {
	return r.start != 0 || r.end != 0
}

// queryPositions stores the source ranges of AST nodes for a single parse.
// It is keyed by node pointers, so ranges never live on AST nodes and do not
// affect comparisons such as reflect.DeepEqual. The whole table is owned by
// the *Query through a weak.Map, which keeps the table alive exactly as long
// as the parsed query is reachable.
type queryPositions struct {
	terms    map[*Term]srcRange
	funcs    map[*Func]srcRange
	funcdefs map[*FuncDef]srcRange
}

var positionsByQuery weak.Map[*Query, *queryPositions]

// positions returns the parser-created position table for a query, if any.
func positions(q *Query) *queryPositions {
	if q == nil {
		return nil
	}
	p, _ := positionsByQuery.Get(q)
	return p
}

func (p *queryPositions) termRange(t *Term) srcRange {
	if p == nil || t == nil {
		return srcRange{}
	}
	return p.terms[t]
}

func (p *queryPositions) funcRange(f *Func) srcRange {
	if p == nil || f == nil {
		return srcRange{}
	}
	return p.funcs[f]
}

func (p *queryPositions) funcDefRange(f *FuncDef) srcRange {
	if p == nil || f == nil {
		return srcRange{}
	}
	return p.funcdefs[f]
}

// pos converts the range into resolved [SourceRange] values in src.
func (r srcRange) pos(filename, src string) SourceRange {
	if !r.valid() {
		return SourceRange{}
	}
	start, end := r.start, r.end
	if start < 0 {
		start = 0
	}
	if end < start || end > len(src) {
		end = start
	}
	return SourceRange{
		Start: positionAt(filename, src, start),
		End:   positionAt(filename, src, end),
	}
}

// positionAt converts a byte offset in src into a 1-based [Position].
func positionAt(filename, src string, offset int) Position {
	line, col := 1, 1
	if offset > len(src) {
		offset = len(src)
	}
	for i := range offset {
		if src[i] == '\n' {
			line++
			col = 1
		} else {
			col++
		}
	}
	return Position{Filename: filename, Offset: offset, Line: line, Column: col}
}
