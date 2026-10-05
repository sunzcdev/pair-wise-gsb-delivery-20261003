// Package section extracts one self-contained document beginning at a heading.
package section

import (
	"errors"

	"github.com/yuin/goldmark/v2/ast"
	extensionast "github.com/yuin/goldmark/v2/extension/ast"
	"github.com/yuin/goldmark/v2/text"
	"github.com/yuin/goldmark/v2/util"
)

var (
	// ErrNilSource is returned when source is nil.
	ErrNilSource = errors.New("section: source cannot be nil")

	// ErrNilHeading is returned when heading is nil.
	ErrNilHeading = errors.New("section: heading cannot be nil")

	// ErrHeadingNotInDocument is returned when heading has no owning document.
	ErrHeadingNotInDocument = errors.New("section: heading does not belong to a document")

	// ErrHeadingNotTopLevel is returned when the heading is nested in another block.
	ErrHeadingNotTopLevel = errors.New("section: heading must be a direct child of its document")
)

// DefinitionKind identifies the syntax family of a missing definition.
type DefinitionKind string

const (
	// LinkDefinition is a Markdown link reference definition.
	LinkDefinition DefinitionKind = "link"

	// FootnoteDefinition is a PHP Markdown Extra footnote definition.
	FootnoteDefinition DefinitionKind = "footnote"
)

// MissingDefinition describes a reference whose definition is absent from the
// source document.
type MissingDefinition struct {
	Kind  DefinitionKind
	Label string
}

// Section is an independently linked document containing one heading section
// and only the definitions required by its transitive references.
type Section struct {
	// Document is the root of the extracted AST. It is never the source AST.
	Document *ast.Document

	// Source is the same byte slice passed to Extract. Extracted source-backed
	// nodes share these bytes as read-only positions; callers must keep Source
	// alive for the lifetime of Document and must not modify it.
	Source []byte

	// MissingDefinitions lists references that have no matching definition.
	MissingDefinitions []MissingDefinition
}

// Extract extracts heading and its descendants up to, but not including, the
// next sibling heading with an equal or higher level.
func Extract(source []byte, heading *ast.Heading) (*Section, error) {
	if source == nil {
		return nil, ErrNilSource
	}
	if heading == nil {
		return nil, ErrNilHeading
	}
	if heading.Parent() == nil {
		return nil, ErrHeadingNotInDocument
	}

	document := heading.OwnerDocument()
	if document == nil {
		return nil, ErrHeadingNotInDocument
	}
	if heading.Parent() != document {
		return nil, ErrHeadingNotTopLevel
	}

	sectionEnd := sectionEnd(heading)
	linkDefs, footnoteDefs := indexDefinitions(source, document)
	inSection := nodesBetween(heading, sectionEnd)
	linkDefsForSection := withPreferredDefinitions(definitionsInSection(linkDefs, inSection), linkDefs)
	footnoteDefsForSection := withPreferredDefinitions(definitionsInSection(footnoteDefs, inSection), footnoteDefs)

	result := &Section{
		Document: ast.NewDocument(),
		Source:   source,
	}

	for node := ast.Node(heading); node != sectionEnd; node = node.NextSibling() {
		result.Document.AppendChild(ast.CloneNode(node))
	}

	collector := &dependencyCollector{
		source:         source,
		linkDefs:       linkDefsForSection,
		footnoteDefs:   footnoteDefsForSection,
		inSection:      inSection,
		linkQueued:     map[string]bool{},
		footnoteQueued: map[string]bool{},
	}
	collector.scanRange(heading, sectionEnd)

	for len(collector.work) > 0 {
		dependency := collector.work[0]
		collector.work = collector.work[1:]

		switch dependency.kind {
		case LinkDefinition:
			definition := collector.linkDefs[dependency.label]
			if definition == nil {
				collector.addMissing(LinkDefinition, dependency.label)
				continue
			}
			if !inSection[definition] {
				result.Document.AppendChild(ast.CloneNode(definition))
			}
		case FootnoteDefinition:
			definition := collector.footnoteDefs[dependency.label]
			if definition == nil {
				collector.addMissing(FootnoteDefinition, dependency.label)
				continue
			}
			if !inSection[definition] {
				result.Document.AppendChild(ast.CloneNode(definition))
			}
			collector.scan(definition)
		}
	}

	result.MissingDefinitions = collector.missing
	return result, nil
}

func sectionEnd(heading *ast.Heading) ast.Node {
	for node := heading.NextSibling(); node != nil; node = node.NextSibling() {
		if next, ok := node.(*ast.Heading); ok && next.Level <= heading.Level {
			return node
		}
	}
	return nil
}

func nodesBetween(start, end ast.Node) map[ast.Node]bool {
	nodes := map[ast.Node]bool{}
	for node := start; node != end; node = node.NextSibling() {
		_ = ast.Walk(node, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
			if entering {
				nodes[node] = true
			}
			return ast.WalkContinue, nil
		})
	}
	return nodes
}

