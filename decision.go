package casbin

import (
	"errors"
	"fmt"
	"reflect"
	"runtime/debug"
	"sort"
	"strings"

	"github.com/casbin/casbin/v3/constant"
	"github.com/casbin/casbin/v3/effector"
	"github.com/casbin/casbin/v3/util"
	"github.com/casbin/govaluate"
)

// Decision is the final authorization decision returned by EnforceDecision.
type Decision int

const (
	// DecisionAllow means the request is allowed.
	DecisionAllow Decision = iota
	// DecisionDeny means the request is denied.
	DecisionDeny
	// DecisionPending means more request attributes are required.
	DecisionPending
)

// DecisionResult describes a three-valued enforcement result.
type DecisionResult struct {
	Decision Decision
	// MissingAttrs contains only attributes that can still affect the current decision.
	MissingAttrs []string
	// MatchedRule contains the decisive policy rule for an allow or deny result.
	MatchedRule []string
}

// EnforceDecision decides a request and returns pending when required attributes are missing.
func (e *Enforcer) EnforceDecision(rvals ...interface{}) (DecisionResult, error) {
	return e.enforceDecision("", rvals...)
}

// EnforceDecisionWithMatcher decides a request with a custom matcher and returns pending when required attributes are missing.
func (e *Enforcer) EnforceDecisionWithMatcher(matcher string, rvals ...interface{}) (DecisionResult, error) {
	return e.enforceDecision(matcher, rvals...)
}

func (e *Enforcer) enforceDecision(matcher string, rvals ...interface{}) (result DecisionResult, err error) {
	logEntry := e.onLogBeforeEventInEnforce(rvals)
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v\n%s", r, debug.Stack())
			if e.logger != nil && logEntry != nil {
				logEntry.Error = err
			}
		}
		e.onLogAfterEventInEnforce(logEntry, result.Decision == DecisionAllow)
	}()

	if !e.enabled {
		return DecisionResult{Decision: DecisionAllow}, nil
	}

	runtimeContext, err := e.prepareEnforce(matcher, rvals...)
	if err != nil {
		if e.logger != nil && logEntry != nil {
			logEntry.Error = err
		}
		return DecisionResult{Decision: DecisionDeny}, err
	}
	result, err = e.enforcePreparedDecision(runtimeContext)
	if err != nil && e.logger != nil && logEntry != nil {
		logEntry.Error = err
	}
	return result, err
}

type triValue struct {
	matched      bool
	pending      bool
	missingAttrs map[string]struct{}
}

type triNode interface {
	evaluate(evaluator *triEvaluator) (triValue, error)
}

type triLeaf struct {
	source string
}

type triUnary struct {
	child triNode
}

type triLogical struct {
	operator string
	left     triNode
	right    triNode
}

type triEvaluator struct {
	parameters *enforceParameters
	functions  map[string]govaluate.ExpressionFunction
}

