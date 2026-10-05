package extension

import (
	"bytes"
	"fmt"
	"io"
	"slices"
	"strconv"

	gast "github.com/yuin/goldmark/v2/ast"
	"github.com/yuin/goldmark/v2/extension/ast"
	"github.com/yuin/goldmark/v2/parser"
	"github.com/yuin/goldmark/v2/renderer"
	"github.com/yuin/goldmark/v2/renderer/html"
	"github.com/yuin/goldmark/v2/text"
	"github.com/yuin/goldmark/v2/util"
)

// {{{ Data

// A FootnoteReference interface represents a footnote reference data object.
type FootnoteReference interface {
	// Label returns the label of the footnote reference.
	Label() []byte

	// Index returns the display index of the referenced footnote definition.
	Index() int

	// RefIndex returns the position of this reference among all references
	// to the same footnote definition (0-based).
	RefIndex() int
}

type footnoteRefInfo struct {
	label    []byte
	index    int
	refIndex int
}

func newFootnoteReferenceFromNode(node *ast.FootnoteReference, src []byte) FootnoteReference {
	return &footnoteRefInfo{
		label:    node.Label.Bytes(src),
		index:    -1,
		refIndex: -1,
	}
}

func (r *footnoteRefInfo) Label() []byte { return r.label }
func (r *footnoteRefInfo) Index() int    { return r.index }
func (r *footnoteRefInfo) RefIndex() int { return r.refIndex }

// A FootnoteDefinition interface represents a footnote definition data object.
type FootnoteDefinition interface {
	// Label returns the label of the footnote definition.
	Label() []byte
}

type footnoteDefInfo struct {
	label []byte
}

func newFootnoteDefinitionFromNode(node *ast.FootnoteDefinition, src []byte) FootnoteDefinition {
	return &footnoteDefInfo{
		label: node.Label.Bytes(src),
	}
}

func (d *footnoteDefInfo) Label() []byte { return d.label }

// Footnotes manages footnote definitions and references during parsing.
type Footnotes interface {
	// AddDefinition registers a footnote definition.
	AddDefinition(def FootnoteDefinition)

	// AddReference registers a footnote reference.
	// It returns false if no matching definition is found for the reference's label.
	AddReference(ref FootnoteReference) bool

	// FindByLabel returns the FootnoteDefinition matching the given label, or nil.
	FindByLabel(label []byte) FootnoteDefinition
}

type defData struct {
	def        FootnoteDefinition
	index      int
	references []int
}

type footnotes struct {
	definitionIndex int
	defsByLabel     map[string]*defData
}

func newFootnotes() *footnotes {
	return &footnotes{
		definitionIndex: 1,
		defsByLabel:     make(map[string]*defData),
	}
}

func (f *footnotes) AddDefinition(def FootnoteDefinition) {
	key := util.BytesToReadOnlyString(def.Label())
	if _, exists := f.defsByLabel[key]; exists {
		return
	}
	f.defsByLabel[key] = &defData{def: def, index: -1}
}

func (f *footnotes) AddReference(ref FootnoteReference) bool {
	key := util.BytesToReadOnlyString(ref.Label())
	dd, ok := f.defsByLabel[key]
	if !ok {
		return false
	}
	if dd.index < 0 {
		dd.index = f.definitionIndex
		f.definitionIndex++
	}
	lr := ref.(*footnoteRefInfo)
	lr.index = dd.index
	lr.refIndex = len(dd.references)
	dd.references = append(dd.references, lr.refIndex)
	return true
}

func (f *footnotes) FindByLabel(label []byte) FootnoteDefinition {
	key := util.BytesToReadOnlyString(label)
	dd, ok := f.defsByLabel[key]
	if !ok {
		return nil
	}
	return dd.def
}

var footnoteContextKey = parser.NewContextKey()

// ContextFootnotes returns the Footnotes stored in the given parser.Context.
// If no Footnotes have been created yet, it creates one and stores it.
func ContextFootnotes(pc parser.Context) Footnotes {
	v := pc.ComputeIfAbsent(footnoteContextKey, func() any {
		return newFootnotes()
	})
	return v.(Footnotes)
}

