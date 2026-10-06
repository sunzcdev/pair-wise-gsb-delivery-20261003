// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package sdp

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// AttrKeyRID is the attribute key for the RID attribute defined in RFC 8851.
const AttrKeyRID = "rid"

var (
	errRIDInvalidSyntax    = errors.New("rid: invalid syntax")
	errRIDInvalidDirection = errors.New("rid: invalid direction")
	errRIDDuplicate        = errors.New("rid: duplicate identifier")
	errRIDEmptyIdentifier  = errors.New("rid: empty identifier")
	errRIDInvalidBPP       = errors.New("rid: invalid max-bpp value")
	errRIDInvalidFPS       = errors.New("rid: invalid max-fps value")
	errRIDInvalidFS        = errors.New("rid: invalid max-fs value")
	errRIDInvalidBR        = errors.New("rid: invalid max-br value")
	errRIDInvalidPayloadPT = errors.New("rid: invalid payload type in pt")
	errRIDInvalidWidth     = errors.New("rid: invalid max-width value")
	errRIDInvalidHeight    = errors.New("rid: invalid max-height value")
	errRIDInvalidScaleDown = errors.New("rid: invalid scale-resolution value")
	errRIDInvalidDepend    = errors.New("rid: invalid depend value")
)

// RIDParameter is an unknown restriction parameter carried by a RID
// attribute. It is preserved verbatim so that parameters not modelled by this
// library survive a marshal/unmarshal round-trip.
// https://datatracker.ietf.org/doc/html/rfc8851#section-4
type RIDParameter struct {
	Name  string
	Value string
}

// Known RID restriction parameter names (RFC 8851 Section 4).
const (
	ridParamMaxBPP          = "max-bpp"
	ridParamMaxFPS          = "max-fps"
	ridParamMaxFS           = "max-fs"
	ridParamMaxBR           = "max-br"
	ridParamMaxWidth        = "max-width"
	ridParamMaxHeight       = "max-height"
	ridParamScaleResolution = "scale-resolution"
	ridParamDepend          = "depend"
	ridParamPT              = "pt"
	// ridParamUnknown is the placeholder used for programmatically
	// constructed RIDs whose unknown parameters have no recorded order.
	ridParamUnknown = ""
)

// RID represents an RTP Stream Identifier (RID) attribute.
//
//	a=rid:<rid-identifier> <direction> [pt=<fmt-list>;]<restriction-app>
//
// https://datatracker.ietf.org/doc/html/rfc8851#section-4
type RID struct {
	// Identifier is the rid-identifier token.
	Identifier string

	// Direction is either DirectionSendOnly or DirectionRecvOnly.
	Direction Direction

	// PayloadTypes is the list of payload types from the "pt=" parameter.
	// It is nil when "pt=" was not present.
	PayloadTypes []uint8

	// MaxBitrate is the "max-br" restriction in bits per second.
	MaxBitrate *uint32

	// MaxFramerate is the "max-fps" restriction in frames per second.
	MaxFramerate *float32

	// MaxPixelsPerFrame is the "max-fs" restriction in pixels per frame.
	MaxPixelsPerFrame *uint32

	// MaxBitsPerPixel is the "max-bpp" restriction in bits per pixel.
	MaxBitsPerPixel *float32

	// ScaleResolutionDownBy is the factor from "scale-resolution=down:<n>".
	ScaleResolutionDownBy *float32

	// Width is the "max-width" restriction in pixels.
	Width *uint32

	// Height is the "max-height" restriction in pixels.
	Height *uint32

	// Dependencies holds the rid-ids referenced by "depend".
	Dependencies []string

	// UnknownParameters holds restriction parameters that are not modelled
	// here, preserved in their original order.
	UnknownParameters []RIDParameter

	// order records the order of parameters so that Marshal reproduces the
	// original layout. Known parameters are recorded by their canonical
	// name and unknown parameters by RIDParameter.Name.
	order []string
}

// Name returns the constant name of this object.
func (r *RID) Name() string {
	return AttrKeyRID
}

// Clone converts this object to an Attribute.
func (r *RID) Clone() Attribute {
	return Attribute{Key: r.Name(), Value: r.string()}
}

// Marshal creates the full attribute string ("rid:...") from a RID.
func (r *RID) Marshal() string {
	return r.Name() + ":" + r.string()
}

