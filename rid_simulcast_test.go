// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package sdp

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// threeLayerSendSDP is a video sender offering three simulcast layers. The
// high layer has two alternative encodings, one of which is paused. It also
// carries an unknown rid restriction parameter that must survive a round
// trip.
const threeLayerSendSDP = "v=0\r\n" +
	"o=- 4611731400430051336 2 IN IP4 127.0.0.1\r\n" +
	"s=-\r\n" +
	"t=0 0\r\n" +
	"m=video 9 UDP/TLS/RTP/SAVPF 96\r\n" +
	"c=IN IP4 0.0.0.0\r\n" +
	"a=mid:video\r\n" +
	"a=sendonly\r\n" +
	"a=rid:low sendonly pt=96;max-fps=15;x-tier=base\r\n" +
	"a=rid:mid sendonly pt=96;max-fps=30\r\n" +
	"a=rid:high sendonly pt=96;max-fps=60\r\n" +
	"a=rid:high-backup sendonly pt=96;max-fps=60\r\n" +
	"a=simulcast:send low;mid;high,~high-backup\r\n"

func TestRIDSimulcast_RoundTripSample(t *testing.T) {
	t.Parallel()

	sd := &SessionDescription{}
	require.NoError(t, sd.UnmarshalString(threeLayerSendSDP))
	require.NoError(t, sd.ValidateRIDSimulcast())

	media := sd.MediaDescriptions[0]

	rids, err := media.RIDs()
	require.NoError(t, err)
	require.Len(t, rids, 4)

	byID := map[string]RID{}
	for _, rid := range rids {
		byID[rid.Identifier] = rid
		assert.Equal(t, DirectionSendOnly, rid.Direction)
	}

	// Unknown restriction parameters are preserved.
	assert.Equal(t, []RIDParameter{{Name: "x-tier", Value: "base"}},
		byID["low"].UnknownParameters)

	simulcast, err := media.Simulcast()
	require.NoError(t, err)
	require.NotNil(t, simulcast)

	send, ok := simulcast.Send()
	require.True(t, ok)
	require.Len(t, send.Layers, 3)

	// The third layer has two alternatives, one of which is paused.
	high := send.Layers[2]
	require.Len(t, high, 2)
	assert.Equal(t, SimulcastStream{RID: "high", Paused: false}, high[0])
	assert.Equal(t, SimulcastStream{RID: "high-backup", Paused: true}, high[1])

	// Generic Attribute interface still carries the raw value verbatim.
	raw, ok := media.Attribute(AttrKeySimulcast)
	require.True(t, ok)
	assert.Equal(t, "send low;mid;high,~high-backup", raw)

	// The whole description round-trips byte-for-byte.
	out, err := sd.Marshal()
	require.NoError(t, err)
	assert.Equal(t, threeLayerSendSDP, string(out))
}

func TestRIDSimulcast_ModifyOneLayer(t *testing.T) {
	t.Parallel()

	parse := func(t *testing.T) *SessionDescription {
		t.Helper()

		sd := &SessionDescription{}
		require.NoError(t, sd.UnmarshalString(threeLayerSendSDP))

		return sd
	}

	original := parse(t)

	// Change only the middle layer's max-fps, then re-serialize.
	modified := parse(t)
	media := modified.MediaDescriptions[0]

	for i, attr := range media.Attributes {
		if attr.Key != AttrKeyRID {
			continue
		}

		rid := RID{}
		require.NoError(t, rid.Unmarshal(attr.Value))

		if rid.Identifier == "mid" {
			require.NotNil(t, rid.MaxFramerate)
			newFPS := float32(45)
			rid.MaxFramerate = &newFPS
			media.Attributes[i] = rid.Clone()
		}
	}

	originalOut, err := original.Marshal()
	require.NoError(t, err)

	modifiedOut, err := modified.Marshal()
	require.NoError(t, err)

	originalLines := strings.Split(string(originalOut), "\r\n")
	modifiedLines := strings.Split(string(modifiedOut), "\r\n")
	require.Len(t, modifiedLines, len(originalLines))

	for i := range originalLines {
		switch {
		case strings.HasPrefix(originalLines[i], "a=rid:mid "):
			assert.Equal(t, "a=rid:mid sendonly pt=96;max-fps=45", modifiedLines[i])
		default:
			// Every other line, including the other rid attributes and the
			// simulcast attribute, keeps its exact meaning/text.
			assert.Equalf(t, originalLines[i], modifiedLines[i], "line %d changed", i)
		}
	}

	// Modification stays consistent.
	require.NoError(t, modified.ValidateRIDSimulcast())
}

