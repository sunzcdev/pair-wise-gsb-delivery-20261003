package xpath

import (
	"errors"
	"fmt"
)

type flag int

var flagsEnum = struct {
	None      flag
	SmartDesc flag
	PosFilter flag
	Filter    flag
	Condition flag
}{
	None:      0,
	SmartDesc: 1,
	PosFilter: 2,
	Filter:    4,
	Condition: 8,
}

type builderProp int

var builderProps = struct {
	None        builderProp
	PosFilter   builderProp
	HasPosition builderProp
	HasLast     builderProp
	NonFlat     builderProp
}{
	None:        0,
	PosFilter:   1,
	HasPosition: 2,
	HasLast:     4,
	NonFlat:     8,
}

// builder provides building an XPath expressions.
type builder struct {
	parseDepth int
	firstInput query
	namespaces map[string]string
	registry   *FunctionRegistry
}

// axisPredicate creates a predicate to predicating for this axis node.
func axisPredicate(root *axisNode) func(NodeNavigator) bool {
	nametest := root.LocalName != "" || root.Prefix != ""
	predicate := func(n NodeNavigator) bool {
		if root.typeTest == n.NodeType() || root.typeTest == allNode {
			if nametest {
				type namespaceURL interface {
					NamespaceURL() string
				}
				if ns, ok := n.(namespaceURL); ok && root.hasNamespaceURI {
					return root.LocalName == n.LocalName() && root.namespaceURI == ns.NamespaceURL()
				}
				if root.LocalName == n.LocalName() && root.Prefix == n.Prefix() {
					return true
				}
			} else {
				return true
			}
		}
		return false
	}

	return predicate
}

// processAxis processes a query for the XPath axis node.
func (b *builder) processAxis(root *axisNode, flags flag, props *builderProp) (query, error) {
	var (
		err      error
		qyInput  query
		qyOutput query
	)
	b.firstInput = nil
	predicate := axisPredicate(root)

	if root.Input == nil {
		qyInput = &contextQuery{}
		*props = builderProps.None
	} else {
		inputFlags := flagsEnum.None
		if (flags & flagsEnum.Filter) == 0 {
			if root.AxisType == "child" && (root.Input.Type() == nodeAxis) {
				if input := root.Input.(*axisNode); input.AxisType == "descendant-or-self" {
					var qyGrandInput query
					if input.Input != nil {
						qyGrandInput, err = b.processNode(input.Input, flagsEnum.SmartDesc, props)
						if err != nil {
							return nil, err
						}
					} else {
						qyGrandInput = &contextQuery{}
					}
					qyOutput = &descendantQuery{name: root.LocalName, Input: qyGrandInput, Predicate: predicate, Self: false}
					*props |= builderProps.NonFlat
					return qyOutput, nil
				}
			}
			if root.AxisType == "descendant" || root.AxisType == "descendant-or-self" {
				inputFlags |= flagsEnum.SmartDesc
			}
		}

		qyInput, err = b.processNode(root.Input, inputFlags, props)
		if err != nil {
			return nil, err
		}
	}

	switch root.AxisType {
	case "ancestor":
		qyOutput = &ancestorQuery{name: root.LocalName, Input: qyInput, Predicate: predicate}
		*props |= builderProps.NonFlat
	case "ancestor-or-self":
		qyOutput = &ancestorQuery{name: root.LocalName, Input: qyInput, Predicate: predicate, Self: true}
		*props |= builderProps.NonFlat
	case "attribute":
		qyOutput = &attributeQuery{name: root.LocalName, Input: qyInput, Predicate: predicate}
	case "child":
		if (*props & builderProps.NonFlat) == 0 {
			qyOutput = &childQuery{name: root.LocalName, Input: qyInput, Predicate: predicate}
		} else {
			qyOutput = &cachedChildQuery{name: root.LocalName, Input: qyInput, Predicate: predicate}
		}
	case "descendant":
		if (flags & flagsEnum.SmartDesc) != flagsEnum.None {
			qyOutput = &descendantOverDescendantQuery{name: root.LocalName, Input: qyInput, MatchSelf: false, Predicate: predicate}
		} else {
			qyOutput = &descendantQuery{name: root.LocalName, Input: qyInput, Predicate: predicate}
		}
		*props |= builderProps.NonFlat
	case "descendant-or-self":
		if (flags & flagsEnum.SmartDesc) != flagsEnum.None {
			qyOutput = &descendantOverDescendantQuery{name: root.LocalName, Input: qyInput, MatchSelf: true, Predicate: predicate}
		} else {
			qyOutput = &descendantQuery{name: root.LocalName, Input: qyInput, Predicate: predicate, Self: true}
		}
		*props |= builderProps.NonFlat
	case "following":
		qyOutput = &followingQuery{Input: qyInput, Predicate: predicate}
		*props |= builderProps.NonFlat
	case "following-sibling":
		qyOutput = &followingQuery{Input: qyInput, Predicate: predicate, Sibling: true}
	case "parent":
		qyOutput = &parentQuery{Input: qyInput, Predicate: predicate}
	case "preceding":
		qyOutput = &precedingQuery{Input: qyInput, Predicate: predicate}
		*props |= builderProps.NonFlat
	case "preceding-sibling":
		qyOutput = &precedingQuery{Input: qyInput, Predicate: predicate, Sibling: true}
	case "self":
		qyOutput = &selfQuery{Input: qyInput, Predicate: predicate}
	case "namespace":
		// haha,what will you do someting??
	default:
		err = fmt.Errorf("unknown axe type: %s", root.AxisType)
		return nil, err
	}
	return qyOutput, nil
}

