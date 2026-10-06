package xpath

// evalContext is the iterator passed through the query tree for an
// Evaluate/EvaluateWith call. It tracks the current context node (which the
// positional and path queries mutate in place, mirroring the legacy
// iteratorFunc behaviour) and carries the variable bindings that belong to a
// single evaluation.
//
// Each Evaluate call builds a fresh evalContext, so concurrent evaluations of
// the same compiled expression never share node cursors or bindings.
type evalContext struct {
	current  NodeNavigator
	bindings map[VariableName]interface{}
	cursors  map[int]*int
}

func newEvalContext(root NodeNavigator, bindings map[VariableName]interface{}) *evalContext {
	return &evalContext{current: root, bindings: bindings, cursors: make(map[int]*int)}
}

// Current returns the current context node. Path queries move the navigator
// in place via MoveTo, exactly as they do against a plain NodeNavigator.
func (c *evalContext) Current() NodeNavigator {
	return c.current
}

// variableValue resolves a declared variable from this evaluation's bindings.
func (c *evalContext) variableValue(name VariableName) (interface{}, bool) {
	if c.bindings == nil {
		return nil, false
	}
	v, ok := c.bindings[name]
	return v, ok
}

// variableCursor returns the shared node-set iteration cursor for a variable
// cluster within this evaluation.
func (c *evalContext) variableCursor(cluster int) *int {
	cur, ok := c.cursors[cluster]
	if !ok {
		start := -1
		cur = &start
		c.cursors[cluster] = cur
	}
	return cur
}
