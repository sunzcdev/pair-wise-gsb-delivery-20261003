package astjson

import (
	"github.com/yuin/goldmark/v2/ast"
	"github.com/yuin/goldmark/v2/text"
)

func newDefaultRegistry() *Registry {
	r := NewRegistry()

	r.Register(ast.KindDocument, AdapterFunc(func(_ ast.Node, _ *Context) ([]Property, error) {
		return nil, nil
	}))

	r.Register(ast.KindParagraph, AdapterFunc(func(_ ast.Node, _ *Context) ([]Property, error) {
		return nil, nil
	}))

	r.Register(ast.KindHeading, AdapterFunc(func(node ast.Node, c *Context) ([]Property, error) {
		n := node.(*ast.Heading)
		return []Property{
			Prop("level", c.Int(n.Level)),
			Prop("headingKind", c.String(n.HeadingKind.String())),
		}, nil
	}))

	r.Register(ast.KindThematicBreak, AdapterFunc(func(_ ast.Node, _ *Context) ([]Property, error) {
		return nil, nil
	}))

	r.Register(ast.KindCodeBlock, AdapterFunc(func(node ast.Node, c *Context) ([]Property, error) {
		n := node.(*ast.CodeBlock)
		props := []Property{
			Prop("codeBlockKind", c.String(n.CodeBlockKind.String())),
			Prop("value", c.LinesValue(n.Value)),
		}
		info := ""
		if !n.Info.IsEmpty() {
			info = n.Info.Value(c.source)
		}
		props = append(props, Prop("info", c.String(info)))
		return props, nil
	}))

	r.Register(ast.KindBlockquote, AdapterFunc(func(_ ast.Node, _ *Context) ([]Property, error) {
		return nil, nil
	}))

	r.Register(ast.KindList, AdapterFunc(func(node ast.Node, c *Context) ([]Property, error) {
		n := node.(*ast.List)
		props := []Property{
			Prop("ordered", c.Bool(n.IsOrdered())),
			Prop("marker", c.String(string(n.Marker))),
			Prop("tight", c.Bool(n.IsTight)),
		}
		if n.IsOrdered() {
			props = append(props, Prop("start", c.Int(n.Start)))
		} else {
			props = append(props, Prop("start", c.Null()))
		}
		return props, nil
	}))

	r.Register(ast.KindListItem, AdapterFunc(func(_ ast.Node, _ *Context) ([]Property, error) {
		return nil, nil
	}))

	r.Register(ast.KindHTMLBlock, AdapterFunc(func(node ast.Node, c *Context) ([]Property, error) {
		n := node.(*ast.HTMLBlock)
		return []Property{
			Prop("htmlBlockKind", c.String(n.HTMLBlockKind.String())),
			Prop("value", c.LinesValue(n.Value)),
		}, nil
	}))

	r.Register(ast.KindLinkReferenceDefinition, AdapterFunc(func(node ast.Node, c *Context) ([]Property, error) {
		n := node.(*ast.LinkReferenceDefinition)
		return []Property{
			Prop("label", c.TextValue(n.Label)),
			Prop("destination", c.TextValue(n.Destination)),
			Prop("title", c.TextValue(n.Title)),
		}, nil
	}))

	r.Register(ast.KindText, AdapterFunc(func(node ast.Node, c *Context) ([]Property, error) {
		n := node.(*ast.Text)
		props := []Property{
			Prop("value", c.TextValue(n.Value)),
		}
		flags := make(jsonArray, 0, 2)
		if n.SoftLineBreak() {
			flags = append(flags, "SoftLineBreak")
		}
		if n.HardLineBreak() {
			flags = append(flags, "HardLineBreak")
		}
		props = append(props, Prop("flags", flags))
		return props, nil
	}))

	r.Register(ast.KindCodeSpan, AdapterFunc(func(node ast.Node, c *Context) ([]Property, error) {
		n := node.(*ast.CodeSpan)
		return []Property{
			Prop("value", c.TextValue(n.Value)),
		}, nil
	}))

	r.Register(ast.KindEmphasis, AdapterFunc(func(_ ast.Node, _ *Context) ([]Property, error) {
		return nil, nil
	}))

	r.Register(ast.KindStrong, AdapterFunc(func(_ ast.Node, _ *Context) ([]Property, error) {
		return nil, nil
	}))

	r.Register(ast.KindLink, AdapterFunc(func(node ast.Node, c *Context) ([]Property, error) {
		n := node.(*ast.Link)
		return linkLikeProps(n.Destination, n.Title, n.Reference, c), nil
	}))

	r.Register(ast.KindImage, AdapterFunc(func(node ast.Node, c *Context) ([]Property, error) {
		n := node.(*ast.Image)
		return linkLikeProps(n.Destination, n.Title, n.Reference, c), nil
	}))

	r.Register(ast.KindAutoLink, AdapterFunc(func(node ast.Node, c *Context) ([]Property, error) {
		n := node.(*ast.AutoLink)
		return []Property{
			Prop("destination", c.TextValue(n.Destination)),
			Prop("label", c.TextValue(n.Label)),
			Prop("text", c.TextValue(n.Text)),
		}, nil
	}))

	r.Register(ast.KindRawHTML, AdapterFunc(func(node ast.Node, c *Context) ([]Property, error) {
		n := node.(*ast.RawHTML)
		return []Property{
			Prop("value", c.TextValue(n.Value)),
		}, nil
	}))

	return r
}

func linkLikeProps(
	destination text.SingleLineValue,
	title text.MultiLineValue,
	reference *ast.ReferenceLink,
	c *Context,
) []Property {
	props := []Property{
		Prop("destination", c.TextValue(destination)),
		Prop("title", c.TextValue(title)),
	}
	if reference == nil {
		props = append(props, Prop("reference", c.Null()))
		return props
	}
	props = append(props, Prop("reference", c.Object(
		c.Field("kind", c.String(reference.ReferenceLinkKind.String())),
		c.Field("value", c.TextValue(reference.Value)),
	)))
	return props
}
