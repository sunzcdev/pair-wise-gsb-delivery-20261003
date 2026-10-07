// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package sdp

import (
	"errors"
	"fmt"
	"strings"
)

// AttrKeyICEUfrag is the ICE username fragment attribute.
// https://datatracker.ietf.org/doc/html/rfc5245#section-15.4
const AttrKeyICEUfrag = "ice-ufrag"

// AttrKeyICEPwd is the ICE password attribute.
// https://datatracker.ietf.org/doc/html/rfc5245#section-15.4
const AttrKeyICEPwd = "ice-pwd"

// TrickleICESDPFragMimeType is the MIME type of an incremental trickle ICE
// SDP fragment as defined by RFC 8840. Such a payload only contains one or
// more media sections carrying trickled ICE state (credentials, candidates
// and the end-of-candidates marker) and must not be treated as a complete
// SDP session description.
//
// https://datatracker.ietf.org/doc/html/rfc8840#section-3
const TrickleICESDPFragMimeType = "application/trickle-ice-sdpfrag"

var (
	// ErrTrickleICESDPFragInvalid is returned when a trickle ICE SDP
	// fragment does not contain a parsable media section.
	ErrTrickleICESDPFragInvalid = errors.New("sdp: invalid trickle-ice-sdpfrag")

	// ErrTrickleICESectionMIDMissing is returned when a fragment media
	// section does not carry an "a=mid" attribute. A fragment without a
	// MID cannot be associated with a media description without guessing
	// the m-line index.
	ErrTrickleICESectionMIDMissing = errors.New("sdp: trickle fragment section is missing a mid")

	// ErrTrickleICEMIDNotFound is returned when no media description of
	// the target session description carries the MID of a fragment.
	ErrTrickleICEMIDNotFound = errors.New("sdp: no media description with the requested mid")
)

// TrickleICESection is the trickled ICE state of a single media section of
// an application/trickle-ice-sdpfrag message. The media section is always
// identified by its MID, never by an m-line index.
type TrickleICESection struct {
	// MID is the value of the section "a=mid" attribute.
	MID string

	// Ufrag is the value of "a=ice-ufrag" when present.
	Ufrag string

	// Pwd is the value of "a=ice-pwd" when present.
	Pwd string

	// Candidates holds the values of every "a=candidate" attribute in
	// their original order.
	Candidates []string

	// EndOfCandidates is true when the section carries the
	// "a=end-of-candidates" property attribute.
	EndOfCandidates bool
}

// HasCredentials reports whether the section carries ICE credentials.
func (t TrickleICESection) HasCredentials() bool {
	return t.Ufrag != "" || t.Pwd != ""
}

// TrickleICESDPFrag is a parsed application/trickle-ice-sdpfrag message.
// A single message may contain several media sections.
type TrickleICESDPFrag struct {
	Sections []TrickleICESection
}

// UnmarshalTrickleICESDPFrag parses an application/trickle-ice-sdpfrag
// payload. Unlike a complete session description a fragment is a sequence
// of media sections ("m=" and media-level "a=" lines) and is not parsed by
// the stateful RFC 4566 unmarshaler.
//
// Each section must carry an "a=mid" attribute: the MID is the only
// permitted way to associate the fragment with a media description, the
// m-line index is never inferred.
func UnmarshalTrickleICESDPFrag(value string) (*TrickleICESDPFrag, error) {
	frag := &TrickleICESDPFrag{}

	var section *TrickleICESection
	flush := func() error {
		if section == nil {
			return nil
		}
		if section.MID == "" {
			return fmt.Errorf("%w (section %d)", ErrTrickleICESectionMIDMissing, len(frag.Sections))
		}
		frag.Sections = append(frag.Sections, *section)

		return nil
	}

	for _, rawLine := range strings.Split(value, "\n") {
		line := strings.TrimRight(rawLine, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}

		switch {
		case strings.HasPrefix(line, "m="):
			if err := flush(); err != nil {
				return nil, err
			}
			section = &TrickleICESection{}
		case strings.HasPrefix(line, "a="):
			if section == nil {
				// Session-level attributes have no media section to be
				// associated with, so the fragment cannot be applied.
				return nil, fmt.Errorf("%w: media-level attribute outside of a media section", ErrTrickleICESDPFragInvalid)
			}

			key, val := splitAttribute(line[len("a="):])
			switch key {
			case AttrKeyMID:
				section.MID = val
			case AttrKeyICEUfrag:
				section.Ufrag = val
			case AttrKeyICEPwd:
				section.Pwd = val
			case AttrKeyCandidate:
				section.Candidates = append(section.Candidates, val)
			case AttrKeyEndOfCandidates:
				section.EndOfCandidates = true
			}
		default:
			return nil, fmt.Errorf("%w: unsupported fragment line %q", ErrTrickleICESDPFragInvalid, line)
		}
	}

	if err := flush(); err != nil {
		return nil, err
	}

	if len(frag.Sections) == 0 {
		return nil, fmt.Errorf("%w: no media section", ErrTrickleICESDPFragInvalid)
	}

	return frag, nil
}

