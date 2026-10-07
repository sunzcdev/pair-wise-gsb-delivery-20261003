package participle

import (
	"reflect"

	"github.com/alecthomas/participle/v2/lexer"
)

// SourceRange is a half-open byte range [StartOffset, EndOffset) over the
// original parser input.
//
// For productions that match zero tokens, StartOffset == EndOffset and the
// range is positioned at the point at which the production matched.
type SourceRange struct {
	// Start is the position of the first token consumed by the production,
	// or the match point for a zero-width production.
	Start lexer.Position
	// End is the position immediately after the last token consumed by the
	// production, or the match point for a zero-width production.
	End lexer.Position
	// StartOffset and EndOffset are byte offsets relative to the original
	// input; the slice input[StartOffset:EndOffset] reproduces the matched
	// source byte for byte.
	StartOffset int
	EndOffset   int
}

// ZeroWidth reports whether the production matched without consuming tokens.
func (r SourceRange) ZeroWidth() bool {
	return r.StartOffset == r.EndOffset
}

// SourceRecord captures the provenance of a single successful production.
//
// Records are only published for productions that are part of the accepted
// parse; productions attempted and rejected during backtracking publish no
// records.
type SourceRecord struct {
	Range SourceRange
	// RawStart and RawEnd are indices into PeekingLexer.RawTokens(), in the
	// half-open [RawStart, RawEnd) convention. Elided tokens between these
	// bounds are the comments and whitespace belonging to the production.
	RawStart lexer.RawCursor
	RawEnd   lexer.RawCursor
}

// SourceView is an optional, opt-in view over a single parse that preserves
// the parts of the input discarded by the AST: comments, whitespace and any
// tokens hidden with Elide().
//
// Obtain a SourceView with TrackSources(), pass the returned ParseOption to
// one of the Parse* methods, and afterwards inspect records with Records(),
// Record(), Elided() and Text().
//
// Source positions are always taken from the lexer and never recovered via
// string search, so they remain correct when Mapper functions change token
// values (eg. Unquote) and when the same text occurs multiple times.
type SourceView struct {
	source    string
	hasSource bool
	tokens    []lexer.Token
	elide     map[lexer.TokenType]bool
	records   []SourceRecord
	byNode    map[any]int
}

// Records returns the published records in publication order.
//
// Productions are recorded depth-first as they complete, so enclosing
// productions follow their children.
func (v *SourceView) Records() []SourceRecord {
	out := make([]SourceRecord, len(v.records))
	copy(out, v.records)
	return out
}

// Record returns the source record for the given AST node, or nil if no
// successful production corresponds to it.
//
// The node argument is an AST value exactly as stored in the grammar: the
// root grammar pointer, a pointer-valued field, or a pointer to a
// struct-valued field. Productions stored as values inside interface fields
// (union members declared as value types) can be located via Records().
func (v *SourceView) Record(node any) *SourceRecord {
	if node == nil || v == nil {
		return nil
	}
	if idx, ok := v.byNode[node]; ok {
		rec := v.records[idx]
		return &rec
	}
	return nil
}

// Elided returns the hidden tokens (comments, whitespace or tokens passed to
// Elide()) contained in the given record, in source order.
//
// Tokens lexed but dropped by the lexer itself (eg. rules with
// lower-cased names in the stateful lexer) are not part of the token stream
// and therefore cannot be returned here.
func (v *SourceView) Elided(record *SourceRecord) []lexer.Token {
	if record == nil {
		return nil
	}
	out := []lexer.Token{}
	for _, token := range v.tokens[record.RawStart:record.RawEnd] {
		if v.elide[token.Type] && token.Pos.Offset < record.Range.EndOffset {
			out = append(out, token)
		}
	}
	return out
}

// Text reproduces the original source matched by the given record.
//
// The returned slice is always input[StartOffset:EndOffset], so it
// corresponds to the original input byte for byte, including comments,
// whitespace and multi-byte characters. It returns nil if the SourceView
// has no associated source text (eg. ParseFromLexer) or for a nil record.
func (v *SourceView) Text(record *SourceRecord) []byte {
	if record == nil || !v.hasSource {
		return nil
	}
	return []byte(v.source[record.Range.StartOffset:record.Range.EndOffset])
}

// SetSourceText associates the original input text with the view.
//
// This is only required when parsing via ParseFromLexer; the ParseString,
// ParseBytes and Parse methods set it automatically.
func (v *SourceView) SetSourceText(source string) {
	v.source = source
	v.hasSource = true
}

// TrackSources returns a ParseOption that records source provenance in the
// returned SourceView.
//
// The view starts empty and is populated after a successful parse.
func TrackSources() (ParseOption, *SourceView) {
	view := &SourceView{
		elide:  map[lexer.TokenType]bool{},
		byNode: map[any]int{},
	}
	option := func(ctx *parseContext) {
		ctx.sources = &sourceCollector{
			view:   view,
			byNode: map[any]int{},
		}
	}
	return option, view
}

// sourceEnabled reports whether the options include TrackSources.
func sourceEnabled(options []ParseOption) bool {
	var probe parseContext
	for _, option := range options {
		option(&probe)
	}
	return probe.sources != nil
}

