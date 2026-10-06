// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package sdp

import "fmt"

// ValidateRIDSimulcast checks the "a=rid" and "a=simulcast" attributes of
// every media description for consistency, as required by RFC 8851 and
// RFC 8853.
//
// It rejects:
//   - duplicate rid identifiers inside a media description,
//   - a simulcast reference to a rid identifier that is not declared, and
//   - a simulcast reference whose direction does not match the referenced rid.
//
// Errors identify the offending media section (by "m=" and "a=mid" when
// available) and the specific attribute.
func (s *SessionDescription) ValidateRIDSimulcast() error {
	for index, media := range s.MediaDescriptions {
		if err := validateMediaRIDSimulcast(index, media); err != nil {
			return err
		}
	}

	return nil
}

func validateMediaRIDSimulcast(index int, media *MediaDescription) error {
	rids, err := media.RIDs()
	if err != nil {
		return fmt.Errorf("media section %d (%s): %w", index, mediaSectionName(media), err)
	}

	ridByID := make(map[string]RID, len(rids))

	for _, rid := range rids {
		if _, exists := ridByID[rid.Identifier]; exists {
			return fmt.Errorf(
				"media section %d (%s): a=%s: %w: %q",
				index, mediaSectionName(media), AttrKeyRID, errRIDDuplicate, rid.Identifier,
			)
		}

		ridByID[rid.Identifier] = rid
	}

	simulcast, err := media.Simulcast()
	if err != nil {
		return fmt.Errorf("media section %d (%s): %w", index, mediaSectionName(media), err)
	}

	if simulcast == nil {
		return nil
	}

	for _, mode := range simulcast.Modes {
		for _, layer := range mode.Layers {
			for _, stream := range layer {
				rid, ok := ridByID[stream.RID]
				if !ok {
					return fmt.Errorf(
						"media section %d (%s): a=%s: references undeclared rid %q",
						index, mediaSectionName(media), AttrKeySimulcast, stream.RID,
					)
				}

				if rid.Direction != mode.Direction {
					return fmt.Errorf(
						"media section %d (%s): a=%s: rid %q has direction %q but is referenced as %q",
						index, mediaSectionName(media), AttrKeySimulcast,
						stream.RID, rid.Direction.String(), mode.Direction.String(),
					)
				}
			}
		}
	}

	return nil
}

func mediaSectionName(media *MediaDescription) string {
	if mid, ok := media.Attribute(AttrKeyMID); ok {
		return "m=" + media.MediaName.Media + " a=mid:" + mid
	}

	return "m=" + media.MediaName.Media
}
