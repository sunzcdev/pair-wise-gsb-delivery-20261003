// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package sdp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSimulcast_Unmarshal_Marshal(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		line  string
		check func(t *testing.T, s *Simulcast)
	}{
		{
			name: "three send layers",
			line: "simulcast:send l;m;h",
			check: func(t *testing.T, s *Simulcast) {
				t.Helper()

				send, ok := s.Send()
				require.True(t, ok)
				assert.Equal(t, DirectionSendOnly, send.Direction)
				require.Len(t, send.Layers, 3)
				assert.Equal(t, "l", send.Layers[0][0].RID)
				assert.False(t, send.Layers[0][0].Paused)
				assert.Equal(t, "h", send.Layers[2][0].RID)

				_, ok = s.Recv()
				assert.False(t, ok)
			},
		},
		{
			name: "alternative streams in one layer with one paused",
			line: "simulcast:send l;m2,~m1;h",
			check: func(t *testing.T, s *Simulcast) {
				t.Helper()

				send, ok := s.Send()
				require.True(t, ok)
				require.Len(t, send.Layers, 3)

				mid := send.Layers[1]
				require.Len(t, mid, 2)
				assert.Equal(t, SimulcastStream{RID: "m2", Paused: false}, mid[0])
				assert.Equal(t, SimulcastStream{RID: "m1", Paused: true}, mid[1])
			},
		},
		{
			name: "send and recv groups",
			line: "simulcast:send s1;s2 recv r1;r2",
			check: func(t *testing.T, s *Simulcast) {
				t.Helper()

				send, ok := s.Send()
				require.True(t, ok)
				assert.Len(t, send.Layers, 2)

				recv, ok := s.Recv()
				require.True(t, ok)
				assert.Equal(t, DirectionRecvOnly, recv.Direction)
				assert.Len(t, recv.Layers, 2)
				assert.Equal(t, "r2", recv.Layers[1][0].RID)
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := &Simulcast{}
			require.NoError(t, s.Unmarshal(tc.line))
			tc.check(t, s)

			assert.Equal(t, tc.line, s.Marshal())

			value := tc.line[len("simulcast:"):]
			noPrefix := &Simulcast{}
			require.NoError(t, noPrefix.Unmarshal(value))
			assert.Equal(t, s, noPrefix)
		})
	}
}

func TestSimulcast_Unmarshal_Errors(t *testing.T) {
	t.Parallel()

	lines := []string{
		"",
		"simulcast:",
		"simulcast:send",
		"simulcast:bogus l;m",
		"simulcast:send l;;m",
		"simulcast:send l,",
		"simulcast:send ~",
		"other:send l",
	}

	for _, line := range lines {
		line := line
		t.Run(line, func(t *testing.T) {
			t.Parallel()

			s := &Simulcast{}
			assert.Error(t, s.Unmarshal(line))
		})
	}
}

func TestSimulcast_Clone(t *testing.T) {
	t.Parallel()

	s := &Simulcast{}
	require.NoError(t, s.Unmarshal("simulcast:send l;m,~m2"))

	attr := s.Clone()
	assert.Equal(t, AttrKeySimulcast, attr.Key)
	assert.Equal(t, "send l;m,~m2", attr.Value)
	assert.Equal(t, "simulcast", s.Name())
}
