package validator

import (
	"context"
	"errors"
	"fmt"
	"reflect"
)

// BatchKeyFunc extracts the external lookup key (e.g. a SKU) from the field
// currently being validated. Returning ok == false skips the external check
// for this field entirely (e.g. an empty optional value); local format
// validations such as "required" still apply independently.
type BatchKeyFunc func(fl FieldLevel) (key string, ok bool)

// BatchScopeFunc extracts an isolation scope (e.g. a tenant ID) from the
// request context. Keys are deduplicated per (rule, scope) pair so identical
// keys belonging to different scopes are never merged into the same backend
// call. A nil BatchScopeFunc means a single shared scope.
type BatchScopeFunc func(ctx context.Context) (scope string)

// BatchCheckFunc verifies a batch of deduplicated keys in a single backend
// call. The returned map reports, for each key, whether it is valid
// (e.g. exists); keys missing from the map are treated as invalid.
//
// A non-nil error means the backend call itself failed (e.g. the service is
// unavailable) and MUST NOT be interpreted as "the keys do not exist"; it is
// surfaced separately as a *BatchExternalError.
type BatchCheckFunc func(ctx context.Context, keys []string) (valid map[string]bool, err error)

// BatchExternalRule describes an external existence/uniqueness rule whose
// keys are collected during local validation and verified with a single
// batched backend call per (rule, scope) instead of one call per field.
type BatchExternalRule struct {
	// Key extracts the external lookup key from a field. Required.
	Key BatchKeyFunc

	// Scope extracts an isolation scope (e.g. tenant) from the context.
	// Optional; nil means all keys share one scope.
	Scope BatchScopeFunc

	// Check verifies a batch of deduplicated keys in one call. Required.
	Check BatchCheckFunc
}

// BatchExternalError is returned when the backend call of a batch external
// rule itself failed (e.g. the service is unavailable or the context was
// cancelled). It is deliberately NOT a ValidationErrors: a backend failure
// does not mean the checked values do not exist. Use errors.As to detect it
// and errors.Is (via Unwrap) to match the underlying cause such as
// context.Canceled.
type BatchExternalError struct {
	// Tag is the validation tag of the rule whose backend call failed.
	Tag string

	// Scope is the isolation scope (e.g. tenant) of the failed call, if any.
	Scope string

	// Err is the underlying backend error.
	Err error
}

// Error returns the BatchExternalError message.
func (e *BatchExternalError) Error() string {
	if e.Scope != "" {
		return fmt.Sprintf("validator: batch external validation '%s' failed for scope '%s': %s", e.Tag, e.Scope, e.Err)
	}
	return fmt.Sprintf("validator: batch external validation '%s' failed: %s", e.Tag, e.Err)
}

// Unwrap returns the underlying backend error.
func (e *BatchExternalError) Unwrap() error {
	return e.Err
}

// RegisterBatchExternal registers a batch external rule under the given tag.
//
// Fields carrying the tag are checked locally as usual (other tags such as
// "required" or format validators still run); the external check itself is
// deferred: keys are collected during validation, deduplicated per
// (rule, scope), verified with one batched backend call per group and the
// results are mapped back onto the individual fields.
//
// The batched call only happens when validating through StructBatch /
// StructBatchCtx; with the regular entry points (Struct, StructCtx, Var, ...)
// the external check is skipped and only local validations run.
//
// NOTE: batch external tags must not be combined inside '|' or-expressions.
//
// NOTE: this method is not thread-safe it is intended that these all be registered prior to any validation
func (v *Validate) RegisterBatchExternal(tag string, rule BatchExternalRule) error {
	if rule.Key == nil {
		return errors.New("batch external rule requires a Key function")
	}
	if rule.Check == nil {
		return errors.New("batch external rule requires a Check function")
	}

	return v.registerValidation(tag, func(ctx context.Context, fl FieldLevel) bool {
		vd, ok := fl.(*validate)
		if !ok || vd.batch == nil {
			// no batch collector attached (regular entry points):
			// skip the external check, local validations already ran.
			return true
		}

		key, ok := rule.Key(fl)
		if !ok || key == "" {
			return true
		}

		var scope string
		if rule.Scope != nil {
			scope = rule.Scope(ctx)
		}

		vd.batch.add(vd, tag, rule, scope, key)
		return true
	}, false, false)
}

// batchGroupID identifies a deduplication group: one backend call is made
// per distinct (tag, scope) pair so data of different tenants never mixes.
type batchGroupID struct {
	tag   string
	scope string
}