// }}}

// {{{ Renderer context helpers

var (
	footnoteDefsKey     = renderer.NewContextKey()
	footnoteDefsInfoKey = renderer.NewContextKey()
)

type defInfo struct {
	index      int
	references []int
}

// {{{ Sectioned numbering

// sectionDefInfo holds per-section rendering information for one footnote label.
type sectionDefInfo struct {
	label string
	index int

	def        *ast.FootnoteDefinition
	references []*ast.FootnoteReference
}

// sectionRefInfo holds per-section rendering information for one reference node.
type sectionRefInfo struct {
	section  int
	index    int
	refIndex int
	sdef     *sectionDefInfo
}

type footnoteSection struct {
	defs  map[string]*sectionDefInfo
	order []*sectionDefInfo
}

func newFootnoteSection() *footnoteSection {
	return &footnoteSection{defs: make(map[string]*sectionDefInfo)}
}

// footnoteSections is the result of partitioning a document by heading level.
type footnoteSections struct {
	headingLevel int

	defsMap  map[string]*ast.FootnoteDefinition
	sections []*footnoteSection
	refs     map[*ast.FootnoteReference]*sectionRefInfo

	// pending is the index of the section whose endnotes have not been emitted yet.
	pending int
}

func newFootnoteSections(headingLevel int) *footnoteSections {
	sd := &footnoteSections{
		headingLevel: headingLevel,
		defsMap:      make(map[string]*ast.FootnoteDefinition),
		refs:         make(map[*ast.FootnoteReference]*sectionRefInfo),
	}
	sd.sections = append(sd.sections, newFootnoteSection())
	return sd
}

func (sd *footnoteSections) build(node gast.Node, source []byte) {
	current := 0
	_ = gast.Walk(node, func(n gast.Node, entering bool) (gast.WalkStatus, error) {
		if !entering {
			return gast.WalkContinue, nil
		}
		switch nn := n.(type) {
		case *ast.FootnoteDefinition:
			label := nn.Label.Str(source)
			if _, exists := sd.defsMap[label]; !exists {
				sd.defsMap[label] = nn
			}
			return gast.WalkSkipChildren, nil
		case *gast.Heading:
			if nn.Level == sd.headingLevel &&
				nn.Parent() != nil && nn.Parent().Kind() == gast.KindDocument {
				sd.sections = append(sd.sections, newFootnoteSection())
				current++
			}
		case *ast.FootnoteReference:
			label := nn.Label.Str(source)
			section := sd.sections[current]
			sdInfo, exists := section.defs[label]
			if !exists {
				sdInfo = &sectionDefInfo{
					label: label,
					index: len(section.order) + 1,
				}
				section.defs[label] = sdInfo
				section.order = append(section.order, sdInfo)
			}
			refIndex := len(sdInfo.references)
			sdInfo.references = append(sdInfo.references, nn)
			sd.refs[nn] = &sectionRefInfo{
				section:  current,
				index:    sdInfo.index,
				refIndex: refIndex,
				sdef:     sdInfo,
			}
		}
		return gast.WalkContinue, nil
	})
}

var footnoteSectionsKey = renderer.NewContextKey()

// writeFootnoteDefID writes the id of a footnote definition.
// A negative section keeps the global id scheme; a non-negative section uses
// the section-qualified scheme so the same label rendered in two chapters
// never collides.
func writeFootnoteDefID(w util.BufWriter, prefix []byte, section, index int) {
	_, _ = w.Write(prefix)
	if section >= 0 {
		_, _ = fmt.Fprintf(w, "fn:%d:%d", section, index)
		return
	}
	_, _ = w.WriteString("fn:")
	_, _ = w.WriteString(strconv.Itoa(index))
}

