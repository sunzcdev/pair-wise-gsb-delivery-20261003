package gojq_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/itchyny/gojq"
)

type stackTracer interface {
	StackFrames() []gojq.StackFrame
}

func stackTraceOf(t *testing.T, err error) []gojq.StackFrame {
	t.Helper()
	var tr stackTracer
	if !errors.As(err, &tr) {
		t.Fatalf("expected a StackTracer error, got %T: %v", err, err)
	}
	return tr.StackFrames()
}

func runWithFrames(t *testing.T, src string, opts ...gojq.CompilerOption) error {
	t.Helper()
	query, err := gojq.Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	code, err := gojq.Compile(query, append(opts, gojq.WithStackFrames())...)
	if err != nil {
		t.Fatal(err)
	}
	iter := code.RunWithContext(context.Background(), nil)
	for {
		v, ok := iter.Next()
		if !ok {
			t.Fatal("expected an error value")
		}
		if e, ok := v.(error); ok {
			return e
		}
	}
}

func TestStackFramesBasic(t *testing.T) {
	err := runWithFrames(t, "def g: .x[]; def f: g; f")
	if got, expected := err.Error(), "cannot iterate over: null"; got != expected {
		t.Fatalf("error message changed:\n expected: %q\n got:      %q", expected, got)
	}
	frames := stackTraceOf(t, err)
	got := make([]string, len(frames))
	for i, f := range frames {
		got[i] = f.Function
	}
	if expected := []string{"g", "f", ""}; !reflect.DeepEqual(got, expected) {
		t.Fatalf("frames: expected %v, got %v", expected, got)
	}
	// g is defined at the start of the source and called at column 20.
	if f := frames[0]; f.Kind != gojq.FrameFunc ||
		f.Def.Start != (gojq.Pos{Line: 1, Column: 1}) ||
		f.Call.Start != (gojq.Pos{Line: 1, Column: 20}) {
		t.Fatalf("unexpected innermost frame: %+v", f)
	}
	// the outermost frame is the top-level query.
	if last := frames[len(frames)-1]; last.Kind != gojq.FrameTop || last.TailCall {
		t.Fatalf("unexpected top frame: %+v", last)
	}
}

func TestStackFramesMultiLineSourceRange(t *testing.T) {
	src := "def g:\n  .x[];\ng"
	err := runWithFrames(t, src)
	frames := stackTraceOf(t, err)
	if f := frames[0]; f.Def.Start != (gojq.Pos{Line: 1, Column: 1}) ||
		f.Def.End != (gojq.Pos{Line: 2, Column: 8}) ||
		f.Call.Start != (gojq.Pos{Line: 2, Column: 8}) {
		t.Fatalf("unexpected ranges for multi-line source: %+v", f)
	}
}

func TestStackFramesNonTailRecursion(t *testing.T) {
	err := runWithFrames(t,
		"def f($n): if $n < 3 then f($n+1) else .x[] end; f(0)")
	frames := stackTraceOf(t, err)
	var count int
	for _, f := range frames {
		if f.Function == "f" {
			count++
			if f.TailCall {
				t.Fatalf("non-tail frame must not be marked TailCall: %+v", f)
			}
		}
	}
	// f(0) .. f(3): four live f frames plus the top-level frame.
	if count != 4 {
		t.Fatalf("expected 4 f frames, got %d: %+v", count, frames)
	}
}

func TestStackFramesTailCallCompression(t *testing.T) {
	err := runWithFrames(t,
		"def r: if . < 1000 then (.+1 | r) else .x[] end; 0 | r")
	frames := stackTraceOf(t, err)
	var count int
	var marked bool
	for _, f := range frames {
		if f.Function == "r" {
			count++
			marked = marked || f.TailCall
		}
	}
	if count != 1 {
		t.Fatalf("tail recursion must compress to one frame, got %d: %+v", count, frames)
	}
	if !marked {
		t.Fatalf("compressed tail frame must carry the TailCall marker: %+v", frames)
	}
}

func TestStackFramesCallrecCompression(t *testing.T) {
	// This filter cannot jump in place (it binds a variable) so it uses the
	// callrec form; the stack must still stay bounded.
	err := runWithFrames(t,
		"def r: . as $x | if $x < 1000 then ($x+1 | r) else .y[] end; 0 | r")
	frames := stackTraceOf(t, err)
	var count int
	for _, f := range frames {
		if f.Function == "r" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("callrec tail recursion must compress to one frame, got %d: %+v",
			count, frames)
	}
}

