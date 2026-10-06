package squirrel

import (
	"bytes"
	"fmt"
	"strings"
)

// PlaceholderFormat is the interface that wraps the ReplacePlaceholders method.
//
// ReplacePlaceholders takes a SQL statement and replaces each question mark
// placeholder with a (possibly different) SQL placeholder.
type PlaceholderFormat interface {
	ReplacePlaceholders(sql string) (string, error)
}

type placeholderDebugger interface {
	debugPlaceholder() string
}

var (
	// Question is a PlaceholderFormat instance that leaves placeholders as
	// question marks.
	Question = questionFormat{}

	// Dollar is a PlaceholderFormat instance that replaces placeholders with
	// dollar-prefixed positional placeholders (e.g. $1, $2, $3).
	Dollar = dollarFormat{}

	// PostgreSQL is a PlaceholderFormat instance that replaces placeholders
	// with dollar-prefixed positional placeholders (e.g. $1, $2, $3), like
	// Dollar, but scans the statement with a PostgreSQL-aware lexer.
	//
	// Question marks inside string literals ('...' and E'...'), quoted
	// identifiers ("..."), dollar-quoted strings ($$...$$ or $tag$...$tag$),
	// line comments (--) and (possibly nested) block comments (/* */) are
	// left untouched, so they no longer need to be escaped as ??. The
	// explicit ?? escape is still honored outside of those constructs, e.g.
	// for the JSON ? and ?| operators. Unterminated literals, comments or
	// dollar-quoted strings are reported as a *PlaceholderSyntaxError.
	PostgreSQL = postgreSQLFormat{}

	// Colon is a PlaceholderFormat instance that replaces placeholders with
	// colon-prefixed positional placeholders (e.g. :1, :2, :3).
	Colon = colonFormat{}

	// AtP is a PlaceholderFormat instance that replaces placeholders with
	// "@p"-prefixed positional placeholders (e.g. @p1, @p2, @p3).
	AtP = atpFormat{}
)

type questionFormat struct{}

func (questionFormat) ReplacePlaceholders(sql string) (string, error) {
	return sql, nil
}

func (questionFormat) debugPlaceholder() string {
	return "?"
}

type dollarFormat struct{}

func (dollarFormat) ReplacePlaceholders(sql string) (string, error) {
	return replacePositionalPlaceholders(sql, "$")
}

func (dollarFormat) debugPlaceholder() string {
	return "$"
}

type postgreSQLFormat struct{}

func (postgreSQLFormat) ReplacePlaceholders(sql string) (string, error) {
	return replacePostgreSQLPlaceholders(sql)
}

func (postgreSQLFormat) debugPlaceholder() string {
	return "$"
}

type colonFormat struct{}

func (colonFormat) ReplacePlaceholders(sql string) (string, error) {
	return replacePositionalPlaceholders(sql, ":")
}

func (colonFormat) debugPlaceholder() string {
	return ":"
}

type atpFormat struct{}

func (atpFormat) ReplacePlaceholders(sql string) (string, error) {
	return replacePositionalPlaceholders(sql, "@p")
}

func (atpFormat) debugPlaceholder() string {
	return "@p"
}

// Placeholders returns a string with count ? placeholders joined with commas.
func Placeholders(count int) string {
	if count < 1 {
		return ""
	}

	return strings.Repeat(",?", count)[1:]
}

func replacePositionalPlaceholders(sql, prefix string) (string, error) {
	buf := &bytes.Buffer{}
	i := 0
	for {
		p := strings.Index(sql, "?")
		if p == -1 {
			break
		}

		if len(sql[p:]) > 1 && sql[p:p+2] == "??" { // escape ?? => ?
			buf.WriteString(sql[:p])
			buf.WriteString("?")
			if len(sql[p:]) == 1 {
				break
			}
			sql = sql[p+2:]
		} else {
			i++
			buf.WriteString(sql[:p])
			fmt.Fprintf(buf, "%s%d", prefix, i)
			sql = sql[p+1:]
		}
	}

	buf.WriteString(sql)
	return buf.String(), nil
}

// PlaceholderSyntaxError describes an unterminated lexical structure found
// while scanning a SQL statement for placeholders.
type PlaceholderSyntaxError struct {
	Kind   string // the unterminated construct, e.g. "string literal"
	Offset int    // byte offset at which the construct starts
	Line   int    // 1-based line at which the construct starts
	Column int    // 1-based column (in bytes) at which the construct starts
}

func (e *PlaceholderSyntaxError) Error() string {
	return fmt.Sprintf("squirrel: unterminated %s starting at line %d, column %d (offset %d)",
		e.Kind, e.Line, e.Column, e.Offset)
}

func newPlaceholderSyntaxError(sql, kind string, offset int) *PlaceholderSyntaxError {
	line, column := 1, 1
	for i := 0; i < offset; i++ {
		if sql[i] == '\n' {
			line++
			column = 1
		} else {
			column++
		}
	}
	return &PlaceholderSyntaxError{Kind: kind, Offset: offset, Line: line, Column: column}
}