// writeFootnoteRefID writes the id (without a leading '#') of a footnote
// reference. See writeFootnoteDefID for the section semantics.
func writeFootnoteRefID(w util.BufWriter, prefix []byte, section, index, refIndex int) {
	_, _ = w.Write(prefix)
	_, _ = w.WriteString("fnref")
	if refIndex > 0 {
		_, _ = w.WriteString(strconv.Itoa(refIndex))
	}
	if section >= 0 {
		_, _ = fmt.Fprintf(w, ":%d:%d", section, index)
		return
	}
	_ = w.WriteByte(':')
	_, _ = w.WriteString(strconv.Itoa(index))
}

// }}}

// }}}

// {{{ Block parser

type footnoteBlockParser struct {
}

var defaultFootnoteBlockParser = &footnoteBlockParser{}

func newFootnoteBlockParser() parser.BlockParser {
	return defaultFootnoteBlockParser
}

func (b *footnoteBlockParser) Trigger() []byte {
	return []byte{'['}
}

func (b *footnoteBlockParser) Open(
	_ gast.Node, reader text.Reader, pc parser.Context,
) (gast.Node, parser.State) {
	line, segment := reader.PeekLine()
	pos := pc.BlockOffset()
	if pos < 0 || line[pos] != '[' {
		return nil, parser.NoChildren
	}
	pos++
	if pos > len(line)-1 || line[pos] != '^' {
		return nil, parser.NoChildren
	}
	open := pos + 1
	var closes int
	closure := findLabelClosure(line[pos+1:])
	closes = pos + 1 + closure
	next := closes + 1
	if closure > -1 {
		if next >= len(line) || line[next] != ':' {
			return nil, parser.NoChildren
		}
	} else {
		return nil, parser.NoChildren
	}
	padding := segment.Padding
	labelStart := segment.Start + open - padding
	labelStop := segment.Start + closes - padding
	label := text.NewSingleLineValueFromIndex(text.NewIndex(labelStart, labelStop), reader.Decoder())
	if util.IsBlank(label.Bytes(reader.Source())) {
		return nil, parser.NoChildren
	}
	item := ast.NewFootnoteDefinition(label)

	pos = next + 1 - padding
	if pos >= len(line) {
		reader.Advance(pos)
		return item, parser.NoChildren
	}
	reader.AdvanceAndSetPadding(pos, padding)
	return item, parser.HasChildren
}

func (b *footnoteBlockParser) Continue(
	_ gast.Node, reader text.Reader, _ parser.Context,
) parser.State {
	line, _ := reader.PeekLine()
	if util.IsBlank(line) {
		return parser.Continue | parser.HasChildren
	}
	childpos, padding := util.IndentPosition(line, reader.LineOffset(), 4)
	if childpos < 0 {
		return parser.Close
	}
	reader.AdvanceAndSetPadding(childpos, padding)
	return parser.Continue | parser.HasChildren
}

func (b *footnoteBlockParser) Close(node gast.Node, reader text.Reader, pc parser.Context) {
	fn := node.(*ast.FootnoteDefinition)
	fns := ContextFootnotes(pc)
	fns.AddDefinition(newFootnoteDefinitionFromNode(fn, reader.Source()))
}

func (b *footnoteBlockParser) CanInterruptParagraph() bool {
	return true
}

func (b *footnoteBlockParser) CanAcceptIndentedLine() bool {
	return false
}

// }}}

// {{{ Inline parser

type footnoteParser struct {
}

var defaultFootnoteParser = &footnoteParser{}

func newFootnoteParser() parser.InlineParser {
	return defaultFootnoteParser
}

func (s *footnoteParser) Trigger() []byte {
	return []byte{'!', '['}
}

func (s *footnoteParser) Parse(
	parent gast.Node, block text.Reader, pc parser.Context,
) gast.Node {
	line, segment := block.PeekLine()
	pos := 1
	if len(line) > 0 && line[0] == '!' {
		pos++
	}
	if pos >= len(line) || line[pos] != '^' {
		return nil
	}
	pos++
	if pos >= len(line) {
		return nil
	}
	open := pos
	closure := findLabelClosure(line[pos:])
	if closure < 0 {
		return nil
	}
	closes := pos + closure
	label := text.NewSingleLineValueFromIndex(text.NewIndex(segment.Start+open, segment.Start+closes), block.Decoder())
	block.Advance(closes + 1)

	fns := ContextFootnotes(pc)
	ref := ast.NewFootnoteReference(label)
	lr := newFootnoteReferenceFromNode(ref, block.Source())
	if !fns.AddReference(lr) {
		return nil
	}
	ref.Index = lr.Index()
	ref.RefIndex = lr.RefIndex()

	if line[0] == '!' {
		parent.AppendChild(gast.NewText(text.NewSingleLineValueFromIndex(
			text.NewIndex(segment.Start, segment.Start+1), block.Decoder())))
	}

	return ref
}

