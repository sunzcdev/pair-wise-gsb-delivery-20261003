package mapstructure

import (
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

type hook2NamedString string

type hook2CustomError struct {
	msg string
}

func (e *hook2CustomError) Error() string { return e.msg }

func TestComposeHooks_TypeChangeChain(t *testing.T) {
	t.Parallel()

	var seen []reflect.Type

	toInt := func(from reflect.Value, to reflect.Value) HookResult {
		seen = append(seen, from.Type())
		if from.Kind() != reflect.String {
			return NotApplicable()
		}
		n, err := strconv.Atoi(from.String())
		if err != nil {
			return Failed(err)
		}
		return Transformed(n)
	}
	annotate := func(from reflect.Value, to reflect.Value) HookResult {
		seen = append(seen, from.Type())
		if from.Kind() != reflect.Int {
			return NotApplicable()
		}
		return Transformed(fmt.Sprintf("value=%d", from.Int()))
	}

	hook := ComposeHooks(toInt, annotate)
	result := hook(reflect.ValueOf("42"), reflect.ValueOf(""))

	if result.Status != StatusTransformed {
		t.Fatalf("expected transformed, got status %v (err %v)", result.Status, result.Err)
	}
	if result.Data != "value=42" {
		t.Fatalf("expected %q, got %#v", "value=42", result.Data)
	}

	// The second hook must observe the type produced by the first hook.
	want := []reflect.Type{reflect.TypeOf(""), reflect.TypeOf(0)}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("expected hook input types %v, got %v", want, seen)
	}
}

func TestComposeHooks_NotApplicablePassthrough(t *testing.T) {
	t.Parallel()

	noop := func(from reflect.Value, to reflect.Value) HookResult {
		return NotApplicable()
	}
	double := func(from reflect.Value, to reflect.Value) HookResult {
		if from.Kind() != reflect.Int {
			return NotApplicable()
		}
		return Transformed(from.Int() * 2)
	}

	hook := ComposeHooks(noop, double, noop)
	result := hook(reflect.ValueOf(21), reflect.ValueOf(0))

	if result.Status != StatusTransformed || result.Data != int64(42) {
		t.Fatalf("expected transformed 42, got status %v data %#v", result.Status, result.Data)
	}

	allNA := ComposeHooks(noop, noop)
	if r := allNA(reflect.ValueOf(1), reflect.ValueOf(0)); r.Status != StatusNotApplicable {
		t.Fatalf("expected not applicable, got status %v", r.Status)
	}
}

func TestComposeHooks_FailurePreservesErrorChain(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("boom")
	cause := &hook2CustomError{msg: "bad value"}

	fail := func(from reflect.Value, to reflect.Value) HookResult {
		return Failed(fmt.Errorf("step 1: %w", fmt.Errorf("%w: %w", sentinel, cause)))
	}
	unreached := func(from reflect.Value, to reflect.Value) HookResult {
		t.Fatal("hook after failure must not run")
		return NotApplicable()
	}

	result := ComposeHooks(fail, unreached)(reflect.ValueOf("x"), reflect.ValueOf(0))

	if result.Status != StatusFailed {
		t.Fatalf("expected failed, got status %v", result.Status)
	}
	if !errors.Is(result.Err, sentinel) {
		t.Fatalf("expected errors.Is to find sentinel, got %v", result.Err)
	}
	var custom *hook2CustomError
	if !errors.As(result.Err, &custom) {
		t.Fatalf("expected errors.As to find hook2CustomError, got %v", result.Err)
	}
}

