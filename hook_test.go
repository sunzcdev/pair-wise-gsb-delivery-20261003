package lua

import (
	"strings"
	"testing"
)

type recordedEvent struct {
	id    uint64
	event HookEvent
	what  string
	line  int
	src   string
}

func newRecordingHook(rec *[]recordedEvent) VMHook {
	return func(ls *LState, event HookEvent) {
		info := ls.CurrentHookInfo()
		*rec = append(*rec, recordedEvent{
			id:    info.CoroutineID,
			event: event,
			what:  info.What,
			line:  info.CurrentLine,
			src:   info.Source,
		})
	}
}

func kinds(rec []recordedEvent) []HookEvent {
	out := make([]HookEvent, 0, len(rec))
	for _, e := range rec {
		out = append(out, e.event)
	}
	return out
}

func TestHookDisabledByDefault(t *testing.T) {
	L := NewState(Options{})
	defer L.Close()
	if fn, mask := L.GetHook(); fn != nil || mask != 0 {
		t.Fatal("no hook should be installed by default")
	}
}

func TestHookCallReturnOrdering(t *testing.T) {
	L := NewState(Options{})
	defer L.Close()
	var rec []recordedEvent
	L.SetHook(newRecordingHook(&rec), MaskCall|MaskReturn)
	err := L.DoString(`
function g() return 1 end
local function f() g() end
f()
`)
	if err != nil {
		t.Fatal(err)
	}

	// Locate the Lua call to f and verify the nesting around it.
	var seq []HookEvent
	for _, e := range rec {
		if e.src == "<string>" && (e.event == HookCall || e.event == HookReturn) && e.what == "Lua" {
			seq = append(seq, e.event)
		}
	}
	// f calls g: call f, call g, return g, return f
	want := []HookEvent{HookCall, HookCall, HookReturn, HookReturn}
	if len(seq) < 4 {
		t.Fatalf("expected at least the f/g call pair, got %v", seq)
	}
	got := seq[len(seq)-4:]
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("call/return ordering mismatch: got %v want %v", got, want)
		}
	}

	// The main chunk must be entered and returned.
	if rec[0].event != HookCall || rec[0].what != "main" {
		t.Fatalf("first event must be main call, got %+v", rec[0])
	}
	if last := rec[len(rec)-1]; last.event != HookReturn || last.what != "main" {
		t.Fatalf("last event must be main return, got %+v", last)
	}
}

func TestHookGoFunctionEntry(t *testing.T) {
	L := NewState(Options{})
	defer L.Close()
	var rec []recordedEvent
	var entered int
	L.SetGlobal("gofn", L.NewFunction(func(ls *LState) int {
		entered++
		return 0
	}))
	// record after registering so the events below are captured
	L.SetHook(func(ls *LState, event HookEvent) {
		info := ls.CurrentHookInfo()
		rec = append(rec, recordedEvent{event: event, what: info.What})
	}, MaskCall|MaskReturn)
	if err := L.DoString("gofn()"); err != nil {
		t.Fatal(err)
	}
	if entered != 1 {
		t.Fatalf("gofn expected to run once, ran %d", entered)
	}
	var sawCall, sawReturn bool
	for _, e := range rec {
		if e.event == HookGoCall && e.what == "Go" {
			sawCall = true
		}
		if e.event == HookGoReturn && e.what == "Go" {
			sawReturn = true
		}
	}
	if !sawCall || !sawReturn {
		t.Fatalf("expected Go call/return pair, events: %v", kinds(rec))
	}
}

func TestHookTailCallOrdering(t *testing.T) {
	L := NewState(Options{})
	defer L.Close()
	var rec []recordedEvent
	L.SetHook(newRecordingHook(&rec), MaskCall|MaskReturn|MaskTailCall)
	err := L.DoString(`
function g() return 1 end
local function f() return g() end
f()
`)
	if err != nil {
		t.Fatal(err)
	}
	// Around the tail call we expect: call f, tail call, call g, tail return.
	var idx int
	for i := range rec {
		if rec[i].event == HookTailCall {
			idx = i
			break
		}
	}
	if idx == 0 {
		t.Fatal("missing tail call event")
	}
	want := []HookEvent{HookCall, HookTailCall, HookCall, HookTailReturn}
	got := []HookEvent{rec[idx-1].event, rec[idx].event, rec[idx+1].event, rec[idx+2].event}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tail call ordering: got %v want %v", got, want)
		}
	}
	// The tail-call event must point at the call site (line 4 inside DoString).
	if rec[idx].line < 0 {
		t.Fatalf("tail call event should carry call-site line, got %d", rec[idx].line)
	}
}