func (e *Enforcer) enforcePreparedDecision(runtimeContext *enforceRuntimeContext) (DecisionResult, error) {
	pType := runtimeContext.pType
	parameters := runtimeContext.parameters
	policyRules := e.model["p"][pType].Policy
	hasPolicyRules := len(policyRules) != 0 && strings.Contains(runtimeContext.expString, pType+"_")

	if !hasPolicyRules {
		parameters.pVals = make([]string, len(parameters.pTokens))
		evaluator := newTriEvaluator(runtimeContext, &parameters)
		value, err := evaluator.evaluate(runtimeContext.expString)
		if err != nil {
			return DecisionResult{Decision: DecisionDeny}, err
		}
		status := triNoMatch
		if value.matched {
			status = triAllow
		} else if value.pending {
			status = triPending
		}
		if e.model["e"][runtimeContext.eType].Value == constant.DenyOverrideEffect {
			if value.pending {
				status = triPending
			} else {
				status = triAllow
			}
		}
		return e.finalDecisionResult(status, value.missingAttrs, pType, -1), nil
	}

	ruleDecisions := make([]ruleDecision, len(policyRules))
	missingAttrs := make(map[string]struct{})
	var explainIndex = -1

	for policyIndex, pvals := range policyRules {
		if len(e.model["p"][pType].Tokens) != len(pvals) {
			return DecisionResult{Decision: DecisionDeny}, fmt.Errorf(
				"invalid policy size: expected %d, got %d, pvals: %v",
				len(e.model["p"][pType].Tokens),
				len(pvals),
				pvals)
		}

		parameters.pVals = pvals
		evaluator := newTriEvaluator(runtimeContext, &parameters)
		value, err := evaluator.evaluate(runtimeContext.expString)
		if err != nil {
			return DecisionResult{Decision: DecisionDeny}, err
		}

		ruleEffect := effector.Allow
		if index, ok := parameters.pTokens[pType+"_eft"]; ok {
			switch pvals[index] {
			case "allow":
				ruleEffect = effector.Allow
			case "deny":
				ruleEffect = effector.Deny
			default:
				ruleEffect = effector.Indeterminate
			}
		}

		decision := ruleDecision{
			status:       triNoMatch,
			effect:       ruleEffect,
			index:        policyIndex,
			missingAttrs: value.missingAttrs,
		}
		if value.matched {
			if ruleEffect == effector.Indeterminate {
				decision.status = triNoMatch
			} else if ruleEffect == effector.Deny {
				decision.status = triDeny
			} else {
				decision.status = triAllow
			}
		} else if value.pending {
			decision.status = triPending
		}
		ruleDecisions[policyIndex] = decision

		for attr := range value.missingAttrs {
			missingAttrs[attr] = struct{}{}
		}

		if finalStatus, finalIndex, done := mergeDecisionStep(
			e.model["e"][runtimeContext.eType].Value,
			ruleDecisions,
			policyIndex,
			len(policyRules),
		); done {
			finalAttrs := missingAttrs
			if finalStatus == triPending {
				finalAttrs = pendingDecisionAttrs(ruleDecisions, finalIndex, e.model["e"][runtimeContext.eType].Value)
			}
			if finalStatus == triAllow || finalStatus == triDeny {
				explainIndex = finalIndex
			}
			return e.finalDecisionResult(finalStatus, finalAttrs, pType, explainIndex), nil
		}
	}

	return DecisionResult{Decision: DecisionDeny}, errors.New("unsupported effect")
}

func (e *Enforcer) finalDecisionResult(status triRuleStatus, attrs map[string]struct{}, pType string, explainIndex int) DecisionResult {
	result := DecisionResult{}
	if status == triAllow {
		result.Decision = DecisionAllow
		if explainIndex >= 0 && explainIndex < len(e.model["p"][pType].Policy) {
			result.MatchedRule = e.model["p"][pType].Policy[explainIndex]
		}
		return result
	}

	if status == triPending {
		result.Decision = DecisionPending
		result.MissingAttrs = sortedAttrs(attrs)
	} else {
		result.Decision = DecisionDeny
	}
	return result
}

type triRuleStatus int

const (
	triNoMatch triRuleStatus = iota
	triAllow
	triDeny
	triPending
)

type ruleDecision struct {
	status       triRuleStatus
	effect       effector.Effect
	index        int
	missingAttrs map[string]struct{}
}