func indexDefinitions(
	source []byte, document *ast.Document,
) (map[string]*ast.LinkReferenceDefinition, map[string]*extensionast.FootnoteDefinition) {
	links := map[string]*ast.LinkReferenceDefinition{}
	footnotes := map[string]*extensionast.FootnoteDefinition{}
	_ = ast.Walk(document, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch definition := node.(type) {
		case *ast.LinkReferenceDefinition:
			label := util.ToLinkReference(definition.Label.Bytes(source))
			if links[label] == nil {
				links[label] = definition
			}
		case *extensionast.FootnoteDefinition:
			label := definition.Label.Str(source)
			if footnotes[label] == nil {
				footnotes[label] = definition
			}
		}
		return ast.WalkContinue, nil
	})
	return links, footnotes
}

func definitionsInSection[T ast.Node](definitions map[string]T, inSection map[ast.Node]bool) map[string]T {
	result := map[string]T{}
	for label, definition := range definitions {
		if inSection[definition] {
			result[label] = definition
		}
	}
	return result
}

func withPreferredDefinitions[T ast.Node](preferred, fallback map[string]T) map[string]T {
	result := make(map[string]T, len(fallback))
	for label, definition := range fallback {
		result[label] = definition
	}
	for label, definition := range preferred {
		result[label] = definition
	}
	return result
}

type dependency struct {
	kind  DefinitionKind
	label string
}

type dependencyCollector struct {
	source         []byte
	linkDefs       map[string]*ast.LinkReferenceDefinition
	footnoteDefs   map[string]*extensionast.FootnoteDefinition
	inSection      map[ast.Node]bool
	work           []dependency
	linkQueued     map[string]bool
	footnoteQueued map[string]bool
	missing        []MissingDefinition
	missingSeen    map[MissingDefinition]bool
}

func (c *dependencyCollector) scan(node ast.Node) {
	_ = ast.Walk(node, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}

		switch node := node.(type) {
		case *ast.Link:
			if label, ok := referenceLabel(node.Reference, node, c.source); ok {
				c.queueLink(label)
			}
		case *ast.Image:
			if label, ok := referenceLabel(node.Reference, node, c.source); ok {
				c.queueLink(label)
			}
		case *extensionast.FootnoteReference:
			if label := node.Label.Str(c.source); label != "" {
				c.queueFootnote(label)
			} else {
				c.addMissing(FootnoteDefinition, "")
			}
		}
		return ast.WalkContinue, nil
	})
}

func (c *dependencyCollector) scanRange(start, end ast.Node) {
	for node := start; node != end; node = node.NextSibling() {
		c.scan(node)
	}
}

func (c *dependencyCollector) queueLink(label string) {
	if c.linkQueued[label] {
		return
	}
	c.linkQueued[label] = true
	if c.linkDefs[label] == nil {
		c.addMissing(LinkDefinition, label)
		return
	}
	c.work = append(c.work, dependency{kind: LinkDefinition, label: label})
}

func (c *dependencyCollector) queueFootnote(label string) {
	if c.footnoteQueued[label] {
		return
	}
	c.footnoteQueued[label] = true
	if c.footnoteDefs[label] == nil {
		c.addMissing(FootnoteDefinition, label)
		return
	}
	c.work = append(c.work, dependency{kind: FootnoteDefinition, label: label})
}

func (c *dependencyCollector) addMissing(kind DefinitionKind, label string) {
	missing := MissingDefinition{Kind: kind, Label: label}
	if c.missingSeen == nil {
		c.missingSeen = map[MissingDefinition]bool{}
	}
	if !c.missingSeen[missing] {
		c.missingSeen[missing] = true
		c.missing = append(c.missing, missing)
	}
}

func referenceLabel(reference *ast.ReferenceLink, node ast.Node, source []byte) (string, bool) {
	if reference == nil {
		return "", false
	}
	if !reference.Value.IsEmpty() {
		return util.ToLinkReference(reference.Value.Bytes(source)), true
	}

	start, stop, ok := inlineBounds(node, source)
	if !ok {
		return "", false
	}
	return util.ToLinkReference(source[start:stop]), true
}

func inlineBounds(node ast.Node, source []byte) (int, int, bool) {
	var start, stop int
	found := false
	_ = ast.Walk(node, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}

		var indices []text.Index
		switch node := node.(type) {
		case *ast.Text:
			indices = node.Value.Indices()
		case *ast.CodeSpan:
			indices = node.Value.Indices()
		}
		for _, index := range indices {
			if index.Start < 0 || index.Stop > len(source) || index.Start >= index.Stop {
				continue
			}
			if !found || index.Start < start {
				start = index.Start
			}
			if index.Stop > stop {
				stop = index.Stop
			}
			found = true
		}
		return ast.WalkContinue, nil
	})
	return start, stop, found
}
