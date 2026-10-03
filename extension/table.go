package extension

import (
	"bytes"
	"fmt"
	"io"

	gast "github.com/yuin/goldmark/v2/ast"
	"github.com/yuin/goldmark/v2/extension/ast"
	"github.com/yuin/goldmark/v2/parser"
	"github.com/yuin/goldmark/v2/renderer"
	"github.com/yuin/goldmark/v2/renderer/html"
	"github.com/yuin/goldmark/v2/text"
	"github.com/yuin/goldmark/v2/util"
)

var escapedPipeCellListKey = parser.NewContextKey()

type escapedPipeCell struct {
	Cell        *ast.TableCell
	Pos         []int
	Transformed bool
}

// TableCellMergeError reports a structural error found while resolving table
// cell span markers (the > and ^ markers enabled by WithTableCellSpan).
type TableCellMergeError struct {
	// Reason is one of the TableCellMergeError* constants describing the error.
	Reason TableCellMergeErrorReason
	// Row is the 0-based row index of the offending marker cell. Row 0 is the
	// header row.
	Row int
	// Column is the 0-based column index of the offending marker cell.
	Column int
	// Line is the source position of the offending marker cell.
	Line int
}

func (e *TableCellMergeError) Error() string {
	return fmt.Sprintf("table cell span error at row %d column %d: %s", e.Row, e.Column, e.Reason)
}

// TableCellMergeErrorReason describes why cell span marker resolution failed.
type TableCellMergeErrorReason string

const (
	// TableCellMergeErrorMergeLeftInFirstColumn indicates that a > marker
	// appeared in the first column, where no cell exists on the left.
	TableCellMergeErrorMergeLeftInFirstColumn TableCellMergeErrorReason = "a > marker cannot be used in the first column"

	// TableCellMergeErrorMergeUpInFirstRow indicates that a ^ marker
	// appeared in the header row, where no cell exists above.
	TableCellMergeErrorMergeUpInFirstRow TableCellMergeErrorReason = "a ^ marker cannot be used in the first row"

	// TableCellMergeErrorMergeAcrossHeaderBody indicates that a ^ marker in
	// the first body row tried to merge across the header/body boundary.
	TableCellMergeErrorMergeAcrossHeaderBody TableCellMergeErrorReason = "a ^ marker cannot merge across the header and body boundary"

	// TableCellMergeErrorNonRectangular indicates that merged cells do not form
	// a rectangle, producing an L-shape or overlapping merge regions.
	TableCellMergeErrorNonRectangular TableCellMergeErrorReason = "merged cells must form a rectangle"
)

// TableCellMergeErrorHandler is notified about structural errors found while
// resolving table cell span markers. Regardless of the handler's action, the
// malformed table is rejected and rendered as a plain paragraph.
type TableCellMergeErrorHandler func(*TableCellMergeError)

// TableCellAlignMethod indicates how table cells are aligned in HTML format.
type TableCellAlignMethod int

const (
	// TableCellAlignDefault renders alignments by default method.
	// With XHTML, alignments are rendered as an align attribute.
	// With HTML5, alignments are rendered as a style attribute.
	TableCellAlignDefault TableCellAlignMethod = iota

	// TableCellAlignAttribute renders alignments as an align attribute.
	TableCellAlignAttribute

	// TableCellAlignStyle renders alignments as a style attribute.
	TableCellAlignStyle

	// TableCellAlignNone does not care about alignments.
	// If you using classes or other styles, you can add these attributes
	// in an ASTTransformer.
	TableCellAlignNone
)

type tableHTMLRendererConfig struct {
	XHTML                bool
	TableCellAlignMethod TableCellAlignMethod
}

// TableHTMLRendererOption is a functional option for the table HTML renderer extension.
type TableHTMLRendererOption interface {
	applyTableHTMLRendererOption(*tableHTMLRendererConfig)
}

type withTableCellAlignMethod struct {
	value TableCellAlignMethod
}

func (o *withTableCellAlignMethod) applyTableHTMLRendererOption(c *tableHTMLRendererConfig) {
	c.TableCellAlignMethod = o.value
}

// WithTableCellAlignMethod is a functional option that indicates how table cells are aligned in HTML format.
func WithTableCellAlignMethod(a TableCellAlignMethod) TableHTMLRendererOption {
	return &withTableCellAlignMethod{value: a}
}

