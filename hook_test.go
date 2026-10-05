package lua

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

type hookRecord struct {
	Type   HookEventType
	Line   int
	Fn     *LFunction
	Source string
	Co     *LState
}

func hookRecorder(events *[]hookRecord) HookFunc {
	return func(L *LState, ev *HookEvent) {
		*events = append(*events, hookRecord{ev.Type, ev.Line, ev.Function, ev.Source, ev.Coroutine})
	}
}

func eventTypes(events []hookRecord) []HookEventType {
	types := make([]HookEventType, len(events))
	for i, ev := range events {
		types[i] = ev.Type
	}
	return types
}

func countLineEvents(events []hookRecord, line int) int {
	n := 0
	for _, ev := range events {
		if ev.Type == HookEventLine && ev.Line == line {
			n++
		}
	}
	return n
}

func TestHookLineEvents(t *testing.T) {
	L := NewState()
	defer L.Close()

	var events []hookRecord
	L.SetHook(hookRecorder(&events), HookMaskLine)
	err := L.DoString(`
local t = 0
for i = 1, 3 do
  t = t + i
end
return t
`)
	errorIfNotNil(t, err)
	errorIfFalse(t, countLineEvents(events, 4) == 3, "expected 3 line events for line 4, got %v (%v)", countLineEvents(events, 4), eventTypes(events))
	errorIfFalse(t, countLineEvents(events, 6) == 1, "expected 1 line event for line 6, got %v", countLineEvents(events, 6))
	for _, ev := range events {
		errorIfFalse(t, ev.Co == L, "expected coroutine %v, got %v", L, ev.Co)
		errorIfFalse(t, ev.Source == "<string>", "expected source <string>, got %v", ev.Source)
	}
}

func TestHookLineEventsSameLineLoop(t *testing.T) {
	L := NewState()
	defer L.Close()

	var events []hookRecord
	L.SetHook(hookRecorder(&events), HookMaskLine)
	// A loop written on a single line must still be observed once per iteration.
	errorIfNotNil(t, L.DoString(`for i = 1, 5 do end`))
	errorIfFalse(t, countLineEvents(events, 1) >= 5, "expected at least 5 line events for a single-line loop, got %v", countLineEvents(events, 1))
}

func TestHookCallReturnOrder(t *testing.T) {
	L := NewState()
	defer L.Close()

	var events []hookRecord
	L.SetHook(hookRecorder(&events), HookMaskCall|HookMaskReturn)
	errorIfNotNil(t, L.DoString(`
local function a() return 1 end
local function b() local x = a() return x end
b()
`))
	expected := []HookEventType{
		HookEventCall,   // main chunk (host PCall)
		HookEventCall,   // b
		HookEventCall,   // a
		HookEventReturn, // a
		HookEventReturn, // b
		HookEventReturn, // main chunk
	}
	actual := eventTypes(events)
	errorIfFalse(t, reflect.DeepEqual(actual, expected), "expected %v, got %v", expected, actual)
	for _, ev := range events {
		errorIfFalse(t, ev.Co == L, "expected coroutine %v, got %v", L, ev.Co)
		errorIfFalse(t, ev.Source == "<string>", "expected source <string>, got %v", ev.Source)
	}
}

func TestHookGoFunctionEvents(t *testing.T) {
	L := NewState()
	defer L.Close()

	gofn := L.NewFunction(func(L *LState) int {
		L.Push(LNumber(1))
		return 1
	})
	L.SetGlobal("gofn", gofn)

	var events []hookRecord
	L.SetHook(hookRecorder(&events), HookMaskCall|HookMaskReturn)
	errorIfNotNil(t, L.DoString(`gofn()`))

	expected := []HookEventType{
		HookEventCall,   // main chunk
		HookEventCall,   // gofn
		HookEventReturn, // gofn
		HookEventReturn, // main chunk
	}
	actual := eventTypes(events)
	errorIfFalse(t, reflect.DeepEqual(actual, expected), "expected %v, got %v", expected, actual)
	errorIfFalse(t, events[1].Fn == gofn, "expected call event for gofn")
	errorIfFalse(t, events[1].Source == "=[Go]", "expected source =[Go], got %v", events[1].Source)
	errorIfFalse(t, events[1].Line == -1, "expected line -1 for a Go function, got %v", events[1].Line)
	errorIfFalse(t, events[2].Fn == gofn, "expected return event for gofn")
}

