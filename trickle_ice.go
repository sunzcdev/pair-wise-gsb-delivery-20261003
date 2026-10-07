// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package sdp

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

var (
	ErrTrickleICEInvalidFragment = errors.New("sdp: invalid trickle-ice-sdpfrag")
	ErrTrickleICEStaleFragment   = errors.New("sdp: stale trickle-ice-sdpfrag")
	ErrTrickleICELateCandidate   = errors.New("sdp: late trickle ICE candidate after end-of-candidates")
)

// MimeTypeTrickleICESDPFragment is the MIME type for trickle ICE SDP fragments.
const MimeTypeTrickleICESDPFragment = "application/trickle-ice-sdpfrag"

// TrickleICEMissingContextError reports that a fragment could not be merged
// because required context was not available.
type TrickleICEMissingContextError struct {
	MID             string
	MissingMID      bool
	MissingMedia    bool
	MissingUfrag    bool
	MissingPassword bool
}

func (e TrickleICEMissingContextError) Error() string {
	switch {
	case e.MissingMID:
		return "sdp: trickle-ice-sdpfrag requires a mid attribute"
	case e.MissingMedia:
		return fmt.Sprintf("sdp: no media description found for mid %q", e.MID)
	case e.MissingUfrag || e.MissingPassword:
		missing := make([]string, 0, 2)
		if e.MissingUfrag {
			missing = append(missing, AttrKeyICEUfrag)
		}
		if e.MissingPassword {
			missing = append(missing, AttrKeyICEPassword)
		}

		return fmt.Sprintf(
			"sdp: trickle-ice-sdpfrag for mid %q requires %s in the fragment or description",
			e.MID,
			strings.Join(missing, " and "),
		)
	default:
		return "sdp: insufficient context to merge trickle-ice-sdpfrag"
	}
}

// TrickleICECredentials identifies an ICE candidate generation.
type TrickleICECredentials struct {
	Ufrag    string
	Password string
}

func (c TrickleICECredentials) complete() bool {
	return c.Ufrag != "" && c.Password != ""
}

// TrickleICESDPFragment is a parsed application/trickle-ice-sdpfrag body. It
// contains one media section identified by its mid attribute.
type TrickleICESDPFragment struct {
	MID             string
	Credentials     *TrickleICECredentials
	Candidates      []string
	EndOfCandidates bool
}