func findLabelClosure(bs []byte) int {
	i := 0
	for i < len(bs) {
		c := bs[i]
		if c == '\\' && i+1 < len(bs) && bs[i+1] == ']' {
			i += 2
			continue
		}
		if c == ']' {
			return i
		}
		i++
	}
	return -1
}

// }}}

// {{{ Renderer config and options

type footnoteHTMLRendererConfig struct {
	XHTML bool

	IDPrefix []byte

	IDPrefixFunction func(gast.Node) []byte

	LinkTitle []byte

	BacklinkTitle []byte

	LinkClass []byte

	BacklinkClass []byte

	BacklinkHTML []byte

	// SectionHeadingLevel enables per-section footnote numbering.
	// Zero (the default) keeps document-global numbering. Values 1 through 6
	// partition the document at headings of that level: each section restarts
	// footnote numbers from 1, content before the first heading is treated as
	// a preface section, and endnotes are emitted at each section boundary.
	SectionHeadingLevel int
}

// FootnoteHTMLRendererOption interface is a functional option interface for the extension.
type FootnoteHTMLRendererOption interface {
	applyFootnoteHTMLRendererOption(*footnoteHTMLRendererConfig)
}

type withFootnoteIDPrefix struct {
	value []byte
}

func (o *withFootnoteIDPrefix) applyFootnoteHTMLRendererOption(c *footnoteHTMLRendererConfig) {
	c.IDPrefix = o.value
}

// WithIDPrefix is a functional option that is a prefix for the id attributes generated by footnotes.
func WithIDPrefix[T []byte | string](a T) FootnoteHTMLRendererOption {
	return &withFootnoteIDPrefix{value: []byte(a)}
}

type withFootnoteIDPrefixFunction struct {
	value func(gast.Node) []byte
}

func (o *withFootnoteIDPrefixFunction) applyFootnoteHTMLRendererOption(c *footnoteHTMLRendererConfig) {
	c.IDPrefixFunction = o.value
}

// WithIDPrefixFunction is a functional option that is a prefix for the id attributes
// generated by footnotes.
func WithIDPrefixFunction(a func(gast.Node) []byte) FootnoteHTMLRendererOption {
	return &withFootnoteIDPrefixFunction{value: a}
}

type withFootnoteLinkTitle struct {
	value []byte
}

func (o *withFootnoteLinkTitle) applyFootnoteHTMLRendererOption(c *footnoteHTMLRendererConfig) {
	c.LinkTitle = o.value
}

// WithLinkTitle is a functional option that is an optional title attribute for footnote links.
func WithLinkTitle[T []byte | string](a T) FootnoteHTMLRendererOption {
	return &withFootnoteLinkTitle{value: []byte(a)}
}

type withFootnoteBacklinkTitle struct {
	value []byte
}

func (o *withFootnoteBacklinkTitle) applyFootnoteHTMLRendererOption(c *footnoteHTMLRendererConfig) {
	c.BacklinkTitle = o.value
}

// WithBacklinkTitle is a functional option that is an optional title attribute
// for footnote backlinks.
func WithBacklinkTitle[T []byte | string](a T) FootnoteHTMLRendererOption {
	return &withFootnoteBacklinkTitle{value: []byte(a)}
}

type withFootnoteLinkClass struct {
	value []byte
}

func (o *withFootnoteLinkClass) applyFootnoteHTMLRendererOption(c *footnoteHTMLRendererConfig) {
	c.LinkClass = o.value
}

