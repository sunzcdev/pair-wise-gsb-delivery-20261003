// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package sdp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRID_Unmarshal_Marshal(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		line  string
		check func(t *testing.T, rid *RID)
	}{
		{
			name: "identifier and direction only",
			line: "rid:low sendonly",
			check: func(t *testing.T, rid *RID) {
				t.Helper()
				assert.Equal(t, "low", rid.Identifier)
				assert.Equal(t, DirectionSendOnly, rid.Direction)
				assert.Nil(t, rid.PayloadTypes)
			},
		},
		{
			name: "receive direction",
			line: "rid:high recvonly",
			check: func(t *testing.T, rid *RID) {
				t.Helper()
				assert.Equal(t, DirectionRecvOnly, rid.Direction)
			},
		},
		{
			name: "full attribute prefix and all restrictions",
			line: "rid:f sendonly pt=96,97;max-br=800000;max-fps=30.5;max-fs=3600;max-bpp=60;" +
				"max-width=1280;max-height=720;scale-resolution=down:2;depend=e,r",
			check: func(t *testing.T, rid *RID) {
				t.Helper()
				assert.Equal(t, []uint8{96, 97}, rid.PayloadTypes)
				require.NotNil(t, rid.MaxBitrate)
				assert.Equal(t, uint32(800000), *rid.MaxBitrate)
				require.NotNil(t, rid.MaxFramerate)
				assert.InDelta(t, float32(30.5), *rid.MaxFramerate, 0.001)
				require.NotNil(t, rid.MaxPixelsPerFrame)
				assert.Equal(t, uint32(3600), *rid.MaxPixelsPerFrame)
				require.NotNil(t, rid.MaxBitsPerPixel)
				assert.InDelta(t, float32(60), *rid.MaxBitsPerPixel, 0.001)
				require.NotNil(t, rid.Width)
				assert.Equal(t, uint32(1280), *rid.Width)
				require.NotNil(t, rid.Height)
				assert.Equal(t, uint32(720), *rid.Height)
				require.NotNil(t, rid.ScaleResolutionDownBy)
				assert.InDelta(t, float32(2), *rid.ScaleResolutionDownBy, 0.001)
				assert.Equal(t, []string{"e", "r"}, rid.Dependencies)
			},
		},
		{
			name: "pt without trailing semicolon",
			line: "rid:foo recvonly pt=100",
			check: func(t *testing.T, rid *RID) {
				t.Helper()
				assert.Equal(t, []uint8{100}, rid.PayloadTypes)
			},
		},
		{
			name: "unknown restriction parameters preserved",
			line: "rid:foo sendonly;max-width=640;x-custom=abc;y-flag=1",
			check: func(t *testing.T, rid *RID) {
				t.Helper()
				require.NotNil(t, rid.Width)
				assert.Equal(t, uint32(640), *rid.Width)
				assert.Equal(t, []RIDParameter{
					{Name: "x-custom", Value: "abc"},
					{Name: "y-flag", Value: "1"},
				}, rid.UnknownParameters)
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rid := &RID{}
			require.NoError(t, rid.Unmarshal(tc.line))
			tc.check(t, rid)

			// Round-trip must reproduce the exact wire value.
			assert.Equal(t, tc.line, rid.Marshal())

			// Parsing the value without the "rid:" prefix must be equivalent.
			value := tc.line[len("rid:"):]
			ridNoPrefix := &RID{}
			require.NoError(t, ridNoPrefix.Unmarshal(value))
			assert.Equal(t, rid, ridNoPrefix)
		})
	}
}

func TestRID_Unmarshal_Errors(t *testing.T) {
	t.Parallel()

	lines := []string{
		"",
		"rid:foo",
		"rid:foo bogus",
		"rid:foo sendrecv",
		"rid:foo inactive",
		"rid:foo sendonly max-br=notanumber",
		"rid:foo sendonly max-fps=abc",
		"rid:foo sendonly max-fs=-1",
		"rid:foo sendonly max-bpp=x",
		"rid:foo sendonly pt=999",
		"rid:foo sendonly pt=abc",
		"rid:foo sendonly scale-resolution=up:2",
		"rid:foo sendonly scale-resolution=down:0",
		"rid:foo sendonly depend=",
		"rid:foo sendonly baretoken",
	}

	for _, line := range lines {
		line := line
		t.Run(line, func(t *testing.T) {
			t.Parallel()

			rid := &RID{}
			assert.Error(t, rid.Unmarshal(line))
		})
	}
}

func TestRID_Clone(t *testing.T) {
	t.Parallel()

	rid := &RID{}
	require.NoError(t, rid.Unmarshal("rid:m sendonly pt=96;max-fps=30"))

	attr := rid.Clone()
	assert.Equal(t, AttrKeyRID, attr.Key)
	assert.Equal(t, "m sendonly pt=96;max-fps=30", attr.Value)
	assert.Equal(t, "rid", rid.Name())
}
