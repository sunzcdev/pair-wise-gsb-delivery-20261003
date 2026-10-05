package lua

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

var errGoSentinel = errors.New("the go sentinel error")

type goTypedError struct {
	code int
	msg  string
}

func (e *goTypedError) Error() string { return e.msg }

type wrappedGoError struct {
	inner error
}

func (e *wrappedGoError) Error() string { return "wrapped: " + e.inner.Error() }
func (e *wrappedGoError) Unwrap() error { return e.inner }

func newStateWithFail(t *testing.T, name string) (*LState, *LFunction) {
	t.Helper()
	L := NewState()
	L.Register(name, func(L *LState) int {
		L.RaiseGoError(errGoSentinel)
		return 0
	})
	return L, L.GetGlobal(name).(*LFunction)
}

func TestRaiseGoErrorPCallChain(t *testing.T) {
	L := NewState()
	defer L.Close()

	root := &goTypedError{code: 42, msg: "typed cause"}
	wrap := &wrappedGoError{inner: root}
	L.Register("boom", func(L *LState) int {
		L.RaiseGoError(wrap)
		return 0
	})

	L.Push(L.GetGlobal("boom"))
	err := L.PCall(0, 0, nil)
	if err == nil {
		t.Fatal("expected an error from PCall")
	}
	if !errors.Is(err, root) {
		t.Fatalf("errors.Is should reach the root cause: %v", err)
	}
	if errors.Is(err, errGoSentinel) {
		t.Fatalf("errors.Is must not report an unrelated sentinel: %v", err)
	}
	var typed *goTypedError
	if !errors.As(err, &typed) {
		t.Fatalf("errors.As should extract the typed cause: %v", err)
	}
	if typed.code != 42 {
		t.Fatalf("unwrapped typed error has wrong code: %d", typed.code)
	}
	var lerr *LGoError
	if !errors.As(err, &lerr) {
		t.Fatalf("errors.As should extract the *LGoError object: %T %v", err, err)
	}
	if lerr.Err != wrap {
		t.Fatalf("LGoError must carry the exact wrapped error")
	}
	if !strings.Contains(err.Error(), "wrapped: typed cause") {
		t.Fatalf("error text should contain the safe message, got %q", err.Error())
	}
}

func TestRaiseGoErrorCallByParamChain(t *testing.T) {
	L := NewState()
	defer L.Close()
	L.Register("boom", func(L *LState) int {
		L.RaiseGoError(&wrappedGoError{inner: errGoSentinel})
		return 0
	})
	fn := L.GetGlobal("boom").(*LFunction)
	err := L.CallByParam(P{Fn: fn, Protect: true, NRet: 0})
	if err == nil {
		t.Fatal("expected CallByParam to fail")
	}
	if !errors.Is(err, errGoSentinel) {
		t.Fatalf("CallByParam error must preserve the chain: %v", err)
	}
}

func TestLuaPCallForwardsGoErrorValue(t *testing.T) {
	L, fn := newStateWithFail(t, "go_fail")
	defer L.Close()

	// Lua pcall forwards the opaque value; tostring gives the safe text.
	if err := L.DoString(`
		local ok, err = pcall(go_fail)
		assert(ok == false)
		assert(type(err) == "userdata")
		assert(tostring(err) == "the go sentinel error")
	`); err != nil {
		t.Fatal(err)
	}

	// The same value propagates through nested protected calls and keeps
	// its identity and chain when it finally reaches the host.
	L.Push(fn)
	if err := L.PCall(0, 0, nil); err == nil || !errors.Is(err, errGoSentinel) {
		t.Fatalf("nested Lua pcall must not break the chain: %v", err)
	}
}

func TestNestedPCallPreservesChain(t *testing.T) {
	L, _ := newStateWithFail(t, "go_fail")
	defer L.Close()
	if err := L.DoString(`
		function level3() go_fail() end
		function level2()
			local ok, err = pcall(level3)
			assert(ok == false)
			error(err) -- re-raise the original value
		end
		function level1()
			local ok, err = pcall(level2)
			assert(ok == false)
			error(err)
		end
		level1()
	`); err == nil {
		t.Fatal("expected failure to reach the host")
	} else if !errors.Is(err, errGoSentinel) {
		t.Fatalf("re-raised go error must keep the chain: %v", err)
	}
}

