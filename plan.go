package mapstructure

import (
	"encoding/binary"
	"fmt"
	"hash"
	"hash/fnv"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/go-viper/mapstructure/v2/internal/errors"
)

// PlanAction describes what decoding would do to a single target field.
type PlanAction int

const (
	// ActionSet means the field will be assigned a decoded value.
	ActionSet PlanAction = iota

	// ActionZero means the field will be reset to its zero value, for
	// example a nil input combined with ZeroFields, or a pointer that is
	// explicitly set to nil.
	ActionZero

	// ActionPreserve means the field is left untouched, for example a
	// struct field with no matching key in the input.
	ActionPreserve
)

// String returns a human-readable name for the action.
func (a PlanAction) String() string {
	switch a {
	case ActionSet:
		return "set"
	case ActionZero:
		return "zero"
	case ActionPreserve:
		return "preserve"
	default:
		return "unknown"
	}
}

// PlanStep describes a single assignment decoding would perform. Steps are
// identified by path (for example "Server.Port" or "Labels[env]") and never
// reference the target value by address.
type PlanStep struct {
	// Path is the dot-separated path of the target field, using the same
	// naming scheme as decode errors and Metadata. The root is "(root)".
	Path string

	// Action is what will happen to the field: set, zeroed, or preserved.
	Action PlanAction

	// SourceType is the type of the input value, empty if there is none.
	SourceType string

	// TargetType is the type of the target field.
	TargetType string

	// Conversion describes the type conversion that will be applied, for
	// example "string -> int (weak)". Empty when no conversion is needed.
	Conversion string

	// Note holds extra details, such as "a new T is allocated" or
	// "merged into the existing map".
	Note string

	// Issue describes a problem the decoder will likely report when the
	// plan is applied, for example an unconvertible type.
	Issue string

	// Opaque is true when the step cannot be previewed without running
	// user code (a DecodeHook or an Unmarshaler). The structural mapping
	// is still listed, but the actual outcome may differ.
	Opaque bool

	// Reason explains why the step is opaque.
	Reason string
}

// String returns a single-line human-readable summary of the step.
func (s PlanStep) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s", s.Action, s.Path)
	if s.TargetType != "" {
		fmt.Fprintf(&b, " (%s)", s.TargetType)
	}
	if s.Conversion != "" {
		fmt.Fprintf(&b, ": %s", s.Conversion)
	}
	if s.Note != "" {
		fmt.Fprintf(&b, " [%s]", s.Note)
	}
	if s.Issue != "" {
		fmt.Fprintf(&b, " ! %s", s.Issue)
	}
	if s.Opaque {
		fmt.Fprintf(&b, " (opaque: %s)", s.Reason)
	}
	return b.String()
}

// ErrStalePlan is returned by AssignmentPlan.Apply when the input or the
// decoder configuration changed after the plan was created. The plan is
// rejected instead of silently applying decisions based on stale data.
var ErrStalePlan = errors.New("mapstructure: plan is stale")

// ErrOpaquePlan is returned by AssignmentPlan.Apply when the plan contains
// steps that cannot be previewed without running user code (a DecodeHook or
// an Unmarshaler) and ApplyOptions.AllowOpaque was not set.
var ErrOpaquePlan = errors.New("mapstructure: plan contains steps that require running user code")

// AssignmentPlan is a preview of what decoding a specific input into a
// specific target type would do. It is created with Plan and executed with
// Apply once the caller has reviewed and approved it.
//
// A plan only stores paths, type names and fingerprints. It never stores
// reflect.Value handles (or any other address) into the target object, so a
// plan stays valid even if the target is moved, copied or garbage collected
// between planning and applying.
type AssignmentPlan struct {
	// Steps lists every assignment, in target field order.
	Steps []PlanStep

	input             any
	config            *DecoderConfig
	targetType        reflect.Type
	inputFingerprint  uint64
	configFingerprint uint64
}

// ApplyOptions controls how AssignmentPlan.Apply executes a plan.
type ApplyOptions struct {
	// AllowOpaque must be set to true to execute a plan that contains
	// opaque steps (steps that require running user code, such as decode
	// hooks or Unmarshaler implementations). When false, Apply refuses to
	// run such plans and returns an error wrapping ErrOpaquePlan.
	AllowOpaque bool
}