func isTableDelim(bs []byte) bool {
	if w, _ := util.IndentWidth(bs, 0); w > 3 {
		return false
	}
	allSep := true
	for _, b := range bs {
		if b != '-' {
			allSep = false
		}
		if !util.IsSpace(b) && b != '-' && b != '|' && b != ':' {
			return false
		}
	}
	return !allSep
}

func classifyTableDelim(col []byte) (alignment ast.Alignment, ok bool) {
	col = util.TrimLeftSpace(col)
	col = util.TrimRightSpace(col)
	left := len(col) > 0 && col[0] == ':'
	if left {
		col = col[1:]
	}
	right := len(col) > 0 && col[len(col)-1] == ':'
	if right {
		col = col[:len(col)-1]
	}
	if len(col) == 0 {
		return ast.AlignNone, false
	}
	for _, b := range col {
		if b != '-' {
			return ast.AlignNone, false
		}
	}
	switch {
	case left && right:
		return ast.AlignCenter, true
	case left:
		return ast.AlignLeft, true
	case right:
		return ast.AlignRight, true
	default:
		return ast.AlignNone, true
	}
}

type tableParserConfig struct {
	cellSpan     bool
	onMergeError TableCellMergeErrorHandler
}

// TableParserOption is a functional option for the table parser extension.
type TableParserOption interface {
	applyTableParserOption(*tableParserConfig)
}

type withTableCellSpan struct {
	enabled bool
}

func (o *withTableCellSpan) applyTableParserOption(c *tableParserConfig) {
	c.cellSpan = o.enabled
}

// WithTableCellSpan enables explicit table cell merging.
//
// A cell whose content is exactly > is merged into the cell on its left, and
// a cell whose content is exactly ^ is merged into the cell above. Escaped
// markers and markers inside code spans are treated as ordinary text. Every
// merged region must form a rectangle.
func WithTableCellSpan() TableParserOption {
	return &withTableCellSpan{enabled: true}
}

type withTableCellMergeErrorHandler struct {
	handler TableCellMergeErrorHandler
}

func (o *withTableCellMergeErrorHandler) applyTableParserOption(c *tableParserConfig) {
	c.onMergeError = o.handler
}

// WithTableCellMergeErrorHandler sets a handler that is notified when cell
// span marker resolution fails. It takes effect only with WithTableCellSpan.
func WithTableCellMergeErrorHandler(handler TableCellMergeErrorHandler) TableParserOption {
	return &withTableCellMergeErrorHandler{handler: handler}
}

type tableParagraphTransformer struct {
	cellSpan     bool
	onMergeError TableCellMergeErrorHandler
}

func newTableParagraphTransformer(opts ...TableParserOption) parser.ParagraphTransformer {
	config := tableParserConfig{}
	for _, opt := range opts {
		opt.applyTableParserOption(&config)
	}
	return &tableParagraphTransformer{
		cellSpan:     config.cellSpan,
		onMergeError: config.onMergeError,
	}
}

func (b *tableParagraphTransformer) Transform(node *gast.Paragraph, reader text.Reader, pc parser.Context) {
	ppos := node.Pos()
	lines := node.Source()
	if len(lines) < 2 {
		return
	}
	for i := 1; i < len(lines); i++ {
		alignments := b.parseDelimiter(lines[i], reader)
		if alignments == nil {
			continue
		}
		header := b.parseRow(lines[i-1], alignments, true, reader, pc)
		if header == nil || len(alignments) != header.ChildCount() {
			return
		}
		bodyRows := make([]*ast.TableRow, 0, len(lines)-i-1)
		for j := i + 1; j < len(lines); j++ {
			bodyRows = append(bodyRows, b.parseRow(lines[j], alignments, false, reader, pc))
		}
		if b.cellSpan {
			if err := b.resolveCellSpan(header, bodyRows, reader.Source()); err != nil {
				if b.onMergeError != nil {
					b.onMergeError(err)
				}
				continue
			}
		}
		table := ast.NewTable()
		table.SetPos(ppos)
		tableHeader := ast.NewTableHeader()
		tableHeader.SetPos(header.Pos())
		for c := header.FirstChild(); c != nil; {
			next := c.NextSibling()
			header.RemoveChild(c)
			tableHeader.AppendChild(c)
			c = next
		}
		table.AppendChild(tableHeader)
		var body *ast.TableBody
		for _, row := range bodyRows {
			if body == nil {
				body = ast.NewTableBody()
				body.SetPos(row.Pos())
				table.AppendChild(body)
			}
			body.AppendChild(row)
		}
		node.SetSource(lines[:i-1])
		node.Parent().InsertAfter(node, table)
		if len(node.Source()) == 0 {
			node.Parent().RemoveChild(node)
			break
		}
		last := node.Source()[i-2]
		last.Stop = last.Stop - 1 // trim last newline(\n)
		node.Source()[i-2] = last
	}
}