// UnmarshalString parses an application/trickle-ice-sdpfrag body.
func (f *TrickleICESDPFragment) UnmarshalString(value string) error {
	*f = TrickleICESDPFragment{}

	var (
		ufrag          string
		password       string
		midSet         bool
		credSet        bool
		sessionMode    bool
		inMediaSection bool
	)

	for _, raw := range splitSDPLines(value) {
		line := raw
		if line == "" {
			continue
		}
		var key, val string
		if strings.HasPrefix(line, "*:") {
			key = "*"
			val = line[2:]
		} else {
			if len(line) < 2 || line[1] != '=' {
				return fmt.Errorf("%w: %q", ErrTrickleICEInvalidFragment, raw)
			}
			key = line[:1]
			val = line[2:]
		}
		switch key {
		case "v", "o", "s", "t":
			if inMediaSection || midSet || credSet {
				return fmt.Errorf("%w: session-level line after media section: %q", ErrTrickleICEInvalidFragment, raw)
			}
			sessionMode = true
			continue
		case "m":
			if inMediaSection || midSet || credSet {
				return fmt.Errorf("%w: multiple media sections", ErrTrickleICEInvalidFragment)
			}
			inMediaSection = true
			sessionMode = false

			continue
		case "*":
			if sessionMode || inMediaSection || midSet {
				return fmt.Errorf("%w: invalid compact mid line %q", ErrTrickleICEInvalidFragment, raw)
			}
			mid, marker, hasMarker := strings.Cut(val, " ")
			if mid == "" || strings.ContainsRune(mid, ':') {
				return fmt.Errorf("%w: invalid compact mid line %q", ErrTrickleICEInvalidFragment, raw)
			}
			if hasMarker && marker != AttrKeyEndOfCandidates {
				return fmt.Errorf("%w: invalid compact mid line %q", ErrTrickleICEInvalidFragment, raw)
			}
			f.MID = mid
			midSet = true
			inMediaSection = true
			if hasMarker {
				f.EndOfCandidates = true
			}

			continue
		case "a":
			// Attribute lines are handled below.
		default:
			return fmt.Errorf("%w: unsupported line %q", ErrTrickleICEInvalidFragment, raw)
		}

		attrKey, attrVal, hasValue := strings.Cut(val, ":")
		if sessionMode {
			return fmt.Errorf("%w: session-level attribute %q", ErrTrickleICEInvalidFragment, raw)
		}

		switch attrKey {
		case AttrKeyMID:
			if !hasValue || attrVal == "" || midSet {
				return fmt.Errorf("%w: invalid mid attribute", ErrTrickleICEInvalidFragment)
			}
			f.MID = attrVal
			midSet = true
		case AttrKeyICEUfrag:
			if !hasValue || attrVal == "" || ufrag != "" {
				return fmt.Errorf("%w: invalid ice-ufrag attribute", ErrTrickleICEInvalidFragment)
			}
			ufrag = attrVal
			credSet = true
		case AttrKeyICEPassword:
			if !hasValue || attrVal == "" || password != "" {
				return fmt.Errorf("%w: invalid ice-pwd attribute", ErrTrickleICEInvalidFragment)
			}
			password = attrVal
			credSet = true
		case AttrKeyCandidate:
			if !hasValue || attrVal == "" {
				return fmt.Errorf("%w: empty candidate attribute", ErrTrickleICEInvalidFragment)
			}
			f.Candidates = append(f.Candidates, attrVal)
		case AttrKeyEndOfCandidates:
			if hasValue {
				return fmt.Errorf("%w: end-of-candidates must be a property attribute", ErrTrickleICEInvalidFragment)
			}
			f.EndOfCandidates = true
		default:
			return fmt.Errorf("%w: unsupported attribute %q", ErrTrickleICEInvalidFragment, attrKey)
		}
	}

	if !midSet {
		return TrickleICEMissingContextError{MissingMID: true}
	}
	if credSet {
		if ufrag == "" || password == "" {
			return TrickleICEMissingContextError{
				MID:             f.MID,
				MissingUfrag:    ufrag == "",
				MissingPassword: password == "",
			}
		}
		f.Credentials = &TrickleICECredentials{Ufrag: ufrag, Password: password}
	}

	return nil
}

// Unmarshal parses an application/trickle-ice-sdpfrag body.
func (f *TrickleICESDPFragment) Unmarshal(value []byte) error {
	return f.UnmarshalString(string(value))
}

// NewTrickleICESDPFragment parses an application/trickle-ice-sdpfrag body.
func NewTrickleICESDPFragment(value string) (*TrickleICESDPFragment, error) {
	f := new(TrickleICESDPFragment)
	if err := f.UnmarshalString(value); err != nil {
		return nil, err
	}

	return f, nil
}

// TrickleICEFragmentMerger merges trickle ICE fragments into a session
// description while remembering accepted ICE credential generations so late
// fragments from a previous generation cannot be applied.
type TrickleICEFragmentMerger struct {
	sd          *SessionDescription
	generations map[string]map[TrickleICECredentials]struct{}
}

// NewTrickleICEFragmentMerger constructs a merger for a description. The
// supplied description is cloned and is never modified by the merger.
func NewTrickleICEFragmentMerger(sd *SessionDescription) *TrickleICEFragmentMerger {
	merger := &TrickleICEFragmentMerger{
		sd:          cloneSessionDescription(sd),
		generations: map[string]map[TrickleICECredentials]struct{}{},
	}
	for _, media := range merger.sd.MediaDescriptions {
		mid, ok := media.Attribute(AttrKeyMID)
		if !ok {
			continue
		}
		credentials := credentialsFromMedia(media)
		if credentials.complete() {
			rememberGeneration(merger.generations, mid, credentials)
		}
	}

	return merger
}

// SessionDescription returns a copy with media attributes isolated from the
// merger's state and from the description supplied to the constructor.
func (m *TrickleICEFragmentMerger) SessionDescription() *SessionDescription {
	return cloneSessionDescription(m.sd)
}