// WithLinkClass is a functional option that is a class for footnote links.
func WithLinkClass[T []byte | string](a T) FootnoteHTMLRendererOption {
	return &withFootnoteLinkClass{[]byte(a)}
}

type withFootnoteBacklinkClass struct {
	value []byte
}

func (o *withFootnoteBacklinkClass) applyFootnoteHTMLRendererOption(c *footnoteHTMLRendererConfig) {
	c.BacklinkClass = o.value
}

// WithBacklinkClass is a functional option that is a class for footnote backlinks.
func WithBacklinkClass[T []byte | string](a T) FootnoteHTMLRendererOption {
	return &withFootnoteBacklinkClass{[]byte(a)}
}

type withFootnoteBacklinkHTML struct {
	value []byte
}

func (o *withFootnoteBacklinkHTML) applyFootnoteHTMLRendererOption(c *footnoteHTMLRendererConfig) {
	c.BacklinkHTML = o.value
}

// WithBacklinkHTML is an HTML content for footnote backlinks.
func WithBacklinkHTML[T []byte | string](a T) FootnoteHTMLRendererOption {
	return &withFootnoteBacklinkHTML{[]byte(a)}
}

type withFootnoteSectionHeadingLevel struct {
	value int
}

func (o *withFootnoteSectionHeadingLevel) applyFootnoteHTMLRendererOption(c *footnoteHTMLRendererConfig) {
	c.SectionHeadingLevel = o.value
}

// WithSectionHeadingLevel is a functional option that partitions footnote
// numbering by headings of the given level (1 to 6). Each section (including
// the preface section before the first such heading) numbers its footnotes
// independently starting from 1, and renders its own endnotes at the end of
// the section. The default, level 0, keeps document-global numbering.
func WithSectionHeadingLevel(level int) FootnoteHTMLRendererOption {
	if level < 0 || level > 6 {
		level = 0
	}
	return &withFootnoteSectionHeadingLevel{value: level}
}

// }}}

// {{{ Renderer extension

type footnoteHTMLRendererExtension struct {
	config footnoteHTMLRendererConfig
}

// NewFootnoteHTMLRenderer returns a new html.Extension for footnotes.
func NewFootnoteHTMLRenderer(opts ...FootnoteHTMLRendererOption) html.Extension {
	cfg := footnoteHTMLRendererConfig{
		LinkTitle:     []byte(""),
		BacklinkTitle: []byte(""),
		LinkClass:     []byte("footnote-ref"),
		BacklinkClass: []byte("footnote-backref"),
		BacklinkHTML:  []byte("&#x21a9;&#xfe0e;"),
	}
	for _, opt := range opts {
		opt.applyFootnoteHTMLRendererOption(&cfg)
	}
	return &footnoteHTMLRendererExtension{config: cfg}
}

func (r *footnoteHTMLRendererExtension) renderFootnoteReference(
	writer io.Writer, source []byte, node gast.Node, entering bool, rc renderer.Context,
) (gast.WalkStatus, error) {
	if !entering {
		return gast.WalkContinue, nil
	}
	w := writer.(util.BufWriter)
	n := node.(*ast.FootnoteReference)
	is := strconv.Itoa(n.Index)
	prefix := r.idPrefix(node)

	section, index, refIndex, refCount := -1, n.Index, n.RefIndex, 0
	if r.config.SectionHeadingLevel > 0 {
		if raw := rc.Get(footnoteSectionsKey); raw != nil {
			if sr, ok := raw.(*footnoteSections).refs[n]; ok {
				section = sr.section
				index = sr.index
				refIndex = sr.refIndex
				refCount = len(sr.sdef.references)
				is = strconv.Itoa(index)
			}
		}
	}
	if section < 0 {
		defsInfo := rc.Get(footnoteDefsInfoKey).(map[string]*defInfo)
		info := defsInfo[n.Label.Str(source)]
		refCount = len(info.references)
	}

	_, _ = w.WriteString(`<sup id="`)
	writeFootnoteRefID(w, prefix, section, index, refIndex)
	_, _ = w.WriteString(`"><a href="#`)
	writeFootnoteDefID(w, prefix, section, index)
	_, _ = w.WriteString(`" class="`)
	_, _ = w.Write(applyFootnoteTemplate(r.config.LinkClass, index, refCount))
	if len(r.config.LinkTitle) > 0 {
		_, _ = w.WriteString(`" title="`)
		_, _ = w.Write(util.EscapeHTML(applyFootnoteTemplate(r.config.LinkTitle, index, refCount)))
	}
	_, _ = w.WriteString(`" role="doc-noteref">`)
	_, _ = w.WriteString(is)
	_, _ = w.WriteString(`</a></sup>`)

	return gast.WalkContinue, nil
}