func TestScriptReplacingErrorBreaksChain(t *testing.T) {
	L, fn := newStateWithFail(t, "go_fail")
	defer L.Close()

	// Replace the opaque value with a plain string: the host sees the
	// string but must not be able to pretend the original chain exists.
	L.Push(fn)
	err := L.PCall(0, 0, L.NewFunction(func(L *LState) int {
		L.Push(LString("handled in script: " + L.CheckAny(1).String()))
		return 1
	}))
	if err == nil {
		t.Fatal("handler-returned string still fails the call")
	}
	if errors.Is(err, errGoSentinel) {
		t.Fatalf("a plain string replacement must not match the original cause: %v", err)
	}
	if !strings.Contains(err.Error(), "handled in script: the go sentinel error") {
		t.Fatalf("host should see the replacement text: %v", err)
	}

	// Replacing the value inside Lua itself also breaks the chain.
	L.SetGlobal("go_fail_fn", fn)
	err = L.DoString(`
		local ok, err = pcall(go_fail_fn)
		assert(ok == false)
		error("lua replaced it")
	`)
	if err == nil {
		t.Fatal("expected failure")
	}
	if errors.Is(err, errGoSentinel) {
		t.Fatalf("string error must not keep the chain: %v", err)
	}
	if !strings.Contains(err.Error(), "lua replaced it") {
		t.Fatalf("unexpected message: %v", err)
	}
}

func TestXPCallHandlerFailsAgain(t *testing.T) {
	L := NewState()
	defer L.Close()

	handlerErr := errors.New("handler exploded")
	L.Register("go_fail", func(L *LState) int {
		L.RaiseGoError(errGoSentinel)
		return 0
	})

	// The message handler itself raises a fresh Go error: that error must
	// win and its chain must be the handler's, not the original one.
	handler := L.NewFunction(func(L *LState) int {
		L.RaiseGoError(handlerErr)
		return 1
	})
	L.Push(L.GetGlobal("go_fail"))
	err := L.PCall(0, 0, handler)
	if err == nil {
		t.Fatal("expected handler failure")
	}
	if !errors.Is(err, handlerErr) {
		t.Fatalf("errors.Is must reach the handler error: %v", err)
	}
	if errors.Is(err, errGoSentinel) {
		t.Fatalf("the original chain must be replaced by the handler failure: %v", err)
	}

	// Same behavior through Lua xpcall.
	err = L.DoString(`
		local ok, msg = xpcall(go_fail, function(e)
			assert(tostring(e) == "the go sentinel error")
			error("lua handler failed")
		end)
		assert(ok == false)
		assert(string.find(msg, "lua handler failed", 1, true) ~= nil)
`)
	if err != nil {
		t.Fatalf("xpcall result handling failed: %v", err)
	}
}

func TestXPCallHandlerCanForwardGoError(t *testing.T) {
	L := NewState()
	defer L.Close()
	L.Register("go_fail", func(L *LState) int {
		L.RaiseGoError(errGoSentinel)
		return 0
	})
	handler := L.NewFunction(func(L *LState) int {
		// forward the original opaque value unchanged
		L.Push(L.CheckAny(1))
		return 1
	})
	L.Push(L.GetGlobal("go_fail"))
	err := L.PCall(0, 0, handler)
	if err == nil {
		t.Fatal("expected failure")
	}
	if !errors.Is(err, errGoSentinel) {
		t.Fatalf("a forwarding handler must preserve the original chain: %v", err)
	}
}

func TestPlainLuaErrorsStillCompatible(t *testing.T) {
	L := NewState()
	defer L.Close()

	if err := L.DoString(`error("a plain lua error")`); err == nil {
		t.Fatal("expected plain error")
	} else if !strings.Contains(err.Error(), "a plain lua error") {
		t.Fatalf("plain error text changed: %v", err)
	} else if errors.Is(err, errGoSentinel) {
		t.Fatal("plain lua error must not match a Go sentinel")
	}

	var aerr *ApiError
	if err := L.DoString(`error("another")`); !errors.As(err, &aerr) {
		t.Fatalf("plain lua errors are still *ApiError: %T", err)
	} else if aerr.Type != ApiErrorRun {
		t.Fatalf("unexpected api error type: %v", aerr.Type)
	}

	// pcall returns (false, string) for string errors.
	if err := L.DoString(`
		local ok, msg = pcall(function() error("str boom") end)
		assert(ok == false)
		assert(type(msg) == "string")
		assert(string.find(msg, "str boom", 1, true) ~= nil)
	`); err != nil {
		t.Fatal(err)
	}

	// assert() style errors keep working too.
	if err := L.DoString(`assert(false, "assertion text")`); err == nil ||
		!strings.Contains(err.Error(), "assertion text") {
		t.Fatalf("assert error mismatch: %v", err)
	}
}