func (b *tableParagraphTransformer) parseRow(segment text.Segment,
	alignments []ast.Alignment, isHeader bool, reader text.Reader, pc parser.Context) *ast.TableRow {
	npos := segment
	source := reader.Source()
	segment = segment.TrimLeftSpace(source)
	segment = segment.TrimRightSpace(source)
	line := segment.Bytes(source)
	pos := 0
	limit := len(line)
	row := ast.NewTableRow()
	row.SetPos(npos.Start)
	if len(line) > 0 && line[pos] == '|' {
		pos++
	}
	if len(line) > 0 && line[limit-1] == '|' {
		limit--
	}
	i := 0
	for ; pos < limit; i++ {
		alignment := ast.AlignNone
		if i >= len(alignments) {
			if !isHeader {
				return row
			}
		} else {
			alignment = alignments[i]
		}

		var escapedCell *escapedPipeCell
		node := ast.NewTableCell(alignment)
		node.SetPos(npos.Start + pos - npos.Padding)
		hasBacktick := false
		closure := pos
		for ; closure < limit; closure++ {
			if line[closure] == '`' {
				hasBacktick = true
			}
			if line[closure] == '|' {
				if closure == 0 || line[closure-1] != '\\' {
					break
				} else if hasBacktick {
					if escapedCell == nil {
						escapedCell = &escapedPipeCell{node, []int{}, false}
						escapedList := pc.ComputeIfAbsent(escapedPipeCellListKey,
							func() any {
								return []*escapedPipeCell{}
							}).([]*escapedPipeCell)
						escapedList = append(escapedList, escapedCell)
						pc.Set(escapedPipeCellListKey, escapedList)
					}
					escapedCell.Pos = append(escapedCell.Pos, segment.Start+closure-1)
				}
			}
		}
		seg := text.NewSegment(segment.Start+pos, segment.Start+closure)
		seg = seg.TrimLeftSpace(source)
		seg = seg.TrimRightSpace(source)
		node.AppendSource(seg)
		row.AppendChild(node)
		pos = closure + 1
	}
	for ; i < len(alignments); i++ {
		row.AppendChild(ast.NewTableCell(alignments[i]))
	}
	return row
}

func (b *tableParagraphTransformer) parseDelimiter(segment text.Segment, reader text.Reader) []ast.Alignment {

	line := segment.Bytes(reader.Source())
	if !isTableDelim(line) {
		return nil
	}
	cols := bytes.Split(line, []byte{'|'})
	if util.IsBlank(cols[0]) {
		cols = cols[1:]
	}
	if len(cols) > 0 && util.IsBlank(cols[len(cols)-1]) {
		cols = cols[:len(cols)-1]
	}

	var alignments []ast.Alignment
	for _, col := range cols {
		alignment, ok := classifyTableDelim(col)
		if !ok {
			return nil
		}
		alignments = append(alignments, alignment)
	}
	return alignments
}

// cellSpanMarker returns > or ^ when the cell's raw content is exactly one of
// the cell span markers. Escaped markers and markers inside code spans are not
// trimmed by the parser, so they never match.
func cellSpanMarker(cell *ast.TableCell, source []byte) byte {
	segs := cell.Source()
	if len(segs) != 1 {
		return 0
	}
	raw := segs[0].Bytes(source)
	if len(raw) != 1 {
		return 0
	}
	switch raw[0] {
	case '>', '^':
		return raw[0]
	}
	return 0
}

