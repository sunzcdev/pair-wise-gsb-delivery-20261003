// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package sdp

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// AttrKeyRID is the attribute key for "a=rid" defined by RFC 8851.
const AttrKeyRID = "rid"

var errRIDSyntax = errors.New("sdp: invalid rid attribute")

// RIDDirection is the direction of an RTP stream identifier as defined by
// RFC 8851 and reused by the simulcast attribute of RFC 8853.
type RIDDirection int

const (
	// RIDDirectionSend marks an RTP stream that is sent.
	RIDDirectionSend RIDDirection = iota + 1
	// RIDDirectionRecv marks an RTP stream that is received.
	RIDDirectionRecv
)

const (
	ridDirectionSendStr = "send"
	ridDirectionRecvStr = "recv"
)

// NewRIDDirection converts a raw direction token into an RIDDirection.
func NewRIDDirection(raw string) (RIDDirection, error) {
	switch raw {
	case ridDirectionSendStr:
		return RIDDirectionSend, nil
	case ridDirectionRecvStr:
		return RIDDirectionRecv, nil
	default:
		return 0, fmt.Errorf("%w: invalid direction %q", errRIDSyntax, raw)
	}
}

// String returns the wire representation of the RID direction.
func (d RIDDirection) String() string {
	switch d {
	case RIDDirectionSend:
		return ridDirectionSendStr
	case RIDDirectionRecv:
		return ridDirectionRecvStr
	default:
		return ""
	}
}

// Well-known rid restriction parameter keys defined by RFC 8851.
const (
	RIDRestrictionMaxWidth            = "max-width"
	RIDRestrictionMaxHeight           = "max-height"
	RIDRestrictionMaxFPS              = "max-fps"
	RIDRestrictionMaxFS               = "max-fs"
	RIDRestrictionMaxBR               = "max-br"
	RIDRestrictionMaxPPS              = "max-pps"
	RIDRestrictionMaxDpb              = "max-dpb"
	RIDRestrictionScaling             = "scaling"
	RIDRestrictionCodecPT             = "codec-pt"
	RIDRestrictionMaxPixelAspectRatio = "max-pixel-aspect-ratio"
	RIDRestrictionCodecFramerate      = "codec-framerate"
	RIDRestrictionDepend              = "depend"
	RIDRestrictionRetr                = "retr"
)

var ridIntegerRestrictions = map[string]struct{}{
	RIDRestrictionMaxWidth:  {},
	RIDRestrictionMaxHeight: {},
	RIDRestrictionMaxFPS:    {},
	RIDRestrictionMaxFS:     {},
	RIDRestrictionMaxBR:     {},
	RIDRestrictionMaxPPS:    {},
	RIDRestrictionMaxDpb:    {},
}

// RIDRestriction is a single "rid" restriction parameter (key=value).
// Parameters that are not defined by RFC 8851 are preserved verbatim.
type RIDRestriction struct {
	Key   string
	Value string
}

// RID is the structured representation of an "a=rid" attribute as defined by
// RFC 8851.
//
//	a=rid:<rid-id> <direction> [pt=<fmt-list>;]<restriction-app>
type RID struct {
	ID           string
	Direction    RIDDirection
	PayloadTypes []string
	Restrictions []RIDRestriction
}

// NewRID constructs an RID with the given identifier and direction.
func NewRID(id string, direction RIDDirection) *RID {
	return &RID{ID: id, Direction: direction}
}

// Restriction returns the value of the first restriction parameter named key
// and whether it was present.
func (r *RID) Restriction(key string) (string, bool) {
	for _, p := range r.Restrictions {
		if p.Key == key {
			return p.Value, true
		}
	}

	return "", false
}

// WithPayloadTypes sets the optional pt=<fmt-list> payload types.
func (r *RID) WithPayloadTypes(payloadTypes ...string) *RID {
	r.PayloadTypes = append(r.PayloadTypes, payloadTypes...)

	return r
}