func TestHookTailCallEvents(t *testing.T) {
	L := NewState()
	defer L.Close()

	var events []hookRecord
	L.SetHook(hookRecorder(&events), HookMaskCall|HookMaskReturn)
	errorIfNotNil(t, L.DoString(`
local function target() return 1 end
local function f() return target() end
f()
`))
	expected := []HookEventType{
		HookEventCall,     // main chunk
		HookEventCall,     // f
		HookEventTailCall, // f -> target
		HookEventReturn,   // target
		HookEventReturn,   // main chunk
	}
	actual := eventTypes(events)
	errorIfFalse(t, reflect.DeepEqual(actual, expected), "expected %v, got %v", expected, actual)
}

func TestHookGoTailCallEvents(t *testing.T) {
	L := NewState()
	defer L.Close()

	gofn := L.NewFunction(func(L *LState) int { return 0 })
	L.SetGlobal("gofn", gofn)

	var events []hookRecord
	L.SetHook(hookRecorder(&events), HookMaskCall|HookMaskReturn)
	errorIfNotNil(t, L.DoString(`
local function f() return gofn() end
f()
`))
	expected := []HookEventType{
		HookEventCall,     // main chunk
		HookEventCall,     // f
		HookEventTailCall, // f -> gofn
		HookEventCall,     // gofn
		HookEventReturn,   // gofn
		HookEventReturn,   // main chunk
	}
	actual := eventTypes(events)
	errorIfFalse(t, reflect.DeepEqual(actual, expected), "expected %v, got %v", expected, actual)
}

func TestHookCoroutineYieldResume(t *testing.T) {
	L := NewState()
	defer L.Close()

	co, _ := L.NewThread()
	var events []hookRecord
	co.SetHook(hookRecorder(&events), HookMaskCall|HookMaskReturn)

	fn, err := L.LoadString(`
coroutine.yield(1)
return 2
`)
	errorIfNotNil(t, err)

	state, err, _ := L.Resume(co, fn)
	errorIfNotNil(t, err)
	errorIfFalse(t, state == ResumeYield, "expected ResumeYield, got %v", state)

	// The yield produces a call event for the Go yield function but no
	// return event, and no call event for the resumed function itself.
	expected := []HookEventType{HookEventCall}
	actual := eventTypes(events)
	errorIfFalse(t, reflect.DeepEqual(actual, expected), "expected %v, got %v", expected, actual)

	state, err, _ = L.Resume(co, nil)
	errorIfNotNil(t, err)
	errorIfFalse(t, state == ResumeOK, "expected ResumeOK, got %v", state)

	// Resuming fires no new call event; the finishing function fires return.
	expected = []HookEventType{HookEventCall, HookEventReturn}
	actual = eventTypes(events)
	errorIfFalse(t, reflect.DeepEqual(actual, expected), "expected %v, got %v", expected, actual)
	for _, ev := range events {
		errorIfFalse(t, ev.Co == co, "expected coroutine %v, got %v", co, ev.Co)
	}
}

func TestHookErrorUnwind(t *testing.T) {
	L := NewState()
	defer L.Close()

	var events []hookRecord
	L.SetHook(hookRecorder(&events), HookMaskCall|HookMaskReturn)
	errorIfNotNil(t, L.DoString(`
function inner() error("boom") end
function outer() inner() end
pcall(outer)
`))
	inner := L.GetGlobal("inner").(*LFunction)
	outer := L.GetGlobal("outer").(*LFunction)

	calledInner, calledOuter := false, false
	for _, ev := range events {
		if ev.Fn == inner {
			errorIfFalse(t, ev.Type == HookEventCall, "unexpected %v event for inner", ev.Type)
			calledInner = true
		}
		if ev.Fn == outer {
			errorIfFalse(t, ev.Type == HookEventCall, "unexpected %v event for outer", ev.Type)
			calledOuter = true
		}
	}
	errorIfFalse(t, calledInner, "expected a call event for inner")
	errorIfFalse(t, calledOuter, "expected a call event for outer")
}