// trickleTrack records the ICE ufrag generations already observed for a MID
// so that late fragments of an earlier generation can be rejected.
type trickleTrack struct {
	ufrags []string
}

// TrickleICEMerger applies a stream of application/trickle-ice-sdpfrag
// messages to session descriptions while remembering the ICE generations
// (identified by ice-ufrag) already seen for each MID.
//
// The zero value is ready to use. A TrickleICEMerger must not be copied
// after first use.
type TrickleICEMerger struct {
	tracks map[string]*trickleTrack
}

// NewTrickleICEMerger returns a ready to use TrickleICEMerger.
func NewTrickleICEMerger() *TrickleICEMerger {
	return &TrickleICEMerger{tracks: map[string]*trickleTrack{}}
}

// Merge parses the fragment and applies every section to a copy of desc.
// It returns the new session description; desc is never modified, even when
// an error is returned.
//
// Credentials differing from the current generation start a new candidate
// set. Fragments that carry credentials of an earlier generation observed
// by this merger, candidates arriving after end-of-candidates and duplicate
// candidates are handled according to RFC 8840.
func (m *TrickleICEMerger) Merge(desc *SessionDescription, value string) (*SessionDescription, error) {
	frag, err := UnmarshalTrickleICESDPFrag(value)
	if err != nil {
		return nil, err
	}

	if m.tracks == nil {
		m.tracks = map[string]*trickleTrack{}
	}

	merged := desc.Clone()

	for _, section := range frag.Sections {
		track := m.tracks[section.MID]
		if err := mergeTrickleICESection(merged, section, track); err != nil {
			return nil, err
		}
	}

	// Remember every ufrag generation seen, including the generation
	// superseded by a restart. Bookkeeping is committed only after every
	// section applied successfully.
	for _, section := range frag.Sections {
		track, ok := m.tracks[section.MID]
		if !ok {
			track = &trickleTrack{}
			m.tracks[section.MID] = track
		}
		media := findMediaDescriptionByMID(merged, section.MID)
		if currentUfrag, ok := media.Attribute(AttrKeyICEUfrag); ok {
			track.ufrags = appendUnique(track.ufrags, currentUfrag)
		}
	}

	return merged, nil
}

// MergeTrickleICESDPFrag parses the application/trickle-ice-sdpfrag payload
// and merges every section into a copy of the session description, which is
// returned untouched on error.
//
// Sections are located by their MID. For each section:
//   - New ice-ufrag/ice-pwd values start a fresh ICE candidate set: the
//     previous credentials and candidates are replaced and the
//     end-of-candidates state is reset.
//   - Candidates are appended and de-duplicated against existing values.
//   - Candidates arriving after end-of-candidates are rejected as late.
//   - end-of-candidates marks the current candidate set complete.
//
// This is a stateless one-shot helper. When fragments of several ICE
// generations are streamed in, use a TrickleICEMerger so that late
// fragments carrying credentials of an earlier generation are rejected.
//
// The original session description is never modified, even on failure.
func (s *SessionDescription) MergeTrickleICESDPFrag(value string) (*SessionDescription, error) {
	frag, err := UnmarshalTrickleICESDPFrag(value)
	if err != nil {
		return nil, err
	}

	merged := s.Clone()

	for _, section := range frag.Sections {
		if err := mergeTrickleICESection(merged, section, nil); err != nil {
			return nil, err
		}
	}

	return merged, nil
}