func (r *RID) string() string {
	output := r.Identifier + " " + r.Direction.String()

	unknownIndex := 0

	for _, name := range r.effectiveOrder() {
		switch name {
		case ridParamPT:
			output += " pt=" + r.payloadTypesString()
		case ridParamMaxBR:
			output += ";max-br=" + strconv.FormatUint(uint64(*r.MaxBitrate), 10)
		case ridParamMaxFPS:
			output += ";max-fps=" + formatRIDFloat(float64(*r.MaxFramerate))
		case ridParamMaxFS:
			output += ";max-fs=" + strconv.FormatUint(uint64(*r.MaxPixelsPerFrame), 10)
		case ridParamMaxBPP:
			output += ";max-bpp=" + formatRIDFloat(float64(*r.MaxBitsPerPixel))
		case ridParamMaxWidth:
			output += ";max-width=" + strconv.FormatUint(uint64(*r.Width), 10)
		case ridParamMaxHeight:
			output += ";max-height=" + strconv.FormatUint(uint64(*r.Height), 10)
		case ridParamScaleResolution:
			output += ";scale-resolution=down:" + formatRIDFloat(float64(*r.ScaleResolutionDownBy))
		case ridParamDepend:
			output += ";depend=" + strings.Join(r.Dependencies, ",")
		default:
			p := r.UnknownParameters[unknownIndex]
			unknownIndex++
			output += ";" + p.Name
			if p.Value != "" {
				output += "=" + p.Value
			}
		}
	}

	return output
}

// effectiveOrder returns the parameter order used when marshaling. Parsed
// values reproduce the original order; values constructed in code with no
// recorded order fall back to the canonical RFC 8851 order.
func (r *RID) effectiveOrder() []string {
	if len(r.order) > 0 {
		return r.order
	}

	order := make([]string, 0, 8)

	if r.PayloadTypes != nil {
		order = append(order, ridParamPT)
	}

	if r.MaxBitrate != nil {
		order = append(order, ridParamMaxBR)
	}

	if r.MaxFramerate != nil {
		order = append(order, ridParamMaxFPS)
	}

	if r.MaxPixelsPerFrame != nil {
		order = append(order, ridParamMaxFS)
	}

	if r.MaxBitsPerPixel != nil {
		order = append(order, ridParamMaxBPP)
	}

	if r.Width != nil {
		order = append(order, ridParamMaxWidth)
	}

	if r.Height != nil {
		order = append(order, ridParamMaxHeight)
	}

	if r.ScaleResolutionDownBy != nil {
		order = append(order, ridParamScaleResolution)
	}

	if r.Dependencies != nil {
		order = append(order, ridParamDepend)
	}

	for range r.UnknownParameters {
		order = append(order, ridParamUnknown)
	}

	return order
}

func (r *RID) payloadTypesString() string {
	pts := make([]string, len(r.PayloadTypes))
	for i, pt := range r.PayloadTypes {
		pts[i] = strconv.Itoa(int(pt))
	}

	return strings.Join(pts, ",")
}

func formatRIDFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 32)
}

// Unmarshal creates a RID from the raw attribute value. The value may either
// be the full attribute ("rid:<id> <dir> ...") or the value part after
// "a=rid:".
func (r *RID) Unmarshal(raw string) error {
	value := raw
	if key, rest, ok := strings.Cut(value, ":"); ok {
		if key == AttrKeyRID {
			value = rest
		}
	}

	// Structure:
	//   <identifier> <direction> [SP pt=<fmt-list>] [";" restrictions]
	// or, when there is no pt parameter:
	//   <identifier> <direction> [";" restrictions]
	//
	// Split off the identifier first.
	id, tail, foundID := strings.Cut(value, " ")
	if !foundID || id == "" {
		return fmt.Errorf("%w: %q", errRIDInvalidSyntax, raw)
	}

	r.Identifier = id
	tail = strings.TrimLeft(tail, " \t")

	// Direction ends at the first whitespace (leading to pt) or ';'
	// (leading to restrictions).
	end := len(tail)

	if i := strings.IndexAny(tail, " \t;"); i >= 0 {
		end = i
	}

	direction := tail[:end]

	switch direction {
	case directionSendOnlyStr:
		r.Direction = DirectionSendOnly
	case directionRecvOnlyStr:
		r.Direction = DirectionRecvOnly
	default:
		return fmt.Errorf("%w: %q", errRIDInvalidDirection, direction)
	}

	tail = tail[end:]

	// Consume an optional leading " pt=<fmt-list>".
	tail = strings.TrimLeft(tail, " \t")

	if ptValue, hasPT := strings.CutPrefix(tail, ridParamPT+"="); hasPT {
		// The pt list runs up to the next ';' (restrictions) or end.
		list := ptValue

		switch semi := strings.IndexAny(ptValue, "; \t"); {
		case semi >= 0 && ptValue[semi] == ';':
			list = ptValue[:semi]
			tail = ptValue[semi+1:]
		case semi >= 0:
			// Whitespace may not follow the pt list.
			return fmt.Errorf("%w: %q", errRIDInvalidSyntax, raw)
		default:
			tail = ""
		}

		payloadTypes, err := parseRIDPayloadTypes(strings.TrimSpace(list))
		if err != nil {
			return err
		}

		r.PayloadTypes = payloadTypes
		r.order = append(r.order, ridParamPT)
	} else {
		// Restrictions follow directly after the ';'.
		tail = strings.TrimPrefix(tail, ";")
	}

	for _, item := range strings.Split(tail, ";") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}

		if err := r.parseRestriction(item); err != nil {
			return err
		}
	}

	return nil
}

