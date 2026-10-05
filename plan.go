package mapstructure

import (
	stderrors "errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/go-viper/mapstructure/v2/internal/errors"
)

// OpKind describes the kind of assignment a planned [Op] performs.
type OpKind int

const (
	// OpSet sets a field, element or the whole target to a new value.
	// Allocating a previously nil pointer is represented as an OpSet.
	OpSet OpKind = iota
	// OpZero resets a value to the zero value of its type. For map entries
	// this means deleting the key, matching decoder semantics.
	OpZero
	// OpRetain leaves an existing value untouched because the input does not
	// address it.
	OpRetain
	// OpUserCode delegates an entire subtree to caller-supplied code: a
	// DecodeHook or an Unmarshaler implementation. The resulting writes
	// cannot be enumerated ahead of time.
	OpUserCode
)

// String returns a human readable name for the operation kind.
func (k OpKind) String() string {
	switch k {
	case OpSet:
		return "set"
	case OpZero:
		return "zero"
	case OpRetain:
		return "retain"
	case OpUserCode:
		return "user-code"
	default:
		return "unknown"
	}
}

// CallbackKind identifies which kind of user code an [OpUserCode] operation
// invokes during execution.
type CallbackKind int

const (
	// HookCallback is a configured [DecodeHookFunc].
	HookCallback CallbackKind = iota
	// UnmarshalerCallback is a value implementing [Unmarshaler].
	UnmarshalerCallback
)

// String returns a human readable name for the callback kind.
func (k CallbackKind) String() string {
	switch k {
	case HookCallback:
		return "decode hook"
	case UnmarshalerCallback:
		return "UnmarshalMapstructure"
	default:
		return "unknown"
	}
}

// SegmentKind distinguishes struct fields from indexed access on a path.
type SegmentKind int

const (
	// FieldSegment addresses a (possibly promoted) struct field.
	FieldSegment SegmentKind = iota
	// KeySegment addresses a map entry.
	KeySegment
	// IndexSegment addresses an array or slice element.
	IndexSegment
	// DerefSegment descends through a pointer or interface. It carries no
	// name and exists only for structural navigation during execution.
	DerefSegment
)

// PathSegment is one step locating an [Op] inside the target value.
//
// Key and Index are only meaningful for [KeySegment] and [IndexSegment]
// respectively. No reflect.Value is ever stored here, so a plan remains safe
// to keep even if the target object is replaced.
type PathSegment struct {
	Kind  SegmentKind
	Field string
	Key   any
	Index int
}

// Op is a single assignment that will be performed when a [Plan] is applied.
type Op struct {
	// Kind is the kind of assignment.
	Kind OpKind
	// Path locates the value inside the root target. It is empty for an
	// operation that replaces the whole root target.
	Path []PathSegment
	// Name is the decoder-style dotted/bracketed name, for example
	// "Person.Address[city]".
	Name string
	// TargetType is the type of the addressed value.
	TargetType reflect.Type
	// Value holds the concrete value that will be written for [OpSet].
	// It is nil for other kinds and for values that cannot be boxed.
	Value any
	// Conversion describes the type conversion used to produce Value.
	Conversion string
	// Callback reports which kind of user code is invoked for [OpUserCode].
	Callback CallbackKind
}

// ApplyOptions controls execution of a [Plan].
type ApplyOptions struct {
	// AllowUserCode permits execution of [OpUserCode] operations. Without
	// it, applying a plan that contains such an operation fails with
	// ErrUserCodeRequired. This gives callers an explicit approval gate for
	// hooks and Unmarshalers that could not be previewed.
	AllowUserCode bool
}

var (
	// ErrUserCodeRequired is returned when applying a plan that needs to run
	// caller-provided code without [ApplyOptions.AllowUserCode] being set.
	ErrUserCodeRequired = stderrors.New("mapstructure: plan requires calling user code (decode hook or Unmarshaler); set ApplyOptions.AllowUserCode to approve")
	// ErrPlanStale is returned when the input, configuration or target has
	// changed since the plan was computed, so applying it would silently act
	// on a stale judgement.
	ErrPlanStale = stderrors.New("mapstructure: plan is stale because the input, configuration or target changed after planning")
)

