// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package sdp

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func buildSimulcastMedia(attrs ...Attribute) *MediaDescription {
	md := NewJSEPMediaDescription("video", nil)
	md.WithValueAttribute(AttrKeyMID, "0")
	md.Attributes = append(md.Attributes, attrs...)

	return md
}

func TestMediaDescription_GetRIDs(t *testing.T) {
	md := buildSimulcastMedia(
		NewAttribute(AttrKeyRID, "h send 97;max-width=1280"),
		NewAttribute(AttrKeyRID, "l send"),
		NewPropertyAttribute(AttrKeySendOnly),
	)

	rids, err := md.GetRIDs()
	assert.NoError(t, err)
	assert.Len(t, rids, 2)
	assert.Equal(t, "h", rids[0].ID)
	assert.Equal(t, RIDDirectionSend, rids[0].Direction)
	assert.Equal(t, []string{"97"}, rids[0].PayloadTypes)

	v, ok := rids[0].Restriction(RIDRestrictionMaxWidth)
	assert.True(t, ok)
	assert.Equal(t, "1280", v)

	assert.Equal(t, "l", rids[1].ID)
}

func TestMediaDescription_GetSimulcast(t *testing.T) {
	md := buildSimulcastMedia(
		NewAttribute(AttrKeySimulcast, "RFC/8853 send h;~m,l"),
	)

	simulcasts, err := md.GetSimulcast()
	assert.NoError(t, err)
	assert.Len(t, simulcasts, 1)

	layers := simulcasts[0].Streams[0].Layers
	assert.Len(t, layers, 2)
	assert.Equal(t, "h", layers[0].Alternatives[0].ID)
	assert.False(t, layers[0].Alternatives[0].Paused)
	assert.True(t, layers[1].Alternatives[0].Paused)
	assert.Equal(t, "m", layers[1].Alternatives[0].ID)
	assert.Equal(t, "l", layers[1].Alternatives[1].ID)
}

func TestMediaDescription_ValidateRIDSimulcast(t *testing.T) {
	t.Run("valid three layers with paused alternative", func(t *testing.T) {
		md := buildSimulcastMedia(
			NewAttribute(AttrKeyRID, "h send 97"),
			NewAttribute(AttrKeyRID, "m send;max-width=1280"),
			NewAttribute(AttrKeyRID, "l send;max-width=320"),
			NewAttribute(AttrKeyRID, "q send"),
			NewAttribute(AttrKeySimulcast, "RFC/8853 send h;~m,l;q"),
		)

		err := md.ValidateRIDSimulcast()
		assert.NoError(t, err)
	})

	t.Run("references undeclared rid", func(t *testing.T) {
		md := buildSimulcastMedia(
			NewAttribute(AttrKeyRID, "h send"),
			NewAttribute(AttrKeySimulcast, "send q"),
		)

		err := md.ValidateRIDSimulcast()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "a=simulcast")
		assert.Contains(t, err.Error(), "undeclared rid")
	})

	t.Run("duplicate rid definition", func(t *testing.T) {
		md := buildSimulcastMedia(
			NewAttribute(AttrKeyRID, "h send"),
			NewAttribute(AttrKeyRID, "h recv"),
		)

		err := md.ValidateRIDSimulcast()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "a=rid")
		assert.Contains(t, err.Error(), "duplicate rid")
	})

	t.Run("direction mismatch", func(t *testing.T) {
		md := buildSimulcastMedia(
			NewAttribute(AttrKeyRID, "h recv"),
			NewAttribute(AttrKeySimulcast, "RFC/8853 send h"),
		)

		err := md.ValidateRIDSimulcast()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "a=simulcast")
		assert.Contains(t, err.Error(), "send")
		assert.Contains(t, err.Error(), "recv")
	})

	t.Run("duplicate rid inside simulcast", func(t *testing.T) {
		md := buildSimulcastMedia(
			NewAttribute(AttrKeyRID, "h send"),
			NewAttribute(AttrKeySimulcast, "send h,h"),
		)

		err := md.ValidateRIDSimulcast()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "duplicate rid")
	})

	t.Run("depend references undeclared rid", func(t *testing.T) {
		md := buildSimulcastMedia(
			NewAttribute(AttrKeyRID, "h send;depend=x"),
		)

		err := md.ValidateRIDSimulcast()
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "a=rid")
		assert.Contains(t, err.Error(), "depends on undeclared rid")
	})

	t.Run("malformed rid attribute", func(t *testing.T) {
		md := buildSimulcastMedia(
			NewAttribute(AttrKeyRID, "h notadirection"),
		)

		err := md.ValidateRIDSimulcast()
		assert.Error(t, err)
		assert.True(t, strings.Contains(err.Error(), "a=rid"))
	})

	t.Run("valid out of order depend", func(t *testing.T) {
		md := buildSimulcastMedia(
			NewAttribute(AttrKeyRID, "h send;depend=l"),
			NewAttribute(AttrKeyRID, "l send"),
			NewAttribute(AttrKeySimulcast, "send h;l"),
		)

		assert.NoError(t, md.ValidateRIDSimulcast())
	})
}

func TestMediaDescription_WithRIDAndSimulcast(t *testing.T) {
	rid := NewRID("h", RIDDirectionSend).
		WithPayloadTypes("97").
		WithRestriction(RIDRestrictionMaxWidth, "1280")

	simulcast := &Simulcast{
		Version: SimulcastVersion,
		Streams: []SimulcastStream{
			{
				Direction: RIDDirectionSend,
				Layers: []SimulcastLayer{
					{Alternatives: []SimulcastEntry{{ID: "h"}}},
				},
			},
		},
	}

	md := NewJSEPMediaDescription("video", nil).
		WithRID(rid).
		WithSimulcast(simulcast)

	value, ok := md.Attribute(AttrKeyRID)
	assert.True(t, ok)
	assert.Equal(t, "h send 97;max-width=1280", value)

	value, ok = md.Attribute(AttrKeySimulcast)
	assert.True(t, ok)
	assert.Equal(t, "RFC/8853 send h", value)
}
