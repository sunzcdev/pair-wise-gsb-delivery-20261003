package lua

import (
	"errors"
	"strings"
	"testing"
)

var errSentinel = errors.New("sentinel failure")

type wrappedErr struct {
	msg   string
	cause error
}

func (e *wrappedErr) Error() string { return e.msg }
func (e *wrappedErr) Unwrap() error { return e.cause }

type asErr struct{ tag string }

func (e *asErr) Error() string { return "as failure: " + e.tag }

func TestGoErrorPCallUnwrapsCause(t *testing.T) {
	L := NewState()
	defer L.Close()
	L.Register("fail", func(L *LState) int {
		L.RaiseGoError(errSentinel)
		return 0
	})
	L.Push(L.GetGlobal("fail"))
	err := L.PCall(0, 0, nil)
	errorIfFalse(t, err != nil, "PCall must return the Go error")
	errorIfFalse(t, errors.Is(err, errSentinel), "errors.Is must reach the sentinel cause")
	errorIfFalse(t, strings.Contains(err.Error(), "sentinel failure"), "safe text must be preserved")
}

func TestGoErrorPCallErrorsAs(t *testing.T) {
	target := &asErr{tag: "x"}
	L := NewState()
	defer L.Close()
	L.Register("fail", func(L *LState) int {
		L.RaiseGoError(&wrappedErr{msg: "wrapped", cause: target})
		return 0
	})
	L.Push(L.GetGlobal("fail"))
	err := L.PCall(0, 0, nil)
	var got *asErr
	errorIfFalse(t, errors.As(err, &got), "errors.As must find the concrete cause")
	if got != nil && got.tag != "x" {
		t.Errorf("errors.As returned unexpected target %v", got)
	}
}

func TestGoErrorNestedProtectedCalls(t *testing.T) {
	L := NewState()
	defer L.Close()
	L.Register("fail", func(L *LState) int {
		L.RaiseGoError(errSentinel)
		return 0
	})
	err := L.DoString(`
		local ok, err = pcall(function()
			pcall(function() fail() end)
			fail()
		end)
		assert(ok == false)
		assert(type(err) == "error")
		assert(tostring(err) == "sentinel failure")
	`)
	errorIfFalse(t, err == nil, "script must succeed: %v", err)

	err = L.DoString(`
		local ok, err = pcall(function() fail() end)
		assert(ok == false)
		error(err)
	`)
	errorIfFalse(t, err != nil, "outer error must propagate")
	errorIfFalse(t, errors.Is(err, errSentinel), "error chain must survive nested pcall")
}

func TestGoErrorPcallReceivesValue(t *testing.T) {
	L := NewState()
	defer L.Close()
	L.Register("fail", func(L *LState) int {
		L.RaiseGoError(errSentinel)
		return 0
	})
	err := L.DoString(`
		local seen
		local ok, e = pcall(fail)
		assert(ok == false)
		assert(type(e) == "error")
		assert(tostring(e) == "sentinel failure")
		assert(e == e)
		seen = e
		assert(seen == e)
	`)
	errorIfFalse(t, err == nil, "script must succeed: %v", err)
}

func TestGoErrorXPCallPreservesChain(t *testing.T) {
	L := NewState()
	defer L.Close()
	L.Register("fail", func(L *LState) int {
		L.RaiseGoError(errSentinel)
		return 0
	})
	err := L.DoString(`
		local ok, e = xpcall(fail, function(e)
			assert(type(e) == "error")
			assert(tostring(e) == "sentinel failure")
			return e
		end)
		assert(ok == false)
		assert(type(e) == "error")
	`)
	errorIfFalse(t, err == nil, "script must succeed: %v", err)

	// At the host boundary, a PCall error handler that forwards the
	// value must keep the Go error chain intact.
	L.Push(L.GetGlobal("fail"))
	err = L.PCall(0, 0, L.NewFunction(func(L *LState) int {
		return 1
	}))
	errorIfFalse(t, err != nil, "xpcall error must reach the host")
	errorIfFalse(t, errors.Is(err, errSentinel), "xpcall must preserve the Go error chain")
}

