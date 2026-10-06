// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package sdp

import (
	"fmt"
	"strings"
)

// GetRIDs parses every "a=rid" attribute of the media description into its
// structured representation. Unknown attributes are left untouched.
func (d *MediaDescription) GetRIDs() ([]*RID, error) {
	rids := []*RID{}

	for _, a := range d.Attributes {
		if a.Key != AttrKeyRID {
			continue
		}

		rid := new(RID)
		if err := rid.Unmarshal(a.Value); err != nil {
			return nil, fmt.Errorf("a=rid: %w", err)
		}

		rids = append(rids, rid)
	}

	return rids, nil
}

// GetSimulcast parses every "a=simulcast" attribute of the media description.
func (d *MediaDescription) GetSimulcast() ([]*Simulcast, error) {
	simulcasts := []*Simulcast{}

	for _, a := range d.Attributes {
		if a.Key != AttrKeySimulcast {
			continue
		}

		simulcast := new(Simulcast)
		if err := simulcast.Unmarshal(a.Value); err != nil {
			return nil, fmt.Errorf("a=simulcast: %w", err)
		}

		simulcasts = append(simulcasts, simulcast)
	}

	return simulcasts, nil
}

// WithRID appends a structured "a=rid" attribute.
func (d *MediaDescription) WithRID(rid *RID) *MediaDescription {
	return d.WithValueAttribute(AttrKeyRID, rid.String())
}

// WithSimulcast appends a structured "a=simulcast" attribute.
func (d *MediaDescription) WithSimulcast(simulcast *Simulcast) *MediaDescription {
	return d.WithValueAttribute(AttrKeySimulcast, simulcast.String())
}

// ValidateRIDSimulcast validates the "a=rid" and "a=simulcast" attributes of
// the media description against RFC 8851 and RFC 8853.
//
// It rejects duplicate rid identifiers, simulcast references to rid
// identifiers that are not declared, and simulcast directions that do not
// match the direction of the referenced rid.
func (d *MediaDescription) ValidateRIDSimulcast() error {
	rids, err := d.GetRIDs()
	if err != nil {
		return err
	}

	simulcasts, err := d.GetSimulcast()
	if err != nil {
		return err
	}

	ridByID := make(map[string]*RID, len(rids))

	for _, rid := range rids {
		if _, exists := ridByID[rid.ID]; exists {
			return fmt.Errorf("a=rid: duplicate rid identifier %q in media section %q", rid.ID, d.mediaSection())
		}

		ridByID[rid.ID] = rid
	}

	for _, rid := range rids {
		if err := validateRIDDependencies(rid, ridByID, d.mediaSection()); err != nil {
			return err
		}
	}

	for _, simulcast := range simulcasts {
		seen := map[string]struct{}{}

		for _, stream := range simulcast.Streams {
			for _, layer := range stream.Layers {
				for _, alt := range layer.Alternatives {
					if _, dup := seen[alt.ID]; dup {
						return fmt.Errorf(
							"a=simulcast: duplicate rid %q in media section %q",
							alt.ID, d.mediaSection(),
						)
					}

					seen[alt.ID] = struct{}{}

					rid, ok := ridByID[alt.ID]
					if !ok {
						return fmt.Errorf(
							"a=simulcast: references undeclared rid %q in media section %q",
							alt.ID, d.mediaSection(),
						)
					}

					if rid.Direction != stream.Direction {
						return fmt.Errorf(
							"a=simulcast: rid %q is %s but referenced as %s in media section %q",
							rid.ID, rid.Direction, stream.Direction, d.mediaSection(),
						)
					}
				}
			}
		}
	}

	return nil
}

// ValidateRIDSimulcast validates the "a=rid" and "a=simulcast" attributes of
// every media description in the session.
func (s *SessionDescription) ValidateRIDSimulcast() error {
	for _, md := range s.MediaDescriptions {
		if err := md.ValidateRIDSimulcast(); err != nil {
			return err
		}
	}

	return nil
}

func validateRIDDependencies(rid *RID, ridByID map[string]*RID, section string) error {
	depend, ok := rid.Restriction(RIDRestrictionDepend)
	if !ok {
		return nil
	}

	dep := strings.TrimSpace(depend)
	if dep == "" || dep == "*" {
		return nil
	}

	target, exists := ridByID[dep]
	if !exists {
		return fmt.Errorf(
			"a=rid: rid %q depends on undeclared rid %q in media section %q",
			rid.ID, dep, section,
		)
	}

	if target.Direction != rid.Direction {
		return fmt.Errorf(
			"a=rid: rid %q is %s but depends on %s rid %q in media section %q",
			rid.ID, rid.Direction, target.Direction, dep, section,
		)
	}

	return nil
}

func (d *MediaDescription) mediaSection() string {
	if mid, ok := d.Attribute(AttrKeyMID); ok {
		return "m=" + d.MediaName.Media + " mid=" + mid
	}

	return "m=" + d.MediaName.Media
}