// Plan previews decoding input into the target type described by
// config.Result and returns the resulting assignment plan.
//
// config.Result must be a non-nil pointer; it is used to determine the
// target type and is inspected read-only (for example to decide whether a
// map would be merged or a pointer allocated). Planning never modifies the
// target and never calls user code: decode hooks and Unmarshaler
// implementations are not executed, the affected steps are marked Opaque
// instead.
//
// The plan captures fingerprints of the input and the configuration. If
// either changes after planning, Apply refuses to run and returns an error
// wrapping ErrStalePlan, so stale decisions are never applied silently.
func Plan(config *DecoderConfig, input any) (*AssignmentPlan, error) {
	if config == nil {
		return nil, errors.New("config must not be nil")
	}
	resultType := reflect.TypeOf(config.Result)
	if resultType == nil || resultType.Kind() != reflect.Ptr {
		return nil, errors.New("config.Result must be a non-nil pointer so the target type is known")
	}
	targetType := resultType.Elem()

	normalized := normalizePlanConfig(config)

	// The target is only inspected, never modified.
	var targetVal reflect.Value
	if resultVal := reflect.ValueOf(config.Result); !resultVal.IsNil() {
		targetVal = resultVal.Elem()
	}

	p := &planner{config: &normalized}
	p.walk(normalized.RootName, input, targetType, targetVal)

	return &AssignmentPlan{
		Steps:             p.steps,
		input:             input,
		config:            config,
		targetType:        targetType,
		inputFingerprint:  fingerprintInput(input),
		configFingerprint: fingerprintConfig(normalized),
	}, nil
}

// Opaque reports whether the plan contains steps that could not be
// previewed without running user code.
func (p *AssignmentPlan) Opaque() bool {
	return len(p.OpaqueSteps()) > 0
}

// OpaqueSteps returns the steps that could not be previewed without running
// user code (decode hooks or Unmarshaler implementations).
func (p *AssignmentPlan) OpaqueSteps() []PlanStep {
	var steps []PlanStep
	for _, s := range p.Steps {
		if s.Opaque {
			steps = append(steps, s)
		}
	}
	return steps
}

// Apply executes the plan against target, which must be a non-nil pointer
// to the same type as the config.Result the plan was created with.
//
// Before executing, Apply verifies that neither the input nor the decoder
// configuration changed since Plan was called; if they did, an error
// wrapping ErrStalePlan is returned. If the plan contains opaque steps and
// opts does not allow them, an error wrapping ErrOpaquePlan is returned and
// nothing is decoded.
//
// Execution performs a regular decode with the planned configuration, so
// the result is identical to decoding directly.
func (p *AssignmentPlan) Apply(target any, opts *ApplyOptions) error {
	if target == nil {
		return errors.New("target must be a non-nil pointer")
	}
	targetType := reflect.TypeOf(target)
	if targetType.Kind() != reflect.Ptr || targetType.Elem() != p.targetType {
		return fmt.Errorf("target must be a pointer to the planned type %s, got %s", p.targetType, targetType)
	}
	if fingerprintConfig(normalizePlanConfig(p.config)) != p.configFingerprint {
		return fmt.Errorf("%w: decoder configuration changed after planning", ErrStalePlan)
	}
	if fingerprintInput(p.input) != p.inputFingerprint {
		return fmt.Errorf("%w: input changed after planning", ErrStalePlan)
	}
	if opts == nil || !opts.AllowOpaque {
		if opaque := p.OpaqueSteps(); len(opaque) > 0 {
			return fmt.Errorf("%w: step %q (%s); approve explicitly with ApplyOptions.AllowOpaque",
				ErrOpaquePlan, opaque[0].Path, opaque[0].Reason)
		}
	}

	config := *p.config
	config.Result = target
	decoder, err := NewDecoder(&config)
	if err != nil {
		return err
	}
	return decoder.Decode(p.input)
}

// normalizePlanConfig returns a copy of config with the same defaults
// NewDecoder applies, without mutating the caller's configuration.
func normalizePlanConfig(config *DecoderConfig) DecoderConfig {
	normalized := *config
	if normalized.TagName == "" {
		normalized.TagName = "mapstructure"
	}
	if normalized.SquashTagOption == "" {
		normalized.SquashTagOption = "squash"
	}
	if normalized.MatchName == nil {
		normalized.MatchName = strings.EqualFold
	}
	if normalized.MapFieldName == nil {
		normalized.MapFieldName = func(s string) string {
			return s
		}
	}
	return normalized
}

// planner walks the input and the target type and records the steps
// decoding would perform. It never sets values on the target and never
// runs user code.
type planner struct {
	config *DecoderConfig
	steps  []PlanStep
}

