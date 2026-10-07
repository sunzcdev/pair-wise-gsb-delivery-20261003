package carddav

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/emersion/go-vcard"
)

func explain(t *testing.T, query *AddressBookQuery, raw string, opts *ExplainOptions) *Explanation {
	t.Helper()
	ao := &AddressObject{Card: decodeCard(t, raw)}
	explanation, err := ExplainMatch(query, ao, opts)
	if err != nil {
		t.Fatal(err)
	}
	return explanation
}

func TestExplainDuplicatePropertyIndices(t *testing.T) {
	query := &AddressBookQuery{PropFilters: []PropFilter{{
		Name:        vcard.FieldEmail,
		TextMatches: []TextMatch{{Text: "work", MatchType: MatchContains}},
	}}}
	raw := "BEGIN:VCARD\nVERSION:4.0\nEMAIL:home@example.net\nEMAIL:work@example.com\nEMAIL:other@example.org\nEND:VCARD\n"

	explanation := explain(t, query, raw, nil)
	if !explanation.Matched() {
		t.Fatal("expected match")
	}
	prop := explanation.Props[0]
	if !prop.Present {
		t.Fatal("expected prop to be present")
	}
	if got, want := prop.MatchedInstances, []int{1}; !equalInts(got, want) {
		t.Fatalf("MatchedInstances = %v, want %v", got, want)
	}
	if len(prop.Instances) != 3 {
		t.Fatalf("expected 3 instance explanations, got %d", len(prop.Instances))
	}
	if prop.Instances[0].Status != StatusNotMatched {
		t.Fatalf("instance 0 status = %q", prop.Instances[0].Status)
	}
	if prop.Instances[1].Status != StatusMatched {
		t.Fatalf("instance 1 status = %q", prop.Instances[1].Status)
	}
	// Every property instance is evaluated independently; instance 2 is
	// simply a non-matching one (rather than an unevaluated short-circuit).
	if prop.Instances[2].Status != StatusNotMatched {
		t.Fatalf("instance 2 status = %q, want not-matched", prop.Instances[2].Status)
	}
}

func TestExplainMultipleMatchedInstances(t *testing.T) {
	query := &AddressBookQuery{PropFilters: []PropFilter{{
		Name:        vcard.FieldEmail,
		TextMatches: []TextMatch{{Text: "work", MatchType: MatchContains}},
	}}}
	raw := "BEGIN:VCARD\nVERSION:4.0\nEMAIL:home@example.net\nEMAIL:work-1@example.com\nEMAIL:work-2@example.com\nEND:VCARD\n"
	explanation := explain(t, query, raw, nil)
	if got, want := explanation.Props[0].MatchedInstances, []int{1, 2}; !equalInts(got, want) {
		t.Fatalf("MatchedInstances = %v, want %v", got, want)
	}
}

func TestExplainMissingVsEmptyValue(t *testing.T) {
	query := &AddressBookQuery{PropFilters: []PropFilter{{
		Name:        vcard.FieldNote,
		TextMatches: []TextMatch{{Text: "hello"}},
	}}}

	missing := explain(t, query, "BEGIN:VCARD\nVERSION:4.0\nEMAIL:a@b.c\nEND:VCARD\n", nil).Props[0]
	if missing.Present {
		t.Fatal("missing property reported as present")
	}
	if missing.Status != StatusNotMatched || len(missing.Instances) != 0 {
		t.Fatalf("missing property explanation = %+v", missing)
	}

	empty := explain(t, query, "BEGIN:VCARD\nVERSION:4.0\nNOTE:\nEND:VCARD\n", nil).Props[0]
	if !empty.Present {
		t.Fatal("empty property reported as missing")
	}
	if len(empty.Instances) != 1 || !empty.Instances[0].EmptyValue {
		t.Fatalf("empty property explanation = %+v", empty.Instances)
	}
	if empty.Status != StatusNotMatched {
		t.Fatalf("empty property status = %q", empty.Status)
	}
}

func TestExplainIsNotDefined(t *testing.T) {
	query := &AddressBookQuery{PropFilters: []PropFilter{{
		Name:         vcard.FieldNote,
		IsNotDefined: true,
	}}}

	missing := explain(t, query, "BEGIN:VCARD\nVERSION:4.0\nEMAIL:a@b.c\nEND:VCARD\n", nil).Props[0]
	if missing.Status != StatusMatched {
		t.Fatalf("is-not-defined on missing prop = %q", missing.Status)
	}

	present := explain(t, query, "BEGIN:VCARD\nVERSION:4.0\nNOTE:hi\nNOTE:yo\nEND:VCARD\n", nil).Props[0]
	if present.Status != StatusNotMatched {
		t.Fatalf("is-not-defined on present prop = %q", present.Status)
	}
	if len(present.Instances) != 2 {
		t.Fatalf("expected 2 instance stubs, got %d", len(present.Instances))
	}
	for _, instance := range present.Instances {
		if instance.Status != StatusNotEvaluated {
			t.Fatalf("instance status = %q, want not-evaluated", instance.Status)
		}
	}
}

