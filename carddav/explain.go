package carddav

import (
	"fmt"
	"strings"

	"github.com/emersion/go-vcard"
)

// MatchStatus is the tri-state verdict of a filter node.
type MatchStatus string

const (
	// StatusMatched means the node was evaluated and matched.
	StatusMatched MatchStatus = "matched"
	// StatusNotMatched means the node was evaluated and did not match.
	StatusNotMatched MatchStatus = "not-matched"
	// StatusNotEvaluated means the node was skipped, typically because an
	// anyof/allof short-circuit happened earlier in the same group or
	// because the property did not exist. It is distinct from
	// StatusNotMatched: a skipped node was never evaluated, so it must not
	// be reported as false.
	StatusNotEvaluated MatchStatus = "not-evaluated"
)

// ExplainOptions controls how much contact data ExplainMatch exposes.
//
// The zero value is privacy-preserving: no vCard values or parameters are
// included in the explanation, only structural information (property and
// parameter names, match type) and matched property instance indices.
type ExplainOptions struct {
	// IndicesOnly restricts the explanation to verdicts and matched
	// property instance indices. When set, query text, match types and
	// presence/emptiness flags are omitted, so the response contains no
	// description beyond the matching structure and indices.
	IndicesOnly bool

	// IncludeValues includes the evaluated vCard property values and
	// parameter values in the explanation. It has no effect when
	// IndicesOnly is set. This can leak contact content, so it must be
	// enabled explicitly by the caller.
	IncludeValues bool
}

// Explanation is the root verdict of ExplainMatch. Matched always equals the
// boolean returned by Match for the same query and address object.
type Explanation struct {
	Test   FilterTest        `json:"test"`
	Status MatchStatus       `json:"status"`
	Props  []PropExplanation `json:"props"`
}

// Matched reports whether the whole query matched.
func (e *Explanation) Matched() bool { return e.Status == StatusMatched }

// PropExplanation records the evaluation of a single prop-filter.
type PropExplanation struct {
	Name string     `json:"name"`
	Test FilterTest `json:"test,omitempty"`

	// IsNotDefined reports that the prop-filter only checks property
	// existence.
	IsNotDefined bool `json:"is_not_defined,omitempty"`
	// Present reports whether the card contains the property. It is what
	// distinguishes a missing property from a present property whose value
	// happens to be empty.
	Present bool `json:"present,omitempty"`

	Status MatchStatus `json:"status"`

	// MatchedInstances holds the indices (within vcard.Card[Name]) of the
	// property instances that satisfy the whole prop-filter.
	MatchedInstances []int                 `json:"matched_instances,omitempty"`
	Instances        []InstanceExplanation `json:"instances,omitempty"`
}

// Matched reports whether the prop-filter matched.
func (p *PropExplanation) Matched() bool { return p.Status == StatusMatched }

// InstanceExplanation records the evaluation of one concrete property
// instance, identified by Index within vcard.Card[prop.Name].
type InstanceExplanation struct {
	Index int `json:"index"`

	// EmptyValue reports that the property exists but its value is empty.
	EmptyValue bool `json:"empty_value,omitempty"`
	// Value is the evaluated property value, only populated when
	// ExplainOptions.IncludeValues is set.
	Value string `json:"value,omitempty"`

	Status MatchStatus `json:"status"`

	Params []ParamExplanation     `json:"params,omitempty"`
	Texts  []TextMatchExplanation `json:"texts,omitempty"`
}

// Matched reports whether the property instance satisfied every param-filter
// and the combined text-match conditions.
func (i *InstanceExplanation) Matched() bool { return i.Status == StatusMatched }

// ParamExplanation records the evaluation of a single param-filter against a
// property instance.
type ParamExplanation struct {
	Name string `json:"name"`

	IsNotDefined bool `json:"is_not_defined,omitempty"`
	// Present reports whether the parameter exists on the property
	// instance; EmptyValue reports that it exists with an empty value.
	// The two are kept separate so a missing parameter is not confused
	// with a present-but-empty one.
	Present    bool `json:"present,omitempty"`
	EmptyValue bool `json:"empty_value,omitempty"`

	Status MatchStatus `json:"status"`

	// Values are the raw parameter values, only populated when
	// ExplainOptions.IncludeValues is set.
	Values []string `json:"values,omitempty"`
	// Text describes the optional param-filter text-match evaluation.
	Text *TextMatchExplanation `json:"text,omitempty"`
}

