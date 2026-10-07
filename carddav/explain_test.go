package carddav

import (
	"reflect"
	"strings"
	"testing"

	"github.com/emersion/go-vcard"
)

func newExplainTestAO(t *testing.T, str string) AddressObject {
	t.Helper()
	card, err := vcard.NewDecoder(strings.NewReader(str)).Decode()
	if err != nil {
		t.Fatal(err)
	}
	return AddressObject{Card: card}
}

// TestExplainConsistentWithMatch checks that the root conclusion of Explain
// always matches the boolean returned by Match, including for duplicate
// properties, negated text conditions and is-not-defined filters.
func TestExplainConsistentWithMatch(t *testing.T) {
	alice := newExplainTestAO(t, `BEGIN:VCARD
VERSION:4.0
UID:urn:uuid:4fbe8971-0bc3-424c-9c26-36c3e1eff6b1
FN:Alice Gopher
N:Gopher;Alice;;;
EMAIL;TYPE=work:alice@example.com
EMAIL;TYPE=home:alice@gopher.example.org
TEL:
END:VCARD`)

	cases := []struct {
		name  string
		query *AddressBookQuery
	}{
		{name: "nil-query", query: nil},
		{
			name: "duplicate-prop-first-instance-misses",
			query: &AddressBookQuery{
				PropFilters: []PropFilter{{
					Name:        vcard.FieldEmail,
					TextMatches: []TextMatch{{Text: "gopher.example.org"}},
				}},
			},
		},
		{
			name: "duplicate-prop-first-instance-matches",
			query: &AddressBookQuery{
				PropFilters: []PropFilter{{
					Name:        vcard.FieldEmail,
					TextMatches: []TextMatch{{Text: "alice@example.com"}},
				}},
			},
		},
		{
			name: "negated-text-condition",
			query: &AddressBookQuery{
				PropFilters: []PropFilter{{
					Name: vcard.FieldEmail,
					TextMatches: []TextMatch{{
						Text:            "bob@example.com",
						NegateCondition: true,
						MatchType:       MatchEquals,
					}},
				}},
			},
		},
		{
			name: "negated-text-condition-allof",
			query: &AddressBookQuery{
				PropFilters: []PropFilter{{
					Name: vcard.FieldEmail,
					Test: FilterAllOf,
					TextMatches: []TextMatch{
						{Text: "alice", MatchType: MatchContains},
						{Text: "example.org", NegateCondition: true, MatchType: MatchEndsWith},
					},
				}},
			},
		},
		{
			name: "is-not-defined-missing-prop",
			query: &AddressBookQuery{
				PropFilters: []PropFilter{{
					Name:         "X-NOT-THERE",
					IsNotDefined: true,
				}},
			},
		},
		{
			name: "is-not-defined-present-prop",
			query: &AddressBookQuery{
				PropFilters: []PropFilter{{
					Name:         vcard.FieldEmail,
					IsNotDefined: true,
				}},
			},
		},
		{
			name: "is-not-defined-empty-value-prop",
			query: &AddressBookQuery{
				PropFilters: []PropFilter{{
					Name:         vcard.FieldTelephone,
					IsNotDefined: true,
				}},
			},
		},
		{
			name: "empty-value-prop-text-match",
			query: &AddressBookQuery{
				PropFilters: []PropFilter{{
					Name:        vcard.FieldTelephone,
					TextMatches: []TextMatch{{Text: "alice"}},
				}},
			},
		},
		{
			name: "anyof-short-circuit",
			query: &AddressBookQuery{
				PropFilters: []PropFilter{
					{
						Name:        vcard.FieldEmail,
						TextMatches: []TextMatch{{Text: "alice"}},
					},
					{
						Name:        vcard.FieldFormattedName,
						TextMatches: []TextMatch{{Text: "Alice"}},
					},
				},
			},
		},
		{
			name: "allof-short-circuit",
			query: &AddressBookQuery{
				FilterTest: FilterAllOf,
				PropFilters: []PropFilter{
					{
						Name:        vcard.FieldEmail,
						TextMatches: []TextMatch{{Text: "bob"}},
					},
					{
						Name:        vcard.FieldFormattedName,
						TextMatches: []TextMatch{{Text: "Alice"}},
					},
				},
			},
		},
		{
			name: "param-filter-present",
			query: &AddressBookQuery{
				PropFilters: []PropFilter{{
					Name:        vcard.FieldEmail,
					Params:      []ParamFilter{{Name: "TYPE"}},
					TextMatches: []TextMatch{{Text: "alice"}},
				}},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cardBefore := copyCard(alice.Card)

			want, err := Match(tc.query, &alice)
			if err != nil {
				t.Fatalf("Match error: %v", err)
			}

			for _, opts := range []*ExplainOptions{nil, {IncludeValues: true}} {
				exp, err := Explain(tc.query, &alice, opts)
				if err != nil {
					t.Fatalf("Explain error: %v", err)
				}
				if got, want := exp.Result, evalResult(want); got != want {
					t.Fatalf("inconsistent root conclusion: Explain=%v, Match=%v", got, want)
				}
			}

			if !reflect.DeepEqual(cardBefore, alice.Card) {
				t.Fatal("Explain modified the vCard")
			}
		})
	}
}

