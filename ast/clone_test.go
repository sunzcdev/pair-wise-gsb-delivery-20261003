package ast_test

import (
	"testing"

	"github.com/yuin/goldmark/v2/ast"
	extensionast "github.com/yuin/goldmark/v2/extension/ast"
	"github.com/yuin/goldmark/v2/text"
)

func TestCloneNodeIsIndependent(t *testing.T) {
	source := []byte("# Original\n")
	heading := ast.NewHeading(1, ast.HeadingKindATX)
	heading.SetSource([]text.Segment{{Start: 2, Stop: 10}})
	heading.SetAttribute("class", text.NewMultiLineValueFromString("original", nil))
	heading.AppendChild(ast.NewText(text.NewSingleLineValue(source[2:10], nil)))

	clone := ast.CloneNode(heading).(*ast.Heading)
	clone.Level = 2
	clone.SetAttribute("class", text.NewMultiLineValueFromString("cloned", nil))
	clone.AppendChild(ast.NewText(text.NewSingleLineValueFromString(" added", nil)))

	if heading.Level != 1 {
		t.Fatalf("original level changed to %d", heading.Level)
	}
	if class, _ := heading.Attribute("class"); class.Str(source) != "original" {
		t.Fatalf("original attribute changed to %q", class.Str(source))
	}
	if heading.ChildCount() != 1 {
		t.Fatalf("original child count changed to %d", heading.ChildCount())
	}
	if clone.Parent() != nil || heading.Parent() != nil {
		t.Fatal("clone or original has an unexpected parent")
	}
	if clone.FirstChild() == heading.FirstChild() {
		t.Fatal("clone shares a child node")
	}

	clone.SetSource(nil)
	if len(heading.Source()) != 1 {
		t.Fatal("original source segments were modified through the clone")
	}
}

func TestCloneNodeCopiesTerminalAndExtensionData(t *testing.T) {
	source := []byte("first\nsecond\n")
	code := ast.NewCodeBlock(
		ast.CodeBlockKindFenced,
		text.NewLines([]text.Segment{{Start: 0, Stop: 6, ForceNewline: true}, {Start: 6, Stop: 12}}),
		ast.WithCodeBlockInfo(text.NewSingleLineValueFromString("go", nil)),
	)
	clonedCode := ast.CloneNode(code).(*ast.CodeBlock)
	segments := clonedCode.Value.Segments()
	segments[0].ForceNewline = false
	if !code.Value.Segments()[0].ForceNewline {
		t.Fatal("original code block segments were modified through the clone")
	}
	if language, _ := clonedCode.Language(source); language != "go" {
		t.Fatalf("cloned language = %q, want go", language)
	}

	footnote := extensionast.NewFootnoteReference(text.NewSingleLineValueFromString("note", nil))
	footnote.Index = 7
	footnote.RefIndex = 2
	clonedFootnote := ast.CloneNode(footnote).(*extensionast.FootnoteReference)
	clonedFootnote.Index = 8
	if footnote.Index != 7 || clonedFootnote.RefIndex != 2 {
		t.Fatalf("footnote data was not copied correctly: original=%d clone=%d", footnote.Index, clonedFootnote.Index)
	}
}
