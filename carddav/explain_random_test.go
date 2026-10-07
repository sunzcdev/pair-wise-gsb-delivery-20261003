package carddav

import (
	"math/rand"
	"testing"

	"github.com/emersion/go-vcard"
)

// TestMatchExplainRandomCrossCheck builds random queries and cards and asserts
// ExplainMatch's root conclusion never diverges from Match's.
func TestMatchExplainRandomCrossCheck(t *testing.T) {
	values := []string{"", "alice", "bob", "alice@example.com", "example.com", "WORK", "home"}
	matchTypes := []MatchType{MatchEquals, MatchContains, MatchStartsWith, MatchEndsWith}
	tests := []FilterTest{FilterAnyOf, FilterAllOf}

	rng := rand.New(rand.NewSource(42))
	for iter := 0; iter < 2000; iter++ {
		card := make(vcard.Card)
		card[vcard.FieldVersion] = []*vcard.Field{{Value: "4.0"}}

		numProps := rng.Intn(3)
		var propNames []string
		for i := 0; i < numProps; i++ {
			name := []string{vcard.FieldEmail, vcard.FieldNote, vcard.FieldTelephone}[rng.Intn(3)]
			propNames = append(propNames, name)
			numInstances := rng.Intn(3)
			for j := 0; j < numInstances; j++ {
				field := &vcard.Field{Value: values[rng.Intn(len(values))], Params: vcard.Params{}}
				if rng.Intn(2) == 0 {
					field.Params["TYPE"] = []string{values[rng.Intn(len(values))]}
				}
				card[name] = append(card[name], field)
			}
		}

		query := &AddressBookQuery{FilterTest: tests[rng.Intn(2)]}
		numFilters := rng.Intn(3)
		for i := 0; i < numFilters; i++ {
			pf := PropFilter{
				Name:         []string{vcard.FieldEmail, vcard.FieldNote, vcard.FieldTelephone}[rng.Intn(3)],
				Test:         tests[rng.Intn(2)],
				IsNotDefined: rng.Intn(4) == 0,
			}
			if !pf.IsNotDefined {
				numTexts := rng.Intn(3)
				for j := 0; j < numTexts; j++ {
					pf.TextMatches = append(pf.TextMatches, TextMatch{
						Text:            values[rng.Intn(len(values))],
						MatchType:       matchTypes[rng.Intn(len(matchTypes))],
						NegateCondition: rng.Intn(2) == 0,
					})
				}
				if rng.Intn(2) == 0 {
					pf.Params = append(pf.Params, ParamFilter{
						Name:         "TYPE",
						IsNotDefined: rng.Intn(3) == 0,
						TextMatch: &TextMatch{
							Text:      values[rng.Intn(len(values))],
							MatchType: matchTypes[rng.Intn(len(matchTypes))],
						},
					})
				}
			}
			query.PropFilters = append(query.PropFilters, pf)
		}

		ao := &AddressObject{Card: card}
		want, err := Match(query, ao)
		explanation, explainErr := ExplainMatch(query, ao, nil)
		if (err != nil) != (explainErr != nil) {
			t.Fatalf("iter %d: error mismatch Match=%v Explain=%v query=%+v", iter, err, explainErr, query)
		}
		if err == nil && explanation.Matched() != want {
			t.Fatalf("iter %d: ExplainMatch=%v Match=%v query=%+v", iter, explanation.Matched(), want, query)
		}
	}
}