func (p *planner) add(step PlanStep) {
	if p.config.DecodeHook != nil && !step.Opaque {
		step.Opaque = true
		step.Reason = "a decode hook may transform this value; preview would require running user code"
	}
	p.steps = append(p.steps, step)
}

func (p *planner) walk(name string, input any, targetType reflect.Type, targetVal reflect.Value) {
	if isNil(input) {
		input = nil
	}
	if input == nil {
		step := PlanStep{
			Path:       displayPath(name),
			TargetType: targetType.String(),
		}
		if p.config.ZeroFields {
			step.Action = ActionZero
			step.Note = "nil input; field is reset to its zero value (ZeroFields)"
		} else {
			step.Action = ActionPreserve
			step.Note = "nil input; field is left untouched"
		}
		p.add(step)
		return
	}

	inputVal := reflect.ValueOf(input)
	inputType := inputVal.Type()

	if !p.config.DisableUnmarshaler && implementsUnmarshaler(targetType) {
		p.add(PlanStep{
			Path:       displayPath(name),
			Action:     ActionSet,
			SourceType: inputType.String(),
			TargetType: targetType.String(),
			Opaque:     true,
			Reason:     "target implements Unmarshaler; preview would require running user code",
		})
		return
	}

	switch kindOf(targetType) {
	case reflect.Struct:
		p.walkStruct(name, inputVal, targetType, targetVal)
	case reflect.Map:
		p.walkMap(name, inputVal, targetType, targetVal)
	case reflect.Ptr:
		p.walkPtr(name, input, inputVal, targetType, targetVal)
	case reflect.Slice:
		p.walkSlice(name, input, inputVal, targetType, targetVal)
	case reflect.Array:
		p.walkArray(name, input, inputVal, targetType, targetVal)
	case reflect.Func:
		step := PlanStep{
			Path:       displayPath(name),
			Action:     ActionSet,
			SourceType: inputType.String(),
			TargetType: targetType.String(),
		}
		if inputType != targetType {
			step.Issue = fmt.Sprintf("cannot assign %s to %s", inputType, targetType)
		}
		p.add(step)
	default:
		p.walkLeaf(name, inputVal, targetType)
	}
}

// walkLeaf records the conversion of a scalar (bool, number, string,
// complex or interface) target field.
func (p *planner) walkLeaf(name string, inputVal reflect.Value, targetType reflect.Type) {
	note, ok := conversionNote(inputVal, targetType, p.config.WeaklyTypedInput)
	step := PlanStep{
		Path:       displayPath(name),
		Action:     ActionSet,
		SourceType: inputVal.Type().String(),
		TargetType: targetType.String(),
		Conversion: note,
	}
	if !ok {
		step.Issue = fmt.Sprintf("cannot convert %s to %s", inputVal.Type(), targetType)
	}
	p.add(step)
}

func (p *planner) walkStruct(name string, inputVal reflect.Value, targetType reflect.Type, targetVal reflect.Value) {
	dataVal := reflect.Indirect(inputVal)
	sourceType := inputVal.Type().String()

	if dataVal.IsValid() && dataVal.Type() == targetType {
		p.add(PlanStep{
			Path:       displayPath(name),
			Action:     ActionSet,
			SourceType: sourceType,
			TargetType: targetType.String(),
			Conversion: sourceType + " -> " + targetType.String(),
		})
		return
	}

	switch dataVal.Kind() {
	case reflect.Map:
		keyKind := dataVal.Type().Key().Kind()
		if keyKind != reflect.String && keyKind != reflect.Interface {
			p.add(PlanStep{
				Path:       displayPath(name),
				Action:     ActionSet,
				SourceType: sourceType,
				TargetType: targetType.String(),
				Issue:      fmt.Sprintf("needs a map with string keys, has %q keys", keyKind),
			})
			return
		}
		p.walkStructFromMap(name, dataVal, targetType, targetVal)
	case reflect.Struct:
		p.add(PlanStep{
			Path:       displayPath(name),
			Action:     ActionSet,
			SourceType: sourceType,
			TargetType: targetType.String(),
			Note:       "input struct is converted through an intermediate map; nested field mapping is not expanded in the preview",
		})
	default:
		p.add(PlanStep{
			Path:       displayPath(name),
			Action:     ActionSet,
			SourceType: sourceType,
			TargetType: targetType.String(),
			Issue:      fmt.Sprintf("expected a map or struct, got %q", dataVal.Kind()),
		})
	}
}

