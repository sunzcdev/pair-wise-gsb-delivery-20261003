// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package sdp

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSimulcast_Unmarshal(t *testing.T) {
	tests := []struct {
		name  string
		raw   string
		want  *Simulcast
		marsh string
	}{
		{
			name:  "RFC form three layers one paused alternative",
			raw:   "simulcast:RFC/8853 send h;~m,l;q",
			marsh: "simulcast:RFC/8853 send h;~m,l;q",
			want: &Simulcast{
				Version: SimulcastVersion,
				Streams: []SimulcastStream{
					{
						Direction: RIDDirectionSend,
						Layers: []SimulcastLayer{
							{Alternatives: []SimulcastEntry{{ID: "h"}}},
							{Alternatives: []SimulcastEntry{
								{ID: "m", Paused: true},
								{ID: "l"},
							}},
							{Alternatives: []SimulcastEntry{{ID: "q"}}},
						},
					},
				},
			},
		},
		{
			name:  "legacy bare form with space separated layers",
			raw:   "simulcast:send 1 2;3",
			marsh: "simulcast:send 1;2;3",
			want: &Simulcast{
				Streams: []SimulcastStream{
					{
						Direction: RIDDirectionSend,
						Layers: []SimulcastLayer{
							{Alternatives: []SimulcastEntry{{ID: "1"}}},
							{Alternatives: []SimulcastEntry{{ID: "2"}}},
							{Alternatives: []SimulcastEntry{{ID: "3"}}},
						},
					},
				},
			},
		},
		{
			name:  "send and recv streams",
			raw:   "simulcast:RFC/8853 send a;b recv c",
			marsh: "simulcast:RFC/8853 send a;b recv c",
			want: &Simulcast{
				Version: SimulcastVersion,
				Streams: []SimulcastStream{
					{
						Direction: RIDDirectionSend,
						Layers: []SimulcastLayer{
							{Alternatives: []SimulcastEntry{{ID: "a"}}},
							{Alternatives: []SimulcastEntry{{ID: "b"}}},
						},
					},
					{
						Direction: RIDDirectionRecv,
						Layers: []SimulcastLayer{
							{Alternatives: []SimulcastEntry{{ID: "c"}}},
						},
					},
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := new(Simulcast)
			assert.NoError(t, s.Unmarshal(tc.raw))
			assert.Equal(t, tc.want, s)
			assert.Equal(t, tc.marsh, s.Marshal())
			assert.Equal(t, "simulcast", s.Name())
			assert.Equal(t,
				Attribute{Key: "simulcast", Value: strings.TrimPrefix(tc.marsh, "simulcast:")},
				s.Clone(),
			)
		})
	}
}

func TestSimulcast_Unmarshal_Errors(t *testing.T) {
	invalid := []string{
		"",
		"simulcast:",
		"foo:send 1",
		"simulcast:RFC/8853",
		"simulcast:RFC/8853 send",
		"simulcast:RFC/x send 1",
		"simulcast:RFC/ send 1",
		"simulcast:send 1;",
		"simulcast:send 1,~",
		"simulcast:send ~",
		"simulcast:send id@",
	}

	for _, raw := range invalid {
		s := new(Simulcast)
		assert.Errorf(t, s.Unmarshal(raw), "expected error for %q", raw)
	}
}
