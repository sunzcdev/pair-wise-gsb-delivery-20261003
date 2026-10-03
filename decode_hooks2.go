package mapstructure

import (
	"reflect"
)

// HookStatus describes the outcome of a [DecodeHook2Func] call.
//
// The new hook protocol makes the three possible outcomes explicit so
// combinators never have to guess whether a hook handled its input (for
// example by comparing the returned value with the input).
type HookStatus int

const (
	// StatusNotApplicable means the hook does not apply to the given input.
	// Combinators may try other hooks or pass the input through unchanged.
	StatusNotApplicable HookStatus = iota

	// StatusTransformed means the hook converted the input into a new value.
	StatusTransformed

	// StatusFailed means the hook failed. The cause is carried by
	// [HookResult.Err] and is propagated without re-wrapping so the original
	// error chain stays intact for [errors.Is] and [errors.As].
	StatusFailed
)

// HookResult is the outcome of a [DecodeHook2Func] call.
//
// Exactly one of the three states applies:
//   - [StatusTransformed]: Data holds the converted value.
//   - [StatusNotApplicable]: the hook did not handle the input.
//   - [StatusFailed]: Err holds the cause.
//
// Use [Transformed], [NotApplicable] and [Failed] to construct results.
type HookResult struct {
	Status HookStatus
	Data   any
	Err    error
}

// Transformed returns a [HookResult] indicating the hook converted the
// input into data.
func Transformed(data any) HookResult {
	return HookResult{Status: StatusTransformed, Data: data}
}

// NotApplicable returns a [HookResult] indicating the hook does not apply
// to the input.
func NotApplicable() HookResult {
	return HookResult{Status: StatusNotApplicable}
}

// Failed returns a [HookResult] indicating the hook failed with err.
func Failed(err error) HookResult {
	return HookResult{Status: StatusFailed, Err: err}
}

// DecodeHook2Func is a decode hook following the tri-state protocol.
//
// Unlike [DecodeHookFunc], it explicitly reports whether the input was
// transformed, does not apply, or failed, instead of overloading the
// returned value and error.
//
// A DecodeHook2Func can be used anywhere a [DecodeHookFunc] is accepted,
// including [DecoderConfig.DecodeHook].
type DecodeHook2Func func(from reflect.Value, to reflect.Value) HookResult

// ComposeHooks creates a single [DecodeHook2Func] that runs the given hooks
// in order, feeding the transformed result of each hook into the next.
//
// Hooks reporting [StatusNotApplicable] pass the current value through
// unchanged. A hook reporting [StatusFailed] aborts the chain and its error
// is returned unwrapped, preserving the original error chain. If no hook
// transforms the value, the composed hook reports [StatusNotApplicable].
func ComposeHooks(hooks ...DecodeHook2Func) DecodeHook2Func {
	return func(from reflect.Value, to reflect.Value) HookResult {
		data := safeInterface(from)
		newFrom := from
		transformed := false

		for _, h := range hooks {
			r := h(newFrom, to)
			switch r.Status {
			case StatusFailed:
				return r
			case StatusTransformed:
				transformed = true
				data = r.Data
				newFrom = hookValueOf(data)
			case StatusNotApplicable:
			}
		}

		if !transformed {
			return NotApplicable()
		}

		return Transformed(data)
	}
}

// OrHooks creates a single [DecodeHook2Func] that tries the given hooks in
// order and returns the first decisive result.
//
// The next hook is only tried when the previous one reports
// [StatusNotApplicable]. The first [StatusTransformed] or [StatusFailed]
// result is returned as-is, so a failure preserves its original error chain
// instead of being merged with other errors. If every hook reports
// [StatusNotApplicable], the composed hook reports [StatusNotApplicable].
func OrHooks(hooks ...DecodeHook2Func) DecodeHook2Func {
	return func(from reflect.Value, to reflect.Value) HookResult {
		for _, h := range hooks {
			r := h(from, to)
			if r.Status == StatusNotApplicable {
				continue
			}

			return r
		}

		return NotApplicable()
	}
}

// AdaptHook adapts a legacy [DecodeHookFunc] to the tri-state protocol.
//
// The adaptation rule is explicit: a non-nil error becomes [StatusFailed]
// and any other result becomes [StatusTransformed], even when the legacy
// hook returned its input unchanged. The legacy protocol has no
// not-applicable signal, so the adapter never derives one from the returned
// value. Use [AdaptHookWhen] to declare when a legacy hook applies.
func AdaptHook(legacy DecodeHookFunc) DecodeHook2Func {
	cached := cachedDecodeHook(legacy)

	return func(from reflect.Value, to reflect.Value) HookResult {
		data, err := cached(from, to)
		if err != nil {
			return Failed(err)
		}

		return Transformed(data)
	}
}

// AdaptHookWhen adapts a legacy [DecodeHookFunc] to the tri-state protocol,
// applying it only when match reports the from/to types as applicable.
//
// When match returns false the hook reports [StatusNotApplicable] and the
// legacy hook is not called. Otherwise the result follows the same explicit
// rule as [AdaptHook]: error becomes [StatusFailed], anything else becomes
// [StatusTransformed].
func AdaptHookWhen(match func(from, to reflect.Type) bool, legacy DecodeHookFunc) DecodeHook2Func {
	cached := cachedDecodeHook(legacy)

	return func(from reflect.Value, to reflect.Value) HookResult {
		var fromType reflect.Type
		if from.IsValid() {
			fromType = from.Type()
		}
		if !match(fromType, to.Type()) {
			return NotApplicable()
		}

		data, err := cached(from, to)
		if err != nil {
			return Failed(err)
		}

		return Transformed(data)
	}
}

// hookValueOf converts a hook result value back into a reflect.Value for the
// next hook in a chain. A nil value yields the zero (invalid) Value.
func hookValueOf(data any) reflect.Value {
	if v, ok := data.(reflect.Value); ok {
		return v
	}
	if data == nil {
		return reflect.Value{}
	}

	return reflect.ValueOf(data)
}

// execDecodeHook2 runs a tri-state hook and maps its result onto the legacy
// (any, error) signature used by the decoder: a not-applicable result passes
// the input through unchanged.
func execDecodeHook2(f DecodeHook2Func, from reflect.Value, to reflect.Value) (any, error) {
	r := f(from, to)
	switch r.Status {
	case StatusTransformed:
		return r.Data, nil
	case StatusFailed:
		return nil, r.Err
	default:
		return safeInterface(from), nil
	}
}
