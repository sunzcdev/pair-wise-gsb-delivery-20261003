package carddav

import (
	"github.com/emersion/go-vcard"
)

func filterProperties(req AddressDataRequest, ao AddressObject) AddressObject {
	if req.AllProp || len(req.Props) == 0 {
		return ao
	}

	if len(ao.Card) == 0 {
		panic("request to process empty vCard")
	}

	result := AddressObject{
		Path:    ao.Path,
		ModTime: ao.ModTime,
		ETag:    ao.ETag,
	}

	result.Card = make(vcard.Card)
	// result would be invalid w/o version
	result.Card[vcard.FieldVersion] = ao.Card[vcard.FieldVersion]
	for _, prop := range req.Props {
		value, ok := ao.Card[prop]
		if ok {
			result.Card[prop] = value
		}
	}

	return result
}

// Filter returns the filtered list of address objects matching the provided query.
// A nil query will return the full list of address objects.
func Filter(query *AddressBookQuery, aos []AddressObject) ([]AddressObject, error) {
	if query == nil {
		// FIXME: should we always return a copy of the provided slice?
		return aos, nil
	}

	n := query.Limit
	if n <= 0 || n > len(aos) {
		n = len(aos)
	}
	out := make([]AddressObject, 0, n)
	for _, ao := range aos {
		ok, err := Match(query, &ao)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}

		out = append(out, filterProperties(query.DataRequest, ao))
		if len(out) >= n {
			break
		}
	}
	return out, nil
}

// Match reports whether the provided AddressObject matches the query.
//
// Match is a thin wrapper around ExplainMatch: both derive their verdict from
// the same read-only evaluation, so the root conclusion returned here is
// always identical to Explanation.Matched returned by ExplainMatch.
func Match(query *AddressBookQuery, ao *AddressObject) (bool, error) {
	explanation, err := ExplainMatch(query, ao, nil)
	if err != nil {
		return false, err
	}
	return explanation.Matched(), nil
}