// MergeString parses and merges one application/trickle-ice-sdpfrag body.
func (m *TrickleICEFragmentMerger) MergeString(value string) (*SessionDescription, error) {
	f := new(TrickleICESDPFragment)
	if err := f.UnmarshalString(value); err != nil {
		return nil, err
	}

	return m.Merge(f)
}

// Merge merges one parsed fragment.
func (m *TrickleICEFragmentMerger) Merge(f *TrickleICESDPFragment) (*SessionDescription, error) {
	if f == nil {
		return nil, fmt.Errorf("%w: nil fragment", ErrTrickleICEInvalidFragment)
	}
	if f.MID == "" {
		return nil, TrickleICEMissingContextError{MissingMID: true}
	}

	target := findMediaByMID(m.sd, f.MID)
	if target == nil {
		return nil, TrickleICEMissingContextError{MID: f.MID, MissingMedia: true}
	}

	current := credentialsFromMedia(target)
	incoming := current
	if f.Credentials != nil {
		incoming = *f.Credentials
	}

	switch {
	case incoming == current && current.complete():
	case incoming.complete() && !seenGeneration(m.generations, f.MID, incoming):
		// New generation.
	default:
		if !incoming.complete() {
			return nil, TrickleICEMissingContextError{
				MID:             f.MID,
				MissingUfrag:    incoming.Ufrag == "",
				MissingPassword: incoming.Password == "",
			}
		}

		return nil, fmt.Errorf("%w: mid %q", ErrTrickleICEStaleFragment, f.MID)
	}

	next := cloneSessionDescription(m.sd)
	nextTarget := findMediaByMID(next, f.MID)

	restart := f.Credentials != nil && *f.Credentials != current
	var acceptedGeneration TrickleICECredentials
	if restart {
		nextTarget.Attributes = removeAttributes(nextTarget.Attributes,
			AttrKeyCandidate,
			AttrKeyEndOfCandidates,
			AttrKeyICEUfrag,
			AttrKeyICEPassword,
		)
		nextTarget.Attributes = append(nextTarget.Attributes,
			NewAttribute(AttrKeyICEUfrag, incoming.Ufrag),
			NewAttribute(AttrKeyICEPassword, incoming.Password),
		)
		acceptedGeneration = incoming
	}

	existing := candidateSet(nextTarget.Attributes)
	newCandidates := make([]string, 0, len(f.Candidates))
	for _, candidate := range f.Candidates {
		if _, ok := existing[candidate]; ok {
			continue
		}
		existing[candidate] = struct{}{}
		newCandidates = append(newCandidates, candidate)
	}

	if hasAttribute(nextTarget.Attributes, AttrKeyEndOfCandidates) && len(newCandidates) > 0 {
		return nil, fmt.Errorf("%w: mid %q", ErrTrickleICELateCandidate, f.MID)
	}

	for _, candidate := range newCandidates {
		nextTarget.Attributes = append(nextTarget.Attributes, NewAttribute(AttrKeyCandidate, candidate))
	}

	if f.EndOfCandidates && !hasAttribute(nextTarget.Attributes, AttrKeyEndOfCandidates) {
		nextTarget.Attributes = append(nextTarget.Attributes, NewPropertyAttribute(AttrKeyEndOfCandidates))
	}

	if restart {
		rememberGeneration(m.generations, f.MID, acceptedGeneration)
	}
	m.sd = next

	return cloneSessionDescription(next), nil
}

