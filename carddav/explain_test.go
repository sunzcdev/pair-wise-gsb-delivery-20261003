package carddav

import (
	"strings"
	"testing"

	"github.com/emersion/go-vcard"
)

func decodeCard(t *testing.T, raw string) vcard.Card {
	t.Helper()
	card, err := vcard.NewDecoder(strings.NewReader(raw)).Decode()
	if err != nil {
		t.Fatal(err)
	}
	return card
}

// every query in this table is exercised against Match and ExplainMatch in
// TestMatchExplainConsistency below.
var explainConsistencyCases = []struct {
	name  string
	query *AddressBookQuery
	card  string
}{
	{
		name: "single-email",
		query: &AddressBookQuery{PropFilters: []PropFilter{{
			Name:        vcard.FieldEmail,
			TextMatches: []TextMatch{{Text: "example.com"}},
		}}},
		card: "BEGIN:VCARD\nVERSION:4.0\nEMAIL:alice@example.com\nEND:VCARD\n",
	},
	{
		name: "duplicate-prop-first-instance",
		query: &AddressBookQuery{PropFilters: []PropFilter{{
			Name:        vcard.FieldEmail,
			TextMatches: []TextMatch{{Text: "work@example.com"}},
		}}},
		card: "BEGIN:VCARD\nVERSION:4.0\nEMAIL:home@example.net\nEMAIL:work@example.com\nEND:VCARD\n",
	},
	{
		name: "duplicate-prop-no-instance",
		query: &AddressBookQuery{PropFilters: []PropFilter{{
			Name:        vcard.FieldEmail,
			TextMatches: []TextMatch{{Text: "nomatch"}},
		}}},
		card: "BEGIN:VCARD\nVERSION:4.0\nEMAIL:home@example.net\nEMAIL:work@example.com\nEND:VCARD\n",
	},
	{
		name: "negated-text",
		query: &AddressBookQuery{PropFilters: []PropFilter{{
			Name:        vcard.FieldEmail,
			TextMatches: []TextMatch{{Text: "example.org", NegateCondition: true}},
		}}},
		card: "BEGIN:VCARD\nVERSION:4.0\nEMAIL:alice@example.com\nEND:VCARD\n",
	},
	{
		name: "negated-text-fails",
		query: &AddressBookQuery{PropFilters: []PropFilter{{
			Name:        vcard.FieldEmail,
			TextMatches: []TextMatch{{Text: "example.com", NegateCondition: true}},
		}}},
		card: "BEGIN:VCARD\nVERSION:4.0\nEMAIL:alice@example.com\nEND:VCARD\n",
	},
	{
		name: "anyof-text-short-circuit",
		query: &AddressBookQuery{PropFilters: []PropFilter{{
			Name: vcard.FieldEmail,
			TextMatches: []TextMatch{
				{Text: "alice@example.com", MatchType: MatchEquals},
				{Text: "unused", MatchType: MatchEquals},
			},
		}}},
		card: "BEGIN:VCARD\nVERSION:4.0\nEMAIL:alice@example.com\nEND:VCARD\n",
	},
	{
		name: "allof-text-short-circuit",
		query: &AddressBookQuery{PropFilters: []PropFilter{{
			Name: vcard.FieldEmail,
			Test: FilterAllOf,
			TextMatches: []TextMatch{
				{Text: "zzz", MatchType: MatchEquals},
				{Text: "alice@example.com", MatchType: MatchEquals},
			},
		}}},
		card: "BEGIN:VCARD\nVERSION:4.0\nEMAIL:alice@example.com\nEND:VCARD\n",
	},
	{
		name: "top-anyof-short-circuit",
		query: &AddressBookQuery{
			FilterTest: FilterAnyOf,
			PropFilters: []PropFilter{
				{Name: vcard.FieldEmail, TextMatches: []TextMatch{{Text: "alice@example.com", MatchType: MatchEquals}}},
				{Name: vcard.FieldNote, TextMatches: []TextMatch{{Text: "secret"}}},
			},
		},
		card: "BEGIN:VCARD\nVERSION:4.0\nEMAIL:alice@example.com\nEND:VCARD\n",
	},
	{
		name: "top-allof-short-circuit",
		query: &AddressBookQuery{
			FilterTest: FilterAllOf,
			PropFilters: []PropFilter{
				{Name: vcard.FieldNote},
				{Name: vcard.FieldEmail},
			},
		},
		card: "BEGIN:VCARD\nVERSION:4.0\nEMAIL:alice@example.com\nEND:VCARD\n",
	},
	{
		name: "is-not-defined-missing",
		query: &AddressBookQuery{PropFilters: []PropFilter{{
			Name:         vcard.FieldNote,
			IsNotDefined: true,
		}}},
		card: "BEGIN:VCARD\nVERSION:4.0\nEMAIL:alice@example.com\nEND:VCARD\n",
	},
	{
		name: "is-not-defined-present",
		query: &AddressBookQuery{PropFilters: []PropFilter{{
			Name:         vcard.FieldNote,
			IsNotDefined: true,
		}}},
		card: "BEGIN:VCARD\nVERSION:4.0\nNOTE:hello\nEND:VCARD\n",
	},
	{
		name: "missing-prop",
		query: &AddressBookQuery{PropFilters: []PropFilter{{
			Name:        vcard.FieldNote,
			TextMatches: []TextMatch{{Text: "hello"}},
		}}},
		card: "BEGIN:VCARD\nVERSION:4.0\nEMAIL:alice@example.com\nEND:VCARD\n",
	},
	{
		name: "empty-value-prop",
		query: &AddressBookQuery{PropFilters: []PropFilter{{
			Name:        vcard.FieldNote,
			TextMatches: []TextMatch{{Text: "hello"}},
		}}},
		card: "BEGIN:VCARD\nVERSION:4.0\nNOTE:\nEND:VCARD\n",
	},
	{
		name: "empty-value-negated",
		query: &AddressBookQuery{PropFilters: []PropFilter{{
			Name:        vcard.FieldNote,
			TextMatches: []TextMatch{{Text: "hello", NegateCondition: true}},
		}}},
		card: "BEGIN:VCARD\nVERSION:4.0\nNOTE:\nEND:VCARD\n",
	},
	{
		name: "param-exists",
		query: &AddressBookQuery{PropFilters: []PropFilter{{
			Name:   vcard.FieldEmail,
			Params: []ParamFilter{{Name: "TYPE"}},
		}}},
		card: "BEGIN:VCARD\nVERSION:4.0\nEMAIL;TYPE=work:alice@example.com\nEND:VCARD\n",
	},
	{
		name: "param-missing",
		query: &AddressBookQuery{PropFilters: []PropFilter{{
			Name:   vcard.FieldEmail,
			Params: []ParamFilter{{Name: "TYPE"}},
		}}},
		card: "BEGIN:VCARD\nVERSION:4.0\nEMAIL:alice@example.com\nEND:VCARD\n",
	},
	{
		name: "param-text-match",
		query: &AddressBookQuery{PropFilters: []PropFilter{{
			Name:   vcard.FieldEmail,
			Params: []ParamFilter{{Name: "TYPE", TextMatch: &TextMatch{Text: "work", MatchType: MatchEquals}}},
		}}},
		card: "BEGIN:VCARD\nVERSION:4.0\nEMAIL;TYPE=home:home@example.net\nEMAIL;TYPE=work:work@example.com\nEND:VCARD\n",
	},
	{
		name: "param-text-no-match",
		query: &AddressBookQuery{PropFilters: []PropFilter{{
			Name:   vcard.FieldEmail,
			Params: []ParamFilter{{Name: "TYPE", TextMatch: &TextMatch{Text: "fax", MatchType: MatchEquals}}},
		}}},
		card: "BEGIN:VCARD\nVERSION:4.0\nEMAIL;TYPE=home:home@example.net\nEMAIL;TYPE=work:work@example.com\nEND:VCARD\n",
	},
	{
		name: "param-and-text",
		query: &AddressBookQuery{PropFilters: []PropFilter{{
			Name:        vcard.FieldEmail,
			Params:      []ParamFilter{{Name: "TYPE", TextMatch: &TextMatch{Text: "work", MatchType: MatchEquals}}},
			TextMatches: []TextMatch{{Text: "work@example.com", MatchType: MatchEquals}},
		}}},
		card: "BEGIN:VCARD\nVERSION:4.0\nEMAIL;TYPE=home:work@example.com\nEMAIL;TYPE=work:work@example.com\nEND:VCARD\n",
	},
	{
		name: "param-is-not-defined",
		query: &AddressBookQuery{PropFilters: []PropFilter{{
			Name:   vcard.FieldEmail,
			Params: []ParamFilter{{Name: "TYPE", IsNotDefined: true}},
		}}},
		card: "BEGIN:VCARD\nVERSION:4.0\nEMAIL;TYPE=work:alice@example.com\nEMAIL:bob@example.org\nEND:VCARD\n",
	},
	{
		name: "multi-value-param",
		query: &AddressBookQuery{PropFilters: []PropFilter{{
			Name:   vcard.FieldEmail,
			Params: []ParamFilter{{Name: "TYPE", TextMatch: &TextMatch{Text: "home", MatchType: MatchEquals}}},
		}}},
		card: "BEGIN:VCARD\nVERSION:4.0\nEMAIL;TYPE=work,home:alice@example.com\nEND:VCARD\n",
	},
	{
		name:  "empty-filter",
		query: &AddressBookQuery{},
		card:  "BEGIN:VCARD\nVERSION:4.0\nEMAIL:alice@example.com\nEND:VCARD\n",
	},
	{
		name:  "empty-prop-filter-present",
		query: &AddressBookQuery{PropFilters: []PropFilter{{Name: vcard.FieldEmail}}},
		card:  "BEGIN:VCARD\nVERSION:4.0\nEMAIL:alice@example.com\nEND:VCARD\n",
	},
	{
		name:  "empty-prop-filter-missing",
		query: &AddressBookQuery{PropFilters: []PropFilter{{Name: vcard.FieldNote}}},
		card:  "BEGIN:VCARD\nVERSION:4.0\nEMAIL:alice@example.com\nEND:VCARD\n",
	},
}