// resolveCellSpan resolves > (merge left) and ^ (merge up) markers and applies
// them to the AST. It returns a structural error when markers are placed at an
// edge, cross the header/body boundary, or form a non-rectangular region.
func (b *tableParagraphTransformer) resolveCellSpan(
	header *ast.TableRow, bodyRows []*ast.TableRow, source []byte) *TableCellMergeError {
	rows := make([]*ast.TableRow, 0, len(bodyRows)+1)
	rows = append(rows, header)
	rows = append(rows, bodyRows...)

	width := 0
	for _, row := range rows {
		if row.ChildCount() > width {
			width = row.ChildCount()
		}
	}

	grid := make([][]*ast.TableCell, len(rows))
	for r, row := range rows {
		grid[r] = make([]*ast.TableCell, row.ChildCount())
		c := 0
		for cell := row.FirstChild(); cell != nil; cell = cell.NextSibling() {
			grid[r][c] = cell.(*ast.TableCell)
			c++
		}
	}

	// owners[r][c] is the origin cell that owns grid position (r, c).
	owners := make([][]*ast.TableCell, len(rows))
	for r := range owners {
		owners[r] = make([]*ast.TableCell, width)
	}

	mergeError := func(reason TableCellMergeErrorReason, r, c int) *TableCellMergeError {
		err := &TableCellMergeError{
			Reason: reason,
			Row:    r,
			Column: c,
			Line:   -1,
		}
		if r < len(grid) && c < len(grid[r]) {
			err.Line = grid[r][c].Pos()
		}
		return err
	}

	for r, row := range grid {
		for c, cell := range row {
			marker := cellSpanMarker(cell, source)
			if marker == 0 {
				owners[r][c] = cell
				continue
			}
			switch marker {
			case '>':
				if c == 0 {
					return mergeError(TableCellMergeErrorMergeLeftInFirstColumn, r, c)
				}
				owners[r][c] = owners[r][c-1]
			case '^':
				if r == 0 {
					return mergeError(TableCellMergeErrorMergeUpInFirstRow, r, c)
				}
				if r == 1 {
					return mergeError(TableCellMergeErrorMergeAcrossHeaderBody, r, c)
				}
				if c >= len(grid[r-1]) {
					return mergeError(TableCellMergeErrorNonRectangular, r, c)
				}
				owners[r][c] = owners[r-1][c]
			}
		}
	}

	// Group covered positions by origin.
	covered := map[*ast.TableCell][][2]int{}
	var origins []*ast.TableCell
	for r := range grid {
		for c := 0; c < width; c++ {
			if c >= len(grid[r]) {
				continue
			}
			origin := owners[r][c]
			if origin == grid[r][c] {
				if _, ok := covered[origin]; !ok {
					covered[origin] = nil
					origins = append(origins, origin)
				}
				continue
			}
			if _, ok := covered[origin]; !ok {
				origins = append(origins, origin)
			}
			covered[origin] = append(covered[origin], [2]int{r, c})
		}
	}

	// Every merge region must be a rectangle.
	for _, origin := range origins {
		list := covered[origin]
		if len(list) == 0 {
			continue
		}
		or, oc := -1, -1
		for r := range grid {
			found := false
			for c := 0; c < len(grid[r]); c++ {
				if grid[r][c] == origin {
					or, oc = r, c
					found = true
					break
				}
			}
			if found {
				break
			}
		}
		maxR, maxC := or, oc
		for _, pos := range list {
			if pos[0] > maxR {
				maxR = pos[0]
			}
			if pos[1] > maxC {
				maxC = pos[1]
			}
		}
		rowspan := maxR - or + 1
		colspan := maxC - oc + 1
		if len(list)+1 != rowspan*colspan {
			// Report the first marker inside the would-be rectangle that
			// leaves a non-rectangular hole behind it.
			for rr := or; rr <= maxR; rr++ {
				for cc := oc; cc <= maxC; cc++ {
					if cc < len(grid[rr]) && owners[rr][cc] == origin &&
						grid[rr][cc] != origin {
						return mergeError(TableCellMergeErrorNonRectangular, rr, cc)
					}
				}
			}
			return mergeError(TableCellMergeErrorNonRectangular, maxR, maxC)
		}
		want := map[[2]int]bool{}
		for rr := or; rr <= maxR; rr++ {
			for cc := oc; cc <= maxC; cc++ {
				want[[2]int{rr, cc}] = true
			}
		}
		want[[2]int{or, oc}] = false
		for _, pos := range list {
			want[pos] = false
		}
		for rr := or; rr <= maxR; rr++ {
			for cc := oc; cc <= maxC; cc++ {
				if want[[2]int{rr, cc}] {
					if cc < len(grid[rr]) {
						if owners[rr][cc] != origin {
							return mergeError(TableCellMergeErrorNonRectangular, rr, cc)
						}
						continue
					}
					return mergeError(TableCellMergeErrorNonRectangular, rr, cc)
				}
			}
		}
		origin.Colspan = colspan
		origin.Rowspan = rowspan
		origin.CoveredCells = make([]*ast.TableCell, 0, len(list))
		for _, pos := range list {
			r, c := pos[0], pos[1]
			origin.CoveredCells = append(origin.CoveredCells, grid[r][c])
			rows[r].RemoveChild(grid[r][c])
		}
	}
	return nil
}