func canBeNumber(q query) bool {
	if q.ValueType() != xpathResultType.Any {
		return q.ValueType() == xpathResultType.Number
	}
	return true
}

// positionalCondition reports whether a condition AST node produces a numeric
// or positional result.
func (b *builder) positionalCondition(n node) bool {
	switch v := n.(type) {
	case *operandNode:
		_, ok := v.Val.(float64)
		return ok
	case *functionNode:
		if v.Prefix != "" {
			if uri, ok := b.namespaces[v.Prefix]; ok && b.registry != nil {
				if spec := b.registry.Lookup(uri, v.FuncName); spec != nil {
					rt := spec.Return.resultType()
					return rt == xpathResultType.Number || rt == xpathResultType.Any
				}
			}
			return false
		}
		switch v.FuncName {
		case "position", "last", "count", "sum", "string-length", "number",
			"floor", "ceiling", "round":
			return true
		}
		for _, arg := range v.Args {
			if b.positionalCondition(arg) {
				return true
			}
		}
		return false
	case *operatorNode:
		switch v.Op {
		case "+", "-", "*", "div", "mod":
			return true
		}
		return b.positionalCondition(v.Left) || b.positionalCondition(v.Right)
	default:
		return false
	}
}

// processFilterNode builds query for the XPath filter predicate.
func (b *builder) processFilter(root *filterNode, flags flag, props *builderProp) (query, error) {
	first := (flags & flagsEnum.Filter) == 0

	// Inspect the condition AST to decide whether to set flagsEnum.Filter.
	// Positional predicates cannot be compiled into a descendantQuery.
	inputFlags := flags
	if b.positionalCondition(root.Condition) {
		inputFlags |= flagsEnum.Filter
	}

	qyInput, err := b.processNode(root.Input, inputFlags, props)
	if err != nil {
		return nil, err
	}
	firstInput := b.firstInput

	var propsCond builderProp
	cond, err := b.processNode(root.Condition, flags, &propsCond)
	if err != nil {
		return nil, err
	}

	// Checking whether is number
	if canBeNumber(cond) || ((propsCond & (builderProps.HasPosition | builderProps.HasLast)) != 0) {
		propsCond |= builderProps.HasPosition
		flags |= flagsEnum.PosFilter
	}

	if root.Input.Type() != nodeFilter {
		*props &= ^builderProps.PosFilter
	}

	if (propsCond & builderProps.HasPosition) != 0 {
		*props |= builderProps.PosFilter
	}

	if (propsCond & builderProps.HasPosition) != builderProps.None {
		if (propsCond & builderProps.HasLast) != 0 {
			// https://github.com/antchfx/xpath/issues/76
			// https://github.com/antchfx/xpath/issues/78
			if qyFunc, ok := cond.(*functionQuery); ok {
				switch qyFunc.Input.(type) {
				case *filterQuery:
					cond = &lastFuncQuery{Input: qyFunc.Input}
				case *groupQuery:
					cond = &lastFuncQuery{Input: qyFunc.Input}
				}
			}
		}
	}

	merge := (qyInput.Properties() & queryProps.Merge) != 0
	if first && firstInput != nil {
		if merge && ((*props & builderProps.PosFilter) != 0) {
			var (
				rootQuery = &contextQuery{}
				parent    query
			)
			switch axisQuery := firstInput.(type) {
			case *ancestorQuery:
				if _, ok := axisQuery.Input.(*contextQuery); !ok {
					parent = axisQuery.Input
					axisQuery.Input = rootQuery
				}
			case *attributeQuery:
				if _, ok := axisQuery.Input.(*contextQuery); !ok {
					parent = axisQuery.Input
					axisQuery.Input = rootQuery
				}
			case *childQuery:
				if _, ok := axisQuery.Input.(*contextQuery); !ok {
					parent = axisQuery.Input
					axisQuery.Input = rootQuery
				}
			case *cachedChildQuery:
				if _, ok := axisQuery.Input.(*contextQuery); !ok {
					parent = axisQuery.Input
					axisQuery.Input = rootQuery
				}
			case *descendantQuery:
				if _, ok := axisQuery.Input.(*contextQuery); !ok {
					parent = axisQuery.Input
					axisQuery.Input = rootQuery
				}
			case *followingQuery:
				if _, ok := axisQuery.Input.(*contextQuery); !ok {
					parent = axisQuery.Input
					axisQuery.Input = rootQuery
				}
			case *precedingQuery:
				if _, ok := axisQuery.Input.(*contextQuery); !ok {
					parent = axisQuery.Input
					axisQuery.Input = rootQuery
				}
			case *parentQuery:
				if _, ok := axisQuery.Input.(*contextQuery); !ok {
					parent = axisQuery.Input
					axisQuery.Input = rootQuery
				}
			case *selfQuery:
				if _, ok := axisQuery.Input.(*contextQuery); !ok {
					parent = axisQuery.Input
					axisQuery.Input = rootQuery
				}
			case *groupQuery:
				if _, ok := axisQuery.Input.(*contextQuery); !ok {
					parent = axisQuery.Input
					axisQuery.Input = rootQuery
				}
			case *descendantOverDescendantQuery:
				if _, ok := axisQuery.Input.(*contextQuery); !ok {
					parent = axisQuery.Input
					axisQuery.Input = rootQuery
				}
			}
			b.firstInput = nil
			child := &filterQuery{Input: qyInput, Predicate: cond, NoPosition: false}
			if parent != nil {
				return &mergeQuery{Input: parent, Child: child}, nil
			}
			return child, nil
		}
		b.firstInput = nil
	}

	resultQuery := &filterQuery{
		Input:      qyInput,
		Predicate:  cond,
		NoPosition: (propsCond & builderProps.HasPosition) == 0,
	}
	return resultQuery, nil
}