// Matched reports whether the param-filter matched.
func (p *ParamExplanation) Matched() bool { return p.Status == StatusMatched }

// TextMatchExplanation records the evaluation of a single text-match
// condition.
type TextMatchExplanation struct {
	// Text is the searched substring, only populated when IndicesOnly is
	// not set.
	Text string `json:"text,omitempty"`
	// MatchType is the comparison used, only populated when IndicesOnly is
	// not set.
	MatchType MatchType `json:"match_type,omitempty"`
	// Negated reports the negate-condition flag, only populated when
	// IndicesOnly is not set.
	Negated bool `json:"negated,omitempty"`

	Status MatchStatus `json:"status"`

	// Value is the concrete value that satisfied a non-negated condition,
	// only populated when ExplainOptions.IncludeValues is set.
	Value string `json:"value,omitempty"`
}

// Matched reports whether the text-match condition matched.
func (t *TextMatchExplanation) Matched() bool { return t.Status == StatusMatched }

// ExplainMatch reports whether the provided AddressObject matches the query
// and records every property filter, parameter filter and text condition
// evaluated while reaching that verdict, including the indices of the
// property instances that matched.
//
// ExplainMatch never mutates the vCard. The root Explanation.Matched is
// always identical to the boolean returned by Match for the same inputs;
// errors are identical as well.
//
// With a nil ExplainOptions the explanation does not contain any vCard value
// or parameter value, so it does not leak contact content. Pass
// ExplainOptions{IndicesOnly: true} to further reduce the response to
// verdicts and matched property instance indices.
func ExplainMatch(query *AddressBookQuery, ao *AddressObject, opts *ExplainOptions) (*Explanation, error) {
	if opts == nil {
		opts = &ExplainOptions{}
	}
	if query == nil {
		return &Explanation{Status: StatusMatched}, nil
	}

	test := query.FilterTest
	switch test {
	case FilterAnyOf, "":
		if test == "" {
			test = FilterAnyOf
		}
		root := &Explanation{Test: test}
		for i := range query.PropFilters {
			prop, err := explainProp(&query.PropFilters[i], ao, opts)
			if err != nil {
				return nil, err
			}
			root.Props = append(root.Props, prop)
			if prop.Status == StatusMatched {
				root.Props = append(root.Props, unevaluatedProps(query.PropFilters[i+1:], opts)...)
				root.Status = StatusMatched
				return root, nil
			}
		}
		root.Status = StatusNotMatched
		return root, nil

	case FilterAllOf:
		root := &Explanation{Test: test}
		for i := range query.PropFilters {
			prop, err := explainProp(&query.PropFilters[i], ao, opts)
			if err != nil {
				return nil, err
			}
			root.Props = append(root.Props, prop)
			if prop.Status != StatusMatched {
				root.Props = append(root.Props, unevaluatedProps(query.PropFilters[i+1:], opts)...)
				root.Status = StatusNotMatched
				return root, nil
			}
		}
		root.Status = StatusMatched
		return root, nil

	default:
		return nil, fmt.Errorf("unknown query filter test %q", query.FilterTest)
	}
}

func unevaluatedProps(filters []PropFilter, opts *ExplainOptions) []PropExplanation {
	props := make([]PropExplanation, 0, len(filters))
	for i := range filters {
		prop := PropExplanation{
			Name:   filters[i].Name,
			Status: StatusNotEvaluated,
		}
		if !opts.IndicesOnly {
			prop.IsNotDefined = filters[i].IsNotDefined
			prop.Test = filters[i].Test
		}
		props = append(props, prop)
	}
	return props
}