func (p *planner) walkStructFromMap(name string, dataVal reflect.Value, targetType reflect.Type, targetVal reflect.Value) {
	fields, remain := p.structFields(targetType, targetVal)
	usedKeys := make(map[any]struct{})

	for _, f := range fields {
		tagValue, _ := getTagValue(f.field, p.config.TagName)
		if tagValue == "" && p.config.IgnoreUntaggedFields {
			continue
		}
		tagValue = strings.SplitN(tagValue, ",", 2)[0]
		fieldName := f.field.Name
		if tagValue != "" {
			fieldName = tagValue
		} else {
			fieldName = p.config.MapFieldName(fieldName)
		}

		rawMapVal := mapLookup(dataVal, fieldName)
		var usedKey any
		if rawMapVal.IsValid() {
			usedKey = fieldName
		} else {
			// Slower search: iterate over the keys and match by name.
			for _, dataKey := range dataVal.MapKeys() {
				keyStr, ok := dataKey.Interface().(string)
				if !ok {
					continue
				}
				if p.config.MatchName(keyStr, fieldName) {
					rawMapVal = dataVal.MapIndex(dataKey)
					usedKey = dataKey.Interface()
					break
				}
			}
		}

		childName := joinName(name, fieldName)
		if !rawMapVal.IsValid() {
			step := PlanStep{
				Path:       displayPath(childName),
				Action:     ActionPreserve,
				TargetType: f.typ.String(),
				Note:       "no matching key in input; field is left untouched",
			}
			if p.config.ErrorUnset && !(p.config.AllowUnsetPointer && f.typ.Kind() == reflect.Ptr) {
				step.Issue = "field is not set by the input (ErrorUnset)"
			}
			p.add(step)
			continue
		}
		usedKeys[usedKey] = struct{}{}

		p.walk(childName, rawMapVal.Interface(), f.typ, f.val)
	}

	unused := 0
	for _, k := range dataVal.MapKeys() {
		if _, ok := usedKeys[k.Interface()]; !ok {
			unused++
		}
	}

	if remain != nil && unused > 0 {
		p.add(PlanStep{
			Path:       displayPath(joinName(name, remain.field.Name)),
			Action:     ActionSet,
			TargetType: remain.typ.String(),
			Note:       fmt.Sprintf("remain field collects %d unused input key(s)", unused),
		})
		unused = 0
	}

	if p.config.ErrorUnused && unused > 0 {
		p.add(PlanStep{
			Path:       displayPath(name),
			Action:     ActionPreserve,
			TargetType: targetType.String(),
			Issue:      fmt.Sprintf("input has %d key(s) with no matching field (ErrorUnused)", unused),
		})
	}
}

// planField is a struct field discovered by the planner, including fields
// of squashed embedded structs.
type planField struct {
	field reflect.StructField
	typ   reflect.Type
	val   reflect.Value // may be invalid
}

// structFields flattens the fields of a struct type, expanding squashed
// embedded structs like decodeStructFromMap does.
func (p *planner) structFields(t reflect.Type, tv reflect.Value) (fields []planField, remain *planField) {
	type item struct {
		typ reflect.Type
		val reflect.Value
	}
	queue := []item{{t, tv}}
	for len(queue) > 0 {
		it := queue[0]
		queue = queue[1:]

		for i := 0; i < it.typ.NumField(); i++ {
			fieldType := it.typ.Field(i)
			if fieldType.PkgPath != "" && !fieldType.Anonymous {
				// Unexported fields cannot be set by the decoder.
				continue
			}

			var fieldVal reflect.Value
			if it.val.IsValid() && it.val.Kind() == reflect.Struct {
				fieldVal = it.val.Field(i)
			}

			squash := false
			remainTag := false
			tagParts := getTagParts(fieldType, p.config.TagName)
			if len(tagParts) == 0 {
				tagParts = []string{""}
			}
			for _, tag := range tagParts[1:] {
				if tag == p.config.SquashTagOption {
					squash = true
				}
				if tag == "remain" {
					remainTag = true
				}
			}

			baseType := fieldType.Type
			for baseType.Kind() == reflect.Ptr {
				baseType = baseType.Elem()
			}
			if p.config.Squash && fieldType.Anonymous && baseType.Kind() == reflect.Struct {
				squash = true
			}

			if squash {
				if baseType.Kind() != reflect.Struct {
					// Interface or unsupported squash targets cannot be
					// expanded statically.
					continue
				}
				var elemVal reflect.Value
				if fieldVal.IsValid() {
					elemVal = fieldVal
					for elemVal.Kind() == reflect.Ptr {
						if elemVal.IsNil() {
							elemVal = reflect.Value{}
							break
						}
						elemVal = elemVal.Elem()
					}
					if elemVal.IsValid() && elemVal.Kind() != reflect.Struct {
						elemVal = reflect.Value{}
					}
				}
				queue = append(queue, item{baseType, elemVal})
				continue
			}

			f := planField{field: fieldType, typ: fieldType.Type, val: fieldVal}
			if remainTag {
				remain = &f
			} else {
				fields = append(fields, f)
			}
		}
	}
	return fields, remain
}

