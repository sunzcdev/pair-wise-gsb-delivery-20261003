package ast

import gast "github.com/yuin/goldmark/v2/ast"

func init() {
	gast.RegisterCanonicalNodeAdapter(KindTable, emptyCanonicalAdapter)
	gast.RegisterCanonicalNodeAdapter(KindTableHeader, emptyCanonicalAdapter)
	gast.RegisterCanonicalNodeAdapter(KindTableBody, emptyCanonicalAdapter)
	gast.RegisterCanonicalNodeAdapter(KindTableRow, emptyCanonicalAdapter)
	gast.RegisterCanonicalNodeAdapter(KindTableCell, tableCellCanonicalAdapter)
	gast.RegisterCanonicalNodeAdapter(KindStrikethrough, emptyCanonicalAdapter)
	gast.RegisterCanonicalNodeAdapter(KindDefinitionList, emptyCanonicalAdapter)
	gast.RegisterCanonicalNodeAdapter(KindDefinitionTerm, emptyCanonicalAdapter)
	gast.RegisterCanonicalNodeAdapter(KindDefinitionDescription, definitionDescriptionCanonicalAdapter)
	gast.RegisterCanonicalNodeAdapter(KindFootnoteDefinition, footnoteDefinitionCanonicalAdapter)
	gast.RegisterCanonicalNodeAdapter(KindFootnoteReference, footnoteReferenceCanonicalAdapter)
}

func emptyCanonicalAdapter(_ gast.Node, _ []byte) ([]gast.CanonicalProperty, error) {
	return nil, nil
}

func tableCellCanonicalAdapter(node gast.Node, _ []byte) ([]gast.CanonicalProperty, error) {
	n := node.(*TableCell)
	return []gast.CanonicalProperty{
		{Name: "alignment", Value: n.Alignment.String()},
	}, nil
}

func definitionDescriptionCanonicalAdapter(node gast.Node, _ []byte) ([]gast.CanonicalProperty, error) {
	n := node.(*DefinitionDescription)
	return []gast.CanonicalProperty{
		{Name: "tight", Value: n.IsTight},
	}, nil
}

func footnoteDefinitionCanonicalAdapter(node gast.Node, source []byte) ([]gast.CanonicalProperty, error) {
	n := node.(*FootnoteDefinition)
	return []gast.CanonicalProperty{
		{Name: "label", Value: n.Label.Value(source)},
	}, nil
}

func footnoteReferenceCanonicalAdapter(node gast.Node, source []byte) ([]gast.CanonicalProperty, error) {
	n := node.(*FootnoteReference)
	return []gast.CanonicalProperty{
		{Name: "label", Value: n.Label.Value(source)},
		{Name: "index", Value: n.Index},
		{Name: "refIndex", Value: n.RefIndex},
	}, nil
}