func mergeTrickleICESection(desc *SessionDescription, section TrickleICESection, track *trickleTrack) error {
	media := findMediaDescriptionByMID(desc, section.MID)
	if media == nil {
		return fmt.Errorf("%w %q (available: %s)", ErrTrickleICEMIDNotFound, section.MID, availableMIDs(desc))
	}

	currentUfrag, _ := media.Attribute(AttrKeyICEUfrag)
	currentPwd, _ := media.Attribute(AttrKeyICEPwd)

	restart, err := classifyTrickleCredentials(section, currentUfrag, currentPwd, track)
	if err != nil {
		return err
	}

	ended := hasPropertyAttribute(media.Attributes, AttrKeyEndOfCandidates)
	if !restart && ended && len(section.Candidates) > 0 {
		return fmt.Errorf("sdp: late trickle candidates for mid %q after end-of-candidates", section.MID)
	}

	mergedAttrs := make([]Attribute, 0, len(media.Attributes)+len(section.Candidates)+1)

	for _, attr := range media.Attributes {
		switch attr.Key {
		case AttrKeyCandidate, AttrKeyEndOfCandidates:
			continue
		case AttrKeyICEUfrag:
			if restart && section.Ufrag != "" {
				continue
			}
		case AttrKeyICEPwd:
			if restart && section.Pwd != "" {
				continue
			}
		}
		mergedAttrs = append(mergedAttrs, attr)
	}

	if restart {
		if section.Ufrag != "" {
			mergedAttrs = append(mergedAttrs, NewAttribute(AttrKeyICEUfrag, section.Ufrag))
		}
		if section.Pwd != "" {
			mergedAttrs = append(mergedAttrs, NewAttribute(AttrKeyICEPwd, section.Pwd))
		}
	}

	seen := make(map[string]struct{}, len(mergedAttrs)+len(section.Candidates))
	for _, attr := range mergedAttrs {
		if attr.Key == AttrKeyCandidate {
			seen[attr.Value] = struct{}{}
		}
	}

	for _, candidate := range section.Candidates {
		if _, ok := seen[candidate]; ok {
			continue
		}
		seen[candidate] = struct{}{}
		mergedAttrs = append(mergedAttrs, NewAttribute(AttrKeyCandidate, candidate))
	}

	markEnd := section.EndOfCandidates || (!restart && ended)
	if markEnd {
		mergedAttrs = append(mergedAttrs, NewPropertyAttribute(AttrKeyEndOfCandidates))
	}

	media.Attributes = mergedAttrs

	return nil
}

func findMediaDescriptionByMID(desc *SessionDescription, mid string) *MediaDescription {
	for _, media := range desc.MediaDescriptions {
		if value, ok := media.Attribute(AttrKeyMID); ok && value == mid {
			return media
		}
	}

	return nil
}

// classifyTrickleCredentials decides whether a fragment continues the current
// ICE generation or starts a new one (ICE restart).
//
// A section without credentials continues the current generation. When the
// section carries credentials, ice-ufrag and ice-pwd must change together.
// If the pair matches an earlier generation remembered by track, the
// fragment is stale and is rejected.
func classifyTrickleCredentials(
	section TrickleICESection,
	currentUfrag, currentPwd string,
	track *trickleTrack,
) (bool, error) {
	if !section.HasCredentials() {
		return false, nil
	}

	ufragChanged := section.Ufrag != currentUfrag
	pwdChanged := section.Pwd != currentPwd

	switch {
	case ufragChanged != pwdChanged:
		return false, fmt.Errorf(
			"sdp: invalid trickle fragment for mid %q: ice-ufrag and ice-pwd must change together",
			section.MID,
		)
	case !ufragChanged:
		// Credentials identical to the current generation.
		return false, nil
	}

	if track != nil && contains(track.ufrags, section.Ufrag) {
		return false, fmt.Errorf(
			"sdp: stale trickle fragment for mid %q: ice-ufrag %q belongs to an earlier generation",
			section.MID,
			section.Ufrag,
		)
	}

	return true, nil
}