func isIdentByte(c byte) bool {
	return c == '_' || c == '$' ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9') || c >= 0x80
}

// replacePostgreSQLPlaceholders replaces ? placeholders with $N positional
// placeholders while skipping PostgreSQL lexical structures that may contain
// question marks: string literals, quoted identifiers, dollar-quoted strings
// and comments.
func replacePostgreSQLPlaceholders(sql string) (string, error) {
	buf := &bytes.Buffer{}
	buf.Grow(len(sql) + 8)

	n := len(sql)
	i := 0
	num := 0

	for i < n {
		c := sql[i]
		switch {
		case c == '?':
			if i+1 < n && sql[i+1] == '?' { // escape ?? => ?
				buf.WriteByte('?')
				i += 2
			} else {
				num++
				fmt.Fprintf(buf, "$%d", num)
				i++
			}

		case c == '\'':
			end, err := scanSingleQuoted(sql, i, false)
			if err != nil {
				return "", err
			}
			buf.WriteString(sql[i:end])
			i = end

		case (c == 'e' || c == 'E') && i+1 < n && sql[i+1] == '\'' &&
			(i == 0 || !isIdentByte(sql[i-1])):
			end, err := scanSingleQuoted(sql, i+1, true)
			if err != nil {
				return "", err
			}
			buf.WriteString(sql[i:end])
			i = end

		case c == '"':
			end, err := scanDoubleQuoted(sql, i)
			if err != nil {
				return "", err
			}
			buf.WriteString(sql[i:end])
			i = end

		case c == '$':
			tagEnd, ok := scanDollarQuoteTag(sql, i)
			if !ok {
				buf.WriteByte(c)
				i++
				continue
			}
			tag := sql[i:tagEnd]
			closing := strings.Index(sql[tagEnd:], tag)
			if closing == -1 {
				return "", newPlaceholderSyntaxError(sql, "dollar-quoted string", i)
			}
			end := tagEnd + closing + len(tag)
			buf.WriteString(sql[i:end])
			i = end

		case c == '-' && i+1 < n && sql[i+1] == '-':
			end := strings.IndexByte(sql[i:], '\n')
			if end == -1 {
				end = n
			} else {
				end += i
			}
			buf.WriteString(sql[i:end])
			i = end

		case c == '/' && i+1 < n && sql[i+1] == '*':
			end, err := scanBlockComment(sql, i)
			if err != nil {
				return "", err
			}
			buf.WriteString(sql[i:end])
			i = end

		default:
			buf.WriteByte(c)
			i++
		}
	}

	return buf.String(), nil
}

// scanSingleQuoted returns the end offset (exclusive) of a single-quoted
// string literal whose opening quote is at offset start. Backslash escapes
// are honored when escapes is true (E'...' strings).
func scanSingleQuoted(sql string, start int, escapes bool) (int, error) {
	for i := start + 1; i < len(sql); {
		switch sql[i] {
		case '\\':
			if escapes {
				i += 2
			} else {
				i++
			}
		case '\'':
			if i+1 < len(sql) && sql[i+1] == '\'' { // doubled quote
				i += 2
			} else {
				return i + 1, nil
			}
		default:
			i++
		}
	}
	return 0, newPlaceholderSyntaxError(sql, "string literal", start)
}

// scanDoubleQuoted returns the end offset (exclusive) of a double-quoted
// identifier whose opening quote is at offset start.
func scanDoubleQuoted(sql string, start int) (int, error) {
	for i := start + 1; i < len(sql); {
		if sql[i] == '"' {
			if i+1 < len(sql) && sql[i+1] == '"' { // doubled quote
				i += 2
			} else {
				return i + 1, nil
			}
		} else {
			i++
		}
	}
	return 0, newPlaceholderSyntaxError(sql, "quoted identifier", start)
}

// scanDollarQuoteTag checks whether a dollar-quoted string opens at offset
// start (which points at '$') and returns the offset just past the opening
// tag. Tags follow identifier rules and may not start with a digit, so "$1"
// is not treated as a tag.
func scanDollarQuoteTag(sql string, start int) (int, bool) {
	i := start + 1
	if i < len(sql) && sql[i] >= '0' && sql[i] <= '9' {
		return 0, false
	}
	for i < len(sql) && isIdentByte(sql[i]) && sql[i] != '$' {
		i++
	}
	if i < len(sql) && sql[i] == '$' {
		return i + 1, true
	}
	return 0, false
}

// scanBlockComment returns the end offset (exclusive) of a block comment
// whose opening "/*" is at offset start. Nested block comments are
// supported, as in PostgreSQL.
func scanBlockComment(sql string, start int) (int, error) {
	depth := 1
	for i := start + 2; i < len(sql); {
		if i+1 < len(sql) && sql[i] == '/' && sql[i+1] == '*' {
			depth++
			i += 2
		} else if i+1 < len(sql) && sql[i] == '*' && sql[i+1] == '/' {
			depth--
			i += 2
			if depth == 0 {
				return i, nil
			}
		} else {
			i++
		}
	}
	return 0, newPlaceholderSyntaxError(sql, "block comment", start)
}
