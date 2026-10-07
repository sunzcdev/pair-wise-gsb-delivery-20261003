package carddav

import (
	"fmt"

	"github.com/emersion/go-vcard"
)

// EvalResult is the tri-state outcome of a single explained node.
type EvalResult string

const (
	// ResultMatched indicates the node was evaluated and matched.
	ResultMatched EvalResult = "matched"
	// ResultNotMatched indicates the node was evaluated and did not match.
	ResultNotMatched EvalResult = "not-matched"
	// ResultNotEvaluated indicates the node was never evaluated, e.g.
	// because an anyof/allof parent short-circuited. It is never used as
	// a substitute for a real false outcome.
	ResultNotEvaluated EvalResult = "not-evaluated"
)

// ExplainOptions configures the Explain function.
type ExplainOptions struct {
	// IncludeValues includes the vCard field values of evaluated property
	// instances in the explanation. It defaults to false so that contact
	// contents are not leaked: only property instance indexes are
	// reported.
	IncludeValues bool
}

// MatchExplanation describes how an AddressBookQuery was evaluated against
// an AddressObject. The root Result is always identical to the result of
// Match for the same query and address object.
type MatchExplanation struct {
	Result     EvalResult
	FilterTest FilterTest

	PropFilters []PropFilterExplanation
}

// PropFilterExplanation describes the evaluation of a single PropFilter.
type PropFilterExplanation struct {
	Name string

	Result EvalResult
	// Reason explains why Result is ResultNotEvaluated, if applicable.
	Reason string

	// Defined reports whether the property is present on the card. A
	// missing property (Defined=false) is distinguished from a property
	// present with an empty value (Defined=true).
	Defined bool
	// IsNotDefined mirrors PropFilter.IsNotDefined.
	IsNotDefined bool
	// InstanceCount is the number of property instances on the card.
	InstanceCount int
	// MatchedFieldIndex is the index of the property instance that
	// satisfied the filter, or -1 if none did.
	MatchedFieldIndex int

	Params      []ParamFilterExplanation
	TextMatches []TextMatchExplanation
}

// ParamFilterExplanation describes the evaluation of a single ParamFilter.
type ParamFilterExplanation struct {
	Name string

	Result EvalResult
	// Reason explains why Result is ResultNotEvaluated, if applicable.
	Reason string
}

// TextMatchExplanation describes the evaluation of a single TextMatch.
type TextMatchExplanation struct {
	TextMatch TextMatch

	Result EvalResult
	// Reason explains why Result is ResultNotEvaluated, if applicable.
	Reason string

	// FieldIndex is the index of the property instance the condition was
	// evaluated against, or -1 if it was not evaluated.
	FieldIndex int
	// FieldValue is the value of the property instance. It is only
	// populated when ExplainOptions.IncludeValues is set.
	FieldValue string
}

func evalResult(ok bool) EvalResult {
	if ok {
		return ResultMatched
	}
	return ResultNotMatched
}

// Explain evaluates the query against the address object exactly like Match
// does, and returns a detailed explanation of every property filter,
// parameter filter and text condition involved. The root Result is
// guaranteed to be identical to the boolean returned by Match for the same
// inputs. Explain never modifies the vCard.
func Explain(query *AddressBookQuery, ao *AddressObject, opts *ExplainOptions) (*MatchExplanation, error) {
	if opts == nil {
		opts = &ExplainOptions{}
	}

	exp := &MatchExplanation{}
	if query == nil {
		exp.Result = ResultMatched
		return exp, nil
	}

	exp.FilterTest = query.FilterTest
	switch query.FilterTest {
	default:
		return nil, fmt.Errorf("unknown query filter test %q", query.FilterTest)

	case FilterAnyOf, "":
		exp.PropFilters = make([]PropFilterExplanation, len(query.PropFilters))
		matched := false
		for i, prop := range query.PropFilters {
			if matched {
				exp.PropFilters[i] = skippedPropFilter(prop, "anyof short-circuit: a previous prop-filter already matched")
				continue
			}
			pe, ok, err := explainPropFilter(prop, ao, opts)
			if err != nil {
				return nil, err
			}
			exp.PropFilters[i] = pe
			if ok {
				matched = true
			}
		}
		exp.Result = evalResult(matched)

	case FilterAllOf:
		exp.PropFilters = make([]PropFilterExplanation, len(query.PropFilters))
		matched := true
		for i, prop := range query.PropFilters {
			if !matched {
				exp.PropFilters[i] = skippedPropFilter(prop, "allof short-circuit: a previous prop-filter did not match")
				continue
			}
			pe, ok, err := explainPropFilter(prop, ao, opts)
			if err != nil {
				return nil, err
			}
			exp.PropFilters[i] = pe
			if !ok {
				matched = false
			}
		}
		exp.Result = evalResult(matched)
	}
	return exp, nil
}