func availableMIDs(desc *SessionDescription) string {
	mids := make([]string, 0, len(desc.MediaDescriptions))
	for _, media := range desc.MediaDescriptions {
		if value, ok := media.Attribute(AttrKeyMID); ok {
			mids = append(mids, value)
		}
	}

	return strings.Join(mids, ", ")
}

func hasPropertyAttribute(attributes []Attribute, key string) bool {
	for _, attr := range attributes {
		if attr.Key == key {
			return true
		}
	}

	return false
}

func splitAttribute(value string) (key, val string) {
	if idx := strings.IndexRune(value, ':'); idx >= 0 {
		return value[:idx], value[idx+1:]
	}

	return value, ""
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}

	return false
}

func appendUnique(list []string, value string) []string {
	if contains(list, value) {
		return list
	}

	return append(list, value)
}

// Clone returns a deep copy of the session description. It is used by the
// trickle ICE merge helpers so that a failed merge never mutates the
// description passed in.
func (s *SessionDescription) Clone() *SessionDescription {
	if s == nil {
		return nil
	}

	clone := *s

	if s.SessionInformation != nil {
		info := *s.SessionInformation
		clone.SessionInformation = &info
	}
	if s.URI != nil {
		uri := *s.URI
		clone.URI = &uri
	}
	if s.EmailAddress != nil {
		email := *s.EmailAddress
		clone.EmailAddress = &email
	}
	if s.PhoneNumber != nil {
		phone := *s.PhoneNumber
		clone.PhoneNumber = &phone
	}
	if s.ConnectionInformation != nil {
		clone.ConnectionInformation = cloneConnectionInformation(s.ConnectionInformation)
	}

	clone.Bandwidth = cloneBandwidths(s.Bandwidth)
	clone.TimeDescriptions = cloneTimeDescriptions(s.TimeDescriptions)

	if s.TimeZones != nil {
		clone.TimeZones = append([]TimeZone(nil), s.TimeZones...)
	}
	if s.EncryptionKey != nil {
		key := *s.EncryptionKey
		clone.EncryptionKey = &key
	}

	clone.Attributes = cloneAttributes(s.Attributes)

	if s.MediaDescriptions != nil {
		clone.MediaDescriptions = make([]*MediaDescription, len(s.MediaDescriptions))
		for i, media := range s.MediaDescriptions {
			clone.MediaDescriptions[i] = cloneMediaDescription(media)
		}
	}

	return &clone
}

func cloneAttributes(attributes []Attribute) []Attribute {
	if attributes == nil {
		return nil
	}

	return append([]Attribute(nil), attributes...)
}

func cloneBandwidths(bandwidth []Bandwidth) []Bandwidth {
	if bandwidth == nil {
		return nil
	}

	return append([]Bandwidth(nil), bandwidth...)
}

func cloneTimeDescriptions(times []TimeDescription) []TimeDescription {
	if times == nil {
		return nil
	}

	clone := make([]TimeDescription, len(times))
	for i, td := range times {
		clone[i] = td
		if td.RepeatTimes != nil {
			clone[i].RepeatTimes = make([]RepeatTime, len(td.RepeatTimes))
			for j, rt := range td.RepeatTimes {
				clone[i].RepeatTimes[j] = rt
				if rt.Offsets != nil {
					clone[i].RepeatTimes[j].Offsets = append([]int64(nil), rt.Offsets...)
				}
			}
		}
	}

	return clone
}

func cloneConnectionInformation(c *ConnectionInformation) *ConnectionInformation {
	clone := &ConnectionInformation{
		NetworkType: c.NetworkType,
		AddressType: c.AddressType,
	}
	if c.Address != nil {
		address := *c.Address
		clone.Address = &address
	}

	return clone
}

func cloneMediaDescription(media *MediaDescription) *MediaDescription {
	clone := *media

	if media.MediaTitle != nil {
		title := *media.MediaTitle
		clone.MediaTitle = &title
	}
	if media.ConnectionInformation != nil {
		clone.ConnectionInformation = cloneConnectionInformation(media.ConnectionInformation)
	}

	clone.Bandwidth = cloneBandwidths(media.Bandwidth)

	if media.EncryptionKey != nil {
		key := *media.EncryptionKey
		clone.EncryptionKey = &key
	}

	clone.Attributes = cloneAttributes(media.Attributes)

	return &clone
}
