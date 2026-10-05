package parser

import (
	"github.com/yuin/goldmark/v2/ast"
	"github.com/yuin/goldmark/v2/text"
	"github.com/yuin/goldmark/v2/util"
)

// caretNode is the AST node produced by the minimal caret extensions.
type caretNode struct {
	ast.BaseInline
	kind ast.NodeKind
}

func (n *caretNode) Kind() ast.NodeKind {
	return n.kind
}

func (n *caretNode) Dump([]byte) *ast.NodeDump {
	return ast.NewNodeDump(n, nil)
}

// caretProcessor is a minimal delimiter processor that triggers on '^'.
type caretProcessor struct {
	name string
	kind ast.NodeKind
	min  int
	max  int
}

func (p *caretProcessor) IsDelimiter(b byte) bool { return b == '^' }

func (p *caretProcessor) CanOpenCloser(opener, closer *Delimiter) bool {
	return opener.Char == closer.Char
}

func (p *caretProcessor) OnMatch(int) ast.Node {
	n := &caretNode{kind: p.kind}
	n.Init(n)
	return n
}

func (p *caretProcessor) DelimiterProcessorName() string { return p.name }

// caretParser is a minimal InlineParser driven by a caretProcessor.
type caretParser struct {
	proc *caretProcessor
}

func (p *caretParser) Trigger() []byte { return []byte{'^'} }

func (p *caretParser) Parse(_ ast.Node, block text.Reader, pc Context) ast.Node {
	// The minimum run length is enforced here: a shorter run is declined so
	// that another handler for a longer run (or vice versa) gets a chance.
	line, _ := block.PeekLine()
	if runLength := countRunLength(line, '^'); runLength < p.proc.min ||
		(p.proc.max >= 0 && runLength > p.proc.max) {
		return nil
	}
	return ParseDelimiter(block, 1, p.proc, pc)
}

func (p *caretParser) DelimiterProcessor() DelimiterProcessor { return p.proc }

func (p *caretParser) DelimiterProcessorName() string { return p.proc.name }

func (p *caretParser) DelimiterRunLengths() (minimum, maximum int) {
	return p.proc.min, p.proc.max
}

func caret(name string, priority, min, max int) util.PrioritizedValue[InlineParser] {
	return util.Prioritized[InlineParser](&caretParser{
		proc: &caretProcessor{name: name, kind: ast.NewNodeKind(name), min: min, max: max},
	}, priority)
}

func nodeKindNames(doc ast.Node) []string {
	var kinds []string
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering {
			kinds = append(kinds, n.Kind().String())
		}
		return ast.WalkContinue, nil
	})
	return kinds
}