func (p *planner) walkMap(name string, inputVal reflect.Value, targetType reflect.Type, targetVal reflect.Value) {
	dataVal := inputVal
	for dataVal.Kind() == reflect.Ptr {
		dataVal = dataVal.Elem()
	}
	sourceType := inputVal.Type().String()

	step := PlanStep{
		Path:       displayPath(name),
		Action:     ActionSet,
		SourceType: sourceType,
		TargetType: targetType.String(),
	}

	switch dataVal.Kind() {
	case reflect.Map:
		switch {
		case p.config.ZeroFields:
			step.Note = "existing map content is discarded (ZeroFields)"
		case !targetVal.IsValid() || targetVal.IsNil():
			step.Note = "a new map is allocated"
		default:
			step.Note = fmt.Sprintf("merged into the existing map (%d existing key(s) preserved)", targetVal.Len())
		}
		p.add(step)

		keys := dataVal.MapKeys()
		sort.Slice(keys, func(i, j int) bool {
			return fmt.Sprintf("%v", safeInterface(keys[i])) < fmt.Sprintf("%v", safeInterface(keys[j]))
		})
		for _, k := range keys {
			childName := name + "[" + fmt.Sprintf("%v", safeInterface(k)) + "]"
			p.walk(childName, safeInterface(dataVal.MapIndex(k)), targetType.Elem(), reflect.Value{})
		}
	case reflect.Struct:
		step.Note = "decoded from struct fields"
		p.add(step)
	case reflect.Slice, reflect.Array:
		if !p.config.WeaklyTypedInput {
			step.Issue = fmt.Sprintf("cannot convert %s to %s", sourceType, targetType)
			p.add(step)
			return
		}
		step.Note = "slice of maps is merged into the map (weak)"
		p.add(step)
		for i := 0; i < dataVal.Len(); i++ {
			p.walk(name+"["+strconv.Itoa(i)+"]", dataVal.Index(i).Interface(), targetType, targetVal)
		}
	default:
		step.Issue = fmt.Sprintf("cannot convert %s to %s", sourceType, targetType)
		p.add(step)
	}
}

func (p *planner) walkPtr(name string, input any, inputVal reflect.Value, targetType reflect.Type, targetVal reflect.Value) {
	nilData := false
	indirect := reflect.Indirect(inputVal)
	switch indirect.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Ptr, reflect.Slice:
		nilData = indirect.IsNil()
	}

	if nilData {
		step := PlanStep{
			Path:       displayPath(name),
			SourceType: inputVal.Type().String(),
			TargetType: targetType.String(),
		}
		if targetVal.IsValid() && !targetVal.IsNil() {
			step.Action = ActionZero
			step.Note = "nil input; pointer is set to nil"
		} else {
			step.Action = ActionPreserve
			step.Note = "nil input; pointer is left untouched"
		}
		p.add(step)
		return
	}

	note := "existing pointed-to value is reused"
	switch {
	case p.config.ZeroFields:
		note = "a new " + targetType.Elem().String() + " is allocated (ZeroFields)"
	case !targetVal.IsValid() || targetVal.IsNil():
		note = "a new " + targetType.Elem().String() + " is allocated"
	}
	p.add(PlanStep{
		Path:       displayPath(name),
		Action:     ActionSet,
		SourceType: inputVal.Type().String(),
		TargetType: targetType.String(),
		Note:       note,
	})

	var elemVal reflect.Value
	if targetVal.IsValid() && !targetVal.IsNil() && !p.config.ZeroFields {
		elemVal = targetVal.Elem()
	}
	p.walk(name, input, targetType.Elem(), elemVal)
}