type tableASTTransformer struct {
}

var defaultTableASTTransformer parser.ASTTransformer = &tableASTTransformer{}

func (a *tableASTTransformer) Transform(_ *gast.Document, reader text.Reader, pc parser.Context) {
	lst := pc.Get(escapedPipeCellListKey)
	if lst == nil {
		return
	}
	pc.Set(escapedPipeCellListKey, nil)
	for _, v := range lst.([]*escapedPipeCell) {
		if v.Transformed {
			continue
		}
		_ = gast.Walk(v.Cell, func(n gast.Node, entering bool) (gast.WalkStatus, error) {
			if !entering || n.Kind() != gast.KindCodeSpan {
				return gast.WalkContinue, nil
			}
			cs := n.(*gast.CodeSpan)
			raw := cs.Value.Bytes(reader.Source())
			fixed := bytes.ReplaceAll(raw, []byte(`\|`), []byte(`|`))
			cs.Value = text.NewMultiLineValueFromString(string(fixed), text.IdentityDecoder)
			v.Transformed = true
			return gast.WalkContinue, nil
		})
	}
}

type tableHTMLRendererExtension struct {
	config tableHTMLRendererConfig
}

// NewTableHTMLRenderer returns a new html.Extension for rendering Table nodes.
func NewTableHTMLRenderer(opts ...TableHTMLRendererOption) html.Extension {
	cfg := tableHTMLRendererConfig{
		TableCellAlignMethod: TableCellAlignDefault,
	}
	for _, opt := range opts {
		opt.applyTableHTMLRendererOption(&cfg)
	}
	return &tableHTMLRendererExtension{config: cfg}
}

func (r *tableHTMLRendererExtension) RendererOptions(c *html.Config) []html.Option {
	if c.XHTML {
		r.config.XHTML = true
	}
	return []html.Option{
		html.WithNodeRenderers(map[gast.NodeKind]html.NodeRenderer{
			ast.KindTable:       html.NodeRendererFunc(r.renderTable),
			ast.KindTableHeader: html.NodeRendererFunc(r.renderTableHeader),
			ast.KindTableBody:   html.NodeRendererFunc(r.renderTableBody),
			ast.KindTableRow:    html.NodeRendererFunc(r.renderTableRow),
			ast.KindTableCell:   html.NodeRendererFunc(r.renderTableCell),
		}),
	}
}

// TableAttributeFilter defines attribute names which table elements can have.
//
//   - align: Deprecated
//   - bgcolor: Deprecated
//   - border: Deprecated
//   - cellpadding: Deprecated
//   - cellspacing: Deprecated
//   - frame: Deprecated
//   - rules: Deprecated
//   - summary: Deprecated
//   - width: Deprecated.
var TableAttributeFilter = html.GlobalAttributeFilter.ExtendString(`align,bgcolor,border,cellpadding,cellspacing,frame,rules,summary,width`) // nolint: lll

func (r *tableHTMLRendererExtension) renderTable(
	writer io.Writer, source []byte, n gast.Node, entering bool, rc renderer.Context) (gast.WalkStatus, error) {
	w := writer.(util.BufWriter)
	if entering {
		_, _ = w.WriteString("<table")
		if n.Attributes() != nil {
			html.RenderAttributes(w, source, n, TableAttributeFilter, rc)
		}
		_, _ = w.WriteString(">\n")
	} else {
		_, _ = w.WriteString("</table>\n")
	}
	return gast.WalkContinue, nil
}

// TableHeaderAttributeFilter defines attribute names which <thead> elements can have.
//
//   - align: Deprecated since HTML4, Obsolete since HTML5
//   - bgcolor: Not Standardized
//   - char: Deprecated since HTML4, Obsolete since HTML5
//   - charoff: Deprecated since HTML4, Obsolete since HTML5
//   - valign: Deprecated since HTML4, Obsolete since HTML5.
var TableHeaderAttributeFilter = html.GlobalAttributeFilter.ExtendString(`align,bgcolor,char,charoff,valign`)

