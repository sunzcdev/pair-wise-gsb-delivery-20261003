package validator

import (
	"context"
	"errors"
	"fmt"
	"reflect"
)

// BatchCheckFunc validates a deduplicated set of keys for a single batch
// rule and scope in one call, e.g. one "do these product IDs exist" request
// to a remote service instead of one request per field.
//
// The returned map reports validity per key; keys absent from the map are
// treated as invalid (fail closed).
//
// A non-nil error signals that the external backend itself failed (e.g. it
// is unavailable). This is NOT reported as ValidationErrors; the validation
// call instead returns a *BatchError wrapping it, so callers can distinguish
// "backend down" from "value does not exist".
type BatchCheckFunc func(ctx context.Context, scope string, keys []string) (map[string]bool, error)

// BatchScopeFunc extracts the scope (e.g. tenant ID) a field's key belongs
// to. Keys are deduplicated and batched per (rule, scope) pair, so identical
// keys from different scopes are never mixed into the same backend call.
type BatchScopeFunc func(ctx context.Context, fl FieldLevel) string

// BatchKeyFunc extracts the key to check from the current field. When nil,
// the field's string value is used (fmt.Sprintf("%v") for non-strings).
type BatchKeyFunc func(ctx context.Context, fl FieldLevel) string

// BatchOption configures a batch validation rule at registration time.
type BatchOption func(*batchRule)

// WithBatchScope sets the BatchScopeFunc used to group and deduplicate keys,
// e.g. to keep different tenants' data in separate backend calls.
func WithBatchScope(fn BatchScopeFunc) BatchOption {
	return func(r *batchRule) {
		r.scopeFn = fn
	}
}

// WithBatchKey sets a custom BatchKeyFunc used to extract keys from fields.
func WithBatchKey(fn BatchKeyFunc) BatchOption {
	return func(r *batchRule) {
		r.keyFn = fn
	}
}

// BatchError is returned by validation when an external batch rule could not
// be resolved, e.g. the backend was unavailable or the context was canceled
// mid-validation. It is deliberately NOT a ValidationErrors: a backend
// failure is not a "value is invalid" verdict.
type BatchError struct {
	// Tag is the batch rule tag that failed.
	Tag string
	// Scope is the scope group being resolved when the failure occurred.
	Scope string
	// Err is the underlying error, e.g. the backend error or context.Canceled.
	Err error
}

// Error returns the string representation of the BatchError.
func (e *BatchError) Error() string {
	if e.Scope != "" {
		return fmt.Sprintf("batch validation '%s' (scope '%s') failed: %v", e.Tag, e.Scope, e.Err)
	}
	return fmt.Sprintf("batch validation '%s' failed: %v", e.Tag, e.Err)
}

// Unwrap returns the underlying error, so errors.Is/errors.As work with
// e.g. context.Canceled.
func (e *BatchError) Unwrap() error {
	return e.Err
}

// batchRule holds a registered batch validation rule.
type batchRule struct {
	tag     string
	fn      BatchCheckFunc
	scopeFn BatchScopeFunc
	keyFn   BatchKeyFunc
}

// batchEntry is a single deferred check collected during traversal.
type batchEntry struct {
	rule  *batchRule
	scope string
	key   string
	fe    *fieldError
}

// batchGroupKey identifies one backend call: one rule, one scope.
type batchGroupKey struct {
	rule  *batchRule
	scope string
}