// processFunctionNode processes query for the XPath function node.
func (b *builder) processFunction(root *functionNode, props *builderProp) (query, error) {
	// Reset builder props
	*props = builderProps.None

	var qyOutput query

	// Extension functions are QName calls prefix:local-name() where prefix
	// is bound to a namespace URI supplied at compile time and the registry
	// carries a function for (URI, local-name). Built-in functions live in
	// the default (empty-prefix) namespace, so a prefixed call never shadows
	// one. Resolution happens before argument compilation so that signature
	// problems are reported even when an argument itself fails to build.
	if root.Prefix != "" {
		spec, err := b.resolveExtension(root)
		if err != nil {
			return nil, err
		}
		return b.processExtensionFunction(root, spec, props)
	}

	switch root.FuncName {
	case "lower-case":
		arg, err := b.processNode(root.Args[0], flagsEnum.None, props)
		if err != nil {
			return nil, err
		}
		qyOutput = &functionQuery{Func: lowerCaseFunc(arg)}
	case "starts-with":
		arg1, err := b.processNode(root.Args[0], flagsEnum.None, props)
		if err != nil {
			return nil, err
		}
		arg2, err := b.processNode(root.Args[1], flagsEnum.None, props)
		if err != nil {
			return nil, err
		}
		qyOutput = &functionQuery{Func: startwithFunc(arg1, arg2)}
	case "ends-with":
		arg1, err := b.processNode(root.Args[0], flagsEnum.None, props)
		if err != nil {
			return nil, err
		}
		arg2, err := b.processNode(root.Args[1], flagsEnum.None, props)
		if err != nil {
			return nil, err
		}
		qyOutput = &functionQuery{Func: endwithFunc(arg1, arg2)}
	case "contains":
		arg1, err := b.processNode(root.Args[0], flagsEnum.None, props)
		if err != nil {
			return nil, err
		}
		arg2, err := b.processNode(root.Args[1], flagsEnum.None, props)
		if err != nil {
			return nil, err
		}
		qyOutput = &functionQuery{Func: containsFunc(arg1, arg2)}
	case "matches":
		//matches(string , pattern)
		if len(root.Args) != 2 {
			return nil, errors.New("xpath: matches function must have two parameters")
		}
		var (
			arg1, arg2 query
			err        error
		)
		if arg1, err = b.processNode(root.Args[0], flagsEnum.None, props); err != nil {
			return nil, err
		}
		if arg2, err = b.processNode(root.Args[1], flagsEnum.None, props); err != nil {
			return nil, err
		}
		// Issue #92, testing the regular expression before.
		if q, ok := arg2.(*constantQuery); ok {
			if _, err = getRegexp(q.Val.(string)); err != nil {
				return nil, fmt.Errorf("matches() got error. %v", err)
			}
		}
		qyOutput = &functionQuery{Func: matchesFunc(arg1, arg2)}
	case "substring":
		//substring( string , start [, length] )
		if len(root.Args) < 2 {
			return nil, errors.New("xpath: substring function must have at least two parameter")
		}
		var (
			arg1, arg2, arg3 query
			err              error
		)
		if arg1, err = b.processNode(root.Args[0], flagsEnum.None, props); err != nil {
			return nil, err
		}
		if arg2, err = b.processNode(root.Args[1], flagsEnum.None, props); err != nil {
			return nil, err
		}
		if len(root.Args) == 3 {
			if arg3, err = b.processNode(root.Args[2], flagsEnum.None, props); err != nil {
				return nil, err
			}
		}
		qyOutput = &functionQuery{Func: substringFunc(arg1, arg2, arg3)}
	case "substring-before", "substring-after":
		//substring-xxxx( haystack, needle )
		if len(root.Args) != 2 {
			return nil, errors.New("xpath: substring-before function must have two parameters")
		}
		var (
			arg1, arg2 query
			err        error
		)
		if arg1, err = b.processNode(root.Args[0], flagsEnum.None, props); err != nil {
			return nil, err
		}
		if arg2, err = b.processNode(root.Args[1], flagsEnum.None, props); err != nil {
			return nil, err
		}
		qyOutput = &functionQuery{
			Func: substringIndFunc(arg1, arg2, root.FuncName == "substring-after"),
		}
	case "string-length":
		// string-length( [string] )
		if len(root.Args) < 1 {
			return nil, errors.New("xpath: string-length function must have at least one parameter")
		}
		arg1, err := b.processNode(root.Args[0], flagsEnum.None, props)
		if err != nil {
			return nil, err
		}
		qyOutput = &functionQuery{Func: stringLengthFunc(arg1)}
	case "normalize-space":
		var arg node
		if len(root.Args) > 0 {
			arg = root.Args[0]
		} else {
			arg = newAxisNode("self", allNode, "", "", "", nil)
		}
		arg1, err := b.processNode(arg, flagsEnum.None, props)
		if err != nil {
			return nil, err
		}
		qyOutput = &functionQuery{Func: normalizespaceFunc(arg1)}
	case "replace":
		//replace( string , string, string )
		if len(root.Args) != 3 {
			return nil, errors.New("xpath: replace function must have three parameters")
		}
		var (
			arg1, arg2, arg3 query
			err              error
		)
		if arg1, err = b.processNode(root.Args[0], flagsEnum.None, props); err != nil {
			return nil, err
		}
		if arg2, err = b.processNode(root.Args[1], flagsEnum.None, props); err != nil {
			return nil, err
		}
		if arg3, err = b.processNode(root.Args[2], flagsEnum.None, props); err != nil {
			return nil, err
		}
		qyOutput = &functionQuery{Func: replaceFunc(arg1, arg2, arg3)}
	case "translate":
		//translate( string , string, string )
		if len(root.Args) != 3 {
			return nil, errors.New("xpath: translate function must have three parameters")
		}
		var (
			arg1, arg2, arg3 query
			err              error
		)
		if arg1, err = b.processNode(root.Args[0], flagsEnum.None, props); err != nil {
			return nil, err
		}
		if arg2, err = b.processNode(root.Args[1], flagsEnum.None, props); err != nil {
			return nil, err
		}
		if arg3, err = b.processNode(root.Args[2], flagsEnum.None, props); err != nil {
			return nil, err
		}
		qyOutput = &functionQuery{Func: translateFunc(arg1, arg2, arg3)}
	case "not":
		if len(root.Args) == 0 {
			return nil, errors.New("xpath: not function must have at least one parameter")
		}
		argQuery, err := b.processNode(root.Args[0], flagsEnum.None, props)
		if err != nil {
			return nil, err
		}
		qyOutput = &functionQuery{Func: notFunc(argQuery)}
	case "name", "local-name", "namespace-uri":
		if len(root.Args) > 1 {
			return nil, fmt.Errorf("xpath: %s function must have at most one parameter", root.FuncName)
		}
		var (
			arg query
			err error
		)
		if len(root.Args) == 1 {
			arg, err = b.processNode(root.Args[0], flagsEnum.None, props)
			if err != nil {
				return nil, err
			}
		}
		switch root.FuncName {
		case "name":
			qyOutput = &functionQuery{Func: nameFunc(arg)}
		case "local-name":
			qyOutput = &functionQuery{Func: localNameFunc(arg)}
		case "namespace-uri":
			qyOutput = &functionQuery{Func: namespaceFunc(arg)}
		}
	case "true", "false":
		val := root.FuncName == "true"
		qyOutput = &functionQuery{
			Func: func(_ query, _ iterator) interface{} {
				return val
			},
		}
	case "last":
		qyOutput = &functionQuery{Input: b.firstInput, Func: lastFunc()}
		*props |= builderProps.HasLast
	case "position":
		qyOutput = &functionQuery{Input: b.firstInput, Func: positionFunc()}
		*props |= builderProps.HasPosition
	case "boolean", "number", "string":
		var inp query
		if len(root.Args) > 1 {
			return nil, fmt.Errorf("xpath: %s function must have at most one parameter", root.FuncName)
		}
		if len(root.Args) == 1 {
			argQuery, err := b.processNode(root.Args[0], flagsEnum.None, props)
			if err != nil {
				return nil, err
			}
			inp = argQuery
		}
		switch root.FuncName {
		case "boolean":
			qyOutput = &functionQuery{Func: booleanFunc(inp)}
		case "string":
			qyOutput = &functionQuery{Func: stringFunc(inp)}
		case "number":
			qyOutput = &functionQuery{Func: numberFunc(inp)}
		}
	case "count":
		if len(root.Args) == 0 {
			return nil, fmt.Errorf("xpath: count(node-sets) function must with have parameters node-sets")
		}
		argQuery, err := b.processNode(root.Args[0], flagsEnum.None, props)
		if err != nil {
			return nil, err
		}
		qyOutput = &functionQuery{Func: countFunc(argQuery)}
	case "sum":
		if len(root.Args) == 0 {
			return nil, fmt.Errorf("xpath: sum(node-sets) function must with have parameters node-sets")
		}
		argQuery, err := b.processNode(root.Args[0], flagsEnum.None, props)
		if err != nil {
			return nil, err
		}
		qyOutput = &functionQuery{Func: sumFunc(argQuery)}
	case "ceiling", "floor", "round":
		if len(root.Args) == 0 {
			return nil, fmt.Errorf("xpath: ceiling(node-sets) function must with have parameters node-sets")
		}
		argQuery, err := b.processNode(root.Args[0], flagsEnum.None, props)
		if err != nil {
			return nil, err
		}
		switch root.FuncName {
		case "ceiling":
			qyOutput = &functionQuery{Func: ceilingFunc(argQuery)}
		case "floor":
			qyOutput = &functionQuery{Func: floorFunc(argQuery)}
		case "round":
			qyOutput = &functionQuery{Func: roundFunc(argQuery)}
		}
	case "concat":
		if len(root.Args) < 2 {
			return nil, fmt.Errorf("xpath: concat() must have at least two arguments")
		}
		var args []query
		for _, v := range root.Args {
			q, err := b.processNode(v, flagsEnum.None, props)
			if err != nil {
				return nil, err
			}
			args = append(args, q)
		}
		qyOutput = &functionQuery{Func: concatFunc(args...)}
	case "reverse":
		if len(root.Args) == 0 {
			return nil, fmt.Errorf("xpath: reverse(node-sets) function must with have parameters node-sets")
		}
		argQuery, err := b.processNode(root.Args[0], flagsEnum.None, props)
		if err != nil {
			return nil, err
		}
		qyOutput = &transformFunctionQuery{Input: argQuery, Func: reverseFunc}
	case "string-join":
		if len(root.Args) != 2 {
			return nil, fmt.Errorf("xpath: string-join(node-sets, separator) function requires node-set and argument")
		}
		input, err := b.processNode(root.Args[0], flagsEnum.None, props)
		if err != nil {
			return nil, err
		}
		arg1, err := b.processNode(root.Args[1], flagsEnum.None, props)
		if err != nil {
			return nil, err
		}
		qyOutput = &functionQuery{Func: stringJoinFunc(input, arg1)}
	default:
		return nil, fmt.Errorf("not yet support this function %s()", root.FuncName)
	}
	return qyOutput, nil
}