func (r *tableHTMLRendererExtension) renderTableHeader(
	writer io.Writer, source []byte, n gast.Node, entering bool, rc renderer.Context) (gast.WalkStatus, error) {
	w := writer.(util.BufWriter)
	if entering {
		_, _ = w.WriteString("<thead")
		if n.Attributes() != nil {
			html.RenderAttributes(w, source, n, TableHeaderAttributeFilter, rc)
		}
		_, _ = w.WriteString(">\n")
		_, _ = w.WriteString("<tr>\n") // Header <tr> has no separate handle
	} else {
		_, _ = w.WriteString("</tr>\n")
		_, _ = w.WriteString("</thead>\n")
	}
	return gast.WalkContinue, nil
}

// TableBodyAttributeFilter defines attribute names which <tbody> elements can have.
//
//   - align: Deprecated since HTML4, Obsolete since HTML5
//   - bgcolor: Not Standardized
//   - char: Deprecated since HTML4, Obsolete since HTML5
//   - charoff: Deprecated since HTML4, Obsolete since HTML5
//   - valign: Deprecated since HTML4, Obsolete since HTML5.
var TableBodyAttributeFilter = html.GlobalAttributeFilter.ExtendString(`align,bgcolor,char,charoff,valign`)

func (r *tableHTMLRendererExtension) renderTableBody(
	writer io.Writer, _ []byte, _ gast.Node, entering bool, _ renderer.Context) (gast.WalkStatus, error) {
	w := writer.(util.BufWriter)
	if entering {
		_, _ = w.WriteString("<tbody>\n")
	} else {
		_, _ = w.WriteString("</tbody>\n")
	}
	return gast.WalkContinue, nil
}

// TableRowAttributeFilter defines attribute names which <tr> elements can have.
//
//   - align: Obsolete since HTML5
//   - bgcolor: Obsolete since HTML5
//   - char: Obsolete since HTML5
//   - charoff: Obsolete since HTML5
//   - valign: Obsolete since HTML5.
var TableRowAttributeFilter = html.GlobalAttributeFilter.ExtendString(`align,bgcolor,char,charoff,valign`)

func (r *tableHTMLRendererExtension) renderTableRow(
	writer io.Writer, source []byte, n gast.Node, entering bool, rc renderer.Context) (gast.WalkStatus, error) {
	w := writer.(util.BufWriter)
	if entering {
		_, _ = w.WriteString("<tr")
		if n.Attributes() != nil {
			html.RenderAttributes(w, source, n, TableRowAttributeFilter, rc)
		}
		_, _ = w.WriteString(">\n")
	} else {
		_, _ = w.WriteString("</tr>\n")
	}
	return gast.WalkContinue, nil
}

// TableThCellAttributeFilter defines attribute names which table <th> cells can have.
//
//   - abbr:  [OK] Contains a short abbreviated description of the cell's content [NOT OK in <td>]
//   - align:  Obsolete since HTML5
//   - axis:  Obsolete since HTML5
//   - bgcolor:  Not Standardized
//   - char:  Obsolete since HTML5
//   - charoff:  Obsolete since HTML5
//   - colspan:  [OK] Number of columns that the cell is to span
//   - headers:  [OK] This attribute contains a list of space-separated strings,
//     each corresponding to the id attribute of the <th> elements that apply to this element
//   - height:  Deprecated since HTML4. Obsolete since HTML5
//   - rowspan:  [OK] Number of rows that the cell is to span
//   - scope:  [OK] This enumerated attribute defines the cells that the header
//     (defined in the <th>) element relates to [NOT OK in <td>]
//   - valign:  Obsolete since HTML5
//   - width:  Deprecated since HTML4. Obsolete since HTML5.
var TableThCellAttributeFilter = html.GlobalAttributeFilter.ExtendString(`abbr,align,axis,bgcolor,char,charoff,colspan,headers,height,rowspan,scope,valign,width`) // nolint:lll