// attach binds the finalised token stream and original source text.
func (c *sourceCollector) attach(tokens []lexer.Token, isElided func(lexer.TokenType) bool, source string, hasSource bool) {
	c.view.tokens = tokens
	c.view.source = source
	c.view.hasSource = hasSource
	for _, token := range tokens {
		if isElided(token.Type) {
			c.view.elide[token.Type] = true
		}
	}
}

// finish publishes collected records to the public view.
func (c *sourceCollector) finish() {
	c.view.records = append([]SourceRecord(nil), c.records...)
	c.view.byNode = copyNodeMap(c.byNode)
}

// sourceCollector collects records for a single parse.
//
// Lookahead branches share the collector but take a checkpoint before they
// start; a rejected branch rolls back to its checkpoint, so records from
// failed backtracking are never published.
type sourceCollector struct {
	view    *SourceView
	records []SourceRecord
	byNode  map[any]int
}

// sourceCheckpoint is a save point used to roll back rejected branches.
type sourceCheckpoint struct {
	records int
	nodes   map[any]int
}

func (c *sourceCollector) checkpoint() sourceCheckpoint {
	return sourceCheckpoint{records: len(c.records), nodes: copyNodeMap(c.byNode)}
}

// rollback drops records and associations created after cp.
func (c *sourceCollector) rollback(cp sourceCheckpoint) {
	c.records = c.records[:cp.records]
	c.byNode = cp.nodes
}

func copyNodeMap(in map[any]int) map[any]int {
	out := make(map[any]int, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// publish records a successful struct production.
//
// rootProduction keeps leading elided tokens (comments/whitespace before the
// first real token) in the record, so the outermost production spans the
// complete input; nested productions attribute that trivia to their parent.
func (c *sourceCollector) publish(ctx *parseContext, value reflect.Value, rawStart, rawEnd lexer.RawCursor, rootProduction bool) {
	tokens := ctx.RawTokens()
	eof := lexer.RawCursor(len(tokens) - 1)
	first := rawStart
	last := rawEnd // exclusive index one past the final consumed real token.
	endRaw := rawEnd
	textEndRaw := rawEnd
	if rootProduction {
		// The outermost production spans trailing comments and whitespace up
		// to EOF.
		for endRaw <= eof && !tokens[endRaw].EOF() {
			endRaw++
		}
	}
	if !rootProduction {
		// Trim leading and trailing elided tokens so trivia adjacent to a
		// child production stays with its enclosing production.
		for first < last && ctx.IsElided(tokens[first].Type) {
			first++
		}
		if first == last {
			// Zero-width production at its match point.
			first = rawStart
			last = rawStart
		} else {
			for last > first && ctx.IsElided(tokens[last-1].Type) {
				last--
			}
			// Text ends where trailing trivia begins...
			textEndRaw = last
			// Boundary at the first raw token after the final consumed real
			// token. This skips intervening trivia using positions, not
			// token values, so it is mapper-safe.
			endRaw = last
			for endRaw <= eof && ctx.IsElided(tokens[endRaw].Type) {
				endRaw++
			}
			if endRaw > eof {
				endRaw = eof
			}
		}
	}
	startPos := tokens[first].Pos
	startOffset := startPos.Offset
	var endPos lexer.Position
	var endOffset int
	if rootProduction {
		endPos = tokens[endRaw].Pos
		endOffset = endPos.Offset
	} else {
		endPos = tokens[textEndRaw].Pos
		endOffset = endPos.Offset
	}

	index := len(c.records)
	c.records = append(c.records, SourceRecord{
		Range: SourceRange{
			Start:       startPos,
			End:         endPos,
			StartOffset: startOffset,
			EndOffset:   endOffset,
		},
		RawStart: first,
		RawEnd:   endRaw,
	})
	if key, ok := nodeIdentity(value); ok {
		c.byNode[key] = index
	}
}

// link makes the record previously keyed by src also discoverable through
// dst, accounting for a struct value being copied into its final AST field.
func (c *sourceCollector) link(dst, src reflect.Value) {
	srcKey, ok := nodeIdentity(src)
	if !ok {
		return
	}
	index, ok := c.byNode[srcKey]
	if !ok {
		return
	}
	if dstKey, ok := nodeIdentity(dst); ok {
		c.setNode(dstKey, index)
	}
}

func (c *sourceCollector) indexOf(key any) (int, bool) {
	index, ok := c.byNode[key]
	return index, ok
}

func (c *sourceCollector) setNode(key any, index int) {
	if _, exists := c.byNode[key]; !exists {
		c.byNode[key] = index
	}
}

// nodeIdentity returns a stable identity for an AST struct production:
// the typed pointer to its backing storage.
func nodeIdentity(v reflect.Value) (any, bool) {
	for v.Kind() == reflect.Interface {
		if v.IsNil() {
			return nil, false
		}
		v = v.Elem()
	}
	switch v.Kind() {
	case reflect.Ptr:
		if v.IsNil() || v.Elem().Kind() != reflect.Struct {
			return nil, false
		}
		return v.Interface(), true
	case reflect.Struct:
		if !v.CanAddr() {
			return nil, false
		}
		return v.Addr().Interface(), true
	}
	return nil, false
}