func TestGoErrorHandlerFailure(t *testing.T) {
	handlerSentinel := errors.New("handler failure")
	L := NewState()
	defer L.Close()
	L.Register("fail", func(L *LState) int {
		L.RaiseGoError(errSentinel)
		return 0
	})
	L.Register("handler_fail", func(L *LState) int {
		L.RaiseGoError(handlerSentinel)
		return 0
	})

	L.Push(L.GetGlobal("fail"))
	err := L.PCall(0, MultRet, L.NewFunction(func(L *LState) int {
		L.Push(L.GetGlobal("handler_fail"))
		L.Call(0, MultRet)
		return 1
	}))
	errorIfFalse(t, err != nil, "handler failure must be returned")
	errorIfFalse(t, errors.Is(err, handlerSentinel), "handler error chain must be reported")
	errorIfFalse(t, !errors.Is(err, errSentinel), "the original chain must not impersonate the handler result")
}

func TestGoErrorXPCallHandlerStringFailure(t *testing.T) {
	L := NewState()
	defer L.Close()
	L.Register("fail", func(L *LState) int {
		L.RaiseGoError(errSentinel)
		return 0
	})
	err := L.DoString(`
		local ok, e = xpcall(fail, function(e) error("handler broke") end)
		assert(ok == false)
		assert(type(e) == "string")
		assert(string.find(e, "handler broke") ~= nil)
	`)
	errorIfFalse(t, err == nil, "script must succeed: %v", err)

	// A Lua xpcall catches a failing handler and reports its message as
	// the returned error, severing the Go chain.
	err = L.DoString(`
		local ok, e = xpcall(fail, function(e) error("handler broke") end)
		assert(ok == false)
		assert(type(e) == "string")
		assert(string.find(e, "handler broke") ~= nil)
	`)
	errorIfFalse(t, err == nil, "xpcall must return handler failure to the script: %v", err)

	// At the host boundary the same handler failure is reported without
	// impersonating the original Go chain.
	L.Push(L.GetGlobal("fail"))
	err = L.PCall(0, 0, L.NewFunction(func(L *LState) int {
		L.RaiseError("handler broke")
		return 1
	}))
	errorIfFalse(t, err != nil, "handler failure must reach the host")
	if err != nil {
		errorIfFalse(t, !errors.Is(err, errSentinel), "a handler string must not carry the Go chain")
		errorIfFalse(t, strings.Contains(err.Error(), "handler broke"), "handler text must be reported")
	}
}

func TestGoErrorStringReplacementSeversChain(t *testing.T) {
	L := NewState()
	defer L.Close()
	L.Register("fail", func(L *LState) int {
		L.RaiseGoError(errSentinel)
		return 0
	})
	err := L.DoString(`
		local ok, e = pcall(fail)
		assert(ok == false)
		local replaced = tostring(e)
		error(replaced)
	`)
	errorIfFalse(t, err != nil, "string error must reach the host")
	errorIfFalse(t, !errors.Is(err, errSentinel), "a replaced plain string must not keep the Go chain")
	errorIfFalse(t, strings.Contains(err.Error(), "sentinel failure"), "safe text remains visible")
}

func TestPlainLuaErrorCompatibility(t *testing.T) {
	L := NewState()
	defer L.Close()
	err := L.DoString(`error("lua boom")`)
	errorIfFalse(t, err != nil, "plain lua error must be returned")
	aerr, ok := err.(*ApiError)
	errorIfFalse(t, ok, "plain lua error is an *ApiError")
	if ok {
		_, isGoErr := aerr.Object.(*LGoError)
		errorIfFalse(t, !isGoErr, "plain lua string error must not be wrapped")
	}
	errorIfFalse(t, strings.Contains(err.Error(), "lua boom"), "lua error text preserved")

	err = L.DoString(`
		local ok, e = pcall(function() error({code = 7}) end)
		assert(ok == false)
		assert(type(e) == "table")
		assert(e.code == 7)
	`)
	errorIfFalse(t, err == nil, "non-string Lua error objects keep working: %v", err)
}

func TestGoErrorCallByParam(t *testing.T) {
	L := NewState()
	defer L.Close()
	fn := L.NewFunction(func(L *LState) int {
		L.RaiseGoError(&wrappedErr{msg: "outer", cause: errSentinel})
		return 0
	})
	err := L.CallByParam(P{Fn: fn, Protect: true, NRet: 0})
	errorIfFalse(t, err != nil, "CallByParam must return the error")
	errorIfFalse(t, errors.Is(err, errSentinel), "CallByParam must expose the chain")
}