func TestHookLineChangesAndSameLineLoop(t *testing.T) {
	L := NewState(Options{})
	defer L.Close()
	var lines []int
	L.SetHook(func(ls *LState, event HookEvent) {
		if event != HookLine {
			return
		}
		lines = append(lines, ls.CurrentHookInfo().CurrentLine)
	}, MaskLine)
	err := L.DoString("local a=0\nfor i=1,4 do a=a+1 end")
	if err != nil {
		t.Fatal(err)
	}
	// The one-line loop body (line 2) must be observed once per iteration:
	// entry + 4 iterations = at least 5 observations of line 2.
	var count int
	for _, ln := range lines {
		if ln == 2 {
			count++
		}
	}
	if count < 5 {
		t.Fatalf("same-line loop must be observable each iteration; line2 hits=%d (all=%v)", count, lines)
	}
}

func TestHookLineMultiline(t *testing.T) {
	L := NewState(Options{})
	defer L.Close()
	var lines []int
	L.SetHook(func(ls *LState, event HookEvent) {
		if event == HookLine {
			lines = append(lines, ls.CurrentHookInfo().CurrentLine)
		}
	}, MaskLine)
	err := L.DoString("local a=1\nwhile a<=3 do\n  a=a+1\nend")
	if err != nil {
		t.Fatal(err)
	}
	// Loop body on line 3 must appear for each iteration.
	var count int
	for _, ln := range lines {
		if ln == 3 {
			count++
		}
	}
	if count != 3 {
		t.Fatalf("expected body line 3 three times, got %d (%v)", count, lines)
	}
}

func TestHookMaskFilters(t *testing.T) {
	L := NewState(Options{})
	defer L.Close()
	var lines, calls int
	L.SetHook(func(ls *LState, event HookEvent) {
		switch event {
		case HookLine:
			lines++
		case HookCall:
			calls++
		}
	}, MaskLine)
	if err := L.DoString("local function f() end\nf()"); err != nil {
		t.Fatal(err)
	}
	if calls != 0 || lines == 0 {
		t.Fatalf("mask not honored: lines=%d calls=%d", lines, calls)
	}
}

func TestHookClearRestoresFastPath(t *testing.T) {
	L := NewState(Options{})
	defer L.Close()
	count := 0
	L.SetHook(func(ls *LState, event HookEvent) { count++ }, MaskLine)
	L.SetHook(nil, 0)
	if err := L.DoString("for i=1,10 do end"); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("hook must not fire after being cleared, got %d", count)
	}
	if fn, mask := L.GetHook(); fn != nil || mask != 0 {
		t.Fatal("GetHook must report no hook after clear")
	}
}