func skippedPropFilter(prop PropFilter, reason string) PropFilterExplanation {
	pe := PropFilterExplanation{
		Name:              prop.Name,
		Result:            ResultNotEvaluated,
		Reason:            reason,
		IsNotDefined:      prop.IsNotDefined,
		MatchedFieldIndex: -1,
	}
	for _, p := range prop.Params {
		pe.Params = append(pe.Params, ParamFilterExplanation{
			Name:   p.Name,
			Result: ResultNotEvaluated,
			Reason: reason,
		})
	}
	for _, tm := range prop.TextMatches {
		pe.TextMatches = append(pe.TextMatches, TextMatchExplanation{
			TextMatch:  tm,
			Result:     ResultNotEvaluated,
			Reason:     reason,
			FieldIndex: -1,
		})
	}
	return pe
}

func explainPropFilter(prop PropFilter, ao *AddressObject, opts *ExplainOptions) (PropFilterExplanation, bool, error) {
	pe := PropFilterExplanation{
		Name:              prop.Name,
		IsNotDefined:      prop.IsNotDefined,
		MatchedFieldIndex: -1,
	}

	fields := ao.Card[prop.Name]
	pe.Defined = len(fields) > 0
	pe.InstanceCount = len(fields)

	// The matcher does not evaluate param filters (see matchPropFilter),
	// so they are recorded as not evaluated instead of inventing an
	// outcome.
	for _, p := range prop.Params {
		pe.Params = append(pe.Params, ParamFilterExplanation{
			Name:   p.Name,
			Result: ResultNotEvaluated,
			Reason: "param filters are not evaluated by the matcher",
		})
	}

	// Match only considers the first property instance.
	field := ao.Card.Get(prop.Name)
	if field == nil {
		ok := prop.IsNotDefined
		pe.Result = evalResult(ok)
		return pe, ok, nil
	} else if prop.IsNotDefined {
		pe.Result = ResultNotMatched
		return pe, false, nil
	}

	if len(prop.TextMatches) == 0 {
		pe.Result = ResultMatched
		pe.MatchedFieldIndex = 0
		return pe, true, nil
	}

	switch prop.Test {
	default:
		return pe, false, fmt.Errorf("unknown property filter test %q", prop.Test)

	case FilterAnyOf, "":
		pe.TextMatches = make([]TextMatchExplanation, len(prop.TextMatches))
		matched := false
		for i, txt := range prop.TextMatches {
			if matched {
				pe.TextMatches[i] = skippedTextMatch(txt, "anyof short-circuit: a previous text-match already matched")
				continue
			}
			te, ok, err := explainTextMatch(txt, field, opts)
			if err != nil {
				return pe, false, err
			}
			pe.TextMatches[i] = te
			if ok {
				matched = true
			}
		}
		pe.Result = evalResult(matched)
		if matched {
			pe.MatchedFieldIndex = 0
		}
		return pe, matched, nil

	case FilterAllOf:
		pe.TextMatches = make([]TextMatchExplanation, len(prop.TextMatches))
		matched := true
		for i, txt := range prop.TextMatches {
			if !matched {
				pe.TextMatches[i] = skippedTextMatch(txt, "allof short-circuit: a previous text-match did not match")
				continue
			}
			te, ok, err := explainTextMatch(txt, field, opts)
			if err != nil {
				return pe, false, err
			}
			pe.TextMatches[i] = te
			if !ok {
				matched = false
			}
		}
		pe.Result = evalResult(matched)
		if matched {
			pe.MatchedFieldIndex = 0
		}
		return pe, matched, nil
	}
}

func skippedTextMatch(txt TextMatch, reason string) TextMatchExplanation {
	return TextMatchExplanation{
		TextMatch:  txt,
		Result:     ResultNotEvaluated,
		Reason:     reason,
		FieldIndex: -1,
	}
}

func explainTextMatch(txt TextMatch, field *vcard.Field, opts *ExplainOptions) (TextMatchExplanation, bool, error) {
	te := TextMatchExplanation{
		TextMatch:  txt,
		FieldIndex: 0,
	}
	if opts.IncludeValues {
		te.FieldValue = field.Value
	}
	ok, err := matchTextMatch(txt, field)
	if err != nil {
		return te, false, err
	}
	te.Result = evalResult(ok)
	return te, ok, nil
}