// WithRestriction appends a key=value restriction parameter.
func (r *RID) WithRestriction(key, value string) *RID {
	r.Restrictions = append(r.Restrictions, RIDRestriction{Key: key, Value: value})

	return r
}

// Clone converts this object to an Attribute.
func (r *RID) Clone() Attribute {
	return Attribute{Key: AttrKeyRID, Value: r.string()}
}

// Name returns the constant name of this object.
func (r *RID) Name() string {
	return AttrKeyRID
}

// Marshal creates a string from an RID in the form "rid:<value>".
func (r *RID) Marshal() string {
	return AttrKeyRID + ":" + r.string()
}

func (r *RID) string() string {
	var b strings.Builder
	b.WriteString(r.ID)
	b.WriteByte(' ')
	b.WriteString(r.Direction.String())

	if len(r.PayloadTypes) > 0 {
		b.WriteByte(' ')
		b.WriteString(strings.Join(r.PayloadTypes, ","))
	}

	for _, p := range r.Restrictions {
		b.WriteByte(';')
		b.WriteString(p.Key)

		if p.Value != "" {
			b.WriteByte('=')
			b.WriteString(p.Value)
		}
	}

	return b.String()
}

// String returns the value portion of the attribute (without "rid:").
func (r *RID) String() string {
	return r.string()
}

// Unmarshal populates the RID from either the full attribute ("rid:<value>")
// or from its value portion ("<id> <direction> ...").
func (r *RID) Unmarshal(raw string) error {
	*r = RID{}

	value := raw
	if key, rest, ok := strings.Cut(value, ":"); ok {
		if key != AttrKeyRID {
			return fmt.Errorf("%w: %q", errRIDSyntax, raw)
		}
		value = rest
	}

	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("%w: %q", errRIDSyntax, raw)
	}

	// Semicolons separate the "<id> <direction> [pt-list]" head from the
	// restriction layers per RFC 8851.
	layers := strings.Split(value, ";")
	if strings.HasSuffix(strings.TrimSpace(value), ";") {
		return fmt.Errorf("%w: empty restriction in %q", errRIDSyntax, raw)
	}

	head := strings.Fields(layers[0])
	if len(head) < 2 || !isRIDIdentifier(head[0]) {
		return fmt.Errorf("%w: %q", errRIDSyntax, raw)
	}

	r.ID = head[0]

	dir, err := NewRIDDirection(head[1])
	if err != nil {
		return fmt.Errorf("%w: %q", errRIDSyntax, raw)
	}

	r.Direction = dir

	switch {
	case len(head) == 3:
		if !isRIDCommaList(head[2]) {
			return fmt.Errorf("%w: invalid pt list %q", errRIDSyntax, head[2])
		}

		r.PayloadTypes = strings.Split(head[2], ",")
	case len(head) > 3:
		return fmt.Errorf("%w: %q", errRIDSyntax, raw)
	}

	for _, layer := range layers[1:] {
		layer = strings.TrimSpace(layer)
		if layer == "" {
			continue
		}

		key, val, hasValue := strings.Cut(layer, "=")
		key = strings.TrimSpace(key)

		if key == "" || !isRIDParamKey(key) {
			return fmt.Errorf("%w: invalid restriction %q", errRIDSyntax, layer)
		}

		val = strings.TrimSpace(val)

		_, known := ridKnownRestriction(key)

		if known && !hasValue {
			return fmt.Errorf("%w: restriction %q requires a value", errRIDSyntax, key)
		}

		if hasValue && val == "" {
			return fmt.Errorf("%w: restriction %q has an empty value", errRIDSyntax, key)
		}

		if known {
			if err := validateRIDRestriction(key, val); err != nil {
				return err
			}
		}

		r.Restrictions = append(r.Restrictions, RIDRestriction{Key: key, Value: val})
	}

	return nil
}