func TestOrHooks_FallsThroughOnlyOnNotApplicable(t *testing.T) {
	t.Parallel()

	calls := 0
	na := func(from reflect.Value, to reflect.Value) HookResult {
		calls++
		return NotApplicable()
	}
	winner := func(from reflect.Value, to reflect.Value) HookResult {
		calls++
		return Transformed("won")
	}
	unreached := func(from reflect.Value, to reflect.Value) HookResult {
		calls++
		t.Fatal("hook after a decisive result must not run")
		return NotApplicable()
	}

	result := OrHooks(na, winner, unreached)(reflect.ValueOf("x"), reflect.ValueOf(""))
	if result.Status != StatusTransformed || result.Data != "won" {
		t.Fatalf("expected transformed %q, got status %v data %#v", "won", result.Status, result.Data)
	}
	if calls != 2 {
		t.Fatalf("expected 2 hooks to run, got %d", calls)
	}

	// A transformed first candidate wins immediately.
	calls = 0
	result = OrHooks(winner, na)(reflect.ValueOf("x"), reflect.ValueOf(""))
	if result.Status != StatusTransformed || calls != 1 {
		t.Fatalf("expected first hook to win after 1 call, got status %v calls %d", result.Status, calls)
	}
}

func TestOrHooks_FailureStopsAndPreservesChain(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("or boom")
	fail := func(from reflect.Value, to reflect.Value) HookResult {
		return Failed(fmt.Errorf("candidate: %w", sentinel))
	}
	unreached := func(from reflect.Value, to reflect.Value) HookResult {
		t.Fatal("hook after failure must not run")
		return NotApplicable()
	}

	result := OrHooks(fail, unreached)(reflect.ValueOf("x"), reflect.ValueOf(""))
	if result.Status != StatusFailed {
		t.Fatalf("expected failed, got status %v", result.Status)
	}
	if !errors.Is(result.Err, sentinel) {
		t.Fatalf("expected errors.Is to find sentinel, got %v", result.Err)
	}
}

func TestOrHooks_AllNotApplicable(t *testing.T) {
	t.Parallel()

	na := func(from reflect.Value, to reflect.Value) HookResult {
		return NotApplicable()
	}

	result := OrHooks(na, na)(reflect.ValueOf("x"), reflect.ValueOf(""))
	if result.Status != StatusNotApplicable {
		t.Fatalf("expected not applicable, got status %v", result.Status)
	}
}

func TestDecodeHook2_NamedType(t *testing.T) {
	t.Parallel()

	// The hook matches the exact named type, not just the underlying kind.
	upper := func(from reflect.Value, to reflect.Value) HookResult {
		if from.Type() != reflect.TypeOf(hook2NamedString("")) {
			return NotApplicable()
		}
		return Transformed(strings.ToUpper(from.String()))
	}

	named := upper(reflect.ValueOf(hook2NamedString("hello")), reflect.ValueOf(""))
	if named.Status != StatusTransformed || named.Data != "HELLO" {
		t.Fatalf("expected transformed HELLO, got status %v data %#v", named.Status, named.Data)
	}

	plain := upper(reflect.ValueOf("hello"), reflect.ValueOf(""))
	if plain.Status != StatusNotApplicable {
		t.Fatalf("expected plain string to be not applicable, got status %v", plain.Status)
	}

	var target string
	decoder, err := NewDecoder(&DecoderConfig{
		DecodeHook: upper,
		Result:     &target,
	})
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if err := decoder.Decode(hook2NamedString("hello")); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if target != "HELLO" {
		t.Fatalf("expected HELLO, got %q", target)
	}
}

func TestComposeHooks_TypedNil(t *testing.T) {
	t.Parallel()

	makeNil := func(from reflect.Value, to reflect.Value) HookResult {
		return Transformed((*url.URL)(nil))
	}

	var seenType reflect.Type
	var seenNil bool
	observe := func(from reflect.Value, to reflect.Value) HookResult {
		seenType = from.Type()
		seenNil = from.IsNil()
		return NotApplicable()
	}

	result := ComposeHooks(makeNil, observe)(reflect.ValueOf("x"), reflect.ValueOf(&url.URL{}))

	if result.Status != StatusTransformed {
		t.Fatalf("expected transformed, got status %v", result.Status)
	}
	if seenType != reflect.TypeOf((*url.URL)(nil)) {
		t.Fatalf("expected next hook to see *url.URL, got %v", seenType)
	}
	if !seenNil {
		t.Fatal("expected next hook to see a typed nil")
	}
	if u, ok := result.Data.(*url.URL); !ok || u != nil {
		t.Fatalf("expected typed nil *url.URL, got %#v", result.Data)
	}
}