// RegisterBatchValidation registers an external validation rule that is
// resolved in batch. During validation the tag only collects the keys to
// check (all other, local validations keep running as usual); once the
// struct has been traversed, the collected keys are deduplicated per
// (rule, scope) and checked with a single fn call per group, and the
// results are mapped back onto the individual fields as ValidationErrors.
//
// NOTES:
// - if the key already exists, the previous validation function will be replaced.
// - this method is not thread-safe it is intended that these all be registered prior to any validation
// - batch tags cannot be used within an 'or' (|) expression; doing so panics.
func (v *Validate) RegisterBatchValidation(tag string, fn BatchCheckFunc, opts ...BatchOption) error {
	if len(tag) == 0 {
		return errors.New("function Key cannot be empty")
	}

	if fn == nil {
		return errors.New("function cannot be empty")
	}

	rule := &batchRule{tag: tag, fn: fn}
	for _, opt := range opts {
		opt(rule)
	}

	if v.batchRules == nil {
		v.batchRules = make(map[string]*batchRule)
	}
	v.batchRules[tag] = rule
	v.hasBatchRules = true

	// register a placeholder so the tag parses like any other; it panics if
	// ever executed directly, which only happens inside an 'or' expression
	// where deferred batch resolution is not supported.
	return v.registerValidation(tag, func(ctx context.Context, fl FieldLevel) bool {
		panic(fmt.Sprintf("batch validation '%s' cannot be used within an or (|) expression", tag))
	}, false, false)
}

// collectBatch defers the external check for the current field: it extracts
// the key and scope and records a pre-built fieldError to append later if
// the key turns out to be invalid.
func (v *validate) collectBatch(ctx context.Context, rule *batchRule, ct *cTag, cf *cField, current reflect.Value, kind reflect.Kind, typ reflect.Type, ns []byte, structNs []byte) {
	var key string
	switch {
	case rule.keyFn != nil:
		key = rule.keyFn(ctx, v)
	case kind == reflect.String:
		key = current.String()
	default:
		key = fmt.Sprintf("%v", getValue(current))
	}

	var scope string
	if rule.scopeFn != nil {
		scope = rule.scopeFn(ctx, v)
	}

	v.str1 = appendAltName(ns, cf.altName)
	if v.v.hasTagNameFunc {
		v.str2 = string(append(structNs, cf.name...))
	} else {
		v.str2 = v.str1
	}

	v.batchPending = append(v.batchPending, batchEntry{
		rule:  rule,
		scope: scope,
		key:   key,
		fe: &fieldError{
			v:              v.v,
			tag:            ct.aliasTag,
			actualTag:      ct.tag,
			ns:             v.str1,
			structNs:       v.str2,
			fieldLen:       uint8(len(cf.altName)),
			structfieldLen: uint8(len(cf.name)),
			value:          getValue(current),
			param:          ct.param,
			kind:           kind,
			typ:            typ,
		},
	})
}

// resolveBatch runs after traversal: it groups the collected keys per
// (rule, scope), deduplicates them, performs one backend call per group and
// maps the results back onto the individual fields. A backend failure or a
// canceled context aborts resolution and returns a *BatchError.
func (v *validate) resolveBatch(ctx context.Context) error {
	if len(v.batchPending) == 0 {
		return nil
	}
	defer func() { v.batchPending = nil }()

	type batchGroup struct {
		rule  *batchRule
		scope string
		keys  []string
		seen  map[string]struct{}
		items []batchEntry
	}

	groups := make([]*batchGroup, 0, 4)
	index := make(map[batchGroupKey]*batchGroup)

	for _, e := range v.batchPending {
		id := batchGroupKey{rule: e.rule, scope: e.scope}
		g, ok := index[id]
		if !ok {
			g = &batchGroup{rule: e.rule, scope: e.scope, seen: make(map[string]struct{})}
			index[id] = g
			groups = append(groups, g)
		}
		if _, dup := g.seen[e.key]; !dup {
			g.seen[e.key] = struct{}{}
			g.keys = append(g.keys, e.key)
		}
		g.items = append(g.items, e)
	}

	for _, g := range groups {
		// honor cancellation between calls: a canceled context must stop
		// remaining backend calls, not report fields as invalid.
		if err := ctx.Err(); err != nil {
			return &BatchError{Tag: g.rule.tag, Scope: g.scope, Err: err}
		}

		results, err := g.rule.fn(ctx, g.scope, g.keys)
		if err != nil {
			return &BatchError{Tag: g.rule.tag, Scope: g.scope, Err: err}
		}

		for _, e := range g.items {
			if !results[e.key] {
				v.errs = append(v.errs, e.fe)
			}
		}
	}

	return nil
}