func TestGoPanicErrorPreservesChain(t *testing.T) {
	L := NewState()
	defer L.Close()
	root := &goTypedError{code: 7, msg: "panicked cause"}
	L.Register("panic_err", func(L *LState) int {
		panic(&wrappedGoError{inner: root})
	})
	L.Push(L.GetGlobal("panic_err"))
	err := L.PCall(0, 0, nil)
	if err == nil {
		t.Fatal("expected panic to be caught")
	}
	if !errors.Is(err, root) {
		t.Fatalf("a panicking Go error should keep its chain: %v", err)
	}
	var typed *goTypedError
	if !errors.As(err, &typed) || typed.code != 7 {
		t.Fatalf("errors.As failed for a panicking error: %v", err)
	}
	if !strings.Contains(err.Error(), "wrapped: panicked cause") {
		t.Fatalf("panic error text mismatch: %q", err.Error())
	}
}

func TestGoErrorDoesNotRetainState(t *testing.T) {
	root := errors.New("stateless error")
	L := NewState()
	L.Register("go_fail", func(L *LState) int {
		L.RaiseGoError(root)
		return 0
	})
	L.Push(L.GetGlobal("go_fail"))
	err := L.PCall(0, 0, nil)
	L.Close()
	if err == nil || !errors.Is(err, root) {
		t.Fatalf("chain must survive LState close: %v", err)
	}
	var lerr *LGoError
	if !errors.As(err, &lerr) {
		t.Fatal("expected LGoError")
	}
	if fmt.Sprintf("%T", lerr) != "*lua.LGoError" {
		t.Fatalf("unexpected concrete type %T", lerr)
	}
	if strings.Contains(fmt.Sprintf("%+v", lerr), "LState") {
		t.Fatalf("LGoError must not hold an *LState: %+v", lerr)
	}
}

func TestGoErrorMetatableTostring(t *testing.T) {
	L := NewState()
	defer L.Close()
	L.Register("go_fail", func(L *LState) int {
		L.RaiseGoError(errGoSentinel)
		return 0
	})
	if err := L.DoString(`
		local mt = { __tostring = function(e) return "custom rendering" end }
		local ok, e = pcall(go_fail)
		assert(ok == false)
		debug.setmetatable(e, mt)
		assert(tostring(e) == "custom rendering")
		assert(getmetatable(e) == mt)
	`); err != nil {
		t.Fatal(err)
	}
}

func TestGoErrorThroughCoroutine(t *testing.T) {
	L := NewState()
	defer L.Close()
	L.Register("go_fail", func(L *LState) int {
		L.RaiseGoError(&wrappedGoError{inner: errGoSentinel})
		return 0
	})
	// The Lua-side coroutine.resume receives the opaque value; re-raising
	// it through error() keeps the chain when it reaches the host.
	if err := L.DoString(`
		local co = coroutine.create(function() go_fail() end)
		local ok, e = coroutine.resume(co)
		assert(ok == false)
		assert(type(e) == "userdata")
		assert(tostring(e) == "wrapped: the go sentinel error")
		error(e)
	`); err == nil {
		t.Fatal("expected failure to reach the host")
	} else if !errors.Is(err, errGoSentinel) {
		t.Fatalf("coroutine error must preserve the chain: %v", err)
	}
}

func TestStringPanicStillRenderedAsText(t *testing.T) {
	L := NewState()
	defer L.Close()
	L.Register("str_panic", func(L *LState) int {
		panic("plain string panic")
	})
	L.Push(L.GetGlobal("str_panic"))
	err := L.PCall(0, 0, nil)
	if err == nil {
		t.Fatal("expected the panic to be converted to an error")
	}
	var aerr *ApiError
	if !errors.As(err, &aerr) || aerr.Type != ApiErrorPanic {
		t.Fatalf("string panics keep the ApiErrorPanic type: %v", err)
	}
	if !strings.Contains(err.Error(), "plain string panic") {
		t.Fatalf("unexpected text: %v", err)
	}
	if errors.Is(err, errGoSentinel) {
		t.Fatal("string panic must not match the go sentinel")
	}
}