// resolveExtension maps a prefixed function QName to the spec registered for
// its namespace URI. Prefix binding and registry lookup are compile-time
// errors: an unbound prefix or unregistered function must fail compilation
// rather than evaluation.
func (b *builder) resolveExtension(root *functionNode) (*FunctionSpec, error) {
	if b.namespaces == nil {
		return nil, fmt.Errorf("xpath: extension function %s:%s() used without namespace declarations",
			root.Prefix, root.FuncName)
	}
	uri, ok := b.namespaces[root.Prefix]
	if !ok {
		return nil, fmt.Errorf("xpath: prefix %q not defined for extension function %s:%s()",
			root.Prefix, root.Prefix, root.FuncName)
	}
	if b.registry == nil {
		return nil, fmt.Errorf("xpath: extension function {%s}%s() is not registered",
			uri, root.FuncName)
	}
	spec := b.registry.Lookup(uri, root.FuncName)
	if spec == nil {
		return nil, fmt.Errorf("xpath: extension function {%s}%s() is not registered",
			uri, root.FuncName)
	}
	return spec, nil
}

// processExtensionFunction compiles a call to a registered extension
// function, performing the signature checks decidable at compile time:
// argument count (arity is fixed) and rejection of statically scalar
// arguments where a node-set is required. Dynamic XPath conversions between
// boolean/number/string are legal in both directions and are applied at
// evaluation time.
func (b *builder) processExtensionFunction(root *functionNode, spec *FunctionSpec, props *builderProp) (query, error) {
	if len(root.Args) != len(spec.Params) {
		return nil, fmt.Errorf("xpath: extension function {%s}%s() requires %d argument(s), got %d",
			spec.NamespaceURI, spec.LocalName, len(spec.Params), len(root.Args))
	}

	// A function used directly as a location-path step must return a node
	// set: path steps contribute nodes, never scalars. This is a static
	// signature problem and is reported at compile time.
	if root.StepInput != nil && spec.Return != ExtensionNodeSet {
		return nil, fmt.Errorf(
			"xpath: extension function {%s}%s() used as a path step must return node-set, returns %s",
			spec.NamespaceURI, spec.LocalName, spec.Return)
	}

	var stepInput query
	isStep := root.StepInput != nil
	if root.StepInput != nil {
		if _, implicit := root.StepInput.(implicitStepInput); implicit {
			// First function of a function-path: input is the implicit
			// context node. A nil input query already means "context".
			stepInput = &contextQuery{}
		} else {
			var err error
			stepProps := builderProps.None
			stepInput, err = b.processNode(root.StepInput, flagsEnum.None, &stepProps)
			if err != nil {
				return nil, err
			}
		}
	}

	// Capture the enclosing node-set axis before argument compilation, just
	// like position()/last(): building an argument resets firstInput.
	input := b.firstInput

	args := make([]query, len(root.Args))
	for i, argNode := range root.Args {
		arg, err := b.processNode(argNode, flagsEnum.None, props)
		if err != nil {
			return nil, err
		}
		paramType := spec.Params[i]
		if paramType == ExtensionNodeSet {
			if !nodeSetAssignable(arg.ValueType()) {
				return nil, fmt.Errorf(
					"xpath: extension function {%s}%s() parameter %d expects node-set, expression has static type %s",
					spec.NamespaceURI, spec.LocalName, i+1, resultTypeName(arg.ValueType()))
			}
		}
		args[i] = arg
	}

	fq := &extensionFuncQuery{
		spec:  spec,
		args:  args,
		input: input,
	}
	if isStep {
		return &functionStepQuery{Input: stepInput, Function: fq}, nil
	}
	return fq, nil
}