func copyCard(card vcard.Card) vcard.Card {
	out := make(vcard.Card, len(card))
	for name, fields := range card {
		copied := make([]*vcard.Field, len(fields))
		for i, f := range fields {
			fieldCopy := *f
			copied[i] = &fieldCopy
		}
		out[name] = copied
	}
	return out
}

func TestExplainDuplicateProperties(t *testing.T) {
	ao := newExplainTestAO(t, `BEGIN:VCARD
VERSION:4.0
FN:Alice Gopher
EMAIL:alice@example.com
EMAIL:alice@gopher.example.org
END:VCARD`)

	query := &AddressBookQuery{
		PropFilters: []PropFilter{{
			Name:        vcard.FieldEmail,
			TextMatches: []TextMatch{{Text: "gopher.example.org"}},
		}},
	}

	exp, err := Explain(query, &ao, nil)
	if err != nil {
		t.Fatalf("Explain error: %v", err)
	}
	if exp.Result != ResultNotMatched {
		t.Fatalf("unexpected root result: %v", exp.Result)
	}

	pf := exp.PropFilters[0]
	if !pf.Defined {
		t.Error("expected Defined=true")
	}
	if pf.InstanceCount != 2 {
		t.Errorf("unexpected instance count: got %d, want 2", pf.InstanceCount)
	}
	if pf.MatchedFieldIndex != -1 {
		t.Errorf("unexpected matched index: got %d, want -1", pf.MatchedFieldIndex)
	}
	if got := pf.TextMatches[0].FieldIndex; got != 0 {
		t.Errorf("text-match evaluated against index %d, want 0", got)
	}
}

func TestExplainMissingPropVsEmptyValue(t *testing.T) {
	ao := newExplainTestAO(t, `BEGIN:VCARD
VERSION:4.0
FN:Alice Gopher
TEL:
END:VCARD`)

	query := &AddressBookQuery{
		PropFilters: []PropFilter{
			{
				Name:        "X-MISSING",
				TextMatches: []TextMatch{{Text: "alice"}},
			},
			{
				Name:        vcard.FieldTelephone,
				TextMatches: []TextMatch{{Text: "alice"}},
			},
		},
	}

	exp, err := Explain(query, &ao, nil)
	if err != nil {
		t.Fatalf("Explain error: %v", err)
	}

	missing := exp.PropFilters[0]
	if missing.Defined || missing.InstanceCount != 0 {
		t.Errorf("missing property: got Defined=%v InstanceCount=%d, want false/0",
			missing.Defined, missing.InstanceCount)
	}
	if len(missing.TextMatches) != 0 {
		t.Error("missing property must not have evaluated text matches")
	}

	empty := exp.PropFilters[1]
	if !empty.Defined || empty.InstanceCount != 1 {
		t.Errorf("empty-value property: got Defined=%v InstanceCount=%d, want true/1",
			empty.Defined, empty.InstanceCount)
	}
	if len(empty.TextMatches) != 1 || empty.TextMatches[0].Result != ResultNotMatched {
		t.Errorf("empty-value property: text match must be evaluated, got %+v", empty.TextMatches)
	}
}

