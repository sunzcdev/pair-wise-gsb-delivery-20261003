package lua

import (
	"context"
	"errors"
	"testing"
)

// makeYieldingClosure runs a coroutine that captures a scalar and a local
// table in a closure, writes into the table, yields the closure and leaves the
// coroutine suspended. The returned closure is shared with the caller.
func makeYieldingClosure(t *testing.T, L *LState) (*LState, *LFunction) {
	t.Helper()
	co, _ := L.NewThread()
	errorIfScriptFail(t, L, `
		function capturer()
			local n = 42
			local t = {1, 2, 3}
			local function get()
				return n, t
			end
			coroutine.yield(get)
			n = 99
			t[1] = 100
		end
	`)
	fn := L.GetGlobal("capturer").(*LFunction)
	st, err, values := L.Resume(co, fn)
	errorIfNotEqual(t, ResumeYield, st)
	errorIfNotNil(t, err)
	if len(values) != 1 {
		t.Fatalf("yield must return the closure, got %d values", len(values))
	}
	cl, ok := values[0].(*LFunction)
	if !ok {
		t.Fatalf("yielded value must be a function, got %T", values[0])
	}
	return co, cl
}

func TestCloseThreadUpvaluesKeepLastLocals(t *testing.T) {
	L := NewState()
	defer L.Close()
	co, cl := makeYieldingClosure(t, L)

	if err := L.CloseThread(co); err != nil {
		t.Fatalf("closing a suspended coroutine must succeed: %v", err)
	}

	// The escaped closure must still read the last locals captured at yield
	// time, even though the coroutine stack and registers have been released.
	if err := L.CallByParam(P{Fn: cl, NRet: 2, Protect: true}); err != nil {
		t.Fatalf("calling the escaped closure must not fail: %v", err)
	}
	errorIfNotEqual(t, LNumber(42), L.Get(-2).(LNumber))
	tbl := L.Get(-1).(*LTable)
	errorIfNotEqual(t, LNumber(1), tbl.RawGetInt(1).(LNumber))
	errorIfNotEqual(t, LNumber(2), tbl.RawGetInt(2).(LNumber))
	errorIfNotEqual(t, LNumber(3), tbl.RawGetInt(3).(LNumber))
	L.Pop(2)

	// None of the closure upvalues may still point into released registers.
	for i := 0; i < len(cl.Upvalues); i++ {
		errorIfFalse(t, cl.Upvalues[i].IsClosed(), "upvalue must be closed")
		errorIfFalse(t, cl.Upvalues[i].reg == nil, "upvalue must not reference the freed registry")
	}
}

func TestCloseThreadReleasesStack(t *testing.T) {
	L := NewState()
	defer L.Close()
	co, _ := makeYieldingClosure(t, L)

	if err := L.CloseThread(co); err != nil {
		t.Fatalf("close failed: %v", err)
	}
	errorIfFalse(t, co.closed, "coroutine must be marked closed")
	errorIfFalse(t, co.Dead, "closed coroutine must be dead")
	errorIfFalse(t, co.IsClosed(), "IsClosed must report the released stack")
	errorIfFalse(t, co.stack == nil, "call stack must be released")
	errorIfFalse(t, co.reg == nil, "registry must be released")
	errorIfFalse(t, co.currentFrame == nil, "current frame must be cleared")
	errorIfFalse(t, co.uvcache == nil, "upvalue cache must be cleared")
}