func ridKnownRestriction(key string) (struct{}, bool) {
	if _, ok := ridIntegerRestrictions[key]; ok {
		return struct{}{}, true
	}

	switch key {
	case RIDRestrictionScaling,
		RIDRestrictionCodecPT,
		RIDRestrictionMaxPixelAspectRatio,
		RIDRestrictionCodecFramerate,
		RIDRestrictionDepend,
		RIDRestrictionRetr:
		return struct{}{}, true
	}

	return struct{}{}, false
}

func validateRIDRestriction(key, value string) error {
	if _, ok := ridIntegerRestrictions[key]; ok {
		n, err := strconv.ParseUint(value, 10, 64)
		if err != nil || n == 0 {
			return fmt.Errorf(
				"%w: restriction %q expects a positive integer, got %q",
				errRIDSyntax, key, value,
			)
		}
	}

	switch key {
	case RIDRestrictionMaxPixelAspectRatio:
		if _, err := parseRational(value); err != nil {
			return fmt.Errorf("%w: invalid %s %q", errRIDSyntax, key, value)
		}
	case RIDRestrictionCodecFramerate:
		if _, err := strconv.ParseFloat(value, 64); err != nil {
			return fmt.Errorf("%w: invalid %s %q", errRIDSyntax, key, value)
		}
	case RIDRestrictionScaling:
		for _, scale := range strings.Split(value, ",") {
			if _, err := strconv.ParseFloat(strings.TrimSpace(scale), 64); err != nil {
				return fmt.Errorf("%w: invalid %s %q", errRIDSyntax, key, value)
			}
		}
	case RIDRestrictionDepend:
		// depend-rids = "*" / 1*rid-id (a single rid-id per the RFC 8851 ABNF).
		if value != "*" && !isRIDIdentifier(value) {
			return fmt.Errorf("%w: invalid %s %q", errRIDSyntax, key, value)
		}
	case RIDRestrictionCodecPT:
		if !isRIDCommaList(value) {
			return fmt.Errorf("%w: invalid %s %q", errRIDSyntax, key, value)
		}
	case RIDRestrictionRetr:
		if value != "rtp" && value != "rtx" {
			return fmt.Errorf("%w: invalid %s %q", errRIDSyntax, key, value)
		}
	}

	return nil
}

func parseRational(value string) (float64, error) {
	numStr, denStr, hasSlash := strings.Cut(value, ":")

	num, err := strconv.ParseFloat(numStr, 64)
	if err != nil {
		return 0, err
	}

	if !hasSlash {
		return num, nil
	}

	den, err := strconv.ParseFloat(denStr, 64)
	if err != nil || den == 0 {
		return 0, fmt.Errorf("%w: invalid rational %q", errRIDSyntax, value)
	}

	return num / den, nil
}

func isRIDIdentifier(s string) bool {
	if s == "" {
		return false
	}

	for _, r := range s {
		if !isRIDAlphaNum(r) && r != '-' && r != '_' {
			return false
		}
	}

	return true
}

func isRIDCommaList(s string) bool {
	if s == "" {
		return false
	}

	if strings.HasPrefix(s, ",") || strings.HasSuffix(s, ",") || strings.Contains(s, ",,") {
		return false
	}

	for _, r := range s {
		if !isRIDTokenChar(r) && r != ',' {
			return false
		}
	}

	return true
}

func isRIDParamKey(s string) bool {
	for _, r := range s {
		if !isRIDAlphaNum(r) && r != '-' && r != '_' {
			return false
		}
	}

	return s != ""
}

func isRIDAlphaNum(r rune) bool {
	return (r >= 'a' && r <= 'z') ||
		(r >= 'A' && r <= 'Z') ||
		(r >= '0' && r <= '9')
}

func isRIDTokenChar(r rune) bool {
	return isRIDAlphaNum(r) ||
		r == '-' ||
		r == '_' ||
		r == '.' ||
		r == '!' ||
		r == '#' ||
		r == '$' ||
		r == '%' ||
		r == '&' ||
		r == '\'' ||
		r == '*' ||
		r == '+' ||
		r == '`' ||
		r == '~'
}
