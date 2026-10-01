package validator

// Option represents a configurations option to be applied to validator during initialization.
type Option func(*Validate)

// WithRequiredStructEnabled enables required tag on non-pointer structs to be applied instead of ignored.
//
// This was made opt-in behaviour in order to maintain backward compatibility with the behaviour previous
// to being able to apply struct level validations on struct fields directly.
//
// It is recommended you enabled this as it will be the default behaviour in v11+
func WithRequiredStructEnabled() Option {
	return func(v *Validate) {
		v.requiredStructEnabled = true
	}
}

// WithPrivateFieldValidation activates validation for unexported fields via the use of the `unsafe` package.
//
// By opting into this feature you are acknowledging that you are aware of the risks and accept any current or future
// consequences of using this feature.
func WithPrivateFieldValidation() Option {
	return func(v *Validate) {
		v.privateFieldValidation = true
	}
}

// WithTagNameFuncBlankOmit makes a blank return from a RegisterTagNameFunc omit
// the field from the error namespace instead of silently falling back to the
// struct field name.
//
// This was made opt-in behaviour to maintain backward compatibility with
// existing callers that rely on the fallback for error namespaces.
//
// It is recommended you enable this as it will be the default behaviour in v11+.
func WithTagNameFuncBlankOmit() Option {
	return func(v *Validate) {
		v.omitBlankFieldNames = true
	}
}

// WithCyclePolicy configures how validation reacts when a value is reached
// that is already present on the current traversal path, i.e. when the
// validated object graph contains a reference cycle (for example a parent
// node whose field points back to one of its ancestors).
//
// The default policy is RejectCycles: a cycle is reported as a
// ValidationError whose Namespace is the path at which the cyclic edge was
// reached, and traversal of the back-edge is cut. TruncateCycles silently
// cuts the back-edge without reporting an error.
//
// Cycle detection is scoped to each traversal path rather than using a
// global visited set, so an object shared by multiple field paths (a diamond
// reference) is still validated independently under every path and its
// constraints. Only re-entering an object already on the active path is
// treated as a cycle.
func WithCyclePolicy(policy CyclePolicy) Option {
	return func(v *Validate) {
		v.cyclePolicy = policy
	}
}

// WithMaxStructDepth limits how deeply a single field path may nest structs
// during one validation call. A value of zero (the default) imposes no
// limit. When the limit is exceeded a ValidationError with the synthetic
// "maxdepth" tag is reported at the path that exceeded the limit and that
// branch is not traversed further.
//
// The limit bounds work even for extremely deep but acyclic input (for
// example a million-node linked list) where cycle detection alone does not
// apply.
func WithMaxStructDepth(depth int) Option {
	return func(v *Validate) {
		v.maxStructDepth = depth
	}
}

// WithMaxTraversalNodes limits the total number of structs visited during a
// single validation call. A value of zero (the default) imposes no limit.
// When the limit is exceeded a ValidationError with the synthetic "maxnodes"
// tag is reported at the path where the budget ran out and traversal stops.
//
// Unlike WithMaxStructDepth this bounds total work for wide graphs, such as
// a deep DAG with many shared nodes, not just the depth of one path.
func WithMaxTraversalNodes(nodes int) Option {
	return func(v *Validate) {
		v.maxTraversalNodes = nodes
	}
}