// TestMatchExplainConsistency verifies the root conclusion of ExplainMatch is
// identical to the Match boolean for every case, under all ExplainOptions.
func TestMatchExplainConsistency(t *testing.T) {
	optionSets := []*ExplainOptions{
		nil,
		{IndicesOnly: true},
		{IncludeValues: true},
		{IndicesOnly: true, IncludeValues: true},
	}

	for _, tc := range explainConsistencyCases {
		t.Run(tc.name, func(t *testing.T) {
			card := decodeCard(t, tc.card)
			var before strings.Builder
			if err := vcard.NewEncoder(&before).Encode(card); err != nil {
				t.Fatal(err)
			}
			ao := &AddressObject{Card: card}

			want, matchErr := Match(tc.query, ao)
			for _, opts := range optionSets {
				explanation, err := ExplainMatch(tc.query, ao, opts)
				if (err != nil) != (matchErr != nil) {
					t.Fatalf("ExplainMatch error = %v, Match error = %v", err, matchErr)
				}
				if err != nil {
					continue
				}
				if explanation.Matched() != want {
					t.Fatalf("ExplainMatch(%+v).Matched = %v, want Match = %v", opts, explanation.Matched(), want)
				}
			}

			var after strings.Builder
			if err := vcard.NewEncoder(&after).Encode(card); err != nil {
				t.Fatal(err)
			}
			if before.String() != after.String() {
				t.Fatalf("ExplainMatch mutated the vCard:\nbefore=%s\nafter=%s", before.String(), after.String())
			}
		})
	}
}