// Plan is an auditable, deferred assignment. It never holds addresses into
// the target object (only type information and detached value copies), so it
// survives changes of the underlying reflect.Values. A plan must be
// re-validated before it mutates anything.
type Plan struct {
	ops            []Op
	inputFP        string
	configFP       string
	targetFP       string
	input          any
	originalInput  any
	result         any
	configSnapshot *DecoderConfig
	originalConfig *DecoderConfig
}

// Ops returns all planned operations in execution (pre-order) order. The
// returned slice must not be modified.
func (p *Plan) Ops() []Op { return p.ops }

// Set returns the operations that write a value, including nil pointer
// allocations.
func (p *Plan) Set() []Op { return p.filter(OpSet) }

// Zero returns the operations that reset values to their zero value.
func (p *Plan) Zero() []Op { return p.filter(OpZero) }

// Retained returns the fields and entries that the input does not change.
func (p *Plan) Retained() []Op { return p.filter(OpRetain) }

// UserCode returns the operations that delegate to a hook or Unmarshaler.
// Each one names the exact target and callback kind.
func (p *Plan) UserCode() []Op { return p.filter(OpUserCode) }

// RequiresUserCode reports whether applying the plan invokes caller code
// that could not be previewed.
func (p *Plan) RequiresUserCode() bool {
	for i := range p.ops {
		if p.ops[i].Kind == OpUserCode {
			return true
		}
	}
	return false
}

func (p *Plan) filter(kind OpKind) []Op {
	out := make([]Op, 0)
	for i := range p.ops {
		if p.ops[i].Kind == kind {
			out = append(out, p.ops[i])
		}
	}
	return out
}

// Plan computes the assignment that [Decoder.Decode] would perform on input
// without modifying the target. The returned [Plan] enumerates every field
// that would be set, zeroed or retained together with the conversions used,
// and flags every hook or Unmarshaler that cannot be previewed. Call
// [Plan.Apply] to execute after review.
func (d *Decoder) Plan(input any) (*Plan, error) {
	root := reflect.ValueOf(d.config.Result).Elem()

	// Detach the input and current target so planning never mutates the
	// caller's data and stored values cannot be invalidated by later changes.
	snapshotInput := deepDetach(reflect.ValueOf(input), map[uintptr]bool{})
	shadowPtr := reflect.New(root.Type())
	shadowPtr.Elem().Set(deepDetach(root, map[uintptr]bool{}))
	var snapshotBoxed any
	if snapshotInput.IsValid() {
		snapshotBoxed = snapshotInput.Interface()
	}

	snapshot := *d.config
	snapshot.Result = shadowPtr.Interface()
	snapshot.Metadata = nil

	shadow, err := NewDecoder(&snapshot)
	if err != nil {
		return nil, err
	}
	pl := newPlanner()
	shadow.planner = pl

	decodeErr := shadow.decode(snapshot.RootName, snapshotBoxed, shadowPtr.Elem())
	if _, ok := decodeErr.(interface{ Unwrap() []error }); ok {
		decodeErr = fmt.Errorf("planning failed due to the following error(s):\n\n%w", decodeErr)
	}
	decodeErr = filterOpaqueErrors(decodeErr, pl.opaqueNames, snapshot.RootName)

	plan := &Plan{
		input:          snapshotBoxed,
		originalInput:  input,
		result:         d.config.Result,
		configSnapshot: &snapshot,
		originalConfig: d.config,
		inputFP:        fingerprint(reflect.ValueOf(input), map[visit]int{}),
		configFP:       configFingerprint(d.config),
		targetFP:       fingerprint(root, map[visit]int{}),
	}

	plan.ops = buildOps(snapshot.RootName, root, shadowPtr.Elem(), snapshotInput, pl)

	if decodeErr != nil {
		return plan, decodeErr
	}
	return plan, nil
}