func (p *planner) walkSlice(name string, input any, inputVal reflect.Value, targetType reflect.Type, targetVal reflect.Value) {
	dataVal := reflect.Indirect(inputVal)
	step := PlanStep{
		Path:       displayPath(name),
		Action:     ActionSet,
		SourceType: inputVal.Type().String(),
		TargetType: targetType.String(),
	}

	if dataVal.Kind() != reflect.Array && dataVal.Kind() != reflect.Slice {
		if p.config.WeaklyTypedInput {
			switch {
			case dataVal.Kind() == reflect.Map && dataVal.Len() == 0:
				step.Note = "empty map decodes to an empty slice (weak)"
				p.add(step)
			case dataVal.Kind() == reflect.Map:
				step.Note = "map is lifted into a single-element slice (weak)"
				p.add(step)
				p.walk(name+"[0]", input, targetType.Elem(), reflect.Value{})
			case dataVal.Kind() == reflect.String && targetType.Elem().Kind() == reflect.Uint8:
				step.Conversion = "string -> []uint8 (weak)"
				p.add(step)
			default:
				step.Note = "single value is lifted into a slice (weak)"
				p.add(step)
				p.walk(name+"[0]", input, targetType.Elem(), reflect.Value{})
			}
			return
		}
		step.Issue = fmt.Sprintf("source data must be an array or slice, got %s", dataVal.Kind())
		p.add(step)
		return
	}

	if dataVal.Kind() != reflect.Array && dataVal.IsNil() {
		step.Action = ActionPreserve
		step.Note = "nil slice input; field is left untouched"
		p.add(step)
		return
	}

	switch {
	case p.config.ZeroFields:
		step.Note = "existing slice is discarded (ZeroFields)"
	case !targetVal.IsValid() || targetVal.IsNil():
		step.Note = "a new slice is allocated"
	default:
		step.Note = "existing slice is reused"
	}
	p.add(step)

	for i := 0; i < dataVal.Len(); i++ {
		p.walk(name+"["+strconv.Itoa(i)+"]", dataVal.Index(i).Interface(), targetType.Elem(), reflect.Value{})
	}
}

func (p *planner) walkArray(name string, input any, inputVal reflect.Value, targetType reflect.Type, targetVal reflect.Value) {
	dataVal := reflect.Indirect(inputVal)
	step := PlanStep{
		Path:       displayPath(name),
		Action:     ActionSet,
		SourceType: inputVal.Type().String(),
		TargetType: targetType.String(),
	}

	if dataVal.Kind() != reflect.Array && dataVal.Kind() != reflect.Slice {
		if p.config.WeaklyTypedInput {
			if dataVal.Kind() == reflect.Map && dataVal.Len() == 0 {
				step.Note = "empty map decodes to a zero array (weak)"
				p.add(step)
				return
			}
			step.Note = "single value is lifted into an array (weak)"
			p.add(step)
			p.walk(name+"[0]", input, targetType.Elem(), reflect.Value{})
			return
		}
		step.Issue = fmt.Sprintf("source data must be an array or slice, got %s", dataVal.Kind())
		p.add(step)
		return
	}

	if dataVal.Len() > targetType.Len() {
		step.Issue = fmt.Sprintf("expected source data to have length less or equal to %d, got %d", targetType.Len(), dataVal.Len())
		p.add(step)
		return
	}

	p.add(step)
	for i := 0; i < dataVal.Len(); i++ {
		p.walk(name+"["+strconv.Itoa(i)+"]", dataVal.Index(i).Interface(), targetType.Elem(), reflect.Value{})
	}
}

// displayPath renders a step path, naming the root "(root)".
func displayPath(name string) string {
	if name == "" {
		return "(root)"
	}
	return name
}

// joinName joins a parent path and a field name the same way decode does.
func joinName(name, field string) string {
	if name == "" {
		return field
	}
	return name + "." + field
}

// mapLookup looks up key in a map, converting the key type if needed.
func mapLookup(dataVal reflect.Value, key string) reflect.Value {
	k := reflect.ValueOf(key)
	keyType := dataVal.Type().Key()
	if k.Type().AssignableTo(keyType) {
		return dataVal.MapIndex(k)
	}
	if k.Type().ConvertibleTo(keyType) {
		return dataVal.MapIndex(k.Convert(keyType))
	}
	return reflect.Value{}
}

// kindOf is the type-based equivalent of getKind.
func kindOf(t reflect.Type) reflect.Kind {
	kind := t.Kind()
	switch {
	case kind >= reflect.Int && kind <= reflect.Int64:
		return reflect.Int
	case kind >= reflect.Uint && kind <= reflect.Uint64:
		return reflect.Uint
	case kind >= reflect.Float32 && kind <= reflect.Float64:
		return reflect.Float32
	case kind >= reflect.Complex64 && kind <= reflect.Complex128:
		return reflect.Complex64
	default:
		return kind
	}
}