func explainProp(filter *PropFilter, ao *AddressObject, opts *ExplainOptions) (PropExplanation, error) {
	prop := PropExplanation{
		Name: filter.Name,
	}
	if !opts.IndicesOnly {
		prop.IsNotDefined = filter.IsNotDefined
		prop.Test = filter.Test
	}

	fields := ao.Card[filter.Name]
	if len(fields) == 0 {
		if filter.IsNotDefined {
			prop.Status = StatusMatched
		} else {
			prop.Status = StatusNotMatched
		}
		return prop, nil
	}

	prop.Present = !opts.IndicesOnly
	if filter.IsNotDefined {
		// RFC 6352: the property must not exist; its value and parameters
		// are intentionally not evaluated.
		prop.Status = StatusNotMatched
		prop.Instances = unevaluatedInstances(fields)
		return prop, nil
	}

	prop.Instances = make([]InstanceExplanation, 0, len(fields))
	for index, field := range fields {
		instance, err := explainInstance(index, field, filter, opts)
		if err != nil {
			prop.Instances = append(prop.Instances, instance)
			prop.Instances = append(prop.Instances, unevaluatedInstances(fields[index+1:])...)
			prop.Status = StatusNotEvaluated
			return prop, err
		}
		prop.Instances = append(prop.Instances, instance)
		if instance.Status == StatusMatched {
			prop.MatchedInstances = append(prop.MatchedInstances, index)
		}
	}
	if len(prop.MatchedInstances) > 0 {
		prop.Status = StatusMatched
	} else {
		prop.Status = StatusNotMatched
	}
	return prop, nil
}

func unevaluatedInstances(fields []*vcard.Field) []InstanceExplanation {
	instances := make([]InstanceExplanation, 0, len(fields))
	for index := range fields {
		instances = append(instances, InstanceExplanation{
			Index:  index,
			Status: StatusNotEvaluated,
		})
	}
	return instances
}

func explainInstance(index int, field *vcard.Field, filter *PropFilter, opts *ExplainOptions) (InstanceExplanation, error) {
	instance := InstanceExplanation{Index: index}
	if field.Value == "" {
		instance.EmptyValue = !opts.IndicesOnly
	}
	if opts.IncludeValues && !opts.IndicesOnly {
		instance.Value = field.Value
	}

	for i := range filter.Params {
		param := explainParam(&filter.Params[i], field, opts)
		instance.Params = append(instance.Params, param)
		if param.Status == StatusNotEvaluated {
			return instance, fmt.Errorf("unknown textmatch type %q", param.Text.MatchType)
		}
	}

	// Param-filters combine as AND; a failing parameter skips the text
	// conditions, which are recorded as not-evaluated rather than false.
	paramsMatched := true
	for _, param := range instance.Params {
		if param.Status != StatusMatched {
			paramsMatched = false
			break
		}
	}

	if len(filter.TextMatches) > 0 {
		switch filter.Test {
		case FilterAnyOf, "", FilterAllOf:
		default:
			instance.Texts = append(instance.Texts, unevaluatedTexts(filter.TextMatches, opts)...)
			return instance, fmt.Errorf("unknown property filter test %q", filter.Test)
		}
	}

	textMatched := len(filter.TextMatches) == 0 || filter.Test == FilterAllOf
textLoop:
	for i := range filter.TextMatches {
		if !paramsMatched {
			instance.Texts = append(instance.Texts, unevaluatedText(&filter.TextMatches[i], opts))
			continue
		}

		text := explainText(&filter.TextMatches[i], field.Value, opts)
		instance.Texts = append(instance.Texts, text)
		if text.Status == StatusNotEvaluated {
			instance.Texts = append(instance.Texts, unevaluatedTexts(filter.TextMatches[i+1:], opts)...)
			return instance, fmt.Errorf("unknown textmatch type %q", filter.TextMatches[i].MatchType)
		}
		switch filter.Test {
		case FilterAnyOf, "":
			if text.Status == StatusMatched {
				textMatched = true
				instance.Texts = append(instance.Texts, unevaluatedTexts(filter.TextMatches[i+1:], opts)...)
				break textLoop
			}
		case FilterAllOf:
			if text.Status != StatusMatched {
				// The remaining text-matches are skipped by the allof
				// short-circuit; they are not evaluated.
				instance.Texts = append(instance.Texts, unevaluatedTexts(filter.TextMatches[i+1:], opts)...)
				instance.Status = StatusNotMatched
				return instance, nil
			}
		}
	}

	if paramsMatched && textMatched {
		instance.Status = StatusMatched
	} else {
		instance.Status = StatusNotMatched
	}
	return instance, nil
}

func unevaluatedTexts(filters []TextMatch, opts *ExplainOptions) []TextMatchExplanation {
	texts := make([]TextMatchExplanation, 0, len(filters))
	for i := range filters {
		texts = append(texts, unevaluatedText(&filters[i], opts))
	}
	return texts
}