// mergeDecisionStep implements policy-level three-valued short-circuiting.
//
// Allow-override returns allow on the first matched allow, and only remains pending for a possible allow.
// Deny-override returns deny on the first matched deny, and only remains pending for a possible deny.
// Allow-and-deny short-circuits on deny, requires an allow at the end, and remains pending for either relevant branch.
// Priority uses the highest-priority matching rule; when it is pending, lower-priority attributes are omitted.
func mergeDecisionStep(expr string, decisions []ruleDecision, policyIndex, policyLength int) (triRuleStatus, int, bool) {
	switch expr {
	case constant.AllowOverrideEffect:
		for i := 0; i <= policyIndex; i++ {
			if decisions[i].status == triAllow && decisions[i].effect == effector.Allow {
				return triAllow, i, true
			}
		}
		if policyIndex == policyLength-1 {
			if hasPending(decisions, constant.AllowOverrideEffect) {
				return triPending, -1, true
			}
			return triNoMatch, -1, true
		}
		return triNoMatch, -1, false
	case constant.DenyOverrideEffect:
		for i := 0; i <= policyIndex; i++ {
			if decisions[i].status == triDeny {
				return triDeny, i, true
			}
		}
		if policyIndex == policyLength-1 {
			if hasPending(decisions, constant.DenyOverrideEffect) {
				return triPending, -1, true
			}
			return triAllow, -1, true
		}
		return triNoMatch, -1, false
	case constant.AllowAndDenyEffect:
		for i := 0; i <= policyIndex; i++ {
			if decisions[i].status == triDeny {
				return triDeny, i, true
			}
		}
		if policyIndex < policyLength-1 {
			return triNoMatch, -1, false
		}
		for i, decision := range decisions {
			if decision.status == triAllow && decision.effect == effector.Allow {
				return triAllow, i, true
			}
		}
		if hasPending(decisions, constant.AllowAndDenyEffect) {
			return triPending, -1, true
		}
		return triNoMatch, -1, true
	case constant.PriorityEffect, constant.SubjectPriorityEffect:
		for i := len(decisions) - 1; i >= 0; i-- {
			switch decisions[i].status {
			case triAllow:
				return triAllow, i, true
			case triDeny:
				return triDeny, i, true
			}
		}
		if policyIndex == policyLength-1 {
			for i := len(decisions) - 1; i >= 0; i-- {
				if decisions[i].status == triPending {
					return triPending, i, true
				}
			}
			return triNoMatch, -1, true
		}
		return triNoMatch, -1, false
	default:
		return triNoMatch, -1, false
	}
}

func hasPending(decisions []ruleDecision, expr string) bool {
	for _, decision := range decisions {
		if decision.status == triPending && pendingRuleRelevant(decision, expr) {
			return true
		}
	}
	return false
}

func pendingDecisionAttrs(decisions []ruleDecision, startIndex int, expr string) map[string]struct{} {
	attrs := make(map[string]struct{})
	if startIndex < 0 {
		for _, decision := range decisions {
			if pendingRuleRelevant(decision, expr) {
				for attr := range decision.missingAttrs {
					attrs[attr] = struct{}{}
				}
			}
		}
		return attrs
	}

	if expr == constant.PriorityEffect || expr == constant.SubjectPriorityEffect {
		for attr := range decisions[startIndex].missingAttrs {
			attrs[attr] = struct{}{}
		}
		return attrs
	}

	for _, decision := range decisions[startIndex:] {
		if pendingRuleRelevant(decision, expr) {
			for attr := range decision.missingAttrs {
				attrs[attr] = struct{}{}
			}
		}
	}
	return attrs
}

func pendingRuleRelevant(decision ruleDecision, expr string) bool {
	if decision.status != triPending {
		return false
	}
	if expr == constant.AllowOverrideEffect {
		return decision.effect == effector.Allow
	}
	if expr == constant.DenyOverrideEffect {
		return decision.effect == effector.Deny
	}
	return true
}

func newTriEvaluator(runtimeContext *enforceRuntimeContext, parameters *enforceParameters) *triEvaluator {
	functions := make(map[string]govaluate.ExpressionFunction, len(runtimeContext.functions))
	for name, function := range runtimeContext.functions {
		functions[name] = function
	}
	functions["eval"] = generateDecisionEvalFunction(functions, parameters)
	return &triEvaluator{
		parameters: parameters,
		functions:  functions,
	}
}

func (evaluator *triEvaluator) evaluate(source string) (triValue, error) {
	node, err := parseTriExpression(source)
	if err != nil {
		return triValue{}, err
	}
	return node.evaluate(evaluator)
}

func (node *triUnary) evaluate(evaluator *triEvaluator) (triValue, error) {
	value, err := node.child.evaluate(evaluator)
	if err != nil || value.pending {
		return value, err
	}
	value.matched = !value.matched
	return value, nil
}