// implementsUnmarshaler reports whether values of type t (or pointers to
// it) implement Unmarshaler.
func implementsUnmarshaler(t reflect.Type) bool {
	if t.Implements(unmarshalerType) {
		return true
	}
	if t.Kind() != reflect.Ptr && reflect.PtrTo(t).Implements(unmarshalerType) {
		return true
	}
	return false
}

// conversionNote describes the conversion decoding would apply to turn the
// input value into the target type, mirroring the scalar decode functions.
// The second return value reports whether the conversion is possible.
func conversionNote(inputVal reflect.Value, targetType reflect.Type, weak bool) (string, bool) {
	from := inputVal.Type()
	pair := from.String() + " -> " + targetType.String()
	if from.AssignableTo(targetType) {
		return pair, true
	}

	if kindOf(targetType) == reflect.Interface {
		if from.Kind() == reflect.Ptr && from.Elem().AssignableTo(targetType) {
			return pair + " (deref)", true
		}
		return "", false
	}

	// The scalar decoders resolve pointer indirection before converting.
	base := from
	for base.Kind() == reflect.Ptr {
		base = base.Elem()
	}
	if base != from && base.AssignableTo(targetType) {
		return pair + " (deref)", true
	}

	basePair := base.String() + " -> " + targetType.String()
	weakNote := basePair + " (weak)"
	fromKind := kindOf(base)
	isJSONNumber := base.PkgPath() == "encoding/json" && base.Name() == "Number"

	switch kindOf(targetType) {
	case reflect.Bool:
		switch fromKind {
		case reflect.Int, reflect.Uint, reflect.Float32, reflect.String:
			if weak {
				return weakNote, true
			}
		}
	case reflect.Int:
		switch {
		case fromKind == reflect.Int || fromKind == reflect.Uint || fromKind == reflect.Float32:
			return basePair, true
		case fromKind == reflect.Bool || fromKind == reflect.String:
			if weak {
				return weakNote, true
			}
		case isJSONNumber:
			return basePair, true
		}
	case reflect.Uint:
		switch {
		case fromKind == reflect.Uint:
			return basePair, true
		case fromKind == reflect.Int || fromKind == reflect.Float32:
			if weak || !isNegativeNumber(inputVal) {
				return basePair, true
			}
		case fromKind == reflect.Bool || fromKind == reflect.String:
			if weak {
				return weakNote, true
			}
		case isJSONNumber:
			return basePair, true
		}
	case reflect.Float32:
		switch {
		case fromKind == reflect.Int || fromKind == reflect.Uint || fromKind == reflect.Float32:
			return basePair, true
		case fromKind == reflect.Bool || fromKind == reflect.String:
			if weak {
				return weakNote, true
			}
		case isJSONNumber:
			return basePair, true
		}
	case reflect.String:
		switch fromKind {
		case reflect.Bool, reflect.Int, reflect.Uint, reflect.Float32:
			if weak {
				return weakNote, true
			}
		case reflect.Slice, reflect.Array:
			if base.Elem().Kind() == reflect.Uint8 && weak {
				return weakNote, true
			}
		}
	}
	return "", false
}

// isNegativeNumber reports whether the input value is a negative number.
func isNegativeNumber(v reflect.Value) bool {
	v = reflect.Indirect(v)
	switch v.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int() < 0
	case reflect.Float32, reflect.Float64:
		return v.Float() < 0
	}
	return false
}

// fingerprintConfig hashes the decoder configuration options that affect
// decoding decisions, so post-plan mutations are detected.
func fingerprintConfig(config DecoderConfig) uint64 {
	h := fnv.New64a()
	bools := []bool{
		config.ErrorUnused,
		config.ErrorUnset,
		config.AllowUnsetPointer,
		config.ZeroFields,
		config.WeaklyTypedInput,
		config.Squash,
		config.Deep,
		config.DecodeNil,
		config.IgnoreUntaggedFields,
		config.DisableUnmarshaler,
	}
	for _, b := range bools {
		if b {
			h.Write([]byte{1})
		} else {
			h.Write([]byte{0})
		}
	}
	h.Write([]byte(config.TagName))
	h.Write([]byte{0})
	h.Write([]byte(config.RootName))
	h.Write([]byte{0})
	h.Write([]byte(config.SquashTagOption))
	h.Write([]byte{0})
	writeFuncFingerprint(h, config.DecodeHook)
	writeFuncFingerprint(h, config.MatchName)
	writeFuncFingerprint(h, config.MapFieldName)
	return h.Sum64()
}