func TestExplainNegatedText(t *testing.T) {
	query := &AddressBookQuery{PropFilters: []PropFilter{{
		Name:        vcard.FieldEmail,
		TextMatches: []TextMatch{{Text: "example.org", NegateCondition: true}},
	}}}
	raw := "BEGIN:VCARD\nVERSION:4.0\nEMAIL:alice@example.com\nEND:VCARD\n"
	explanation := explain(t, query, raw, nil)
	if !explanation.Matched() {
		t.Fatal("expected negated match to succeed")
	}
	text := explanation.Props[0].Instances[0].Texts[0]
	if !text.Negated || text.Status != StatusMatched {
		t.Fatalf("text explanation = %+v", text)
	}
}

func TestExplainAnyOfShortCircuitNodes(t *testing.T) {
	query := &AddressBookQuery{
		FilterTest: FilterAnyOf,
		PropFilters: []PropFilter{
			{Name: vcard.FieldEmail, TextMatches: []TextMatch{{Text: "alice", MatchType: MatchContains}}},
			{Name: vcard.FieldNote, TextMatches: []TextMatch{{Text: "secret"}}},
		},
	}
	raw := "BEGIN:VCARD\nVERSION:4.0\nEMAIL:alice@example.com\nNOTE:secret\nEND:VCARD\n"
	explanation := explain(t, query, raw, nil)
	if !explanation.Matched() {
		t.Fatal("expected match")
	}
	if explanation.Props[1].Status != StatusNotEvaluated {
		t.Fatalf("second top-level prop status = %q", explanation.Props[1].Status)
	}
	// The skipped node must not carry evaluated instances or a false verdict.
	if len(explanation.Props[1].Instances) != 0 {
		t.Fatalf("skipped prop unexpectedly has instances: %+v", explanation.Props[1].Instances)
	}
}

func TestExplainAllOfTextShortCircuit(t *testing.T) {
	query := &AddressBookQuery{PropFilters: []PropFilter{{
		Name: vcard.FieldEmail,
		Test: FilterAllOf,
		TextMatches: []TextMatch{
			{Text: "nope", MatchType: MatchEquals},
			{Text: "alice", MatchType: MatchContains},
		},
	}}}
	raw := "BEGIN:VCARD\nVERSION:4.0\nEMAIL:alice@example.com\nEND:VCARD\n"
	explanation := explain(t, query, raw, nil)
	texts := explanation.Props[0].Instances[0].Texts
	if texts[0].Status != StatusNotMatched {
		t.Fatalf("text 0 = %q", texts[0].Status)
	}
	if texts[1].Status != StatusNotEvaluated {
		t.Fatalf("text 1 = %q, want not-evaluated", texts[1].Status)
	}
}

func TestExplainParams(t *testing.T) {
	query := &AddressBookQuery{PropFilters: []PropFilter{{
		Name: vcard.FieldEmail,
		Params: []ParamFilter{
			{Name: "TYPE", TextMatch: &TextMatch{Text: "work", MatchType: MatchEquals}},
		},
	}}}
	raw := "BEGIN:VCARD\nVERSION:4.0\nEMAIL;TYPE=home:home@example.net\nEMAIL;TYPE=work:work@example.com\nEND:VCARD\n"
	explanation := explain(t, query, raw, nil)
	if !explanation.Matched() {
		t.Fatal("expected match")
	}
	if got, want := explanation.Props[0].MatchedInstances, []int{1}; !equalInts(got, want) {
		t.Fatalf("MatchedInstances = %v, want %v", got, want)
	}
	homeParam := explanation.Props[0].Instances[0].Params[0]
	workParam := explanation.Props[0].Instances[1].Params[0]
	if !homeParam.Present || homeParam.Status != StatusNotMatched {
		t.Fatalf("home param = %+v", homeParam)
	}
	if workParam.Status != StatusMatched || workParam.Text == nil || workParam.Text.Status != StatusMatched {
		t.Fatalf("work param = %+v", workParam)
	}
}