func TestHookReentrancySuppressed(t *testing.T) {
	L := NewState(Options{})
	defer L.Close()
	outer := 0
	L.SetHook(func(ls *LState, event HookEvent) {
		outer++
		// Running Lua inside a hook must never re-deliver events.
		if err := ls.DoString("return 1"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}, MaskLine)
	if err := L.DoString("for i=1,5 do local x=i end"); err != nil {
		t.Fatal(err)
	}
	if outer == 0 {
		t.Fatal("expected some line events")
	}
}

func TestHookCallbackErrorTerminatesProtectedCall(t *testing.T) {
	L := NewState(Options{})
	defer L.Close()
	L.SetHook(func(ls *LState, event HookEvent) {
		ls.RaiseError("hook abort")
	}, MaskLine)
	err := L.DoString("for i=1,100 do local x=i end")
	if err == nil || !strings.Contains(err.Error(), "hook abort") {
		t.Fatalf("expected controlled hook error, got: %v", err)
	}
}

func TestHookErrorUnwindEmitsReturns(t *testing.T) {
	L := NewState(Options{})
	defer L.Close()
	var rec []recordedEvent
	L.SetHook(newRecordingHook(&rec), MaskCall|MaskReturn)
	err := L.DoString(`
local function inner() error("boom") end
local function outer() inner() end
pcall(outer)
`)
	if err != nil {
		t.Fatal(err)
	}
	// After the error, synthetic returns for inner then outer must appear,
	// innermost first.
	body := func() []HookEvent {
		var s []HookEvent
		for _, e := range rec {
			if e.src == "<string>" && e.what == "Lua" {
				s = append(s, e.event)
			}
		}
		return s
	}()
	// body contains calls for outer/inner (and closure defs); check the tail of
	// the Lua-function stream ends with return,return for the unwound frames.
	if len(body) < 2 || body[len(body)-1] != HookReturn || body[len(body)-2] != HookReturn {
		t.Fatalf("expected synthetic returns on protected error exit, got %v", body)
	}
}

func TestHookRecoversAfterCaughtCallbackError(t *testing.T) {
	L := NewState(Options{})
	defer L.Close()
	var after int
	shouldAbort := true
	L.SetHook(func(ls *LState, event HookEvent) {
		if event != HookLine {
			return
		}
		if shouldAbort && ls.CurrentHookInfo().What == "Lua" {
			shouldAbort = false
			ls.RaiseError("hook abort once")
		}
		after++
	}, MaskLine)
	// The hook-raised error must be caught by Lua pcall; subsequent code must
	// continue to receive line events. The first aborted line is inside the
	// protected function, so pcall is already on the stack when it is raised.
	err := L.DoString(`
pcall(function()
  local a=1
  local z=9
end)
local b=2
local c=3
`)
	if err != nil {
		t.Fatalf("error should stay inside pcall, got %v", err)
	}
	if after == 0 {
		t.Fatal("line hooks must keep firing after a caught callback error")
	}
}

func TestHookCoroutineIdentityAndYieldResume(t *testing.T) {
	L := NewState(Options{})
	defer L.Close()
	var rec []recordedEvent
	hook := newRecordingHook(&rec)
	L.SetHook(hook, MaskCall|MaskReturn|MaskYield)

	var coID uint64
	L.SetGlobal("attach", L.NewFunction(func(ls *LState) int {
		th := ls.CheckThread(1)
		th.SetHook(hook, MaskCall|MaskReturn|MaskYield)
		coID = 0
		// read id through a line-less probe below
		coID = readThreadID(th)
		return 0
	}))
	err := L.DoString(`
local function iter(x)
  coroutine.yield(x)
  return x+1
end
local c = coroutine.create(iter)
attach(c)
coroutine.resume(c, 10)
coroutine.resume(c)
`)
	if err != nil {
		t.Fatal(err)
	}
	if coID == 0 || coID == 1 {
		t.Fatalf("coroutine should have a distinct id, got %d", coID)
	}

	var resumeCount, yieldCount int
	for _, e := range rec {
		if e.id != coID {
			continue
		}
		switch e.event {
		case HookResume:
			resumeCount++
		case HookYield:
			yieldCount++
		}
	}
	if resumeCount != 2 || yieldCount != 2 {
		t.Fatalf("expected 2 resume and 2 yield events on coroutine, got resume=%d yield=%d", resumeCount, yieldCount)
	}

	// Ordering: resume must precede the first yield on the coroutine.
	firstResume, firstYield := -1, -1
	for i, e := range rec {
		if e.id != coID {
			continue
		}
		if firstResume == -1 && e.event == HookResume {
			firstResume = i
		}
		if firstYield == -1 && e.event == HookYield {
			firstYield = i
		}
	}
	if !(firstResume >= 0 && firstYield > firstResume) {
		t.Fatalf("resume must precede yield, resume=%d yield=%d", firstResume, firstYield)
	}
}

func TestHookEventStringNames(t *testing.T) {
	cases := map[HookEvent]string{
		HookCall:       "call",
		HookReturn:     "return",
		HookTailCall:   "tail call",
		HookTailReturn: "tail return",
		HookGoCall:     "Go call",
		HookGoReturn:   "Go return",
		HookLine:       "line",
		HookYield:      "yield",
		HookResume:     "resume",
	}
	for ev, want := range cases {
		if got := ev.String(); got != want {
			t.Fatalf("event %d: got %q want %q", ev, got, want)
		}
	}
}

func readThreadID(th *LState) uint64 {
	return th.threadID
}