func unevaluatedText(filter *TextMatch, opts *ExplainOptions) TextMatchExplanation {
	text := TextMatchExplanation{Status: StatusNotEvaluated}
	if !opts.IndicesOnly {
		text.Text = filter.Text
		text.MatchType = filter.MatchType
		text.Negated = filter.NegateCondition
	}
	return text
}

func explainText(filter *TextMatch, value string, opts *ExplainOptions) TextMatchExplanation {
	text := TextMatchExplanation{}
	if !opts.IndicesOnly {
		text.Text = filter.Text
		text.MatchType = filter.MatchType
		text.Negated = filter.NegateCondition
	}

	matched, err := compareText(filter.MatchType, filter.Text, value)
	if err != nil {
		text.Status = StatusNotEvaluated
		return text
	}
	if filter.NegateCondition {
		matched = !matched
	}
	if matched {
		text.Status = StatusMatched
		if opts.IncludeValues && !opts.IndicesOnly {
			text.Value = value
		}
	} else {
		text.Status = StatusNotMatched
	}
	return text
}

func explainParam(filter *ParamFilter, field *vcard.Field, opts *ExplainOptions) ParamExplanation {
	param := ParamExplanation{Name: filter.Name}
	if !opts.IndicesOnly {
		param.IsNotDefined = filter.IsNotDefined
	}

	values, present := lookupParam(field.Params, filter.Name)
	param.Present = !opts.IndicesOnly && present
	if present {
		for _, value := range values {
			if value == "" {
				param.EmptyValue = !opts.IndicesOnly
				break
			}
		}
	}
	if opts.IncludeValues && !opts.IndicesOnly && present {
		param.Values = append(param.Values, values...)
	}

	if !present {
		if filter.IsNotDefined {
			param.Status = StatusMatched
		} else {
			param.Status = StatusNotMatched
		}
		return param
	}
	if filter.IsNotDefined {
		param.Status = StatusNotMatched
		return param
	}
	if filter.TextMatch == nil {
		// A param-filter without text-match only requires the parameter to
		// exist.
		param.Status = StatusMatched
		return param
	}

	text := TextMatchExplanation{}
	if !opts.IndicesOnly {
		text.Text = filter.TextMatch.Text
		text.MatchType = filter.TextMatch.MatchType
		text.Negated = filter.TextMatch.NegateCondition
	}

	// The parameter matches when at least one parameter value satisfies
	// the text-match. With a negated condition a value satisfies it only
	// when the comparison fails, so every parameter value must be checked
	// rather than stopping at the first.
	invalid := false
	matchedValues := 0
	var matchedValue string
	for _, value := range values {
		matched, err := compareText(filter.TextMatch.MatchType, filter.TextMatch.Text, value)
		if err != nil {
			invalid = true
			break
		}
		if filter.TextMatch.NegateCondition {
			matched = !matched
		}
		if matched {
			matchedValues++
			if matchedValue == "" {
				matchedValue = value
			}
			if !filter.TextMatch.NegateCondition {
				break
			}
		}
	}
	if invalid {
		text.Status = StatusNotEvaluated
		param.Status = StatusNotEvaluated
	} else if (!filter.TextMatch.NegateCondition && matchedValues > 0) ||
		(filter.TextMatch.NegateCondition && matchedValues == len(values)) {
		text.Status = StatusMatched
		param.Status = StatusMatched
		if opts.IncludeValues && !opts.IndicesOnly {
			text.Value = matchedValue
		}
	} else {
		text.Status = StatusNotMatched
		param.Status = StatusNotMatched
	}
	param.Text = &text
	return param
}

// lookupParam returns the values of the named parameter. go-vcard stores
// parameter names upper-cased; a fallback case-insensitive lookup covers
// parameters constructed directly.
func lookupParam(params vcard.Params, name string) ([]string, bool) {
	if values, ok := params[name]; ok && len(values) > 0 {
		return values, true
	}
	for key, values := range params {
		if strings.EqualFold(key, name) && len(values) > 0 {
			return values, true
		}
	}
	return nil, false
}

func compareText(matchType MatchType, text, value string) (bool, error) {
	switch matchType {
	case MatchEquals:
		return text == value, nil
	case MatchContains, "":
		return strings.Contains(value, text), nil
	case MatchStartsWith:
		return strings.HasPrefix(value, text), nil
	case MatchEndsWith:
		return strings.HasSuffix(value, text), nil
	default:
		return false, fmt.Errorf("unknown textmatch type %q", matchType)
	}
}