func TestCloseThreadDeterministicStates(t *testing.T) {
	L := NewState()
	defer L.Close()

	// A never started coroutine can be closed without error.
	unstarted, _ := L.NewThread()
	errorIfNotNil(t, L.CloseThread(unstarted))
	errorIfFalse(t, unstarted.closed, "never started coroutine must be closed")

	// Repeated close is a no-op that succeeds.
	errorIfNotNil(t, L.CloseThread(unstarted))

	// An already finished coroutine can be closed once.
	done, _ := L.NewThread()
	errorIfScriptFail(t, L, `function noop_body() end`)
	fn := L.GetGlobal("noop_body").(*LFunction)
	st, _, _ := L.Resume(done, fn)
	errorIfNotEqual(t, ResumeOK, st)
	errorIfFalse(t, done.Dead, "coroutine must be dead after finishing")
	errorIfNotNil(t, L.CloseThread(done))
	errorIfNotNil(t, L.CloseThread(done))

	// A running coroutine cannot be torn down, neither from inside itself nor
	// from an active resume chain.
	running, _ := L.NewThread()
	tryClose := L.NewFunction(func(L *LState) int {
		if err := L.CloseThread(running); !errors.Is(err, ErrCannotCloseRunningThread) {
			t.Errorf("closing self must fail with ErrCannotCloseRunningThread, got %v", err)
		}
		return 0
	})
	L.SetGlobal("try_close_self", tryClose)
	errorIfScriptFail(t, L, `function running_body() try_close_self() end`)
	runFn := L.GetGlobal("running_body").(*LFunction)
	st, err, _ := L.Resume(running, runFn)
	errorIfNotEqual(t, ResumeOK, st)
	errorIfNotNil(t, err)
	errorIfFalse(t, !running.closed, "running coroutine must stay open after a failed close")
	errorIfFalse(t, running.Dead, "coroutine must be dead after its body finishes")
	errorIfNotNil(t, L.CloseThread(running))
}

func TestCloseThreadResumeAfterClose(t *testing.T) {
	L := NewState()
	defer L.Close()

	// Go API: Resume reports the identifiable closed error.
	co, _ := makeYieldingClosure(t, L)
	errorIfNotNil(t, L.CloseThread(co))
	fn := L.NewFunction(func(L *LState) int { return 0 })
	st, err, values := L.Resume(co, fn)
	errorIfNotEqual(t, ResumeError, st)
	errorIfFalse(t, errors.Is(err, ErrThreadClosed), "Resume must return ErrThreadClosed, got %v", err)
	errorIfFalse(t, values == nil, "no resume values expected after close")

	// Lua API: coroutine.resume returns false plus the same recognizable error.
	script := `
		local co = coroutine.create(function () coroutine.yield(1) end)
		assert(coroutine.resume(co) == true)
		close_test_close(co)
		local ok, err = coroutine.resume(co)
		assert(ok == false)
		assert(err == "attempt to resume a closed coroutine", err)
	`
	L.Register("close_test_close", func(L *LState) int {
		th := L.CheckThread(1)
		if err := L.CloseThread(th); err != nil {
			L.RaiseError(err.Error())
		}
		return 0
	})
	errorIfScriptFail(t, L, script)
}

func TestCloseThreadCancelsChildContext(t *testing.T) {
	L := NewState()
	defer L.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	L.SetContext(ctx)

	co, cocancel := L.NewThread()
	defer cocancel()
	L.Register("close_test_myyield", func(L *LState) int {
		return L.Yield(LNumber(1))
	})
	errorIfScriptFail(t, L, `
		function close_test_yielder()
			close_test_myyield()
		end
	`)
	fn := L.GetGlobal("close_test_yielder").(*LFunction)
	st, err, values := L.Resume(co, fn)
	errorIfNotEqual(t, ResumeYield, st)
	errorIfNotNil(t, err)
	errorIfNotEqual(t, LNumber(1), values[0].(LNumber))

	if co.Context() == nil || co.Context().Err() != nil {
		t.Fatalf("child context must be live before close")
	}
	errorIfNotNil(t, L.CloseThread(co))
	errorIfFalse(t, errors.Is(co.Context().Err(), context.Canceled), "child context must be canceled after close")

	st, err, _ = L.Resume(co, fn)
	errorIfNotEqual(t, ResumeError, st)
	errorIfFalse(t, errors.Is(err, ErrThreadClosed), "later Resume must fail with ErrThreadClosed")
}
