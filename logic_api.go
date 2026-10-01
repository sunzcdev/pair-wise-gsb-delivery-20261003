package validator

import (
	"context"
	"errors"
	"reflect"
)

// VarRule validates a single variable using an explicitly nested Rule built
// with And, Or, Not, Tag and Func.
//
// field is the value to validate, exactly as for Var. On failure it returns a
// *LogicError describing the failure in logical terms; callers may type-assert
// the error to inspect the tree or call Flatten to obtain the underlying
// FieldErrors, each located at its rule node.
func (v *Validate) VarRule(field interface{}, rule Rule) error {
	return v.VarRuleCtx(context.Background(), field, rule)
}

// VarRuleCtx is the context.Context aware variant of VarRule.
func (v *Validate) VarRuleCtx(ctx context.Context, field interface{}, rule Rule) error {
	if rule == nil {
		return &InvalidValidationError{Type: reflect.TypeOf(field)}
	}

	val := reflect.ValueOf(field)

	vd := v.pool.Get().(*validate)
	vd.top = val
	vd.isPartial = false

	lr := logicRunner{
		v:        v,
		ctx:      ctx,
		vd:       vd,
		parent:   val,
		current:  val,
		ns:       vd.ns[0:0],
		structNs: vd.actualNs[0:0],
		cf:       defaultCField,
	}

	le := lr.evaluate(rule, "")

	v.pool.Put(vd)

	if le != nil {
		return le
	}
	return nil
}

// StructRules validates a struct's fields using explicitly nested Rules bound
// with Field. Ordinary struct tag validation is bypassed for this call; the
// supplied rules describe the complete validation to perform.
//
// Only the named fields are validated. Cross-field leaf tags (for example
// eqfield=Other) resolve siblings against the struct normally.
//
// It returns InvalidValidationError for bad values passed in. A failed field is
// reported as a FieldLogicError; multiple failed fields are joined.
func (v *Validate) StructRules(s interface{}, rules ...FieldRule) error {
	return v.StructRulesCtx(context.Background(), s, rules...)
}

// StructRulesCtx is the context.Context aware variant of StructRules.
func (v *Validate) StructRulesCtx(ctx context.Context, s interface{}, rules ...FieldRule) error {
	val := reflect.ValueOf(s)
	top := val

	if val.Kind() == reflect.Ptr && !val.IsNil() {
		val = val.Elem()
	}

	if val.Kind() != reflect.Struct || val.Type().ConvertibleTo(timeType) {
		return &InvalidValidationError{Type: reflect.TypeOf(s)}
	}

	cs, ok := v.structCache.Get(val.Type())
	if !ok {
		cs = v.extractStructCache(val, val.Type().Name())
	}

	vd := v.pool.Get().(*validate)
	vd.top = top
	vd.isPartial = false

	ns := vd.ns[0:0]
	structNs := vd.actualNs[0:0]

	if len(cs.name) != 0 {
		ns = append(ns, cs.name...)
		ns = append(ns, '.')

		structNs = append(structNs, cs.name...)
		structNs = append(structNs, '.')
	}

	var errs []error

	for _, fr := range rules {
		if fr.rule == nil {
			continue
		}

		cf, found := fieldByName(cs, fr.fieldName)
		if !found {
			panic("validator: field '" + fr.fieldName + "' not found in struct '" + val.Type().String() + "'")
		}

		current := val.Field(cf.idx)

		// Each field gets its own namespace buffers: traverseField (and dives)
		// append in place, so sharing the backing arrays would leak one
		// field's namespace into the next.
		fieldNs := append([]byte(nil), ns...)
		fieldStructNs := append([]byte(nil), structNs...)

		lr := logicRunner{
			v:        v,
			ctx:      ctx,
			vd:       vd,
			parent:   val,
			current:  current,
			ns:       fieldNs,
			structNs: fieldStructNs,
			cf:       cf,
		}

		if le := lr.evaluate(fr.rule, ""); le != nil {
			errs = append(errs, &FieldLogicError{LogicError: le})
		}
	}

	v.pool.Put(vd)

	return errors.Join(errs...)
}

func fieldByName(cs *cStruct, name string) (*cField, bool) {
	for _, f := range cs.fields {
		if f.name == name {
			return f, true
		}
	}
	return nil, false
}

// FieldLogicError is a *LogicError located on a specific struct field,
// returned by Validate.StructRules. It implements the error interface and
// embeds the full logical failure tree.
type FieldLogicError struct {
	*LogicError
}
