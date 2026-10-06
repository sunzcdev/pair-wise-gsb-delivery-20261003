// Package text implements a renderer that outputs plain text by walking the
// AST directly.
//
// Unlike rendering to HTML and then stripping tags, this renderer keeps block
// boundaries intact: paragraphs are separated with a blank line, tight list
// items are separated with a single newline, and code blocks keep their
// internal whitespace verbatim.
package text

import (
	"io"

	"github.com/yuin/goldmark/v2/ast"
	"github.com/yuin/goldmark/v2/renderer"
)

// Renderer is a renderer that renders AST nodes as plain text.
type Renderer = renderer.Renderer[io.Writer]

// NodeRenderer renders AST nodes as plain text.
type NodeRenderer = renderer.NodeRenderer[io.Writer]

// NodeRendererDecorator is a decorator that decorates NodeRenderer.
type NodeRendererDecorator = renderer.NodeRendererDecorator[io.Writer]

// NodeRendererFunc adapts f into a NodeRenderer that renders AST nodes as plain text.
func NodeRendererFunc(f func(w io.Writer, source []byte,
	n ast.Node, entering bool, rc renderer.Context) (ast.WalkStatus, error)) NodeRenderer {
	return renderer.NodeRendererFunc(f)
}

// Extension is an extension that extends plain text based renderers.
type Extension = renderer.Extension[Config]

// Option is a functional option for plain text based renderers.
type Option = renderer.Option[Config]

// WithNodeRenderers sets node renderers for the given node kinds.
//
// Node kinds without a registered renderer are rendered by walking their
// children, so unknown extension nodes (tables, footnotes, ...) contribute
// their text contents instead of being dropped. Register a renderer here to
// customize their output.
func WithNodeRenderers(nodeRenderers map[ast.NodeKind]NodeRenderer) Option {
	return renderer.WithNodeRenderers[io.Writer, Config](nodeRenderers)
}

// WithNodeRenderer sets a node renderer for the given node kind.
//
// See WithNodeRenderers for the default handling of unregistered kinds.
func WithNodeRenderer(kind ast.NodeKind, nodeRenderer NodeRenderer) Option {
	return renderer.WithNodeRenderer[io.Writer, Config](kind, nodeRenderer)
}

// WithNodeRendererDecorators sets node renderer decorators.
func WithNodeRendererDecorators(decorators map[ast.NodeKind]NodeRendererDecorator) Option {
	return renderer.WithNodeRendererDecorators[io.Writer, Config](decorators)
}

// WithNodeRendererDecorator sets a node renderer decorator for the given node kind.
func WithNodeRendererDecorator(kind ast.NodeKind, decorator NodeRendererDecorator) Option {
	return renderer.WithNodeRendererDecorator[io.Writer, Config](kind, decorator)
}

// WithExtensions adds extensions.
func WithExtensions(extensions ...Extension) Option {
	return renderer.WithExtensions[io.Writer](extensions...)
}

// SoftLineBreakStyle determines how soft line breaks (newlines in the source
// that do not end a block) are rendered.
type SoftLineBreakStyle int

const (
	// SoftLineBreakSpace renders soft line breaks as a single space.
	// This is the default and matches how the line break is treated as
	// inline whitespace by Markdown.
	SoftLineBreakSpace SoftLineBreakStyle = iota

	// SoftLineBreakNewline renders soft line breaks as a newline.
	SoftLineBreakNewline
)

// RawHTMLPolicy determines how raw HTML (both HTML blocks and inline raw HTML)
// is handled. This renderer never parses HTML.
type RawHTMLPolicy int

const (
	// RawHTMLDrop drops raw HTML nodes entirely. This is the default.
	RawHTMLDrop RawHTMLPolicy = iota

	// RawHTMLKeep emits the raw HTML source bytes verbatim. No HTML parsing
	// or tag stripping is performed; this is an explicit pass-through.
	RawHTMLKeep
)

// A Config struct has configurations for the plain text renderer.
type Config struct {
	renderer.Config[io.Writer, Config]

	// SoftLineBreak controls how soft line breaks are rendered.
	// The default is SoftLineBreakSpace.
	SoftLineBreak SoftLineBreakStyle

	// ImageURLs appends the destination of an image after its alternative
	// text (e.g. "alt (https://example.com/a.png)"). The default is false:
	// only the alternative text is rendered.
	ImageURLs bool

	// LinkURLs appends the destination of a link after its label
	// (e.g. "label (https://example.com)"). The default is false: only the
	// link label is rendered.
	LinkURLs bool

	// RawHTML controls how raw HTML blocks and inline raw HTML are handled.
	// The default is RawHTMLDrop.
	RawHTML RawHTMLPolicy
}

// Default returns a Config with default values.
func (c Config) Default() Config {
	return Config{
		SoftLineBreak: SoftLineBreakSpace,
		ImageURLs:     false,
		LinkURLs:      false,
		RawHTML:       RawHTMLDrop,
	}
}

// WithSoftLineBreakNewline is a functional option that renders soft line
// breaks as newlines instead of spaces.
func WithSoftLineBreakNewline() Option {
	return renderer.NewOptionFunc(func(c *Config) {
		c.SoftLineBreak = SoftLineBreakNewline
	})
}

// WithImageURLs is a functional option that appends image destinations after
// their alternative text.
func WithImageURLs() Option {
	return renderer.NewOptionFunc(func(c *Config) {
		c.ImageURLs = true
	})
}

// WithLinkURLs is a functional option that appends link destinations after
// their labels.
func WithLinkURLs() Option {
	return renderer.NewOptionFunc(func(c *Config) {
		c.LinkURLs = true
	})
}

// WithRawHTML is a functional option that emits raw HTML verbatim instead of
// dropping it. The raw bytes are passed through as-is; no HTML parsing is
// performed.
func WithRawHTML() Option {
	return renderer.NewOptionFunc(func(c *Config) {
		c.RawHTML = RawHTMLKeep
	})
}