func (r *footnoteHTMLRendererExtension) renderFootnoteDefinition(
	_ io.Writer, _ []byte, _ gast.Node, _ bool, _ renderer.Context,
) (gast.WalkStatus, error) {
	return gast.WalkSkipChildren, nil
}

func (r *footnoteHTMLRendererExtension) idPrefix(node gast.Node) []byte {
	if r.config.IDPrefix != nil {
		return r.config.IDPrefix
	}
	if r.config.IDPrefixFunction != nil {
		return r.config.IDPrefixFunction(node)
	}
	return []byte("")
}

func (r *footnoteHTMLRendererExtension) RendererOptions(c *html.Config) []html.Option {
	if c.XHTML {
		r.config.XHTML = true
	}

	decorator := &footnoteDecorator{config: &r.config}
	return []html.Option{
		html.WithNodeRenderers(map[gast.NodeKind]html.NodeRenderer{
			ast.KindFootnoteReference:  html.NodeRendererFunc(r.renderFootnoteReference),
			ast.KindFootnoteDefinition: html.NodeRendererFunc(r.renderFootnoteDefinition),
		}),
		html.WithNodeRendererDecorators(map[gast.NodeKind]html.NodeRendererDecorator{
			gast.KindDocument: decorator.Decorate,
			gast.KindHeading:  decorator.Decorate,
		}),
	}
}

// FootnoteHTMLRenderer is a default [html.Extension] for footnotes.
var FootnoteHTMLRenderer = NewFootnoteHTMLRenderer()

// }}}

// {{{ PostRender hook

type footnoteDecorator struct {
	config *footnoteHTMLRendererConfig
}