func TestExplainShortCircuitNotEvaluated(t *testing.T) {
	ao := newExplainTestAO(t, `BEGIN:VCARD
VERSION:4.0
FN:Alice Gopher
EMAIL:alice@example.com
END:VCARD`)

	query := &AddressBookQuery{
		PropFilters: []PropFilter{
			{
				Name: vcard.FieldEmail,
				Test: FilterAnyOf,
				TextMatches: []TextMatch{
					{Text: "alice"},
					{Text: "bob"},
				},
			},
			{
				Name:        vcard.FieldFormattedName,
				TextMatches: []TextMatch{{Text: "Alice"}},
			},
		},
	}

	exp, err := Explain(query, &ao, nil)
	if err != nil {
		t.Fatalf("Explain error: %v", err)
	}
	if exp.Result != ResultMatched {
		t.Fatalf("unexpected root result: %v", exp.Result)
	}

	if got := exp.PropFilters[0].TextMatches[1].Result; got != ResultNotEvaluated {
		t.Errorf("short-circuited text-match: got %v, want %v", got, ResultNotEvaluated)
	}
	if exp.PropFilters[0].TextMatches[1].Reason == "" {
		t.Error("short-circuited text-match must carry a reason")
	}

	skipped := exp.PropFilters[1]
	if skipped.Result != ResultNotEvaluated {
		t.Errorf("short-circuited prop-filter: got %v, want %v", skipped.Result, ResultNotEvaluated)
	}
	if skipped.Reason == "" {
		t.Error("short-circuited prop-filter must carry a reason")
	}
	if skipped.Defined || skipped.InstanceCount != 0 {
		t.Error("short-circuited prop-filter must not inspect the card")
	}
}

func TestExplainParamFiltersNotEvaluated(t *testing.T) {
	ao := newExplainTestAO(t, `BEGIN:VCARD
VERSION:4.0
FN:Alice Gopher
EMAIL;TYPE=work:alice@example.com
END:VCARD`)

	query := &AddressBookQuery{
		PropFilters: []PropFilter{{
			Name:        vcard.FieldEmail,
			Params:      []ParamFilter{{Name: "TYPE"}},
			TextMatches: []TextMatch{{Text: "alice"}},
		}},
	}

	exp, err := Explain(query, &ao, nil)
	if err != nil {
		t.Fatalf("Explain error: %v", err)
	}

	params := exp.PropFilters[0].Params
	if len(params) != 1 {
		t.Fatalf("expected 1 param explanation, got %d", len(params))
	}
	if params[0].Result != ResultNotEvaluated || params[0].Reason == "" {
		t.Errorf("param filter must be marked not-evaluated with a reason, got %+v", params[0])
	}
}

func TestExplainDoesNotLeakValuesByDefault(t *testing.T) {
	ao := newExplainTestAO(t, `BEGIN:VCARD
VERSION:4.0
FN:Alice Gopher
EMAIL:alice@example.com
END:VCARD`)

	query := &AddressBookQuery{
		PropFilters: []PropFilter{{
			Name:        vcard.FieldEmail,
			TextMatches: []TextMatch{{Text: "alice"}},
		}},
	}

	exp, err := Explain(query, &ao, nil)
	if err != nil {
		t.Fatalf("Explain error: %v", err)
	}
	if got := exp.PropFilters[0].TextMatches[0].FieldValue; got != "" {
		t.Errorf("default explanation leaks field value %q", got)
	}
	if got := exp.PropFilters[0].TextMatches[0].FieldIndex; got != 0 {
		t.Errorf("default explanation must report the index, got %d", got)
	}

	exp, err = Explain(query, &ao, &ExplainOptions{IncludeValues: true})
	if err != nil {
		t.Fatalf("Explain error: %v", err)
	}
	if got, want := exp.PropFilters[0].TextMatches[0].FieldValue, "alice@example.com"; got != want {
		t.Errorf("IncludeValues: got field value %q, want %q", got, want)
	}
}
