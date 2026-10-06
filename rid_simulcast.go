// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package sdp

import (
	"fmt"
)

// RIDs parses every "a=rid" attribute of the media description into structured
// RID objects. The raw attributes are left untouched, so the generic Attribute
// interface keeps working.
func (d *MediaDescription) RIDs() ([]RID, error) {
	rids := make([]RID, 0)

	for _, a := range d.Attributes {
		if a.Key != AttrKeyRID {
			continue
		}

		rid := RID{}
		if err := rid.Unmarshal(a.Value); err != nil {
			return nil, fmt.Errorf("a=%s: %w", AttrKeyRID, err)
		}

		rids = append(rids, rid)
	}

	return rids, nil
}

// Simulcast parses the "a=simulcast" attribute of the media description. It
// returns nil when the attribute is not present.
func (d *MediaDescription) Simulcast() (*Simulcast, error) {
	var value string

	found := false

	for _, a := range d.Attributes {
		if a.Key == AttrKeySimulcast {
			value = a.Value
			found = true

			break
		}
	}

	if !found {
		return nil, nil //nolint:nilnil
	}

	simulcast := &Simulcast{}
	if err := simulcast.Unmarshal(value); err != nil {
		return nil, fmt.Errorf("a=%s: %w", AttrKeySimulcast, err)
	}

	return simulcast, nil
}

// WithRID appends a structured RID attribute to the media description.
func (d *MediaDescription) WithRID(rid RID) *MediaDescription {
	d.Attributes = append(d.Attributes, rid.Clone())

	return d
}

// WithSimulcast appends a structured simulcast attribute to the media
// description.
func (d *MediaDescription) WithSimulcast(simulcast Simulcast) *MediaDescription {
	d.Attributes = append(d.Attributes, simulcast.Clone())

	return d
}