func (r *RID) parseRestriction(item string) error {
	name, val, hasValue := strings.Cut(item, "=")

	switch name {
	case ridParamPT:
		// A pt restriction is only valid in the leading space-separated
		// position; a second occurrence is a syntax error.
		return fmt.Errorf("%w: %q", errRIDInvalidSyntax, item)
	case ridParamMaxBR:
		v, err := parseRIDUint32(val)
		if err != nil {
			return fmt.Errorf("%w: %q", errRIDInvalidBR, val)
		}

		r.MaxBitrate = &v
	case ridParamMaxFPS:
		f, err := parseRIDFloat32(val)
		if err != nil || f < 0 {
			return fmt.Errorf("%w: %q", errRIDInvalidFPS, val)
		}

		r.MaxFramerate = &f
	case ridParamMaxFS:
		v, err := parseRIDUint32(val)
		if err != nil {
			return fmt.Errorf("%w: %q", errRIDInvalidFS, val)
		}

		r.MaxPixelsPerFrame = &v
	case ridParamMaxBPP:
		f, err := parseRIDFloat32(val)
		if err != nil || f < 0 {
			return fmt.Errorf("%w: %q", errRIDInvalidBPP, val)
		}

		r.MaxBitsPerPixel = &f
	case ridParamMaxWidth:
		v, err := parseRIDUint32(val)
		if err != nil {
			return fmt.Errorf("%w: %q", errRIDInvalidWidth, val)
		}

		r.Width = &v
	case ridParamMaxHeight:
		v, err := parseRIDUint32(val)
		if err != nil {
			return fmt.Errorf("%w: %q", errRIDInvalidHeight, val)
		}

		r.Height = &v
	case ridParamScaleResolution:
		direction, factor, ok := strings.Cut(val, ":")
		if !ok || direction != "down" {
			return fmt.Errorf("%w: %q", errRIDInvalidScaleDown, val)
		}

		f, err := parseRIDFloat32(factor)
		if err != nil || f <= 0 {
			return fmt.Errorf("%w: %q", errRIDInvalidScaleDown, val)
		}

		r.ScaleResolutionDownBy = &f
	case ridParamDepend:
		dependencies := strings.Split(val, ",")
		for _, dep := range dependencies {
			dep = strings.TrimSpace(dep)
			if dep == "" {
				return fmt.Errorf("%w: %q", errRIDInvalidDepend, val)
			}

			r.Dependencies = append(r.Dependencies, dep)
		}
	default:
		if !hasValue {
			// Restrictions are key=value; a bare token is invalid.
			return fmt.Errorf("%w: %q", errRIDInvalidSyntax, item)
		}

		r.UnknownParameters = append(r.UnknownParameters, RIDParameter{
			Name:  name,
			Value: val,
		})
	}

	r.order = append(r.order, name)

	return nil
}

func parseRIDPayloadTypes(list string) ([]uint8, error) {
	if list == "" {
		return nil, fmt.Errorf("%w: %q", errRIDInvalidPayloadPT, list)
	}

	payloadTypes := make([]uint8, 0)

	for _, item := range strings.Split(list, ",") {
		item = strings.TrimSpace(item)
		v, err := strconv.ParseUint(item, 10, 8)
		if err != nil {
			return nil, fmt.Errorf("%w: %q", errRIDInvalidPayloadPT, item)
		}

		payloadTypes = append(payloadTypes, uint8(v))
	}

	return payloadTypes, nil
}

func parseRIDUint32(value string) (uint32, error) {
	v, err := strconv.ParseUint(value, 10, 32)
	if err != nil {
		return 0, err
	}

	return uint32(v), nil
}

func parseRIDFloat32(value string) (float32, error) {
	v, err := strconv.ParseFloat(value, 32)
	if err != nil {
		return 0, err
	}

	return float32(v), nil
}