func TestGoErrorGPCall(t *testing.T) {
	L := NewState()
	defer L.Close()
	err := L.GPCall(func(L *LState) int {
		L.RaiseGoError(errSentinel)
		return 0
	}, LNil)
	errorIfFalse(t, err != nil, "GPCall must return the error")
	errorIfFalse(t, errors.Is(err, errSentinel), "GPCall must expose the chain")
}

func TestNewGoErrorValue(t *testing.T) {
	e := NewGoError(errSentinel)
	gerr, ok := e.(*LGoError)
	errorIfFalse(t, ok, "NewGoError returns *LGoError")
	if ok {
		errorIfNotEqual(t, LTGoError, gerr.Type())
		errorIfNotEqual(t, "sentinel failure", gerr.String())
		errorIfFalse(t, errors.Is(gerr, errSentinel), "LGoError itself supports errors.Is")
	}
	errorIfFalse(t, NewGoError(nil) == LNil, "nil error maps to LNil")
}

func TestGoErrorDoesNotHoldState(t *testing.T) {
	L := NewState()
	var captured LValue
	L.Register("fail", func(L *LState) int {
		captured = NewGoError(errSentinel)
		L.Error(captured, 0)
		return 0
	})
	L.Push(L.GetGlobal("fail"))
	err := L.PCall(0, 0, nil)
	errorIfFalse(t, errors.Is(err, errSentinel), "chain available")
	L.Close()
	errorIfNotEqual(t, "sentinel failure", captured.String())
}

func TestGoErrorTypeString(t *testing.T) {
	errorIfNotEqual(t, "error", LTGoError.String())
	L := NewState()
	defer L.Close()
	L.Register("fail", func(L *LState) int {
		L.RaiseGoError(errSentinel)
		return 0
	})
	err := L.DoString(`
		local ok, e = pcall(fail)
		assert(type(e) == "error")
	`)
	errorIfFalse(t, err == nil, "type() must report 'error': %v", err)
}

func TestGoErrorCoroutine(t *testing.T) {
	L := NewState()
	defer L.Close()
	L.Register("fail", func(L *LState) int {
		L.RaiseGoError(errSentinel)
		return 0
	})
	err := L.DoString(`
		local co = coroutine.create(function() fail() end)
		local ok, e = coroutine.resume(co)
		assert(ok == false)
		assert(type(e) == "error")
		assert(tostring(e) == "sentinel failure")
	`)
	errorIfFalse(t, err == nil, "coroutine must surface the error value: %v", err)

	co, _ := L.NewThread()
	fn, lerr := L.LoadString(`fail()`)
	errorIfFalse(t, lerr == nil, "load failed: %v", lerr)
	status, rerr, _ := L.Resume(co, fn)
	errorIfFalse(t, status == ResumeError, "coroutine must resume with error")
	errorIfFalse(t, rerr != nil, "resume must return error")
	if rerr != nil {
		errorIfFalse(t, errors.Is(rerr, errSentinel), "resume error must keep the Go chain")
	}
}

func TestGoErrorTostringMeta(t *testing.T) {
	L := NewState()
	defer L.Close()
	var captured LValue
	L.Register("fail", func(L *LState) int {
		captured = NewGoError(errSentinel)
		L.Error(captured, 0)
		return 0
	})
	err := L.DoString(`
		local ok, e = pcall(fail)
		assert(ok == false)
		assert(tostring(e) == "sentinel failure")
		print("raised:", e)
	`)
	errorIfFalse(t, err == nil, "tostring/print must work: %v", err)
	errorIfFalse(t, L.ToStringMeta(captured).String() == "sentinel failure", "ToStringMeta safe text")
}

func TestGoErrorWrapping(t *testing.T) {
	L := NewState()
	defer L.Close()
	inner := &asErr{tag: "deep"}
	middle := &wrappedErr{msg: "middle", cause: inner}
	outer := &wrappedErr{msg: "outer", cause: middle}
	L.Register("fail", func(L *LState) int {
		L.RaiseGoError(outer)
		return 0
	})
	L.Push(L.GetGlobal("fail"))
	err := L.PCall(0, 0, nil)
	errorIfFalse(t, errors.Is(err, outer), "outer wrapper reachable")
	errorIfFalse(t, errors.Is(err, middle), "middle wrapper reachable")
	var target *asErr
	errorIfFalse(t, errors.As(err, &target), "deep cause reachable via As")
	if target != nil {
		errorIfNotEqual(t, "deep", target.tag)
	}
}