func writeFuncFingerprint(h hash.Hash64, fn any) {
	v := reflect.ValueOf(fn)
	if !v.IsValid() || v.Kind() != reflect.Func {
		h.Write([]byte("nil"))
		h.Write([]byte{0})
		return
	}
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], uint64(v.Pointer()))
	h.Write(buf[:])
}

// fingerprintMaxDepth bounds the input fingerprint walk so cyclic inputs
// cannot cause infinite recursion.
const fingerprintMaxDepth = 64

// fingerprintInput hashes a canonical representation of the input value so
// post-plan mutations are detected. Map keys are sorted by their own
// fingerprint, making the result independent of map iteration order.
func fingerprintInput(input any) uint64 {
	h := fnv.New64a()
	fingerprintValue(h, reflect.ValueOf(input), 0)
	return h.Sum64()
}

func fingerprintValue(h hash.Hash64, v reflect.Value, depth int) {
	if depth > fingerprintMaxDepth {
		h.Write([]byte("<max-depth>"))
		return
	}
	if !v.IsValid() {
		h.Write([]byte("<invalid>"))
		return
	}

	h.Write([]byte(v.Type().String()))
	h.Write([]byte{0})

	switch v.Kind() {
	case reflect.Interface:
		if v.IsNil() {
			h.Write([]byte("<nil>"))
			return
		}
		fingerprintValue(h, v.Elem(), depth+1)
	case reflect.Ptr:
		if v.IsNil() {
			h.Write([]byte("<nil-ptr>"))
			return
		}
		fingerprintValue(h, v.Elem(), depth+1)
	case reflect.Map:
		if v.IsNil() {
			h.Write([]byte("<nil-map>"))
			return
		}
		type entry struct {
			keyHash string
			keyName string
			value   reflect.Value
		}
		keys := v.MapKeys()
		entries := make([]entry, 0, len(keys))
		for _, k := range keys {
			kh := fnv.New64a()
			fingerprintValue(kh, k, depth+1)
			entries = append(entries, entry{
				keyHash: string(sum64Bytes(kh)),
				keyName: fmt.Sprintf("%v", safeInterface(k)),
				value:   v.MapIndex(k),
			})
		}
		sort.Slice(entries, func(i, j int) bool {
			if entries[i].keyHash != entries[j].keyHash {
				return entries[i].keyHash < entries[j].keyHash
			}
			return entries[i].keyName < entries[j].keyName
		})
		for _, e := range entries {
			h.Write([]byte(e.keyHash))
			fingerprintValue(h, e.value, depth+1)
		}
	case reflect.Slice, reflect.Array:
		if v.Kind() == reflect.Slice && v.IsNil() {
			h.Write([]byte("<nil-slice>"))
			return
		}
		var buf [8]byte
		binary.LittleEndian.PutUint64(buf[:], uint64(v.Len()))
		h.Write(buf[:])
		for i := 0; i < v.Len(); i++ {
			fingerprintValue(h, v.Index(i), depth+1)
		}
	case reflect.String:
		h.Write([]byte(v.String()))
	case reflect.Bool:
		if v.Bool() {
			h.Write([]byte{1})
		} else {
			h.Write([]byte{0})
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		var buf [8]byte
		binary.LittleEndian.PutUint64(buf[:], uint64(v.Int()))
		h.Write(buf[:])
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		var buf [8]byte
		binary.LittleEndian.PutUint64(buf[:], v.Uint())
		h.Write(buf[:])
	case reflect.Float32, reflect.Float64:
		var buf [8]byte
		binary.LittleEndian.PutUint64(buf[:], math.Float64bits(v.Float()))
		h.Write(buf[:])
	case reflect.Complex64, reflect.Complex128:
		var buf [16]byte
		c := v.Complex()
		binary.LittleEndian.PutUint64(buf[:8], math.Float64bits(real(c)))
		binary.LittleEndian.PutUint64(buf[8:], math.Float64bits(imag(c)))
		h.Write(buf[:])
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			fingerprintValue(h, v.Field(i), depth+1)
		}
	case reflect.Func, reflect.Chan, reflect.UnsafePointer:
		var buf [8]byte
		binary.LittleEndian.PutUint64(buf[:], uint64(v.Pointer()))
		h.Write(buf[:])
	default:
		h.Write([]byte("<kind:" + v.Kind().String() + ">"))
	}
}

func sum64Bytes(h hash.Hash64) []byte {
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], h.Sum64())
	return buf[:]
}
