package lua

import "sync/atomic"

var threadIDCounter uint64

func nextThreadID() uint64 {
	return atomic.AddUint64(&threadIDCounter, 1)
}

// hookEnterFrame reports entry into the function of the current frame. It is
// invoked right after a frame has been pushed/initialized. Go functions and Lua
// functions produce distinct events.
func (ls *LState) hookEnterFrame() {
	cf := ls.currentFrame
	cf.LastLine = 0
	ls.fireHook(HookCall)
}

// hookLeaveFrame reports the normal return of a frame that is still the
// current one. tailcall selects the Lua "tail return" event.
func (ls *LState) hookLeaveFrame(tailcall bool) {
	if tailcall {
		ls.fireHook(HookTailReturn)
	} else {
		ls.fireHook(HookReturn)
	}
}

// hookTailCallEvent reports the tail-call control transfer while the current
// (caller) frame is still active. The matching callee entry event is delivered
// separately once the new frame is in place.
func (ls *LState) hookTailCallEvent() {
	if ls.hook != nil {
		ls.fireHook(HookTailCall)
	}
}

// hookResume reports that ls is about to (re)enter execution as a coroutine.
// It is invoked at each resume boundary before the coroutine's main loop runs.
func (ls *LState) hookResume() {
	if ls.hook != nil {
		ls.fireHook(HookResume)
	}
}

// HookEvent identifies the kind of observation event reported to a VMHook.
type HookEvent int

const (
	// HookCall fires when a Lua function is entered. A call to a Go function
	// is reported separately as HookGoCall.
	HookCall HookEvent = iota
	// HookReturn fires when a Lua function is about to return normally.
	HookReturn
	// HookTailCall fires at a Lua tail call. Semantically it is a return of
	// the current function combined with a call to the new one: a HookTailCall
	// event is emitted instead of the matching HookReturn/HookCall pair and the
	// call stack does not grow.
	HookTailCall
	// HookTailReturn fires when a function that was entered through a tail
	// call returns. It pairs with the HookCall that was suppressed at the tail
	// call site, matching the Lua "tail return" hook event.
	HookTailReturn
	// HookGoCall fires when a Go (native) function is entered.
	HookGoCall
	// HookGoReturn fires when a Go (native) function returns normally.
	HookGoReturn
	// HookLine fires immediately before a Lua instruction on a new source line
	// is executed, and also after a backward jump (so loops written on a single
	// line stay observable).
	HookLine
	// HookYield fires when the running coroutine yields to its parent.
	HookYield
	// HookResume fires when the running coroutine is resumed by its parent.
	HookResume
)

// String returns the canonical Lua name of the hook event.
func (e HookEvent) String() string {
	switch e {
	case HookCall:
		return "call"
	case HookReturn:
		return "return"
	case HookTailCall:
		return "tail call"
	case HookTailReturn:
		return "tail return"
	case HookGoCall:
		return "Go call"
	case HookGoReturn:
		return "Go return"
	case HookLine:
		return "line"
	case HookYield:
		return "yield"
	case HookResume:
		return "resume"
	default:
		return "unknown"
	}
}

// HookMask selects the events delivered to a VMHook.
type HookMask int

const (
	// MaskCall selects HookCall and HookGoCall events.
	MaskCall HookMask = 1 << iota
	// MaskReturn selects HookReturn, HookTailReturn and HookGoReturn events.
	MaskReturn
	// MaskTailCall selects HookTailCall events.
	MaskTailCall
	// MaskLine selects HookLine events.
	MaskLine
	// MaskYield selects HookYield and HookResume events.
	MaskYield
)

// MaskCallReturn is shorthand for MaskCall|MaskReturn.
const MaskCallReturn = MaskCall | MaskReturn

// Has reports whether the mask contains all of the bits in other.
func (m HookMask) Has(other HookMask) bool {
	return m&other == other
}

// VMHook is an observer callback installed by the Go host. It is used for
// sampling/profiling and must not change the running program: the LState must
// not be used to call Lua code or to alter the stack while a hook runs.
//
// Hooks are never re-entered: events generated while a hook is executing are
// not delivered. If a hook panics with an *ApiError (for example through
// LState.RaiseError), the protected call that contains the current Lua call is
// terminated with that error in the usual way; any other panic terminates the
// whole VM invocation through the thread Panic handler.
type VMHook func(ls *LState, event HookEvent)

// HookInfo describes the source location of a hook event. It is valid only
// for the duration of the hook callback.
type HookInfo struct {
	// Event is the kind of event being reported.
	Event HookEvent
	// Thread is the coroutine that generated the event. For the main thread it
	// is the LState passed to the hook; for coroutines it is the coroutine's
	// own LState.
	Thread *LState
	// CoroutineID is a stable, process-unique identifier for Thread. The main
	// thread is always 1.
	CoroutineID uint64
	// Fn is the function associated with the event. It is nil for line events
	// emitted on synthetic frames and is the Go function closure for Go events.
	Fn *LFunction
	// What is "Lua" for Lua functions, "Go" for Go functions and "main" for the
	// main chunk.
	What string
	// Source is the chunk source name (SourceName of the function prototype).
	Source string
	// LineDefined is the first line of the function definition. It is -1 for Go
	// functions.
	LineDefined int
	// LastLineDefined is the last line of the function definition. It is -1 for
	// Go functions.
	LastLineDefined int
	// CurrentLine is the line about to execute for line events, and the line of
	// the call/return site otherwise. It is -1 when no line is available.
	CurrentLine int
}

var hookEventMask = [...]HookMask{
	HookCall:       MaskCall,
	HookReturn:     MaskReturn,
	HookTailCall:   MaskTailCall,
	HookTailReturn: MaskReturn,
	HookGoCall:     MaskCall,
	HookGoReturn:   MaskReturn,
	HookLine:       MaskLine,
	HookYield:      MaskYield,
	HookResume:     MaskYield,
}

