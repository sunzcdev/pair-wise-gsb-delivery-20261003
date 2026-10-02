package httphead

import "fmt"

// Status describes how scanning of options terminated.
type Status int

const (
	// StatusOK reports that the whole input was scanned successfully.
	StatusOK Status = iota
	// StatusBreak reports that scanning was stopped early by the callback
	// returning ControlBreak. The input consumed so far was wellformed.
	StatusBreak
	// StatusError reports that scanning failed on malformed input.
	StatusError
)

// String represents status as a string.
func (s Status) String() string {
	switch s {
	case StatusOK:
		return "ok"
	case StatusBreak:
		return "break"
	case StatusError:
		return "error"
	default:
		return fmt.Sprintf("status(%d)", int(s))
	}
}

// ErrorKind categorizes malformed input errors.
type ErrorKind int

const (
	// ErrUnexpectedToken reports that a token was found where the grammar
	// does not allow one, e.g. a bad token right after a comma.
	ErrUnexpectedToken ErrorKind = iota
	// ErrUnclosedQuote reports a quoted-string without closing quote.
	ErrUnclosedQuote
	// ErrUnclosedComment reports a comment without closing parenthesis,
	// including unbalanced nested comments.
	ErrUnclosedComment
	// ErrMissingValue reports a parameter name followed by "=" without a
	// value.
	ErrMissingValue
	// ErrUnexpectedEOF reports that the input ended before any complete
	// option could be scanned.
	ErrUnexpectedEOF
)

// String represents error kind as a string.
func (k ErrorKind) String() string {
	switch k {
	case ErrUnexpectedToken:
		return "unexpected token"
	case ErrUnclosedQuote:
		return "unclosed quote"
	case ErrUnclosedComment:
		return "unclosed comment"
	case ErrMissingValue:
		return "missing parameter value"
	case ErrUnexpectedEOF:
		return "unexpected end of input"
	default:
		return fmt.Sprintf("error(%d)", int(k))
	}
}

// Diagnostic describes how a scan of options terminated. It is a plain
// value: collecting diagnostics does not allocate any intermediate syntax
// tree, so the scanning fast path stays allocation free.
type Diagnostic struct {
	// Status tells whether the scan ended normally, was stopped early by
	// the caller, or failed on malformed input.
	Status Status
	// Kind categorizes the error. It is only meaningful when Status is
	// StatusError.
	Kind ErrorKind
	// Offset is the byte offset of the offending byte in the original
	// input. It always refers to raw input bytes, never to unquoted or
	// unescaped token contents. For errors detected at the end of input
	// it equals len(input).
	Offset int
	// Index is the index of the option that was being parsed.
	Index int
	// Param is the index of the parameter within the current option, or
	// -1 if no parameter was seen for it yet.
	Param int
}

// OK reports whether the scan succeeded, that is, either reached the end of
// input or was stopped early by the caller. It matches the boolean result
// of the non-diagnostic API variants.
func (d Diagnostic) OK() bool {
	return d.Status != StatusError
}

// String represents diagnostic as a string.
func (d Diagnostic) String() string {
	if d.Status != StatusError {
		return d.Status.String()
	}
	return fmt.Sprintf("%s at byte %d (option %d, param %d)",
		d.Kind, d.Offset, d.Index, d.Param)
}