func TestValidateRIDSimulcast_Errors(t *testing.T) {
	t.Parallel()

	t.Run("duplicate RID", func(t *testing.T) {
		t.Parallel()

		sdp := strings.ReplaceAll(threeLayerSendSDP,
			"a=rid:high-backup sendonly",
			"a=rid:high sendonly")

		sd := &SessionDescription{}
		require.NoError(t, sd.UnmarshalString(sdp))

		err := sd.ValidateRIDSimulcast()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "a=rid")
		assert.Contains(t, err.Error(), "a=mid:video")
		assert.ErrorIs(t, err, errRIDDuplicate)
	})

	t.Run("reference to undeclared RID", func(t *testing.T) {
		t.Parallel()

		sdp := strings.ReplaceAll(threeLayerSendSDP,
			"high,~high-backup", "high,~ghost")

		sd := &SessionDescription{}
		require.NoError(t, sd.UnmarshalString(sdp))

		err := sd.ValidateRIDSimulcast()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "a=simulcast")
		assert.Contains(t, err.Error(), `"ghost"`)
	})

	t.Run("direction mismatch", func(t *testing.T) {
		t.Parallel()

		sdp := strings.ReplaceAll(threeLayerSendSDP,
			"a=rid:high-backup sendonly",
			"a=rid:high-backup recvonly")

		sd := &SessionDescription{}
		require.NoError(t, sd.UnmarshalString(sdp))

		err := sd.ValidateRIDSimulcast()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "a=simulcast")
		assert.Contains(t, err.Error(), "high-backup")
		assert.Contains(t, err.Error(), "recvonly")
	})

	t.Run("malformed RID attribute", func(t *testing.T) {
		t.Parallel()

		sdp := strings.ReplaceAll(threeLayerSendSDP,
			"a=rid:low sendonly pt=96;max-fps=15;x-tier=base",
			"a=rid:low notadirection")

		sd := &SessionDescription{}
		require.NoError(t, sd.UnmarshalString(sdp))

		err := sd.ValidateRIDSimulcast()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "a=rid")
	})
}

func TestMediaDescription_WithRIDAndSimulcast(t *testing.T) {
	t.Parallel()

	bitrate := uint32(500000)

	media := NewJSEPMediaDescription("video", nil)
	media.WithRID(RID{
		Identifier: "a",
		Direction:  DirectionSendOnly,
		MaxBitrate: &bitrate,
	}).WithSimulcast(Simulcast{
		Modes: []SimulcastMode{{
			Direction: DirectionSendOnly,
			Layers: [][]SimulcastStream{
				{{RID: "a"}},
			},
		}},
	})

	rids, err := media.RIDs()
	require.NoError(t, err)
	require.Len(t, rids, 1)

	simulcast, err := media.Simulcast()
	require.NoError(t, err)
	require.NotNil(t, simulcast)

	attr, ok := media.Attribute(AttrKeyRID)
	require.True(t, ok)
	assert.Equal(t, "a sendonly;max-br=500000", attr)

	_, ok = media.Attribute(AttrKeySimulcast)
	assert.True(t, ok)
}

func TestMediaDescription_SimulcastMissing(t *testing.T) {
	t.Parallel()

	media := NewJSEPMediaDescription("audio", nil)

	simulcast, err := media.Simulcast()
	require.NoError(t, err)
	assert.Nil(t, simulcast)

	rids, err := media.RIDs()
	require.NoError(t, err)
	assert.Empty(t, rids)
}