func TestDecoder_Hook2ErrorChain(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("decoder boom")
	cause := &hook2CustomError{msg: "cannot convert"}

	hook := ComposeHooks(
		func(from reflect.Value, to reflect.Value) HookResult {
			if from.Kind() != reflect.String {
				return NotApplicable()
			}
			return Failed(fmt.Errorf("hook: %w", fmt.Errorf("%w: %w", sentinel, cause)))
		},
	)

	var target struct {
		Name string `mapstructure:"name"`
	}
	decoder, err := NewDecoder(&DecoderConfig{
		DecodeHook: hook,
		Result:     &target,
	})
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	err = decoder.Decode(map[string]any{"name": "x"})
	if err == nil {
		t.Fatal("expected decode error")
	}

	// The Decoder wraps hook errors, but the original chain must survive.
	var decodeErr *DecodeError
	if !errors.As(err, &decodeErr) {
		t.Fatalf("expected DecodeError wrapper, got %T", err)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected errors.Is to find sentinel through decoder wrapping, got %v", err)
	}
	var custom *hook2CustomError
	if !errors.As(err, &custom) {
		t.Fatalf("expected errors.As to find hook2CustomError through decoder wrapping, got %v", err)
	}
}

func TestAdaptHook(t *testing.T) {
	t.Parallel()

	legacyErr := errors.New("legacy failure")
	legacy := func(f reflect.Type, t reflect.Type, data any) (any, error) {
		if s, ok := data.(string); ok {
			return strings.ToUpper(s), nil
		}
		return nil, legacyErr
	}

	hook := AdaptHook(legacy)

	ok := hook(reflect.ValueOf("hi"), reflect.ValueOf(""))
	if ok.Status != StatusTransformed || ok.Data != "HI" {
		t.Fatalf("expected transformed HI, got status %v data %#v", ok.Status, ok.Data)
	}

	bad := hook(reflect.ValueOf(1), reflect.ValueOf(""))
	if bad.Status != StatusFailed || !errors.Is(bad.Err, legacyErr) {
		t.Fatalf("expected failed with legacy error, got status %v err %v", bad.Status, bad.Err)
	}
}

func TestAdaptHookWhen(t *testing.T) {
	t.Parallel()

	called := false
	legacy := func(f reflect.Type, t reflect.Type, data any) (any, error) {
		called = true
		return data, nil
	}

	hook := AdaptHookWhen(func(from, to reflect.Type) bool {
		return from != nil && from.Kind() == reflect.String
	}, legacy)

	skipped := hook(reflect.ValueOf(1), reflect.ValueOf(""))
	if skipped.Status != StatusNotApplicable {
		t.Fatalf("expected not applicable, got status %v", skipped.Status)
	}
	if called {
		t.Fatal("legacy hook must not run when the predicate rejects the types")
	}

	applied := hook(reflect.ValueOf("x"), reflect.ValueOf(""))
	if applied.Status != StatusTransformed || applied.Data != "x" {
		t.Fatalf("expected transformed x, got status %v data %#v", applied.Status, applied.Data)
	}
	if !called {
		t.Fatal("legacy hook must run when the predicate accepts the types")
	}
}

func TestDecodeHookExec_Hook2(t *testing.T) {
	t.Parallel()

	hook := DecodeHook2Func(func(from reflect.Value, to reflect.Value) HookResult {
		if from.Kind() != reflect.String {
			return NotApplicable()
		}
		return Transformed(strings.ToUpper(from.String()))
	})

	out, err := DecodeHookExec(hook, reflect.ValueOf("hi"), reflect.ValueOf(""))
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if out != "HI" {
		t.Fatalf("expected HI, got %#v", out)
	}

	// Not applicable passes the input through unchanged.
	out, err = DecodeHookExec(hook, reflect.ValueOf(7), reflect.ValueOf(""))
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if out != 7 {
		t.Fatalf("expected 7, got %#v", out)
	}
}