func (d *footnoteDecorator) Decorate(next html.NodeRenderer) html.NodeRenderer {
	return html.NodeRendererFunc(func(w io.Writer, source []byte, node gast.Node,
		entering bool, rc renderer.Context) (gast.WalkStatus, error) {
		if _, isHeading := node.(*gast.Heading); isHeading {
			if entering && d.config.SectionHeadingLevel > 0 {
				heading := node.(*gast.Heading)
				if raw := rc.Get(footnoteSectionsKey); raw != nil {
					if sd, ok := raw.(*footnoteSections); ok &&
						heading.Level == d.config.SectionHeadingLevel &&
						node.Parent() != nil && node.Parent().Kind() == gast.KindDocument {
						d.flushSection(w.(util.BufWriter), source, sd, rc)
					}
				}
			}
			return next.Render(w, source, node, entering, rc)
		}

		if entering {
			if d.config.SectionHeadingLevel > 0 {
				sd := newFootnoteSections(d.config.SectionHeadingLevel)
				sd.build(node, source)
				rc.Set(footnoteSectionsKey, sd)
				return next.Render(w, source, node, entering, rc)
			}

			defsMap := map[string]*ast.FootnoteDefinition{}
			infos := map[string]*defInfo{}
			_ = gast.Walk(node, func(node gast.Node, entering bool) (gast.WalkStatus, error) {
				if !entering {
					return gast.WalkContinue, nil
				}
				if def, ok := node.(*ast.FootnoteDefinition); ok {
					label := def.Label.Str(source)
					defsMap[label] = def
					if _, exists := infos[label]; !exists {
						infos[label] = &defInfo{index: -1}
					}
					return gast.WalkSkipChildren, nil
				}
				if ref, ok := node.(*ast.FootnoteReference); ok {
					label := ref.Label.Str(source)
					info, exists := infos[label]
					if !exists {
						info = &defInfo{index: -1}
						infos[label] = info
					}
					if info.index < 0 {
						info.index = ref.Index
					}
					info.references = append(info.references, ref.RefIndex)
				}
				return gast.WalkContinue, nil
			})
			rc.Set(footnoteDefsKey, defsMap)
			rc.Set(footnoteDefsInfoKey, infos)

			return next.Render(w, source, node, entering, rc)
		}

		if d.config.SectionHeadingLevel > 0 {
			raw := rc.Get(footnoteSectionsKey)
			if raw == nil {
				return next.Render(w, source, node, entering, rc)
			}
			sd := raw.(*footnoteSections)
			bw := w.(util.BufWriter)
			for sd.pending < len(sd.sections) {
				d.flushSection(bw, source, sd, rc)
			}
			return next.Render(w, source, node, entering, rc)
		}

		rawDefsMap := rc.Get(footnoteDefsKey)
		rawInfos := rc.Get(footnoteDefsInfoKey)
		if rawDefsMap == nil || rawInfos == nil {
			return next.Render(w, source, node, entering, rc)
		}
		defsMap := rawDefsMap.(map[string]*ast.FootnoteDefinition)
		infos := rawInfos.(map[string]*defInfo)

		type defEntry struct {
			node *ast.FootnoteDefinition
			info *defInfo
		}
		var referenced []defEntry
		for label, info := range infos {
			if len(info.references) > 0 {
				if node, ok := defsMap[label]; ok {
					referenced = append(referenced, defEntry{node: node, info: info})
				}
			}
		}
		if len(referenced) == 0 {
			return next.Render(w, source, node, entering, rc)
		}

		slices.SortFunc(referenced, func(a, b defEntry) int {
			return a.info.index - b.info.index
		})

		bw := w.(util.BufWriter)
		_, _ = bw.WriteString("<div class=\"footnotes\" role=\"doc-endnotes\">\n")
		if d.config.XHTML {
			_, _ = bw.WriteString("<hr />\n")
		} else {
			_, _ = bw.WriteString("<hr>\n")
		}
		_, _ = bw.WriteString("<ol>\n")

		for _, entry := range referenced {
			d.renderDefinition(
				bw, source, entry.node, -1, entry.info.index,
				len(entry.info.references), entry.info.references, rc,
			)
		}

		_, _ = bw.WriteString("</ol>\n")
		_, _ = bw.WriteString("</div>\n")

		return next.Render(w, source, node, entering, rc)
	})
}

func (d *footnoteDecorator) renderDefinition(
	w util.BufWriter, source []byte, def *ast.FootnoteDefinition,
	section, index int, refCount int, refs []int, rc renderer.Context,
) {
	_, _ = w.WriteString(`<li id="`)
	writeFootnoteDefID(w, d.idPrefix(def), section, index)
	_, _ = w.WriteString("\"")
	if def.Attributes() != nil {
		html.RenderAttributes(w, source, def, html.ListItemAttributeFilter, rc)
	}
	_, _ = w.WriteString(">\n")

	var lastPara gast.Node
	if lc := def.LastChild(); lc != nil && gast.IsParagraph(lc) {
		lastPara = lc
	}

	for child := def.FirstChild(); child != nil; child = child.NextSibling() {
		if child == lastPara {
			d.renderParagraphWithBacklinks(w, source, child, section, index, refCount, refs, rc)
		} else {
			_ = rc.Render(w, source, child)
		}
	}

	if lastPara == nil && refCount > 0 {
		d.renderBacklinks(w, def, section, index, refCount, refs)
	}

	_, _ = w.WriteString("</li>\n")
}

func (d *footnoteDecorator) renderParagraphWithBacklinks(
	w util.BufWriter, source []byte, para gast.Node,
	section, index, refCount int, refs []int, rc renderer.Context,
) {
	_, _ = w.WriteString("<p>")
	for child := para.FirstChild(); child != nil; child = child.NextSibling() {
		_ = rc.Render(w, source, child)
	}
	d.renderBacklinks(w, para, section, index, refCount, refs)
	_, _ = w.WriteString("</p>\n")
}

