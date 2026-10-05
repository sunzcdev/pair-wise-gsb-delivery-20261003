package gojq

// Pos represents a position in a jq source, expressed in 1-based line and
// byte column within the line.
type Pos struct {
	Line   int
	Column int
}

// SourceRange represents a contiguous range in a jq source. File is empty
// when the frame originates from the main query passed to [Parse].
type SourceRange struct {
	File  string
	Start Pos
	End   Pos
}

// FrameKind describes the origin of a [StackFrame].
type FrameKind int

const (
	// FrameTop is the top-level query (the entry point, outside any def).
	FrameTop FrameKind = iota
	// FrameFunc is a jq filter defined with def, including module functions.
	FrameFunc
	// FrameLambda is an inline filter argument, such as the f in map(f).
	FrameLambda
	// FrameGoFunc is a function implemented in Go, such as a built-in or a
	// function added with [WithFunction] or [WithIterFunction].
	FrameGoFunc
)

func (kind FrameKind) String() string {
	switch kind {
	case FrameTop:
		return "top"
	case FrameFunc:
		return "func"
	case FrameLambda:
		return "lambda"
	case FrameGoFunc:
		return "go"
	default:
		return "unknown"
	}
}

// StackFrame is a single structured frame of a runtime error stack trace.
//
// Frames are ordered from the innermost (where the error occurred) to the
// outermost (the caller). Module is the module name or the file path of an
// imported module and is empty for the main query. Function is the fully
// qualified function name (including the module prefix). Def is the source
// range of the function definition and Call is the call site inside the
// caller; either may be the zero value when the information is not available,
// such as functions implemented in Go.
//
// TailCall reports that the frame was entered through a tail call. Tail calls
// reuse a single frame with an explicit compression marker instead of
// accumulating one frame per iteration, so recursing filters never grow the
// stack trace without bound.
type StackFrame struct {
	Kind     FrameKind
	Module   string
	Function string
	Def      SourceRange
	Call     SourceRange
	TailCall bool
}

// StackTracer is implemented by runtime errors that carry a structured stack
// trace. Use [errors.As] to retrieve the frames of an error returned from
// [Iter.Next]; the frames do not affect the error message and are completely
// invisible to try-catch inside jq.
//
//	var tracer interface{ StackFrames() []gojq.StackFrame }
//	if errors.As(err, &tracer) {
//	    for _, frame := range tracer.StackFrames() {
//	        // frame.Function, frame.Def, frame.Call, ...
//	    }
//	}
type StackTracer interface {
	StackFrames() []StackFrame
}

// sourceInfo keeps a named source text so byte offsets recorded in the
// compiled code can be resolved to line/column ranges on demand.
type sourceInfo struct {
	name     string
	file     string
	contents string
}

// funcFrame records the static frame metadata associated with a scope id.
type funcFrame struct {
	kind     FrameKind
	module   string
	function string
	defStart int
	defEnd   int
}

// callInfo records a call site for a jq/lambda function, keyed by the pc of
// the opcall/opcallpc/opcallrec/opjumprec instruction.
type callInfo struct {
	module     string
	function   string
	start, end int
}

// stackFrame is the internal, offset-based representation of a [StackFrame].
type stackFrame struct {
	kind     FrameKind
	module   string
	function string
	defStart int
	defEnd   int
	call     *callInfo
	tailCall bool
}

// stackError wraps a runtime error with an offset-based stack trace. It keeps
// the wrapped error message unchanged. It does not implement [ValueError]
// itself; [errors.As] reaches the inner error through Unwrap, so a plain type
// error is not mistaken for a catchable value error. Stack frames are attached
// only after a value escapes try-catch, so they are invisible inside jq.
type stackError struct {
	err    error
	code   *Code
	frames []stackFrame
}

func (err *stackError) Error() string {
	return err.err.Error()
}

func (err *stackError) Unwrap() error {
	return err.err
}

func (err *stackError) ExitCode() int {
	if e, ok := err.err.(interface{ ExitCode() int }); ok {
		return e.ExitCode()
	}
	return 5
}

// StackFrames implements [StackTracer], resolving offsets lazily.
func (err *stackError) StackFrames() []StackFrame {
	return err.code.stackFrames(err.frames)
}

func (code *Code) sourceOf(name string) sourceInfo {
	if name == "" {
		return code.mainSource
	}
	for i := range code.sources {
		if code.sources[i].name == name {
			return code.sources[i]
		}
	}
	return sourceInfo{name: name}
}

func (code *Code) resolveRange(module string, start, end int) SourceRange {
	src := code.sourceOf(module)
	r := SourceRange{File: src.file}
	if src.contents == "" {
		return r
	}
	start = max(start, 0)
	end = max(end, start)
	if end > len(src.contents) {
		end = len(src.contents)
	}
	r.Start = positionAt(src.contents, start)
	r.End = positionAt(src.contents, end)
	return r
}

func positionAt(contents string, offset int) Pos {
	line, col := 1, 1
	for i := 0; i < offset && i < len(contents); i++ {
		if contents[i] == '\n' {
			line++
			col = 1
		} else if contents[i] == '\r' {
			// ignore; the following \n advances the line
		} else {
			col++
		}
	}
	return Pos{Line: line, Column: col}
}

func (code *Code) stackFrames(frames []stackFrame) []StackFrame {
	if len(frames) == 0 {
		return nil
	}
	out := make([]StackFrame, len(frames))
	for i, frame := range frames {
		out[i] = StackFrame{
			Kind:     frame.kind,
			Module:   frame.module,
			Function: frame.function,
			TailCall: frame.tailCall,
		}
		if frame.kind == FrameLambda {
			// Inline filter arguments have no user-visible def name; the
			// synthetic lambda:N label is compiler-internal.
			out[i].Function = ""
		}
		if frame.defStart >= 0 {
			out[i].Def = code.resolveRange(frame.module, frame.defStart, frame.defEnd)
		}
		if frame.call != nil {
			out[i].Call = code.resolveRange(frame.call.module, frame.call.start, frame.call.end)
		}
		if frame.kind == FrameLambda && out[i].Def.Start.Line == 0 && out[i].Call.Start.Line > 0 {
			out[i].Def = out[i].Call
		}
	}
	return out
}
