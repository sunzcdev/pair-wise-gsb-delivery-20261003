package participle

import (
	"fmt"
	"io"
	"reflect"
	"strings"

	"github.com/alecthomas/participle/v2/lexer"
)

type contextFieldSet struct {
	tokens     []lexer.Token
	strct      reflect.Value
	field      structLexerField
	fieldValue []reflect.Value
	sourceFrom int // Slice length (or 0) before this capture was applied.
}

// Context for a single parse.
type parseContext struct {
	lexer.PeekingLexer
	depth             int
	trace             io.Writer
	deepestError      error
	deepestErrorDepth int
	lookahead         int
	caseInsensitive   map[lexer.TokenType]bool
	symbols           map[lexer.TokenType]string
	apply             []*contextFieldSet
	allowTrailing     bool
	sources           *sourceCollector
	sourceText        string
	hasSourceText     bool
	sourceRootDepth   int
	sourceCheckpoint  *sourceCheckpoint
}

func newParseContext(lex *lexer.PeekingLexer, def lexer.Definition, lookahead int, caseInsensitive map[lexer.TokenType]bool) parseContext {
	return parseContext{
		PeekingLexer:    *lex,
		caseInsensitive: caseInsensitive,
		symbols:         lexer.SymbolsByRune(def),
		lookahead:       lookahead,
	}
}

// tokenTypeName returns the symbolic name of the given token type - in the
// same `<lower-symbol>` form used elsewhere for grammar references - or ""
// if the token type has no symbolic name (eg. literal tokens).
func (p *parseContext) tokenTypeName(t lexer.TokenType) string {
	symbol := p.symbols[t]
	if symbol == "" {
		return ""
	}
	return "<" + strings.ToLower(symbol) + ">"
}

func (p *parseContext) DeepestError(err error) error {
	if p.PeekingLexer.Cursor() >= p.deepestErrorDepth {
		return err
	}
	if p.deepestError != nil {
		return p.deepestError
	}
	return err
}

// Defer adds a function to be applied once a branch has been picked.
func (p *parseContext) Defer(tokens []lexer.Token, strct reflect.Value, field structLexerField, fieldValue []reflect.Value) {
	sourceFrom := 0
	if p.sources != nil {
		if f := strct.FieldByIndex(field.Index); f.Kind() == reflect.Slice {
			sourceFrom = f.Len()
		}
	}
	p.apply = append(p.apply, &contextFieldSet{tokens, strct, field, fieldValue, sourceFrom})
}

// ApplyFrom applies deferred functions from the given offset onwards, leaving
// earlier ones queued for the enclosing struct.
func (p *parseContext) ApplyFrom(start int) error {
	for _, apply := range p.apply[start:] {
		if err := setField(apply.tokens, apply.strct, apply.field, apply.fieldValue); err != nil {
			return err
		}
		if p.sources != nil {
			p.linkCapture(apply)
		}
	}
	p.apply = p.apply[:start]
	return nil
}

// linkCapture associates a production capture's source record with the
// backing storage it ends up in within the AST.
func (p *parseContext) linkCapture(apply *contextFieldSet) {
	field := apply.strct.FieldByIndex(apply.field.Index)
	switch field.Kind() {
	case reflect.Ptr:
		if field.IsNil() {
			return
		}
		field = field.Elem()
		if field.Kind() == reflect.Struct {
			for _, value := range apply.fieldValue {
				p.sources.link(field, value)
			}
		}
	case reflect.Slice:
		for i, value := range apply.fieldValue {
			target := field.Index(apply.sourceFrom + i)
			if srcKey, ok := nodeIdentity(value); ok {
				if index, ok := p.sources.indexOf(srcKey); ok {
					if dstKey, ok := nodeIdentity(target); ok {
						p.sources.setNode(dstKey, index)
					}
				}
			} else {
				p.sources.link(target, value)
			}
		}
	default:
		for _, value := range apply.fieldValue {
			p.sources.link(field, value)
		}
	}
}

// Branch accepts the branch as the correct branch.
func (p *parseContext) Accept(branch *parseContext) {
	p.apply = append(p.apply, branch.apply...)
	p.PeekingLexer = branch.PeekingLexer
	p.acceptSourceBranch(branch)
	if branch.deepestErrorDepth >= p.deepestErrorDepth {
		p.deepestErrorDepth = branch.deepestErrorDepth
		p.deepestError = branch.deepestError
	}
}

// Branch starts a new lookahead branch.
func (p *parseContext) Branch() *parseContext {
	branch := &parseContext{}
	*branch = *p
	branch.apply = nil
	if p.sources != nil {
		cp := p.sources.checkpoint()
		branch.sourceCheckpoint = &cp
	}
	return branch
}

// acceptSourceBranch marks a successful branch; its records are retained.
func (p *parseContext) acceptSourceBranch(branch *parseContext) {
	if branch.sourceCheckpoint != nil {
		branch.sourceCheckpoint = nil
	}
}

// rejectSourceBranch rolls back records published by a discarded branch.
func (p *parseContext) rejectSourceBranch(branch *parseContext) {
	if p.sources != nil && branch.sourceCheckpoint != nil {
		p.sources.rollback(*branch.sourceCheckpoint)
		branch.sourceCheckpoint = nil
	}
}

// rollbackToCheckpoint discards records produced by a production that ended
// up not matching.
func (p *parseContext) rollbackToCheckpoint(cp *sourceCheckpoint) {
	if p.sources != nil && cp != nil {
		p.sources.rollback(*cp)
	}
}

func (p *parseContext) MaybeUpdateError(err error) {
	if p.PeekingLexer.Cursor() >= p.deepestErrorDepth {
		p.deepestError = err
		p.deepestErrorDepth = p.PeekingLexer.Cursor()
	}
}

// Stop returns true if parsing should terminate after the given "branch" failed to match.
//
// Additionally, track the deepest error in the branch - the deeper the error, the more useful it usually is.
// It could already be the deepest error in the branch (only if deeper than current parent context deepest),
// or it could be "err", the latest error on the branch (even if same depth; the lexer holds the position).
func (p *parseContext) Stop(err error, branch *parseContext) bool {
	if branch.deepestErrorDepth > p.deepestErrorDepth {
		p.deepestError = branch.deepestError
		p.deepestErrorDepth = branch.deepestErrorDepth
	} else if branch.PeekingLexer.Cursor() >= p.deepestErrorDepth {
		p.deepestError = err
		p.deepestErrorDepth = maxInt(branch.PeekingLexer.Cursor(), branch.deepestErrorDepth)
	}
	if !p.hasInfiniteLookahead() && branch.PeekingLexer.Cursor() > p.PeekingLexer.Cursor()+p.lookahead {
		p.Accept(branch)
		return true
	}
	return false
}

func (p *parseContext) hasInfiniteLookahead() bool { return p.lookahead < 0 }

// finishSources publishes the collected source records to the SourceView.
func (p *parseContext) finishSources() {
	if p.sources != nil {
		p.sources.finish()
	}
}

func (p *parseContext) printTrace(n node) func() {
	if p.trace != nil {
		tok := p.PeekingLexer.Peek()
		fmt.Fprintf(p.trace, "%s%q %s\n", strings.Repeat(" ", p.depth*2), tok, n.GoString())
		p.depth++
		return func() { p.depth-- }
	}
	return func() {}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