// TestMatchExplainNilQuery mirrors the nil-query Match behavior.
func TestMatchExplainNilQuery(t *testing.T) {
	ao := &AddressObject{Card: decodeCard(t, "BEGIN:VCARD\nVERSION:4.0\nEND:VCARD\n")}
	matched, err := Match(nil, ao)
	if err != nil || !matched {
		t.Fatalf("Match(nil) = %v, %v", matched, err)
	}
	explanation, err := ExplainMatch(nil, ao, nil)
	if err != nil || !explanation.Matched() {
		t.Fatalf("ExplainMatch(nil) = %+v, %v", explanation, err)
	}
}

// TestMatchExplainErrors verifies invalid queries produce identical errors.
func TestMatchExplainErrors(t *testing.T) {
	card := decodeCard(t, "BEGIN:VCARD\nVERSION:4.0\nEMAIL:alice@example.com\nEND:VCARD\n")
	ao := &AddressObject{Card: card}

	queries := []*AddressBookQuery{
		{FilterTest: "bogus", PropFilters: []PropFilter{{Name: vcard.FieldEmail}}},
		{PropFilters: []PropFilter{{
			Name:        vcard.FieldEmail,
			Test:        "bogus",
			TextMatches: []TextMatch{{Text: "alice"}},
		}}},
		{PropFilters: []PropFilter{{
			Name:        vcard.FieldEmail,
			TextMatches: []TextMatch{{Text: "alice", MatchType: "bogus"}},
		}}},
	}
	for i, query := range queries {
		_, matchErr := Match(query, ao)
		_, explainErr := ExplainMatch(query, ao, nil)
		if matchErr == nil || explainErr == nil {
			t.Fatalf("case %d: expected error, Match=%v ExplainMatch=%v", i, matchErr, explainErr)
		}
		if matchErr.Error() != explainErr.Error() {
			t.Fatalf("case %d: error mismatch Match=%q ExplainMatch=%q", i, matchErr, explainErr)
		}
	}
}
