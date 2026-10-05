package gojq

// Position is a position in a source query.
//
// Offsets are byte offsets into the original source string. Line and column
// are 1-based, and the column counts bytes (not runes) from the beginning of
// the line. The zero value means that the position is not available.
type Position struct {
	// Filename is the name of the source file, if known. It is empty for
	// queries parsed from strings and for built-in functions.
	Filename string
	// Offset is the byte offset of the position in the source.
	Offset int
	// Line is the 1-based line number of the position.
	Line int
	// Column is the 1-based byte column number of the position.
	Column int
}

// SourceRange is a half-open byte range [Start, End) in a source query.
type SourceRange struct {
	Start Position
	End   Position
}

// StackFrame describes a single frame of the call stack attached to a
// runtime error. The frames describe jq function boundaries, including
// user-defined functions, functions imported from modules, built-in
// functions and custom functions registered via [WithFunction] (whose
// frames are marked as Native).
type StackFrame struct {
	// Module is the module path ("import "mod" as ...") that defines the
	// function. It is empty for functions defined in the main query and
	// for native functions.
	Module string

	// Function is the name of the function as written in the query,
	// including the module qualifier and the arity suffix, such as "f/0"
	// or "math::pi/0". Lambda arguments are rendered as "<lambda>".
	Function string

	// Native reports whether the frame belongs to a Go function registered
	// with [WithFunction] or [WithIterFunction], or to a built-in function
	// implemented in Go. Such frames have no source definition in jq.
	Native bool

	// Definition is the source range of the function definition
	// ("def f: ...;"). It is the zero value for native functions and for
	// the synthetic top-level frame.
	Definition SourceRange

	// Call is the source range of the call site that entered this frame.
	// It is the zero value for the top-level frame and for frames that
	// cannot be attributed to a specific call site.
	Call SourceRange

	// TailCalls is the number of tail calls that were compressed into this
	// frame. It is zero for ordinary frames; a positive value means that
	// the frame represents a tail-recursive loop.
	TailCalls int
}

// StackTracer is implemented by runtime errors that carry structured stack
// frames. Use errors.As to retrieve the frames of an uncaught error:
//
//	var stackErr interface{ StackTrace() []gojq.StackFrame }
//	if errors.As(err, &stackErr) {
//		for _, frame := range stackErr.StackTrace() {
//			...
//		}
//	}
//
// The Error method of the implementing error keeps the original, concise
// error message; the stack trace is never rendered into the terminal string.
//
// Errors caught by try-catch are unwrapped to their caught value (see
// [ValueError]), so they do not propagate a stack trace. A [*HaltError]
// emitted by halt_error is never caught by try-catch, but it does carry a
// stack trace while preserving its distinct exit-code semantics.
type StackTracer interface {
	error
	StackTrace() []StackFrame
}
