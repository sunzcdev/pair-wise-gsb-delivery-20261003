// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package sdp

import (
	"errors"
	"fmt"
	"strings"
)

// AttrKeySimulcast is the attribute key for the simulcast attribute defined
// in RFC 8853.
const AttrKeySimulcast = "simulcast"

var (
	errSimulcastInvalidSyntax = errors.New("simulcast: invalid syntax")
	errSimulcastEmptyLayer    = errors.New("simulcast: empty layer")
	errSimulcastEmptyChoice   = errors.New("simulcast: empty alternative")
	errSimulcastPaused        = errors.New("simulcast: invalid pause marker")
)

const (
	simulcastSendToken = "send"
	simulcastRecvToken = "recv"
)

// SimulcastStream is a single RID reference inside a simulcast layer. When
// Paused is true the reference is prefixed with '~', meaning the layer is not
// currently being sent or received.
// https://datatracker.ietf.org/doc/html/rfc8853#section-4
type SimulcastStream struct {
	// RID is the referenced rid-identifier.
	RID string

	// Paused reports whether the reference is prefixed with '~'.
	Paused bool
}

// SimulcastMode is one direction group of a simulcast attribute (the "send" or
// "recv" block). Layers are separated by ';' and alternatives within a layer
// by ','.
// https://datatracker.ietf.org/doc/html/rfc8853#section-4
type SimulcastMode struct {
	// Direction is DirectionSendOnly for "send" or DirectionRecvOnly for
	// "recv".
	Direction Direction

	// Layers is the ordered list of simulcast layers. Each layer contains one
	// or more alternative streams.
	Layers [][]SimulcastStream
}

// Simulcast represents a simulcast attribute.
//
//	a=simulcast:<send|recv> rid-id[,...,rid-id] [;... ] [ <send|recv> ...]
//
// https://datatracker.ietf.org/doc/html/rfc8853#section-4
type Simulcast struct {
	// Modes is the ordered list of direction groups. RFC 8853 allows at most
	// one send and one recv group.
	Modes []SimulcastMode
}

// Name returns the constant name of this object.
func (s *Simulcast) Name() string {
	return AttrKeySimulcast
}

// Clone converts this object to an Attribute.
func (s *Simulcast) Clone() Attribute {
	return Attribute{Key: s.Name(), Value: s.string()}
}

// Marshal creates the full attribute string ("simulcast:...") from a
// Simulcast.
func (s *Simulcast) Marshal() string {
	return s.Name() + ":" + s.string()
}

// Send returns the send mode block and whether it was present.
func (s *Simulcast) Send() (SimulcastMode, bool) {
	return s.mode(DirectionSendOnly)
}

// Recv returns the recv mode block and whether it was present.
func (s *Simulcast) Recv() (SimulcastMode, bool) {
	return s.mode(DirectionRecvOnly)
}

func (s *Simulcast) mode(direction Direction) (SimulcastMode, bool) {
	for i := range s.Modes {
		if s.Modes[i].Direction == direction {
			return s.Modes[i], true
		}
	}

	return SimulcastMode{}, false
}

func (s *Simulcast) string() string {
	blocks := make([]string, 0, len(s.Modes))

	for _, mode := range s.Modes {
		blocks = append(blocks, mode.string())
	}

	return strings.Join(blocks, " ")
}

func (m SimulcastMode) string() string {
	layers := make([]string, 0, len(m.Layers))

	for _, alternatives := range m.Layers {
		choices := make([]string, 0, len(alternatives))
		for _, stream := range alternatives {
			choice := stream.RID
			if stream.Paused {
				choice = "~" + choice
			}

			choices = append(choices, choice)
		}

		layers = append(layers, strings.Join(choices, ","))
	}

	return m.token() + " " + strings.Join(layers, ";")
}

func (m SimulcastMode) token() string {
	switch m.Direction {
	case DirectionSendOnly:
		return simulcastSendToken
	case DirectionRecvOnly:
		return simulcastRecvToken
	default:
		return ""
	}
}

// Unmarshal creates a Simulcast from the raw attribute value. The value may
// either be the full attribute ("simulcast:send ...") or the value part after
// "a=simulcast:".
func (s *Simulcast) Unmarshal(raw string) error {
	value := raw
	if key, rest, ok := strings.Cut(value, ":"); ok {
		if key == AttrKeySimulcast {
			value = rest
		}
	}

	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("%w: %q", errSimulcastInvalidSyntax, raw)
	}

	// Parse "<send|recv> <layers>[ <send|recv> <layers>...]". The layer list is
	// ';' delimited with no whitespace, so scanning token by token keeps each
	// direction group together.
	tokens := strings.Fields(value)
	for i := 0; i < len(tokens); {
		mode := SimulcastMode{}

		switch tokens[i] {
		case simulcastSendToken:
			mode.Direction = DirectionSendOnly
		case simulcastRecvToken:
			mode.Direction = DirectionRecvOnly
		default:
			return fmt.Errorf("%w: %q", errSimulcastInvalidSyntax, tokens[i])
		}

		i++
		if i >= len(tokens) {
			return fmt.Errorf("%w: %q", errSimulcastInvalidSyntax, raw)
		}

		layerList := tokens[i]
		i++

		for _, layer := range strings.Split(layerList, ";") {
			if layer == "" {
				return fmt.Errorf("%w: %q", errSimulcastEmptyLayer, layerList)
			}

			alternatives := []SimulcastStream{}

			for _, choice := range strings.Split(layer, ",") {
				stream, err := parseSimulcastStream(choice)
				if err != nil {
					return err
				}

				alternatives = append(alternatives, stream)
			}

			mode.Layers = append(mode.Layers, alternatives)
		}

		s.Modes = append(s.Modes, mode)
	}

	if len(s.Modes) == 0 {
		return fmt.Errorf("%w: %q", errSimulcastInvalidSyntax, raw)
	}

	return nil
}

func parseSimulcastStream(token string) (SimulcastStream, error) {
	if token == "" {
		return SimulcastStream{}, fmt.Errorf("%w: %q", errSimulcastEmptyChoice, token)
	}

	stream := SimulcastStream{RID: token}

	if token[0] == '~' {
		if len(token) == 1 {
			return SimulcastStream{}, fmt.Errorf("%w: %q", errSimulcastPaused, token)
		}

		stream.Paused = true
		stream.RID = token[1:]
	}

	return stream, nil
}
