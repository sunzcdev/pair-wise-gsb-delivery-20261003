package text

import (
	"bytes"
	"io"
	"strconv"
	"strings"

	"github.com/yuin/goldmark/v2/ast"
	"github.com/yuin/goldmark/v2/util"
)

// plainWriter is a stateful writer that maintains block separation, list
// indentation and trailing newline state.
//
// Content written through Write/text is indented at the start of physical
// lines. Newlines inside the content are preserved, so code blocks keep
// their internal whitespace verbatim.
type plainWriter struct {
	w util.BufWriter

	// hasContent is true once any non-newline byte has been written.
	hasContent bool
	// atLineStart is true when the cursor is at the beginning of a line
	// after a newline and no indentation or content has been emitted yet.
	atLineStart bool
	// trailingNL counts consecutive trailing newlines, capped at 2.
	trailingNL int

	// indent is emitted at the start of every ordinary physical line.
	indent string
	// indents stores indentation strings of open list items.
	indents []string
	// itemStates has one entry per open list item. While firstBlock is true,
	// the item marker has been written but its first block has not been
	// rendered yet.
	itemStates []itemState

	err error
}

// itemState records state of a list item currently being rendered.
type itemState struct {
	tight      bool
	firstBlock bool
}

func newPlainWriter(w util.BufWriter) *plainWriter {
	return &plainWriter{
		w:           w,
		atLineStart: true,
		indents:     make([]string, 0, 4),
		itemStates:  make([]itemState, 0, 4),
	}
}

// Write writes p with list indentation at the start of physical lines.
func (pw *plainWriter) Write(p []byte) (int, error) {
	if pw.err != nil {
		return 0, pw.err
	}
	written := 0
	for len(p) > 0 {
		// A bare newline at the line start must not emit indentation.
		if p[0] == '\n' {
			if err := pw.w.WriteByte('\n'); err != nil {
				pw.err = err
				return written, err
			}
			written++
			pw.atLineStart = true
			if pw.trailingNL < 2 {
				pw.trailingNL++
			}
			p = p[1:]
			continue
		}
		if pw.atLineStart {
			if pw.indent != "" {
				if _, err := io.WriteString(pw.w, pw.indent); err != nil {
					pw.err = err
					return written, err
				}
			}
			pw.atLineStart = false
		}
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			n, err := pw.w.Write(p)
			written += n
			if err != nil {
				pw.err = err
				return written, err
			}
			pw.hasContent = true
			pw.trailingNL = 0
			return written, nil
		}
		if i > 0 {
			n, err := pw.w.Write(p[:i])
			written += n
			if err != nil {
				pw.err = err
				return written, err
			}
			pw.hasContent = true
			pw.trailingNL = 0
		}
		if err := pw.w.WriteByte('\n'); err != nil {
			pw.err = err
			return written, err
		}
		written += i + 1
		p = p[i+1:]
		pw.atLineStart = true
		if pw.trailingNL < 2 {
			pw.trailingNL++
		}
	}
	return written, nil
}

// writeByte writes a single byte with indentation handling.
func (pw *plainWriter) writeByte(c byte) error {
	_, err := pw.Write([]byte{c})
	return err
}

// text writes a string with indentation handling.
func (pw *plainWriter) text(s string) error {
	if s == "" {
		return pw.err
	}
	_, err := pw.Write(util.StringToReadOnlyBytes(s))
	return err
}

// WriteString writes s with list indentation at the start of physical lines.
// It is intended for custom node renderers obtained via
// [ContextPlainWriter].
func (pw *plainWriter) WriteString(s string) error {
	return pw.text(s)
}

// WriteNewLine ends the current line when it does not already end with a
// newline. It is intended for custom node renderers.
func (pw *plainWriter) WriteNewLine() error {
	return pw.nl()
}

// WriteBlankLine ensures a blank line separates subsequent content. It is
// intended for custom node renderers.
func (pw *plainWriter) WriteBlankLine() error {
	return pw.blank()
}

// raw writes p verbatim without adding list indentation and updates state.
// It is used for code blocks: their internal whitespace must be preserved
// exactly and must not be altered by list indentation. The cursor ends on a
// fresh (unindented) line, so following blocks render at their own level.
func (pw *plainWriter) raw(p []byte) error {
	if pw.err != nil {
		return pw.err
	}
	if _, err := pw.w.Write(p); err != nil {
		pw.err = err
		return err
	}
	if len(p) > 0 {
		pw.hasContent = true
		pw.atLineStart = p[len(p)-1] == '\n'
		count := 0
		for i := len(p) - 1; i >= 0 && p[i] == '\n'; i-- {
			count++
			if count == 2 {
				break
			}
		}
		pw.trailingNL = count
	}
	return nil
}

