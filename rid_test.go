// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package sdp

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRIDDirection(t *testing.T) {
	dir, err := NewRIDDirection("send")
	assert.NoError(t, err)
	assert.Equal(t, RIDDirectionSend, dir)
	assert.Equal(t, "send", dir.String())

	dir, err = NewRIDDirection("recv")
	assert.NoError(t, err)
	assert.Equal(t, RIDDirectionRecv, dir)
	assert.Equal(t, "recv", dir.String())

	_, err = NewRIDDirection("sendrecv")
	assert.Error(t, err)

	assert.Equal(t, "", RIDDirection(0).String())
}

func TestRID_Marshal(t *testing.T) {
	rid := NewRID("h", RIDDirectionSend).
		WithPayloadTypes("97", "98").
		WithRestriction(RIDRestrictionMaxWidth, "1280").
		WithRestriction("x-custom", "abc")

	assert.Equal(
		t,
		"rid:h send 97,98;max-width=1280;x-custom=abc",
		rid.Marshal(),
	)
	assert.Equal(
		t,
		Attribute{Key: "rid", Value: "h send 97,98;max-width=1280;x-custom=abc"},
		rid.Clone(),
	)

	v, ok := rid.Restriction(RIDRestrictionMaxWidth)
	assert.True(t, ok)
	assert.Equal(t, "1280", v)

	_, ok = rid.Restriction("missing")
	assert.False(t, ok)
}

func TestRID_Unmarshal(t *testing.T) {
	tests := []struct {
		name  string
		raw   string
		want  *RID
		again string
	}{
		{
			name: "send with pt list and restrictions",
			raw:  "rid:h send 97;max-width=1280;max-height=720;max-fps=30;x-custom=keep",
			want: &RID{
				ID:           "h",
				Direction:    RIDDirectionSend,
				PayloadTypes: []string{"97"},
				Restrictions: []RIDRestriction{
					{Key: RIDRestrictionMaxWidth, Value: "1280"},
					{Key: RIDRestrictionMaxHeight, Value: "720"},
					{Key: RIDRestrictionMaxFPS, Value: "30"},
					{Key: "x-custom", Value: "keep"},
				},
			},
		},
		{
			name:  "recv without pt list",
			raw:   "rid:l recv;max-width=320",
			again: "rid:l recv;max-width=320",
			want: &RID{
				ID:        "l",
				Direction: RIDDirectionRecv,
				Restrictions: []RIDRestriction{
					{Key: RIDRestrictionMaxWidth, Value: "320"},
				},
			},
		},
		{
			name:  "value without key prefix",
			raw:   "m recv",
			again: "rid:m recv",
			want:  &RID{ID: "m", Direction: RIDDirectionRecv},
		},
		{
			name: "pixel aspect ratio rational",
			raw:  "rid:h send;max-pixel-aspect-ratio=4:3;codec-framerate=29.97;scaling=1.0,1.25",
			want: &RID{
				ID:        "h",
				Direction: RIDDirectionSend,
				Restrictions: []RIDRestriction{
					{Key: RIDRestrictionMaxPixelAspectRatio, Value: "4:3"},
					{Key: RIDRestrictionCodecFramerate, Value: "29.97"},
					{Key: RIDRestrictionScaling, Value: "1.0,1.25"},
				},
			},
		},
		{
			name: "retr and depend",
			raw:  "rid:h send;retr=rtp;depend=l",
			want: &RID{
				ID:        "h",
				Direction: RIDDirectionSend,
				Restrictions: []RIDRestriction{
					{Key: RIDRestrictionRetr, Value: "rtp"},
					{Key: RIDRestrictionDepend, Value: "l"},
				},
			},
		},
		{
			name: "unknown flag restriction preserved",
			raw:  "rid:h send;x-flag",
			want: &RID{
				ID:        "h",
				Direction: RIDDirectionSend,
				Restrictions: []RIDRestriction{
					{Key: "x-flag", Value: ""},
				},
			},
			again: "rid:h send;x-flag",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rid := new(RID)
			assert.NoError(t, rid.Unmarshal(tc.raw))
			assert.Equal(t, tc.want, rid)

			wantMarshal := tc.again
			if wantMarshal == "" {
				// Normalize canonical representation for pt list cases.
				wantMarshal = "rid:" + rid.String()
			}

			assert.Equal(t, wantMarshal, rid.Marshal())

			roundTrip := new(RID)
			assert.NoError(t, roundTrip.Unmarshal(rid.Marshal()))
			assert.Equal(t, rid, roundTrip)
		})
	}
}

func TestRID_Unmarshal_Errors(t *testing.T) {
	invalid := []string{
		"",
		"rid:",
		"foo:h send",
		"h",
		"h invalid",
		"h@ send",
		"h send;max-width=big",
		"h send;max-width=0",
		"h send;retr=nack",
		"h send;max-pixel-aspect-ratio=4:",
		"h send;depend=l,m",
		"h send;=value",
		"h send;max-width=1280;",
		"h send;scaling=1.0,x",
		"h send;codec-pt=,97",
	}

	for _, raw := range invalid {
		rid := new(RID)
		assert.Errorf(t, rid.Unmarshal(raw), "expected error for %q", raw)
	}
}
