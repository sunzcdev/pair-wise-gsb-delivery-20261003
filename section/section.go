// Package section extracts one independently renderable chapter from a Markdown AST.
package section

import (
	"bytes"
	"errors"
	"regexp"
	"slices"

	"github.com/yuin/goldmark/v2/ast"
	extast "github.com/yuin/goldmark/v2/extension/ast"
	"github.com/yuin/goldmark/v2/util"
)

var (
	// ErrInvalidHeading reports that Extract received a node that is not a top-level ast.Heading.
	ErrInvalidHeading = errors.New("section: heading must be an ast.Heading")

	// ErrHeadingNotInDocument reports that the heading does not belong to the supplied document.
	ErrHeadingNotInDocument = errors.New("section: heading does not belong to document")
)

// DefinitionKind identifies the Markdown definition type referenced by a label.
type DefinitionKind int

const (
	// LinkDefinition is a CommonMark link reference definition.
	LinkDefinition DefinitionKind = iota + 1

	// FootnoteDefinition is a PHP Markdown Extra footnote definition.
	FootnoteDefinition
)

func (k DefinitionKind) String() string {
	switch k {
	case LinkDefinition:
		return "link"
	case FootnoteDefinition:
		return "footnote"
	default:
		return "unknown"
	}
}

// MissingDefinition describes a reference whose definition is absent from the source document.
type MissingDefinition struct {
	Kind  DefinitionKind
	Label string
}

// Result is an independently owned chapter AST.
type Result struct {
	// Document is the root of the extracted tree. It never shares node links with the input tree.
	Document *ast.Document

	// MissingDefinitions contains references that could not be resolved in the source document.
	MissingDefinitions []MissingDefinition
}

// Extract returns the chapter beginning at heading and ending before the next top-level heading
// with the same or a higher level.
//
// The returned AST is a deep copy. Appending, removing, or relinking nodes in the result cannot
// change document or heading. Node values that refer to source positions still index source, so
// callers must keep source alive, unmodified, and must not mutate it while rendering or inspecting
// either the original or extracted AST. Owned node values and node links are copied; source bytes
// are intentionally shared read only.
func Extract(document *ast.Document, heading *ast.Heading, source []byte) (*Result, error) {
	if document == nil {
		return nil, errors.New("section: document must not be nil")
	}
	if heading == nil {
		return nil, ErrInvalidHeading
	}
	if heading.Parent() != document {
		if heading.OwnerDocument() != document {
			return nil, ErrHeadingNotInDocument
		}
		return nil, ErrInvalidHeading
	}

	extractor := newExtractor(document, source)
	nodes := extractor.sectionNodes(heading)
	result := ast.NewDocument()
	result.SetBlankPreviousLines(document.HasBlankPreviousLines())
	copyDocumentMetadata(result, document)

	for _, node := range nodes {
		result.AppendChild(cloneNode(node))
	}

	referenced := extractor.collectReferences(nodes)
	required := extractor.requiredDefinitions(&referenced)
	for _, definition := range required {
		result.AppendChild(cloneNode(definition))
	}

	return &Result{
		Document:           result,
		MissingDefinitions: extractor.missingDefinitions(&referenced),
	}, nil
}

// ExtractPreface returns top-level content before the first top-level heading.
//
// Source ownership and AST independence are the same as described by Extract.
func ExtractPreface(document *ast.Document, source []byte) (*Result, error) {
	if document == nil {
		return nil, errors.New("section: document must not be nil")
	}

	extractor := newExtractor(document, source)
	nodes := prefaceNodes(document)
	result := ast.NewDocument()
	result.SetBlankPreviousLines(document.HasBlankPreviousLines())
	copyDocumentMetadata(result, document)

	for _, node := range nodes {
		result.AppendChild(cloneNode(node))
	}

	referenced := extractor.collectReferences(nodes)
	for _, definition := range extractor.requiredDefinitions(&referenced) {
		result.AppendChild(cloneNode(definition))
	}

	return &Result{
		Document:           result,
		MissingDefinitions: extractor.missingDefinitions(&referenced),
	}, nil
}

type references struct {
	links     map[string]struct{}
	footnotes map[string]struct{}
	linkOrder []string
	noteOrder []string
}

func newReferences() references {
	return references{
		links:     map[string]struct{}{},
		footnotes: map[string]struct{}{},
	}
}

func (r *references) addLink(label string) {
	if _, ok := r.links[label]; !ok {
		r.links[label] = struct{}{}
		r.linkOrder = append(r.linkOrder, label)
	}
}

func (r *references) addFootnote(label string) {
	if _, ok := r.footnotes[label]; !ok {
		r.footnotes[label] = struct{}{}
		r.noteOrder = append(r.noteOrder, label)
	}
}

type extractor struct {
	source       []byte
	linkDefs     map[string]*ast.LinkReferenceDefinition
	footnoteDefs map[string]*extast.FootnoteDefinition
}

func newExtractor(document *ast.Document, source []byte) *extractor {
	e := &extractor{
		source:       source,
		linkDefs:     map[string]*ast.LinkReferenceDefinition{},
		footnoteDefs: map[string]*extast.FootnoteDefinition{},
	}

	_ = ast.Walk(document, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch node := node.(type) {
		case *ast.LinkReferenceDefinition:
			label := util.ToLinkReference(node.Label.Bytes(source))
			if _, ok := e.linkDefs[label]; !ok {
				e.linkDefs[label] = node
			}
		case *extast.FootnoteDefinition:
			label := node.Label.Str(source)
			if _, ok := e.footnoteDefs[label]; !ok {
				e.footnoteDefs[label] = node
			}
		}
		return ast.WalkContinue, nil
	})
	return e
}