func (d *footnoteDecorator) renderBacklinks(
	w util.BufWriter, node gast.Node,
	section, index, refCount int, refs []int,
) {
	for _, refIdx := range refs {
		_, _ = w.WriteString(`&#160;<a href="#`)
		writeFootnoteRefID(w, d.idPrefix(node), section, index, refIdx)
		_, _ = w.WriteString(`" class="`)
		_, _ = w.Write(applyFootnoteTemplate(d.config.BacklinkClass, index, refCount))
		if len(d.config.BacklinkTitle) > 0 {
			_, _ = w.WriteString(`" title="`)
			_, _ = w.Write(
				util.EscapeHTML(applyFootnoteTemplate(d.config.BacklinkTitle, index, refCount)))
		}
		_, _ = w.WriteString(`" role="doc-backlink">`)
		_, _ = w.Write(applyFootnoteTemplate(d.config.BacklinkHTML, index, refCount))
		_, _ = w.WriteString(`</a>`)
	}
}

func (d *footnoteDecorator) flushSection(
	w util.BufWriter, source []byte, sd *footnoteSections, rc renderer.Context,
) {
	if sd.pending >= len(sd.sections) {
		return
	}
	sectionIndex := sd.pending
	section := sd.sections[sd.pending]

	var referenced []*sectionDefInfo
	for _, sdInfo := range section.order {
		if def, ok := sd.defsMap[sdInfo.label]; ok {
			sdInfo.def = def
			referenced = append(referenced, sdInfo)
		}
	}
	sd.pending++
	if len(referenced) == 0 {
		return
	}

	_, _ = w.WriteString("<div class=\"footnotes\" role=\"doc-endnotes\">\n")
	if d.config.XHTML {
		_, _ = w.WriteString("<hr />\n")
	} else {
		_, _ = w.WriteString("<hr>\n")
	}
	_, _ = w.WriteString("<ol>\n")

	for _, sdInfo := range referenced {
		refs := make([]int, 0, len(sdInfo.references))
		for _, ref := range sdInfo.references {
			refs = append(refs, sd.refs[ref].refIndex)
		}
		d.renderDefinition(
			w, source, sdInfo.def, sectionIndex, sdInfo.index,
			len(sdInfo.references), refs, rc,
		)
	}

	_, _ = w.WriteString("</ol>\n")
	_, _ = w.WriteString("</div>\n")
}

func (d *footnoteDecorator) idPrefix(node gast.Node) []byte {
	if d.config.IDPrefix != nil {
		return d.config.IDPrefix
	}
	if d.config.IDPrefixFunction != nil {
		return d.config.IDPrefixFunction(node)
	}
	return []byte("")
}

// }}}

// {{{ Parser extension

type footnoteParserExtension struct {
}

// NewFootnoteParser returns a new parser.Extension for parsing PHP Markdown Extra footnotes.
func NewFootnoteParser() parser.Extension {
	return &footnoteParserExtension{}
}

func (p *footnoteParserExtension) ParserOptions(_ *parser.Config) []parser.Option {
	return []parser.Option{
		parser.WithBlockParsers(
			util.Prioritized(newFootnoteBlockParser(), 999),
		),
		parser.WithInlineParsers(
			util.Prioritized(newFootnoteParser(), 101),
		),
	}
}

// FootnoteParser is a default [parser.Extension] for footnotes.
var FootnoteParser = NewFootnoteParser()

// }}}

func applyFootnoteTemplate(b []byte, index, refCount int) []byte {
	fast := true
	for i, c := range b {
		if i != 0 {
			if b[i-1] == '^' && c == '^' {
				fast = false
				break
			}
			if b[i-1] == '%' && c == '%' {
				fast = false
				break
			}
		}
	}
	if fast {
		return b
	}
	is := []byte(strconv.Itoa(index))
	rs := []byte(strconv.Itoa(refCount))
	ret := bytes.ReplaceAll(b, []byte("^^"), is)
	return bytes.ReplaceAll(ret, []byte("%%"), rs)
}