func (node *triLogical) evaluate(evaluator *triEvaluator) (triValue, error) {
	// This mirrors govaluate's AND/OR short-circuit order, while retaining the
	// attributes required by the branch that can still determine the result.
	left, err := node.left.evaluate(evaluator)
	if err != nil {
		return triValue{}, err
	}
	if node.operator == "&&" && !left.matched && !left.pending {
		return triValue{matched: false}, nil
	}
	if node.operator == "||" && left.matched {
		return triValue{matched: true}, nil
	}

	right, err := node.right.evaluate(evaluator)
	if err != nil {
		return triValue{}, err
	}
	if node.operator == "&&" {
		if !right.matched && !right.pending {
			return triValue{matched: false}, nil
		}
		if left.matched && right.matched {
			return triValue{matched: true}, nil
		}
		return pendingTriValue(left, right), nil
	}

	if right.matched {
		return triValue{matched: true}, nil
	}
	if !left.pending && !right.pending {
		return triValue{matched: false}, nil
	}
	return pendingTriValue(left, right), nil
}

func (leaf *triLeaf) evaluate(evaluator *triEvaluator) (triValue, error) {
	source := strings.TrimSpace(leaf.source)
	if source == "true" {
		return triValue{matched: true}, nil
	}
	if source == "false" {
		return triValue{matched: false}, nil
	}
	if rule, ok := evaluator.evalRuleArgument(source); ok {
		return evaluator.evaluate(util.EscapeAssertion(rule))
	}

	missingAttrs, err := evaluator.missingAttributes(source)
	if err != nil {
		return triValue{}, err
	}
	if len(missingAttrs) != 0 {
		return triValue{pending: true, missingAttrs: missingAttrs}, nil
	}

	expression, err := govaluate.NewEvaluableExpressionWithFunctions(source, evaluator.functions)
	if err != nil {
		return triValue{}, err
	}
	result, err := expression.Eval(evaluator.parameters)
	if err != nil {
		return triValue{}, err
	}

	matched, err := truthyMatcherResult(result)
	if err != nil {
		return triValue{}, err
	}
	return triValue{matched: matched}, nil
}

func (evaluator *triEvaluator) evalRuleArgument(source string) (string, bool) {
	if !strings.HasPrefix(source, "eval(") || !strings.HasSuffix(source, ")") {
		return "", false
	}
	argument := strings.TrimSpace(source[5 : len(source)-1])
	if value, err := evaluator.parameters.Get(argument); err == nil {
		rule, ok := value.(string)
		return rule, ok
	}
	if len(argument) < 2 {
		return "", false
	}
	quote := rune(argument[0])
	if (quote != '\'' && quote != '"') || rune(argument[len(argument)-1]) != quote {
		return "", false
	}
	return argument[1 : len(argument)-1], true
}

func pendingTriValue(values ...triValue) triValue {
	result := triValue{pending: true, missingAttrs: make(map[string]struct{})}
	for _, value := range values {
		for attr := range value.missingAttrs {
			result.missingAttrs[attr] = struct{}{}
		}
	}
	return result
}

func truthyMatcherResult(result interface{}) (bool, error) {
	switch result := result.(type) {
	case bool:
		return result, nil
	case float64:
		return result != 0, nil
	default:
		return false, errors.New("matcher result should be bool, int or float")
	}
}

func sortedAttrs(attrs map[string]struct{}) []string {
	result := make([]string, 0, len(attrs))
	for attr := range attrs {
		result = append(result, attr)
	}
	sort.Strings(result)
	return result
}

func parseTriExpression(source string) (triNode, error) {
	source = strings.TrimSpace(source)
	if balancedEnclosed(source) {
		return parseTriExpression(source[1 : len(source)-1])
	}
	if index := topLevelSplit(source, "||"); index >= 0 {
		left, err := parseTriExpression(source[:index])
		if err != nil {
			return nil, err
		}
		right, err := parseTriExpression(source[index+2:])
		if err != nil {
			return nil, err
		}
		return &triLogical{operator: "||", left: left, right: right}, nil
	}
	if index := topLevelSplit(source, "&&"); index >= 0 {
		left, err := parseTriExpression(source[:index])
		if err != nil {
			return nil, err
		}
		right, err := parseTriExpression(source[index+2:])
		if err != nil {
			return nil, err
		}
		return &triLogical{operator: "&&", left: left, right: right}, nil
	}
	if strings.HasPrefix(source, "!") {
		child, err := parseTriExpression(source[1:])
		if err != nil {
			return nil, err
		}
		return &triUnary{child: child}, nil
	}
	return &triLeaf{source: source}, nil
}

