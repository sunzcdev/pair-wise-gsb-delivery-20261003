// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package sdp

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// Three sending layers; the middle layer offers two alternative RIDs with the
// "m" stream paused. Also carries an unknown rid restriction that must survive
// a marshal/unmarshal round trip.
const simulcastRoundTripSDP = "v=0\r\n" +
	"o=- 4611731400430051336 2 IN IP4 127.0.0.1\r\n" +
	"s=-\r\n" +
	"t=0 0\r\n" +
	"m=video 9 UDP/TLS/RTP/SAVPF 96 97 98\r\n" +
	"c=IN IP4 0.0.0.0\r\n" +
	"a=rtcp:9 IN IP4 0.0.0.0\r\n" +
	"a=mid:0\r\n" +
	"a=sendonly\r\n" +
	"a=rid:h send 96,97;max-width=1280;max-height=720;max-fps=30\r\n" +
	"a=rid:m send 96;max-width=640;x-vendor=keep-me\r\n" +
	"a=rid:l send 96;max-width=320\r\n" +
	"a=rid:f send 98\r\n" +
	"a=simulcast:RFC/8853 send h;~m,l;f\r\n"

func TestSimulcastRoundTrip(t *testing.T) {
	var desc SessionDescription
	assert.NoError(t, desc.UnmarshalString(simulcastRoundTripSDP))

	assert.Len(t, desc.MediaDescriptions, 1)

	md := desc.MediaDescriptions[0]
	assert.NoError(t, md.ValidateRIDSimulcast())

	rids, err := md.GetRIDs()
	assert.NoError(t, err)
	assert.Len(t, rids, 4)

	// Unknown restriction parameters are preserved.
	vendor, ok := rids[1].Restriction("x-vendor")
	assert.True(t, ok)
	assert.Equal(t, "keep-me", vendor)

	simulcasts, err := md.GetSimulcast()
	assert.NoError(t, err)
	assert.Len(t, simulcasts, 1)

	stream := simulcasts[0].Streams[0]
	assert.Equal(t, RIDDirectionSend, stream.Direction)
	assert.Len(t, stream.Layers, 3)

	assert.Equal(t, []SimulcastEntry{{ID: "h"}}, stream.Layers[0].Alternatives)

	middle := stream.Layers[1].Alternatives
	assert.Equal(t, []SimulcastEntry{
		{ID: "m", Paused: true},
		{ID: "l"},
	}, middle)

	assert.Equal(t, []SimulcastEntry{{ID: "f"}}, stream.Layers[2].Alternatives)

	// Exact byte-for-byte round trip.
	out, err := desc.Marshal()
	assert.NoError(t, err)
	assert.Equal(t, simulcastRoundTripSDP, string(out))
}

// Modifying one layer must not change the meaning of the other rid and
// simulcast attributes.
func TestSimulcastModifyOneLayer(t *testing.T) {
	var desc SessionDescription
	assert.NoError(t, desc.UnmarshalString(simulcastRoundTripSDP))

	md := desc.MediaDescriptions[0]

	before, err := md.GetRIDs()
	assert.NoError(t, err)

	beforeSim, err := md.GetSimulcast()
	assert.NoError(t, err)

	// Add another alternative ("q") to the first layer only.
	md.WithValueAttribute(AttrKeyRID, "q send 96;max-width=1920")

	// Rewrite just the simulcast attribute in place.
	for i := range md.Attributes {
		if md.Attributes[i].Key != AttrKeySimulcast {
			continue
		}

		sim := new(Simulcast)
		assert.NoError(t, sim.Unmarshal(md.Attributes[i].Value))
		sim.Streams[0].Layers[0].Alternatives = append(
			sim.Streams[0].Layers[0].Alternatives,
			SimulcastEntry{ID: "q"},
		)
		md.Attributes[i].Value = sim.String()
	}

	assert.NoError(t, md.ValidateRIDSimulcast())

	after, err := md.GetRIDs()
	assert.NoError(t, err)

	afterSim, err := md.GetSimulcast()
	assert.NoError(t, err)

	// Pre-existing rids are unchanged.
	assert.Equal(t, before, after[:len(before)])

	// Layers 1 and 2, and the paused flag, keep their meaning.
	assert.Equal(t, beforeSim[0].Streams[0].Layers[1:], afterSim[0].Streams[0].Layers[1:])

	assert.Equal(t, []SimulcastEntry{
		{ID: "h"},
		{ID: "q"},
	}, afterSim[0].Streams[0].Layers[0].Alternatives)

	// Everything still marshals and re-parses identically.
	out, err := desc.Marshal()
	assert.NoError(t, err)

	var reparsed SessionDescription
	assert.NoError(t, reparsed.UnmarshalString(string(out)))
	assert.NoError(t, reparsed.ValidateRIDSimulcast())

	reparsedSim, err := reparsed.MediaDescriptions[0].GetSimulcast()
	assert.NoError(t, err)
	assert.Equal(t, afterSim, reparsedSim)
}

func TestSessionDescriptionValidateRIDSimulcast(t *testing.T) {
	var good SessionDescription
	assert.NoError(t, good.UnmarshalString(simulcastRoundTripSDP))
	assert.NoError(t, good.ValidateRIDSimulcast())

	bad := strings.NewReplacer(
		"a=rid:m send", "a=rid:m recv",
	).Replace(simulcastRoundTripSDP)

	var desc SessionDescription
	assert.NoError(t, desc.UnmarshalString(bad))

	err := desc.ValidateRIDSimulcast()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "a=simulcast")
}