// MergeTrickleICESDPFragment parses and merges a fragment into a copy of sd.
// Use TrickleICEFragmentMerger for a stream that can contain ICE restarts.
func MergeTrickleICESDPFragment(sd *SessionDescription, value string) (*SessionDescription, error) {
	fragment, err := NewTrickleICESDPFragment(value)
	if err != nil {
		return nil, err
	}

	target := findMediaByMID(sd, fragment.MID)
	if target == nil {
		return nil, TrickleICEMissingContextError{MID: fragment.MID, MissingMedia: true}
	}

	current := credentialsFromMedia(target)
	incoming := current
	if fragment.Credentials != nil {
		incoming = *fragment.Credentials
	}
	if !incoming.complete() {
		return nil, TrickleICEMissingContextError{
			MID:             fragment.MID,
			MissingUfrag:    incoming.Ufrag == "",
			MissingPassword: incoming.Password == "",
		}
	}
	if fragment.Credentials != nil && current.complete() && *fragment.Credentials != current {
		return nil, fmt.Errorf("%w: mid %q", ErrTrickleICEStaleFragment, fragment.MID)
	}

	next := cloneSessionDescription(sd)
	nextTarget := findMediaByMID(next, fragment.MID)

	if !current.complete() {
		nextTarget.Attributes = append(nextTarget.Attributes,
			NewAttribute(AttrKeyICEUfrag, incoming.Ufrag),
			NewAttribute(AttrKeyICEPassword, incoming.Password),
		)
	}

	existing := candidateSet(nextTarget.Attributes)
	newCandidates := make([]string, 0, len(fragment.Candidates))
	for _, candidate := range fragment.Candidates {
		if _, ok := existing[candidate]; ok {
			continue
		}
		existing[candidate] = struct{}{}
		newCandidates = append(newCandidates, candidate)
	}
	if hasAttribute(nextTarget.Attributes, AttrKeyEndOfCandidates) && len(newCandidates) > 0 {
		return nil, fmt.Errorf("%w: mid %q", ErrTrickleICELateCandidate, fragment.MID)
	}
	for _, candidate := range newCandidates {
		nextTarget.Attributes = append(nextTarget.Attributes, NewAttribute(AttrKeyCandidate, candidate))
	}

	if fragment.EndOfCandidates {
		nextTarget.Attributes = append(nextTarget.Attributes, NewPropertyAttribute(AttrKeyEndOfCandidates))
	}

	return next, nil
}

func findMediaByMID(sd *SessionDescription, mid string) *MediaDescription {
	if sd == nil {
		return nil
	}
	for _, media := range sd.MediaDescriptions {
		if value, ok := media.Attribute(AttrKeyMID); ok && value == mid {
			return media
		}
	}

	return nil
}

func credentialsFromMedia(media *MediaDescription) TrickleICECredentials {
	credentials := TrickleICECredentials{}
	if ufrag, ok := media.Attribute(AttrKeyICEUfrag); ok {
		credentials.Ufrag = ufrag
	}
	if password, ok := media.Attribute(AttrKeyICEPassword); ok {
		credentials.Password = password
	}

	return credentials
}

func candidateSet(attributes []Attribute) map[string]struct{} {
	candidates := make(map[string]struct{})
	for _, attribute := range attributes {
		if attribute.Key == AttrKeyCandidate {
			candidates[attribute.Value] = struct{}{}
		}
	}

	return candidates
}

func hasAttribute(attributes []Attribute, key string) bool {
	for _, attribute := range attributes {
		if attribute.Key == key {
			return true
		}
	}

	return false
}

func removeAttributes(attributes []Attribute, keys ...string) []Attribute {
	filtered := make([]Attribute, 0, len(attributes))
	for _, attribute := range attributes {
		if !slices.Contains(keys, attribute.Key) {
			filtered = append(filtered, attribute)
		}
	}

	return filtered
}

func seenGeneration(generations map[string]map[TrickleICECredentials]struct{}, mid string, credentials TrickleICECredentials) bool {
	byMID, ok := generations[mid]
	if !ok {
		return false
	}
	_, ok = byMID[credentials]

	return ok
}

func rememberGeneration(generations map[string]map[TrickleICECredentials]struct{}, mid string, credentials TrickleICECredentials) {
	byMID, ok := generations[mid]
	if !ok {
		byMID = map[TrickleICECredentials]struct{}{}
		generations[mid] = byMID
	}
	byMID[credentials] = struct{}{}
}

func splitSDPLines(value string) []string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")

	return strings.Split(value, "\n")
}

func cloneSessionDescription(in *SessionDescription) *SessionDescription {
	if in == nil {
		return &SessionDescription{}
	}

	out := *in
	out.Attributes = slices.Clone(in.Attributes)
	out.MediaDescriptions = make([]*MediaDescription, len(in.MediaDescriptions))
	for i, inMedia := range in.MediaDescriptions {
		if inMedia == nil {
			continue
		}

		media := *inMedia
		media.Attributes = slices.Clone(inMedia.Attributes)
		out.MediaDescriptions[i] = &media
	}

	return &out
}