// Apply validates that nothing changed since planning and performs the
// recorded assignment. By default [OpUserCode] operations are refused; the
// caller must approve them explicitly.
func (p *Plan) Apply(opts *ApplyOptions) error {
	allowUserCode := opts != nil && opts.AllowUserCode

	root := reflect.ValueOf(p.result).Elem()
	if fingerprint(root, map[visit]int{}) != p.targetFP ||
		fingerprint(reflect.ValueOf(p.originalInput), map[visit]int{}) != p.inputFP ||
		configFingerprint(p.originalConfig) != p.configFP {
		return ErrPlanStale
	}

	hasUserCode := false
	for i := range p.ops {
		if p.ops[i].Kind == OpUserCode {
			hasUserCode = true
			break
		}
	}
	if hasUserCode && !allowUserCode {
		return ErrUserCodeRequired
	}

	cfgCopy := *p.configSnapshot
	cfgCopy.Result = p.result
	cfgCopy.Metadata = nil
	if hasUserCode {
		// User code writes cannot be replayed from recorded values; run the
		// real decoder only after every freshness check above passed.
		dec, err := NewDecoder(&cfgCopy)
		if err != nil {
			return err
		}
		return dec.Decode(p.input)
	}

	for i := range p.ops {
		if err := replayOp(root, p.ops[i]); err != nil {
			return err
		}
	}
	return nil
}

// planner collects side-channel information while the shadow decoder runs.
type planner struct {
	zeroed      map[string]bool
	opaqueNames []string
	opaque      map[string]opaqueInfo
	inputTypes  map[string]reflect.Type
}

type opaqueInfo struct {
	callback CallbackKind
}

func newPlanner() *planner {
	return &planner{
		zeroed:     map[string]bool{},
		opaque:     map[string]opaqueInfo{},
		inputTypes: map[string]reflect.Type{},
	}
}

func (p *planner) markZero(name string) {
	p.zeroed[name] = true
}

// markInputType remembers the concrete input type seen at a node. It is
// captured at the start of decoding, before values are boxed into
// interface{} while traversing a map.
func (p *planner) markInputType(name string, input any) {
	if input == nil {
		return
	}
	t := reflect.TypeOf(input)
	if t == nil {
		return
	}
	p.inputTypes[name] = t
}

func (p *planner) markOpaque(name string, cb CallbackKind) {
	if _, exists := p.opaque[name]; !exists {
		p.opaque[name] = opaqueInfo{callback: cb}
		p.opaqueNames = append(p.opaqueNames, name)
	}
}

// underOpaque returns info when name is an opaque node itself or nested in one.
func (p *planner) underOpaque(name string) (opaqueInfo, string, bool) {
	best := ""
	for n := range p.opaque {
		if n == name || nameWithin(name, n) {
			if len(n) > len(best) {
				best = n
			}
		}
	}
	if best == "" {
		return opaqueInfo{}, "", false
	}
	return p.opaque[best], best, true
}

func nameWithin(name, prefix string) bool {
	if prefix == "" {
		return name != ""
	}
	if !strings.HasPrefix(name, prefix) {
		return false
	}
	rest := name[len(prefix):]
	return len(rest) == 0 || rest[0] == '.' || rest[0] == '['
}

// filterOpaqueErrors removes wrapped errors whose name is inside an opaque
// subtree. A fully opaque root drops all errors.
func filterOpaqueErrors(err error, opaqueNames []string, rootName string) error {
	if err == nil || len(opaqueNames) == 0 {
		return err
	}
	drop := func(name string) bool {
		for _, n := range opaqueNames {
			if n == rootName {
				return true
			}
			if name == n || nameWithin(name, n) {
				return true
			}
		}
		return false
	}
	filtered := []error{}
	walkErrors(err, func(e error) {
		var de *DecodeError
		if stderrors.As(e, &de) && drop(de.Name()) {
			return
		}
		filtered = append(filtered, e)
	})
	return errors.Join(filtered...)
}

func walkErrors(err error, fn func(error)) {
	if err == nil {
		return
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, e := range joined.Unwrap() {
			walkErrors(e, fn)
		}
		return
	}
	fn(err)
}