func balancedEnclosed(source string) bool {
	if len(source) < 2 || source[0] != '(' || source[len(source)-1] != ')' {
		return false
	}
	depth := 0
	var quote rune
	for i := 0; i < len(source)-1; i++ {
		ch := rune(source[i])
		if quote != 0 {
			if ch == '\\' {
				i++
			} else if ch == quote {
				quote = 0
			}
			continue
		}
		switch ch {
		case '\'', '"':
			quote = ch
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return false
			}
		}
	}
	return true
}

func topLevelSplit(source string, operator string) int {
	depth := 0
	var quote rune
	for i := 0; i < len(source); i++ {
		ch := rune(source[i])
		if quote != 0 {
			if ch == '\\' {
				i++
			} else if ch == quote {
				quote = 0
			}
			continue
		}
		switch ch {
		case '\'', '"':
			quote = ch
		case '(':
			depth++
		case ')':
			depth--
		default:
			if depth == 0 && i+len(operator) <= len(source) && source[i:i+len(operator)] == operator {
				return i
			}
		}
	}
	return -1
}

func (evaluator *triEvaluator) missingAttributes(source string) (map[string]struct{}, error) {
	tokens, err := parseDecisionTokens(source, evaluator.functions)
	if err != nil {
		return nil, err
	}

	missing := make(map[string]struct{})
	for _, token := range tokens {
		switch token.Kind {
		case govaluate.VARIABLE:
			name := token.Value.(string)
			if _, err := evaluator.parameters.Get(name); err != nil {
				missing[publicAttrName(name)] = struct{}{}
			}
		case govaluate.ACCESSOR:
			path := token.Value.([]string)
			if attr, ok := evaluator.missingAccessor(path); ok {
				missing[attr] = struct{}{}
			}
		}
	}
	return missing, nil
}

func parseDecisionTokens(source string, functions map[string]govaluate.ExpressionFunction) ([]govaluate.ExpressionToken, error) {
	parseFunctions := make(map[string]govaluate.ExpressionFunction, len(functions))
	for name, function := range functions {
		parseFunctions[name] = function
	}

	shimExpression, err := govaluate.NewEvaluableExpressionWithFunctions(source, parseFunctions)
	if err != nil {
		return nil, err
	}
	return shimExpression.Tokens(), nil
}

func (evaluator *triEvaluator) missingAccessor(path []string) (string, bool) {
	value, err := evaluator.parameters.Get(path[0])
	if err != nil {
		return publicAttrName(strings.Join(path, ".")), true
	}

	current := reflect.ValueOf(value)
	for i := 1; i < len(path); i++ {
		for current.IsValid() && current.Kind() == reflect.Ptr {
			current = current.Elem()
		}
		if !current.IsValid() {
			return publicAttrName(strings.Join(path[:i], ".")), true
		}

		switch current.Kind() {
		case reflect.Interface:
			current = current.Elem()
			continue
		case reflect.Map:
			element := current.MapIndex(reflect.ValueOf(path[i]))
			if !element.IsValid() {
				return publicAttrName(strings.Join(path[:i+1], ".")), true
			}
			current = element
			if i == len(path)-1 && current.IsValid() && current.Kind() == reflect.Interface && current.IsNil() {
				return publicAttrName(strings.Join(path[:i+1], ".")), true
			}
		case reflect.Struct:
			field := current.FieldByName(path[i])
			if !field.IsValid() {
				return "", false
			}
			current = field
		default:
			return "", false
		}
	}
	return "", false
}

func publicAttrName(name string) string {
	name = strings.Replace(name, "_", ".", 1)
	return name
}

func generateDecisionEvalFunction(functions map[string]govaluate.ExpressionFunction, parameters *enforceParameters) govaluate.ExpressionFunction {
	return func(args ...interface{}) (interface{}, error) {
		return nil, errors.New("eval is only supported as a complete matcher branch")
	}
}