func TestStackFramesLambda(t *testing.T) {
	err := runWithFrames(t, "def h(f): f; h(.x[])")
	frames := stackTraceOf(t, err)
	if f := frames[0]; f.Kind != gojq.FrameLambda {
		t.Fatalf("innermost frame must be a lambda, got %+v", f)
	}
	if frames[1].Kind != gojq.FrameFunc || frames[1].Function != "h" {
		t.Fatalf("expected h caller frame, got %+v", frames[1])
	}
}

type stackGoError struct{}

func (stackGoError) Error() string { return "go error" }

func TestStackFramesCustomGoFunction(t *testing.T) {
	err := runWithFrames(t, "cgo",
		gojq.WithFunction("cgo", 0, 0, func(any, []any) any {
			return stackGoError{}
		}))
	frames := stackTraceOf(t, err)
	if f := frames[0]; f.Kind != gojq.FrameGoFunc || f.Function != "cgo" {
		t.Fatalf("expected a Go function frame for cgo, got %+v", f)
	}
	if f := frames[0]; f.Call.Start != (gojq.Pos{Line: 1, Column: 1}) {
		t.Fatalf("Go frame must report the call site, got %+v", f.Call)
	}
	if f := frames[len(frames)-1]; f.Kind != gojq.FrameTop {
		t.Fatalf("expected top frame, got %+v", f)
	}
}

func TestStackFramesBuiltinJqFunction(t *testing.T) {
	// map is defined in builtin.jq; its body iterates over null, which errors
	// before the user lambda is entered. The builtin frame still appears with
	// the user call site, but without a user-source definition range.
	err := runWithFrames(t, "map(.x[])")
	frames := stackTraceOf(t, err)
	var found bool
	for _, f := range frames {
		if f.Function == "map" {
			found = true
			if f.Kind != gojq.FrameFunc {
				t.Fatalf("builtin map must be a func frame, got %s", f.Kind)
			}
			if f.Def.Start != (gojq.Pos{}) {
				t.Fatalf("builtin frame has no user def range, got %+v", f.Def)
			}
			if f.Call.Start != (gojq.Pos{Line: 1, Column: 1}) {
				t.Fatalf("builtin frame must report the call site, got %+v", f.Call)
			}
		}
	}
	if !found {
		t.Fatalf("expected a map frame in %+v", frames)
	}
}

type stackValueError struct{}

func (*stackValueError) Error() string { return "value error" }
func (*stackValueError) Value() any    { return "the-value" }

func runOnce(t *testing.T, src string, opts ...gojq.CompilerOption) any {
	t.Helper()
	query, err := gojq.Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	code, err := gojq.Compile(query, append(opts, gojq.WithStackFrames())...)
	if err != nil {
		t.Fatal(err)
	}
	v, ok := code.Run(nil).Next()
	if !ok {
		t.Fatal("expected a value")
	}
	return v
}

func TestStackFramesTryCatchKeepsJqValue(t *testing.T) {
	// A ValueError raised by a Go function is caught as its value, unchanged.
	got := runOnce(t, "try cgo catch .",
		gojq.WithFunction("cgo", 0, 0, func(any, []any) any {
			return &stackValueError{}
		}))
	if got != "the-value" {
		t.Fatalf("caught value mismatch: %#v", got)
	}
	// A plain error is caught as its error string.
	got = runOnce(t, "try cgo catch .",
		gojq.WithFunction("cgo", 0, 0, func(any, []any) any {
			return stackGoError{}
		}))
	if got != "go error" {
		t.Fatalf("caught plain error must become a string: %#v", got)
	}
	// error(...) is caught as its argument.
	if got := runOnce(t, `try error("boom") catch .`); got != "boom" {
		t.Fatalf("caught error() mismatch: %#v", got)
	}
}

func TestStackFramesHaltErrorNotCaught(t *testing.T) {
	v := runOnce(t, `try halt_error(7) catch "caught"`)
	e, ok := v.(error)
	if !ok {
		t.Fatalf("halt_error must propagate, got %#v", v)
	}
	var halt *gojq.HaltError
	if !errors.As(e, &halt) {
		t.Fatalf("expected *HaltError, got %T: %v", e, e)
	}
	if halt.ExitCode() != 7 {
		t.Fatalf("exit code mismatch: %d", halt.ExitCode())
	}
	frames := stackTraceOf(t, e)
	if len(frames) == 0 || frames[0].Kind != gojq.FrameGoFunc {
		t.Fatalf("halt_error trace must start at the halt_error Go frame: %+v", frames)
	}
}

type inMemoryModuleLoader map[string]string

func (l inMemoryModuleLoader) LoadInitModules() ([]*gojq.Query, error) { return nil, nil }
func (l inMemoryModuleLoader) LoadModule(name string) (*gojq.Query, error) {
	src, ok := l[name]
	if !ok {
		return nil, nil
	}
	return gojq.Parse(src)
}
func (l inMemoryModuleLoader) LoadModuleWithMeta(name string, _ map[string]any) (*gojq.Query, error) {
	return l.LoadModule(name)
}