// batchGroup holds the deduplicated keys of a single backend call.
type batchGroup struct {
	id    batchGroupID
	rule  BatchExternalRule
	keys  []string
	seen  map[string]struct{}
	valid map[string]bool
}

// batchItem links one validated field to its group and key.
type batchItem struct {
	group *batchGroup
	key   string
	fe    *fieldError
}

// batchCollector gathers the external keys of one validation run.
type batchCollector struct {
	items  []batchItem
	groups map[batchGroupID]*batchGroup
	order  []*batchGroup
}

func newBatchCollector() *batchCollector {
	return &batchCollector{groups: make(map[batchGroupID]*batchGroup)}
}

// add records one field's external key. The fieldError template is built
// immediately because the namespace buffers of the validate struct are
// reused as traversal continues.
func (c *batchCollector) add(vd *validate, tag string, rule BatchExternalRule, scope, key string) {
	id := batchGroupID{tag: tag, scope: scope}

	g := c.groups[id]
	if g == nil {
		g = &batchGroup{
			id:   id,
			rule: rule,
			seen: make(map[string]struct{}),
		}
		c.groups[id] = g
		c.order = append(c.order, g)
	}

	if _, dup := g.seen[key]; !dup {
		g.seen[key] = struct{}{}
		g.keys = append(g.keys, key)
	}

	ns := appendAltName(vd.flNs, vd.cf.altName)
	structNs := ns
	if vd.v.hasTagNameFunc {
		structNs = string(append(vd.flStructNs, vd.cf.name...))
	}

	c.items = append(c.items, batchItem{
		group: g,
		key:   key,
		fe: &fieldError{
			v:              vd.v,
			tag:            vd.ct.aliasTag,
			actualTag:      vd.ct.tag,
			ns:             ns,
			structNs:       structNs,
			fieldLen:       uint8(len(vd.cf.altName)),
			structfieldLen: uint8(len(vd.cf.name)),
			value:          getValue(vd.flField),
			param:          vd.ct.param,
			kind:           vd.flField.Kind(),
			typ:            vd.flField.Type(),
		},
	})
}

// run executes one batched backend call per group and maps the results back
// onto the collected fields. A backend failure aborts the run and is returned
// separately as a *BatchExternalError (or the context error on cancellation);
// it is never converted into per-field "does not exist" errors.
func (c *batchCollector) run(ctx context.Context) (ValidationErrors, error) {
	for _, g := range c.order {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		valid, err := g.rule.Check(ctx, g.keys)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			return nil, &BatchExternalError{Tag: g.id.tag, Scope: g.id.scope, Err: err}
		}
		g.valid = valid
	}

	var errs ValidationErrors
	for i := range c.items {
		item := &c.items[i]
		if !item.group.valid[item.key] {
			errs = append(errs, item.fe)
		}
	}
	return errs, nil
}

// StructBatch validates a structs exposed fields like Struct, and additionally
// runs the registered batch external rules: keys are collected during local
// validation, deduplicated per (rule, scope) and verified with one batched
// backend call per group, then mapped back onto the individual fields.
//
// It returns InvalidValidationError for bad values passed in, a
// *BatchExternalError (or context error) when a backend call itself failed,
// and nil or ValidationErrors as error otherwise.
func (v *Validate) StructBatch(s interface{}) error {
	return v.StructBatchCtx(context.Background(), s)
}

// StructBatchCtx is the context-aware variant of StructBatch. The context is
// passed to the batch external rules' Scope and Check functions; cancelling
// it aborts the batch phase and the context error is returned instead of
// per-field validation errors.
func (v *Validate) StructBatchCtx(ctx context.Context, s interface{}) (err error) {
	val := reflect.ValueOf(s)
	top := val

	if val.Kind() == reflect.Ptr && !val.IsNil() {
		val = val.Elem()
	}

	if val.Kind() != reflect.Struct || val.Type().ConvertibleTo(timeType) {
		return &InvalidValidationError{Type: reflect.TypeOf(s)}
	}

	// good to validate
	vd := v.pool.Get().(*validate)
	vd.top = top
	vd.isPartial = false
	vd.batch = newBatchCollector()

	vd.validateStruct(ctx, top, val, val.Type(), vd.ns[0:0], vd.actualNs[0:0], nil)

	batch := vd.batch
	vd.batch = nil

	if len(batch.items) > 0 {
		batchErrs, batchErr := batch.run(ctx)
		if batchErr != nil {
			vd.errs = nil
			v.pool.Put(vd)
			return batchErr
		}
		vd.errs = append(vd.errs, batchErrs...)
	}

	if len(vd.errs) > 0 {
		err = vd.errs
		vd.errs = nil
	}

	v.pool.Put(vd)

	return
}