// nl ends the current line. It is a no-op before any content or when the
// output already ends with a newline.
func (pw *plainWriter) nl() error {
	if !pw.hasContent || pw.trailingNL >= 1 {
		return pw.err
	}
	return pw.writeByte('\n')
}

// blank ensures the output ends with two newlines (a blank line separating
// subsequent content). It is a no-op before the first content.
func (pw *plainWriter) blank() error {
	if !pw.hasContent {
		return pw.err
	}
	for pw.trailingNL < 2 {
		if err := pw.writeByte('\n'); err != nil {
			return err
		}
	}
	return pw.err
}

// beforeBlock is called when a block starts other than a tight list item's
// first paragraph. Every block except the first of the document is preceded
// by a blank line.
func (pw *plainWriter) beforeBlock() error {
	if n := len(pw.itemStates); n > 0 {
		pw.itemStates[n-1].firstBlock = false
	}
	return pw.blank()
}

// beginFirstParagraph is called when a paragraph starts. It reports whether
// the enclosing list is tight and whether this is the item's first block.
// The first paragraph of a tight item follows its marker directly; every
// other paragraph is preceded by a blank line.
func (pw *plainWriter) beginFirstParagraph() (tight, first bool, err error) {
	if n := len(pw.itemStates); n > 0 {
		st := &pw.itemStates[n-1]
		if st.firstBlock {
			st.firstBlock = false
			return st.tight, true, nil
		}
	}
	return false, false, nil
}

// isInTightListItem reports whether the renderer is currently inside a list
// item whose enclosing list is tight.
func (pw *plainWriter) isInTightListItem() bool {
	if n := len(pw.itemStates); n > 0 {
		return pw.itemStates[n-1].tight
	}
	return false
}

// consumeFirstBlock marks the first block of the innermost open list item as
// rendered without emitting separation. It is used for nested lists in tight
// items, which follow the lead paragraph directly.
func (pw *plainWriter) consumeFirstBlock() {
	if n := len(pw.itemStates); n > 0 {
		pw.itemStates[n-1].firstBlock = false
	}
}

// beforeListItem writes the item marker at the parent indentation level and
// pushes indentation for the item's contents. It emits a blank line before
// loose item markers and a single newline before tight sibling markers.
func (pw *plainWriter) beforeListItem(list *ast.List, item *ast.ListItem) error {
	var marker string
	if list.IsOrdered() {
		number := list.Start
		for s := item.PreviousSibling(); s != nil; s = s.PreviousSibling() {
			number++
		}
		var buf [20]byte
		marker = string(strconv.AppendInt(buf[:0], int64(number), 10)) +
			string(list.Marker) + " "
	} else {
		marker = "- "
	}

	parentIndent := pw.indent
	if list.IsTight {
		if item.PreviousSibling() != nil {
			if err := pw.nl(); err != nil {
				return err
			}
		}
	} else if item.PreviousSibling() == nil {
		// The first loose item is separated from the preceding block by a
		// blank line. Sibling separation already follows from the blank line
		// emitted between blocks within loose items.
		if err := pw.blank(); err != nil {
			return err
		}
	}
	pw.indents = append(pw.indents, parentIndent)
	pw.indent = parentIndent + strings.Repeat(" ", len(marker))
	state := itemState{tight: list.IsTight, firstBlock: true}
	pw.itemStates = append(pw.itemStates, state)

	// Emit the marker at the line start without the new item indentation.
	pw.atLineStart = false
	if parentIndent != "" {
		if _, err := io.WriteString(pw.w, parentIndent); err != nil {
			pw.err = err
			return err
		}
	}
	if _, err := io.WriteString(pw.w, marker); err != nil {
		pw.err = err
		return err
	}
	pw.hasContent = true
	pw.trailingNL = 0
	return nil
}

// afterListItem pops the indentation pushed for the item and ensures its
// contents end on a new line.
func (pw *plainWriter) afterListItem() error {
	if n := len(pw.indents); n > 0 {
		pw.indent = pw.indents[n-1]
		pw.indents = pw.indents[:n-1]
	}
	if n := len(pw.itemStates); n > 0 {
		pw.itemStates = pw.itemStates[:n-1]
	}
	return pw.nl()
}

// finalize ensures the output ends with exactly one newline when any content
// was written.
func (pw *plainWriter) finalize() error {
	if !pw.hasContent || pw.trailingNL > 0 {
		return pw.err
	}
	return pw.writeByte('\n')
}