// nodeSetAssignable reports whether a static XPath type can be supplied where
// a node-set parameter is declared. XPath 1.0 has no scalar-to-node-set
// conversion, so boolean/number/string arguments are rejected at compile
// time; node-set and any-typed expressions are accepted.
func nodeSetAssignable(t resultType) bool {
	return t == xpathResultType.NodeSet || t == xpathResultType.Any
}

func resultTypeName(t resultType) string {
	switch t {
	case xpathResultType.Boolean:
		return "boolean"
	case xpathResultType.Number:
		return "number"
	case xpathResultType.String:
		return "string"
	case xpathResultType.NodeSet:
		return "node-set"
	default:
		return "any"
	}
}

func (b *builder) processOperator(root *operatorNode, props *builderProp) (query, error) {
	var (
		leftProp  builderProp
		rightProp builderProp
	)

	left, err := b.processNode(root.Left, flagsEnum.None, &leftProp)
	if err != nil {
		return nil, err
	}
	right, err := b.processNode(root.Right, flagsEnum.None, &rightProp)
	if err != nil {
		return nil, err
	}
	*props = leftProp | rightProp

	var qyOutput query
	switch root.Op {
	case "+", "-", "*", "div", "mod": // Numeric operator
		var exprFunc func(iterator, interface{}, interface{}) interface{}
		switch root.Op {
		case "+":
			exprFunc = plusFunc
		case "-":
			exprFunc = minusFunc
		case "*":
			exprFunc = mulFunc
		case "div":
			exprFunc = divFunc
		case "mod":
			exprFunc = modFunc
		}
		qyOutput = &numericQuery{Left: left, Right: right, Do: exprFunc}
	case "=", ">", ">=", "<", "<=", "!=":
		var exprFunc func(iterator, interface{}, interface{}) interface{}
		switch root.Op {
		case "=":
			exprFunc = eqFunc
		case ">":
			exprFunc = gtFunc
		case ">=":
			exprFunc = geFunc
		case "<":
			exprFunc = ltFunc
		case "<=":
			exprFunc = leFunc
		case "!=":
			exprFunc = neFunc
		}
		qyOutput = &logicalQuery{Left: left, Right: right, Do: exprFunc}
	case "or", "and":
		isOr := false
		if root.Op == "or" {
			isOr = true
		}
		qyOutput = &booleanQuery{Left: left, Right: right, IsOr: isOr}
	case "|":
		*props |= builderProps.NonFlat
		qyOutput = &unionQuery{Left: left, Right: right}
	}
	return qyOutput, nil
}