func (e *extractor) sectionNodes(heading *ast.Heading) []ast.Node {
	var nodes []ast.Node
	inSection := false
	for child := range heading.Parent().Children() {
		if !inSection {
			if child == heading {
				inSection = true
			}
		} else if next, ok := child.(*ast.Heading); ok && next.Level <= heading.Level {
			break
		}
		if inSection && !isDefinition(child) {
			nodes = append(nodes, child)
		}
	}
	return nodes
}

func prefaceNodes(document *ast.Document) []ast.Node {
	var nodes []ast.Node
	for child := range document.Children() {
		if _, ok := child.(*ast.Heading); ok {
			break
		}
		if !isDefinition(child) {
			nodes = append(nodes, child)
		}
	}
	return nodes
}

func isDefinition(node ast.Node) bool {
	switch node.(type) {
	case *ast.LinkReferenceDefinition, *extast.FootnoteDefinition:
		return true
	default:
		return false
	}
}

func (e *extractor) collectReferences(roots []ast.Node) references {
	return e.collectReferencesWith(roots, map[string]struct{}{}, map[string]struct{}{})
}

func (e *extractor) collectReferencesWith(
	roots []ast.Node,
	hasUnresolvedLink map[string]struct{},
	hasUnresolvedFootnote map[string]struct{},
) references {
	refs := newReferences()
	var raw bytes.Buffer

	for _, root := range roots {
		_ = ast.Walk(root, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
			if !entering {
				return ast.WalkContinue, nil
			}
			switch n := node.(type) {
			case *ast.Link:
				if n.Reference != nil {
					refs.addLink(util.ToLinkReference(n.Reference.Value.Bytes(e.source)))
				}
			case *ast.Image:
				if n.Reference != nil {
					refs.addLink(util.ToLinkReference(n.Reference.Value.Bytes(e.source)))
				}
			case *extast.FootnoteReference:
				refs.addFootnote(n.Label.Str(e.source))
			case *ast.Text:
				raw.WriteString(n.Value.Str(e.source))
				if n.SoftLineBreak() || n.HardLineBreak() {
					raw.WriteByte('\n')
				}
			}
			return ast.WalkContinue, nil
		})
	}

	rawText := raw.String()
	e.collectUnresolvedFootnoteReferences(&refs, rawText, hasUnresolvedFootnote)
	e.collectUnresolvedLinkReferences(&refs, rawText, hasUnresolvedLink)
	return refs
}

var (
	unresolvedFootnotePattern = regexp.MustCompile(`\[\^([^\]]+)\]`)
	unresolvedLinkPattern     = regexp.MustCompile(`\[([^\]]*)\]\[([^\]]*)\]`)
)

func (e *extractor) collectUnresolvedFootnoteReferences(
	refs *references,
	rawText string,
	seen map[string]struct{},
) {
	for _, match := range unresolvedFootnotePattern.FindAllStringSubmatch(rawText, -1) {
		label := match[1]
		if label != "" {
			if _, ok := seen[label]; ok {
				continue
			}
			seen[label] = struct{}{}
			refs.addFootnote(label)
		}
	}
}

func (e *extractor) collectUnresolvedLinkReferences(
	refs *references,
	rawText string,
	seen map[string]struct{},
) {
	for _, match := range unresolvedLinkPattern.FindAllStringSubmatch(rawText, -1) {
		label := match[2]
		if label == "" {
			label = match[1]
		}
		if label != "" && !bytes.HasPrefix([]byte(label), []byte("^")) {
			normalized := util.ToLinkReference([]byte(label))
			if _, ok := seen[normalized]; ok {
				continue
			}
			seen[normalized] = struct{}{}
			refs.addLink(normalized)
		}
	}
}

func (e *extractor) requiredDefinitions(refs *references) []ast.Node {
	queue := slices.Clone(refs.noteOrder)
	unresolvedLinks := map[string]struct{}{}
	unresolvedFootnotes := map[string]struct{}{}

	var definitions []ast.Node

	for len(queue) > 0 {
		label := queue[0]
		queue = queue[1:]
		definition, ok := e.footnoteDefs[label]
		if !ok {
			continue
		}
		definitions = append(definitions, definition)

		nested := e.collectReferencesWith(
			[]ast.Node{definition},
			unresolvedLinks,
			unresolvedFootnotes,
		)
		for _, label := range nested.linkOrder {
			refs.addLink(label)
		}
		for _, nestedLabel := range nested.noteOrder {
			if _, seen := refs.footnotes[nestedLabel]; !seen {
				refs.addFootnote(nestedLabel)
				queue = append(queue, nestedLabel)
			}
		}
	}
	for label := range refs.links {
		if definition, ok := e.linkDefs[label]; ok {
			definitions = append(definitions, definition)
		}
	}
	sortDefinitionsByPosition(definitions)
	return definitions
}

func sortDefinitionsByPosition(definitions []ast.Node) {
	slices.SortStableFunc(definitions, func(a, b ast.Node) int {
		return a.Pos() - b.Pos()
	})
}

func (e *extractor) missingDefinitions(refs *references) []MissingDefinition {
	missing := make([]MissingDefinition, 0)
	seen := map[MissingDefinition]struct{}{}
	add := func(kind DefinitionKind, label string) {
		entry := MissingDefinition{Kind: kind, Label: label}
		if _, ok := seen[entry]; ok {
			return
		}
		seen[entry] = struct{}{}
		missing = append(missing, entry)
	}

	for _, label := range refs.linkOrder {
		if _, ok := e.linkDefs[label]; !ok {
			add(LinkDefinition, label)
		}
	}
	for _, label := range refs.noteOrder {
		if _, ok := e.footnoteDefs[label]; !ok {
			add(FootnoteDefinition, label)
		}
	}
	return missing
}
