// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package sdp

import (
	"errors"
	"fmt"
	"strings"
)

// AttrKeySimulcast is the attribute key for "a=simulcast" defined by RFC 8853.
const AttrKeySimulcast = "simulcast"

var errSimulcastSyntax = errors.New("sdp: invalid simulcast attribute")

// SimulcastVersion is the version token emitted for RFC 8853 simulcast.
const SimulcastVersion = "RFC/8853"

// SimulcastEntry is a single alternative RTP stream inside a simulcast layer.
// Paused marks an entry prefixed with "~", meaning the stream is paused per
// RFC 8853 section 4.
type SimulcastEntry struct {
	ID     string
	Paused bool
}

// SimulcastLayer is one simulcast layer. Alternatives are the comma-separated
// RTP streams that may be used for the layer.
type SimulcastLayer struct {
	Alternatives []SimulcastEntry
}

// SimulcastStream groups the semicolon-separated layers for one direction.
type SimulcastStream struct {
	Direction RIDDirection
	Layers    []SimulcastLayer
}

// Simulcast is the structured representation of an "a=simulcast" attribute as
// defined by RFC 8853.
//
//	a=simulcast:<version> <send|recv> <simulcast-stream>
type Simulcast struct {
	Version string
	Streams []SimulcastStream
}

// Clone converts this object to an Attribute.
func (s *Simulcast) Clone() Attribute {
	return Attribute{Key: AttrKeySimulcast, Value: s.string()}
}

// Name returns the constant name of this object.
func (s *Simulcast) Name() string {
	return AttrKeySimulcast
}

// Marshal creates a string from a Simulcast in the form "simulcast:<value>".
func (s *Simulcast) Marshal() string {
	return AttrKeySimulcast + ":" + s.string()
}

func (s *Simulcast) string() string {
	var b strings.Builder

	if s.Version != "" {
		b.WriteString(s.Version)
		b.WriteByte(' ')
	}

	for streamIdx, stream := range s.Streams {
		if streamIdx > 0 {
			b.WriteByte(' ')
		}

		b.WriteString(stream.Direction.String())

		for layerIdx, layer := range stream.Layers {
			if layerIdx > 0 {
				b.WriteByte(';')
			} else {
				b.WriteByte(' ')
			}

			for altIdx, alt := range layer.Alternatives {
				if altIdx > 0 {
					b.WriteByte(',')
				}

				if alt.Paused {
					b.WriteByte('~')
				}

				b.WriteString(alt.ID)
			}
		}
	}

	return b.String()
}

// String returns the value portion of the attribute (without "simulcast:").
func (s *Simulcast) String() string {
	return s.string()
}

// Unmarshal populates the Simulcast from either the full attribute
// ("simulcast:<value>") or from its value portion.
//
// Layers are separated by ";" and alternatives by ",". Both the RFC 8853
// form ("RFC/8853 send ...") and the legacy bare form ("send ...") are
// accepted.
func (s *Simulcast) Unmarshal(raw string) error {
	*s = Simulcast{}

	value := raw
	if key, rest, ok := strings.Cut(value, ":"); ok {
		if key != AttrKeySimulcast {
			return fmt.Errorf("%w: %q", errSimulcastSyntax, raw)
		}
		value = rest
	}

	fields := strings.Fields(value)
	if len(fields) == 0 {
		return fmt.Errorf("%w: %q", errSimulcastSyntax, raw)
	}

	idx := 0

	if isSimulcastVersion(fields[0]) {
		s.Version = fields[0]
		idx++
	}

	if idx >= len(fields) {
		return fmt.Errorf("%w: missing direction in %q", errSimulcastSyntax, raw)
	}

	for idx < len(fields) {
		dir, err := NewRIDDirection(fields[idx])
		if err != nil {
			return fmt.Errorf("%w: %q", errSimulcastSyntax, raw)
		}

		stream := SimulcastStream{Direction: dir}
		idx++

		// Collect layer text up to the next bare direction token.
		altList := ""
		for idx < len(fields) && !isSimulcastDirectionToken(fields[idx]) {
			if altList != "" {
				altList += " "
			}

			altList += fields[idx]
			idx++
		}

		if altList == "" {
			return fmt.Errorf("%w: %s stream without layers in %q", errSimulcastSyntax, dir, raw)
		}

		// Layers are separated by ";" per RFC 8853; the legacy draft used
		// spaces, so accept both.
		if strings.Contains(altList, ";;") ||
			strings.HasSuffix(strings.TrimSpace(altList), ";") ||
			strings.HasPrefix(strings.TrimSpace(altList), ";") {
			return fmt.Errorf("%w: empty layer in %q", errSimulcastSyntax, raw)
		}

		layerTokens := strings.FieldsFunc(altList, func(r rune) bool {
			return r == ';' || r == ' '
		})

		for _, layerToken := range layerTokens {
			layer, err := parseSimulcastLayer(layerToken)
			if err != nil {
				return err
			}

			stream.Layers = append(stream.Layers, layer)
		}

		s.Streams = append(s.Streams, stream)
	}

	return nil
}

func parseSimulcastLayer(token string) (SimulcastLayer, error) {
	layer := SimulcastLayer{}

	token = strings.TrimSpace(token)
	if token == "" {
		return layer, fmt.Errorf("%w: empty layer", errSimulcastSyntax)
	}

	for _, rawAlt := range strings.Split(token, ",") {
		entry := SimulcastEntry{}
		id := strings.TrimSpace(rawAlt)

		if strings.HasPrefix(id, "~") {
			entry.Paused = true
			id = strings.TrimSpace(id[1:])
		}

		if id == "" || !isRIDIdentifier(id) {
			return layer, fmt.Errorf("%w: invalid alt-layer-id %q", errSimulcastSyntax, rawAlt)
		}

		entry.ID = id
		layer.Alternatives = append(layer.Alternatives, entry)
	}

	if len(layer.Alternatives) == 0 {
		return layer, fmt.Errorf("%w: empty layer %q", errSimulcastSyntax, token)
	}

	return layer, nil
}

func isSimulcastVersion(field string) bool {
	const prefix = "RFC/"
	if !strings.HasPrefix(field, prefix) {
		return false
	}

	digits := field[len(prefix):]
	if digits == "" {
		return false
	}

	for _, r := range digits {
		if r < '0' || r > '9' {
			return false
		}
	}

	return true
}

func isSimulcastDirectionToken(field string) bool {
	_, err := NewRIDDirection(field)

	return err == nil
}