func TestExplainParamMissingVsEmpty(t *testing.T) {
	query := &AddressBookQuery{PropFilters: []PropFilter{{
		Name:   vcard.FieldEmail,
		Params: []ParamFilter{{Name: "LABEL"}},
	}}}

	missing := explain(t, query, "BEGIN:VCARD\nVERSION:4.0\nEMAIL:a@b.c\nEND:VCARD\n", nil).Props[0].Instances[0].Params[0]
	if missing.Present || missing.Status != StatusNotMatched {
		t.Fatalf("missing param = %+v", missing)
	}
}

func TestExplainNegatedMultiValueParam(t *testing.T) {
	query := &AddressBookQuery{PropFilters: []PropFilter{{
		Name:   vcard.FieldEmail,
		Params: []ParamFilter{{Name: "TYPE", TextMatch: &TextMatch{Text: "fax", MatchType: MatchEquals, NegateCondition: true}}},
	}}}

	// One value is "fax", so the negation must not match even though the
	// other value also differs from "fax".
	mixed := explain(t, query, "BEGIN:VCARD\nVERSION:4.0\nEMAIL;TYPE=work,fax:a@b.c\nEND:VCARD\n", nil)
	if mixed.Matched() {
		t.Fatal("negated multi-value param unexpectedly matched when one value equals the text")
	}

	none := explain(t, query, "BEGIN:VCARD\nVERSION:4.0\nEMAIL;TYPE=work,home:a@b.c\nEND:VCARD\n", nil)
	if !none.Matched() {
		t.Fatal("negated multi-value param should match when no value equals the text")
	}
}

func TestExplainDefaultDoesNotLeakValues(t *testing.T) {
	query := &AddressBookQuery{PropFilters: []PropFilter{{
		Name:        vcard.FieldEmail,
		TextMatches: []TextMatch{{Text: "alice"}},
	}}}
	raw := "BEGIN:VCARD\nVERSION:4.0\nEMAIL;TYPE=work:alice-secret@example.com\nEND:VCARD\n"

	explanation := explain(t, query, raw, nil)
	instance := explanation.Props[0].Instances[0]
	if instance.Value != "" {
		t.Fatalf("default explanation leaked property value: %q", instance.Value)
	}
	if serialized := encodeExplanation(explanation); strings.Contains(serialized, "alice-secret") || strings.Contains(serialized, "example.com") {
		t.Fatalf("default explanation leaked contact content: %s", serialized)
	}
}

func TestExplainIndicesOnly(t *testing.T) {
	query := &AddressBookQuery{PropFilters: []PropFilter{{
		Name:        vcard.FieldEmail,
		TextMatches: []TextMatch{{Text: "alice", MatchType: MatchContains}},
	}}}
	raw := "BEGIN:VCARD\nVERSION:4.0\nEMAIL;TYPE=work:alice@example.com\nEND:VCARD\n"

	explanation := explain(t, query, raw, &ExplainOptions{IndicesOnly: true})
	if !explanation.Matched() {
		t.Fatal("expected match")
	}
	prop := explanation.Props[0]
	if prop.Test != "" || prop.Present {
		t.Fatalf("indices-only leaked descriptors: %+v", prop)
	}
	if got, want := prop.MatchedInstances, []int{0}; !equalInts(got, want) {
		t.Fatalf("MatchedInstances = %v, want %v", got, want)
	}
	text := prop.Instances[0].Texts[0]
	if text.Text != "" || text.MatchType != "" || text.Negated || text.Value != "" {
		t.Fatalf("indices-only leaked text descriptor: %+v", text)
	}
	if serialized := encodeExplanation(explanation); strings.Contains(serialized, "alice") {
		t.Fatalf("indices-only leaked query/contact text: %s", serialized)
	}

	// IncludeValues must be ignored when IndicesOnly is set.
	withValues := explain(t, query, raw, &ExplainOptions{IndicesOnly: true, IncludeValues: true})
	if withValues.Props[0].Instances[0].Value != "" {
		t.Fatal("IncludeValues leaked under IndicesOnly")
	}
}

func TestExplainIncludeValues(t *testing.T) {
	query := &AddressBookQuery{PropFilters: []PropFilter{{
		Name:        vcard.FieldEmail,
		TextMatches: []TextMatch{{Text: "alice"}},
	}}}
	raw := "BEGIN:VCARD\nVERSION:4.0\nEMAIL;TYPE=work:alice@example.com\nEND:VCARD\n"

	explanation := explain(t, query, raw, &ExplainOptions{IncludeValues: true})
	instance := explanation.Props[0].Instances[0]
	if instance.Value != "alice@example.com" {
		t.Fatalf("IncludeValues value = %q", instance.Value)
	}
}

func encodeExplanation(e *Explanation) string {
	b, err := json.Marshal(e)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
