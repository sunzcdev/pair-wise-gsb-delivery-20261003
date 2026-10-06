package squirrel

import (
	"bytes"
	"fmt"
	"strings"
)

// DollarPG is a PlaceholderFormat instance that replaces question mark
// placeholders with dollar-prefixed positional placeholders (e.g. $1, $2,
// $3), like Dollar, but uses a PostgreSQL-aware lexer so that question marks
// appearing inside lexical constructs are not mistaken for placeholders.
//
// The lexer understands single-quoted string literals (with doubled-quote
// escapes),
// E'...' escape strings (with backslash escapes), double-quoted identifiers,
// dollar-quoted strings with optional tags (e.g. $func$ ... $func$), line
// comments (-- ...) and nestable block comments (/* ... /* ... */ ... */).
// Question marks inside any of these constructs are left untouched and do
// not affect the numbering of real placeholders.
//
// Outside of these constructs the explicit escape rule still applies: ??
// produces a literal ? (for PostgreSQL JSON operators such as ?, ?| and ?&).
// The lexer does not try to guess whether a bare ? is a placeholder or an
// operator; use ?? for operators.
//
// Unterminated strings, identifiers, comments or dollar-quoted strings are
// reported as a *LexError carrying the position of the offending construct.
var DollarPG = dollarPGFormat{}

type dollarPGFormat struct{}

func (dollarPGFormat) ReplacePlaceholders(sql string) (string, error) {
	return replacePositionalPlaceholdersPG(sql, "$")
}

func (dollarPGFormat) debugPlaceholder() string {
	return "$"
}

// LexError describes an unterminated PostgreSQL lexical construct found
// while replacing placeholders.
type LexError struct {
	// Construct names the kind of construct, e.g. "string literal",
	// "double-quoted identifier", "block comment" or "dollar-quoted string".
	Construct string
	// Offset is the byte offset in the SQL string where the construct starts.
	Offset int
	// Line is the 1-based line where the construct starts.
	Line int
	// Column is the 1-based byte column where the construct starts.
	Column int
}

func (e *LexError) Error() string {
	return fmt.Sprintf("squirrel: unterminated %s at offset %d (line %d, column %d)",
		e.Construct, e.Offset, e.Line, e.Column)
}

func newLexError(construct, sql string, offset int) *LexError {
	line, column := 1, 1
	for i := 0; i < offset && i < len(sql); i++ {
		if sql[i] == '\n' {
			line++
			column = 1
		} else {
			column++
		}
	}
	return &LexError{Construct: construct, Offset: offset, Line: line, Column: column}
}

func replacePositionalPlaceholdersPG(sql, prefix string) (string, error) {
	var buf bytes.Buffer
	buf.Grow(len(sql) + 16)

	n := len(sql)
	i := 0 // placeholder counter
	pos := 0
	for pos < n {
		c := sql[pos]
		switch {
		case c == '\'':
			end := scanPGString(sql, pos, false)
			if end < 0 {
				return "", newLexError("string literal", sql, pos)
			}
			buf.WriteString(sql[pos:end])
			pos = end
		case (c == 'e' || c == 'E') && pos+1 < n && sql[pos+1] == '\'':
			end := scanPGString(sql, pos+1, true)
			if end < 0 {
				return "", newLexError("escape string literal", sql, pos)
			}
			buf.WriteString(sql[pos:end])
			pos = end
		case c == '"':
			end := scanPGQuotedIdent(sql, pos)
			if end < 0 {
				return "", newLexError("double-quoted identifier", sql, pos)
			}
			buf.WriteString(sql[pos:end])
			pos = end
		case c == '-' && pos+1 < n && sql[pos+1] == '-':
			end := strings.IndexByte(sql[pos+2:], '\n')
			if end < 0 {
				buf.WriteString(sql[pos:])
				pos = n
			} else {
				end += pos + 2
				buf.WriteString(sql[pos:end])
				pos = end
			}
		case c == '/' && pos+1 < n && sql[pos+1] == '*':
			end := scanPGBlockComment(sql, pos)
			if end < 0 {
				return "", newLexError("block comment", sql, pos)
			}
			buf.WriteString(sql[pos:end])
			pos = end
		case c == '$':
			if tag, ok := scanPGDollarTag(sql, pos); ok {
				close := strings.Index(sql[pos+len(tag):], tag)
				if close < 0 {
					return "", newLexError("dollar-quoted string", sql, pos)
				}
				end := pos + len(tag) + close + len(tag)
				buf.WriteString(sql[pos:end])
				pos = end
			} else {
				buf.WriteByte(c)
				pos++
			}
		case c == '?':
			if pos+1 < n && sql[pos+1] == '?' { // escape ?? => ?
				buf.WriteByte('?')
				pos += 2
			} else {
				i++
				fmt.Fprintf(&buf, "%s%d", prefix, i)
				pos++
			}
		default:
			buf.WriteByte(c)
			pos++
		}
	}
	return buf.String(), nil
}

// scanPGString scans a string literal whose opening quote is at start and
// returns the offset just past the closing quote, or -1 if unterminated.
// In escape strings (E'...') a backslash escapes the following character.
func scanPGString(sql string, start int, escape bool) int {
	for i := start + 1; i < len(sql); i++ {
		switch sql[i] {
		case '\\':
			if escape {
				i++
			}
		case '\'':
			if i+1 < len(sql) && sql[i+1] == '\'' {
				i++
			} else {
				return i + 1
			}
		}
	}
	return -1
}

// scanPGQuotedIdent scans a double-quoted identifier whose opening quote is
// at start and returns the offset just past the closing quote, or -1 if
// unterminated.
func scanPGQuotedIdent(sql string, start int) int {
	for i := start + 1; i < len(sql); i++ {
		if sql[i] == '"' {
			if i+1 < len(sql) && sql[i+1] == '"' {
				i++
			} else {
				return i + 1
			}
		}
	}
	return -1
}

// scanPGBlockComment scans a possibly nested block comment whose opening /*
// is at start and returns the offset just past the closing */, or -1 if
// unterminated.
func scanPGBlockComment(sql string, start int) int {
	depth := 1
	for i := start + 2; i < len(sql); i++ {
		if sql[i] == '/' && i+1 < len(sql) && sql[i+1] == '*' {
			depth++
			i++
		} else if sql[i] == '*' && i+1 < len(sql) && sql[i+1] == '/' {
			depth--
			i++
			if depth == 0 {
				return i + 1
			}
		}
	}
	return -1
}

// scanPGDollarTag checks whether sql[start:] begins a dollar-quoted string
// ($tag$) and returns the full opening tag including both dollar signs.
func scanPGDollarTag(sql string, start int) (string, bool) {
	i := start + 1
	for i < len(sql) && isPGDollarTagChar(sql[i]) {
		i++
	}
	if i >= len(sql) || sql[i] != '$' {
		return "", false
	}
	// A tag follows unquoted identifier rules and cannot start with a digit,
	// which also keeps positional parameters like $1 from matching.
	if i > start+1 && sql[start+1] >= '0' && sql[start+1] <= '9' {
		return "", false
	}
	return sql[start : i+1], true
}

func isPGDollarTagChar(c byte) bool {
	return c == '_' ||
		(c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9')
}