func TestStackFramesModuleFunction(t *testing.T) {
	query, err := gojq.Parse(`import "mylib" as m; m::f`)
	if err != nil {
		t.Fatal(err)
	}
	loader := inMemoryModuleLoader{"mylib": "def f: .x[];"}
	code, err := gojq.Compile(query,
		gojq.WithStackFrames(), gojq.WithModuleLoader(loader))
	if err != nil {
		t.Fatal(err)
	}
	v, _ := code.Run(nil).Next()
	e, ok := v.(error)
	if !ok {
		t.Fatal("expected an error")
	}
	frames := stackTraceOf(t, e)
	if f := frames[0]; f.Kind != gojq.FrameFunc || f.Function != "f" ||
		f.Module != "m" || f.Def.File != "mylib" {
		t.Fatalf("unexpected module frame: %+v", f)
	}
}

func TestStackFramesNestedModule(t *testing.T) {
	query, err := gojq.Parse(`import "a" as x; x::f`)
	if err != nil {
		t.Fatal(err)
	}
	loader := inMemoryModuleLoader{
		"a": `import "b" as y; def f: y::g;`,
		"b": `def g: .z[];`,
	}
	code, err := gojq.Compile(query,
		gojq.WithStackFrames(), gojq.WithModuleLoader(loader))
	if err != nil {
		t.Fatal(err)
	}
	v, _ := code.Run(nil).Next()
	e, ok := v.(error)
	if !ok {
		t.Fatal("expected an error")
	}
	frames := stackTraceOf(t, e)
	if f := frames[0]; f.Module != "x::y" || f.Function != "g" || f.Def.File != "b" {
		t.Fatalf("unexpected innermost nested-module frame: %+v", f)
	}
	if f := frames[1]; f.Module != "x" || f.Function != "f" || f.Def.File != "a" {
		t.Fatalf("unexpected outer module frame: %+v", f)
	}
}

func TestStackFramesDisabledByDefault(t *testing.T) {
	query, err := gojq.Parse("def g: .x[]; g")
	if err != nil {
		t.Fatal(err)
	}
	code, err := gojq.Compile(query) // no WithStackFrames
	if err != nil {
		t.Fatal(err)
	}
	v, _ := code.Run(nil).Next()
	e, ok := v.(error)
	if !ok {
		t.Fatal("expected an error")
	}
	var tr stackTracer
	if errors.As(e, &tr) {
		t.Fatalf("no stack trace must be attached without the option: %v", e)
	}
	if e.Error() != "cannot iterate over: null" {
		t.Fatalf("unexpected message: %v", e)
	}
}

func TestStackFramesUnwrapsOriginalError(t *testing.T) {
	err := runWithFrames(t, "def g: .x[]; g")
	var target interface{ Unwrap() error }
	if !errors.As(err, &target) {
		t.Fatal("stack error must wrap the original error")
	}
}

func TestStackFramesDoesNotFakeValueError(t *testing.T) {
	// A plain type error does not implement ValueError, and the stack wrapper
	// must not change that; only genuine value errors (error(...)) do.
	typeErr := runWithFrames(t, ".x[]")
	var ve gojq.ValueError
	if errors.As(typeErr, &ve) {
		t.Fatalf("plain type error must not satisfy ValueError: %v", typeErr)
	}
	valueErr := runWithFrames(t, `error("boom")`)
	if !errors.As(valueErr, &ve) {
		t.Fatal("error(...) must satisfy ValueError through Unwrap")
	}
	if ve.Value() != "boom" {
		t.Fatalf("unexpected value: %#v", ve.Value())
	}
}

func TestStackFramesCatchDoesNotLeakGoFrame(t *testing.T) {
	// The Go function fails and is caught; then the catch handler raises a
	// pure jq error. That later error must not carry the swallowed Go frame.
	query, err := gojq.Parse("(try cgo catch null) | .x[]")
	if err != nil {
		t.Fatal(err)
	}
	code, err := gojq.Compile(query, gojq.WithStackFrames(),
		gojq.WithFunction("cgo", 0, 0, func(any, []any) any {
			return stackGoError{}
		}))
	if err != nil {
		t.Fatal(err)
	}
	v, _ := code.Run(nil).Next()
	e, ok := v.(error)
	if !ok {
		t.Fatal("expected an error from the catch handler")
	}
	if e.Error() != "cannot iterate over: null" {
		t.Fatalf("unexpected error: %v", e)
	}
	for _, f := range stackTraceOf(t, e) {
		if f.Kind == gojq.FrameGoFunc {
			t.Fatalf("swallowed Go frame leaked into later error: %+v", f)
		}
	}
}