func TestHookReentrantCallsAreNotObserved(t *testing.T) {
	L := NewState()
	defer L.Close()

	nested := L.NewFunction(func(L *LState) int { return 0 })
	reentrant := false
	inside := false
	L.SetHook(func(L *LState, ev *HookEvent) {
		if inside {
			reentrant = true
			return
		}
		inside = true
		L.CallByParam(P{Fn: nested, NRet: 0, Protect: true})
		inside = false
	}, HookMaskCall|HookMaskReturn|HookMaskLine)

	errorIfNotNil(t, L.DoString(`local x = 1`))
	errorIfFalse(t, !reentrant, "hook was invoked reentrantly")
}

func TestHookPanicTerminatesCurrentCall(t *testing.T) {
	L := NewState()
	defer L.Close()

	L.SetHook(func(L *LState, ev *HookEvent) {
		panic("hook failure")
	}, HookMaskLine)

	err := L.DoString(`local x = 1`)
	errorIfNil(t, err)
	errorIfFalse(t, strings.Contains(err.Error(), "hook failure"), "expected hook failure error, got %v", err)

	// Observation is re-armed after a controlled termination.
	count := 0
	L.SetHook(func(L *LState, ev *HookEvent) { count++ }, HookMaskLine)
	errorIfNotNil(t, L.DoString(`local x = 1`))
	errorIfFalse(t, count > 0, "expected hook to be re-armed after a panic")
}

func TestHookDisableRestoresFastPath(t *testing.T) {
	L := NewState()
	defer L.Close()

	fastPath := reflect.ValueOf(mainLoop).Pointer()
	hookPath := reflect.ValueOf(mainLoopWithHook).Pointer()
	contextPath := reflect.ValueOf(mainLoopWithContext).Pointer()

	errorIfFalse(t, reflect.ValueOf(L.mainLoop).Pointer() == fastPath, "expected the fast main loop by default")

	L.SetHook(func(L *LState, ev *HookEvent) {}, HookMaskCall|HookMaskLine)
	errorIfFalse(t, reflect.ValueOf(L.mainLoop).Pointer() == hookPath, "expected the hook main loop")

	L.SetHook(nil, 0)
	fn, mask := L.GetHook()
	errorIfFalse(t, fn == nil && mask == 0, "expected hook to be disabled")
	errorIfFalse(t, reflect.ValueOf(L.mainLoop).Pointer() == fastPath, "expected the fast main loop to be restored")

	// Context and hook selection compose.
	L.SetContext(context.Background())
	errorIfFalse(t, reflect.ValueOf(L.mainLoop).Pointer() == contextPath, "expected the context main loop")
	L.SetHook(func(L *LState, ev *HookEvent) {}, HookMaskLine)
	errorIfFalse(t, reflect.ValueOf(L.mainLoop).Pointer() == hookPath, "expected the hook main loop")
	L.RemoveContext()
	errorIfFalse(t, reflect.ValueOf(L.mainLoop).Pointer() == hookPath, "expected the hook main loop")
	L.SetHook(nil, 0)
	errorIfFalse(t, reflect.ValueOf(L.mainLoop).Pointer() == fastPath, "expected the fast main loop to be restored")

	// No events are delivered after disabling.
	events := 0
	L.SetHook(func(L *LState, ev *HookEvent) { events++ }, HookMaskCall|HookMaskReturn|HookMaskLine)
	L.SetHook(nil, 0)
	errorIfNotNil(t, L.DoString(`local x = 1`))
	errorIfFalse(t, events == 0, "expected no events after disabling the hook, got %v", events)
}