func (b *builder) processNode(root node, flags flag, props *builderProp) (q query, err error) {
	if b.parseDepth = b.parseDepth + 1; b.parseDepth > 1024 {
		err = errors.New("the xpath expressions is too complex")
		return
	}
	*props = builderProps.None
	switch root.Type() {
	case nodeConstantOperand:
		n := root.(*operandNode)
		q = &constantQuery{Val: n.Val}
	case nodeRoot:
		q = &absoluteQuery{}
	case nodeAxis:
		q, err = b.processAxis(root.(*axisNode), flags, props)
		b.firstInput = q
	case nodeFilter:
		q, err = b.processFilter(root.(*filterNode), flags, props)
		b.firstInput = q
	case nodeFunction:
		q, err = b.processFunction(root.(*functionNode), props)
	case nodeOperator:
		q, err = b.processOperator(root.(*operatorNode), props)
	case nodeGroup:
		q, err = b.processNode(root.(*groupNode).Input, flagsEnum.None, props)
		if err != nil {
			return
		}
		q = &groupQuery{Input: q}
		b.firstInput = q
	case nodeVariable:
		// Variables have no binding in a compiled expression, so they cannot
		// resolve to a query. A bare "$x" already surfaces as an undeclared
		// variable error because the nil result reaches the caller, but a
		// variable nested in a larger expression (e.g. "$x/@attr") would be
		// swallowed here and left as a nil sub-query that panics at select
		// time. Report it as undeclared instead.
		n := root.(*variableNode)
		name := n.Name
		if n.Prefix != "" {
			name = n.Prefix + ":" + name
		}
		err = fmt.Errorf("undeclared variable in XPath expression: $%s", name)
	}
	b.parseDepth--
	return
}

// build builds a specified XPath expressions expr.
func build(expr string, namespaces map[string]string, registry *FunctionRegistry) (q query, err error) {
	defer func() {
		if e := recover(); e != nil {
			switch x := e.(type) {
			case string:
				err = errors.New(x)
			case error:
				err = x
			default:
				err = errors.New("unknown panic")
			}
		}
	}()
	root := parse(expr, namespaces)
	b := &builder{namespaces: namespaces, registry: registry}
	props := builderProps.None
	return b.processNode(root, flagsEnum.None, &props)
}