// SetHook installs fn as the observation hook for this (sub)thread, replacing
// any previously installed hook. The mask selects the reported events; a zero
// mask (or fn == nil) disables observation and restores the fast execution
// path.
//
// Hooks are per LState: install a hook on the main thread to observe it, and
// on a coroutine (typically right after it is created) to observe that
// coroutine.
func (ls *LState) SetHook(fn VMHook, mask HookMask) {
	if fn == nil || mask == 0 {
		ls.hook = nil
		ls.hookMask = 0
		ls.hookOn = false
		ls.hookDepth = 0
		return
	}
	ls.hook = fn
	ls.hookMask = mask
	ls.hookOn = true
}

// GetHook returns the currently installed hook and its event mask.
func (ls *LState) GetHook() (VMHook, HookMask) {
	return ls.hook, ls.hookMask
}

// CurrentHookInfo returns source information for the event being delivered. It
// is intended to be called from inside a VMHook and returns nil if no hook is
// running.
func (ls *LState) CurrentHookInfo() *HookInfo {
	if ls.hookInfo == nil {
		return nil
	}
	return ls.hookInfo
}

// frameLine returns the source line associated with the frame's current
// instruction, or -1 when it is not available.
func frameLine(cf *callFrame) int {
	if cf == nil || cf.Fn == nil || cf.Fn.IsG || cf.Fn.Proto == nil {
		return -1
	}
	pos := cf.Pc - 1
	if pos >= 0 && pos < len(cf.Fn.Proto.DbgSourcePositions) {
		return cf.Fn.Proto.DbgSourcePositions[pos]
	}
	return -1
}

// fireHook delivers an event to the installed hook when it is selected by the
// mask. The CurrentLine field of the delivered HookInfo is taken from the
// current frame's instruction unless the caller provides an explicit line.
//
// Nested delivery (a hook that runs Lua code, or a hook that itself triggers
// events) is suppressed: hooks are observation-only and must never be
// re-entered. A panic raised by the callback (typically via RaiseError) is
// intentionally allowed to propagate so that protected calls unwind normally.
func (ls *LState) fireHook(event HookEvent) {
	if ls.hook == nil || ls.hookDepth != 0 {
		return
	}
	mask := hookEventMask[event]
	if ls.hookMask&mask == 0 {
		return
	}

	cf := ls.currentFrame
	info := &HookInfo{
		Event:           event,
		Thread:          ls,
		CoroutineID:     ls.threadID,
		CurrentLine:     -1,
		LineDefined:     -1,
		LastLineDefined: -1,
	}

	if cf != nil {
		fn := cf.Fn
		info.Fn = fn
		if fn != nil {
			if fn.IsG {
				info.What = "Go"
				info.Source = "[Go]"
			} else if fn.Proto != nil {
				if fn.Proto.LineDefined == 0 {
					info.What = "main"
				} else {
					info.What = "Lua"
				}
				info.Source = fn.Proto.SourceName
				info.LineDefined = fn.Proto.LineDefined
				info.LastLineDefined = fn.Proto.LastLineDefined
			}
		}
	}
	switch event {
	case HookLine, HookYield, HookResume:
		info.CurrentLine = frameLine(cf)
	case HookTailCall, HookReturn, HookTailReturn:
		// These concern the current (returning/tail-calling) frame's site.
		info.CurrentLine = frameLine(cf)
	default:
		// HookCall and HookGoCall report the call site in the caller.
		if cf != nil && cf.CallLine != 0 {
			info.CurrentLine = cf.CallLine
		} else {
			info.CurrentLine = frameLine(cf)
		}
	}

	ls.hookInfo = info
	ls.hookDepth = 1
	func() {
		// Reset observation state even if the callback panics. A hook-raised
		// error caught by an enclosing pcall must not silence later events.
		defer func() {
			ls.hookDepth = 0
			ls.hookInfo = nil
		}()
		ls.hook(ls, event)
	}()
}

// lineHook fires a line event for the current instruction when the source line
// changed, or when force is true (a backward jump landed on the same line). It
// is invoked once per dispatched instruction.
func (ls *LState) lineHook(force bool) {
	if ls.hookMask&MaskLine != 0 {
		cf := ls.currentFrame
		if cf != nil && cf.Fn != nil && !cf.Fn.IsG {
			pos := cf.Pc - 1
			// No instruction fetched yet (a frame that was just entered): the
			// first dispatched instruction reports the entry line itself.
			if pos < 0 || pos >= len(cf.Fn.Proto.DbgSourcePositions) {
				return
			}
			line := cf.Fn.Proto.DbgSourcePositions[pos]
			if force || line != cf.LastLine {
				cf.LastLine = line
				ls.fireHook(HookLine)
				return
			}
			cf.LastLine = line
		}
	}
}

// unwindHooks emits return events for the Lua and Go frames abandoned by an
// error propagating through the protected call that starts at stack depth sp.
// Events are delivered from the innermost frame outward, mirroring the order in
// which normal returns happen. It does not alter the stack.
func (ls *LState) unwindHooks(sp int) {
	if ls.hook == nil || ls.hookMask&MaskReturn == 0 {
		return
	}
	for idx := ls.stack.Sp() - 1; idx >= sp; idx-- {
		cf := ls.stack.At(idx)
		if cf == nil || cf.Fn == nil {
			continue
		}
		ls.currentFrame = cf
		if cf.Fn.IsG {
			ls.fireHook(HookGoReturn)
		} else if cf.TailCall > 0 {
			ls.fireHook(HookTailReturn)
		} else {
			ls.fireHook(HookReturn)
		}
	}
}