// TableTdCellAttributeFilter defines attribute names which table <td> cells can have.
//
//   - abbr:  Obsolete since HTML5. [OK in <th>]
//   - align:  Obsolete since HTML5
//   - axis:  Obsolete since HTML5
//   - bgcolor:  Not Standardized
//   - char:  Obsolete since HTML5
//   - charoff:  Obsolete since HTML5
//   - colspan:  [OK] Number of columns that the cell is to span
//   - headers:  [OK] This attribute contains a list of space-separated strings, each corresponding
//     to the id attribute of the <th> elements that apply to this element
//   - height:  Deprecated since HTML4. Obsolete since HTML5
//   - rowspan:  [OK] Number of rows that the cell is to span
//   - scope:  Obsolete since HTML5. [OK in <th>]
//   - valign:  Obsolete since HTML5
//   - width:  Deprecated since HTML4. Obsolete since HTML5.
var TableTdCellAttributeFilter = html.GlobalAttributeFilter.ExtendString(`abbr,align,axis,bgcolor,char,charoff,colspan,headers,height,rowspan,scope,valign,width`) // nolint: lll

func (r *tableHTMLRendererExtension) renderTableCell(
	writer io.Writer, source []byte, node gast.Node, entering bool, rc renderer.Context) (gast.WalkStatus, error) {
	w := writer.(util.BufWriter)
	n := node.(*ast.TableCell)
	tag := "td"
	if n.Parent().Kind() == ast.KindTableHeader {
		tag = "th"
	}
	if entering {
		_, _ = fmt.Fprintf(w, "<%s", tag)
		if n.Alignment != ast.AlignNone {
			amethod := r.config.TableCellAlignMethod
			if amethod == TableCellAlignDefault {
				if r.config.XHTML {
					amethod = TableCellAlignAttribute
				} else {
					amethod = TableCellAlignStyle
				}
			}
			switch amethod {
			case TableCellAlignAttribute:
				if _, ok := n.Attribute("align"); !ok { // Skip align render if overridden
					_, _ = fmt.Fprintf(w, ` align="%s"`, n.Alignment.String())
				}
			case TableCellAlignStyle:
				v, ok := n.Attribute("style")
				var cob util.CopyOnWriteBuffer
				if ok {
					cob = util.NewCopyOnWriteBuffer(v.Bytes(nil))
					cob.AppendByte(';')
				}
				style := fmt.Sprintf("text-align:%s", n.Alignment.String())
				cob.AppendString(style)
				n.SetAttribute("style", text.NewMultiLineValueFromString(string(cob.Bytes()), text.IdentityDecoder))
			}
		}
		if n.Colspan > 1 {
			if _, ok := n.Attribute("colspan"); !ok {
				_, _ = fmt.Fprintf(w, ` colspan="%d"`, n.Colspan)
			}
		}
		if n.Rowspan > 1 {
			if _, ok := n.Attribute("rowspan"); !ok {
				_, _ = fmt.Fprintf(w, ` rowspan="%d"`, n.Rowspan)
			}
		}
		if n.Attributes() != nil {
			if tag == "td" {
				html.RenderAttributes(w, source, n, TableTdCellAttributeFilter, rc) // <td>
			} else {
				html.RenderAttributes(w, source, n, TableThCellAttributeFilter, rc) // <th>
			}
		}
		_ = w.WriteByte('>')
	} else {
		_, _ = fmt.Fprintf(w, "</%s>\n", tag)
	}
	return gast.WalkContinue, nil
}

type tableParserExtension struct {
	config tableParserConfig
}

// NewTableParser returns a new parser.Extension for parsing GFM tables.
func NewTableParser(opts ...TableParserOption) parser.Extension {
	config := tableParserConfig{}
	for _, opt := range opts {
		opt.applyTableParserOption(&config)
	}
	return &tableParserExtension{config: config}
}

func (e *tableParserExtension) ParserOptions(_ *parser.Config) []parser.Option {
	return []parser.Option{
		parser.WithParagraphTransformers(
			util.Prioritized(newTableParagraphTransformer(
				&withTableCellSpan{enabled: e.config.cellSpan},
				&withTableCellMergeErrorHandler{handler: e.config.onMergeError},
			), 200),
		),
		parser.WithASTTransformers(
			util.Prioritized(defaultTableASTTransformer, 0),
		),
	}
}

// TableParser is a default [parser.Extension] for parsing GFM tables.
var TableParser = NewTableParser()

// TableHTMLRenderer is a default [html.Extension] for rendering Table nodes.
var TableHTMLRenderer = NewTableHTMLRenderer()
