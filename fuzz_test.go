// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package sdp

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func FuzzUnmarshal(f *testing.F) {
	f.Add("")
	f.Add(CanonicalUnmarshalSDP)
	f.Fuzz(func(t *testing.T, data string) {
		// Check that unmarshalling any byte slice does not panic.
		var sd SessionDescription
		if err := sd.UnmarshalString(data); err != nil {
			return
		}
		// Check that we can marshal anything we unmarshalled.
		_, err := sd.Marshal()
		assert.NoError(t, err)
	})
}

func FuzzUnmarshalTrickleICESDPFrag(f *testing.F) {
	f.Add("m=audio 9 UDP/TLS/RTP/SAVPF 111\r\na=mid:0\r\n")
	f.Add("a=mid:0\r\n")
	f.Fuzz(func(t *testing.T, data string) {
		frag, err := UnmarshalTrickleICESDPFrag(data)
		if err != nil {
			return
		}

		desc, err := NewJSEPSessionDescription(false)
		assert.NoError(t, err)
		desc = desc.WithMedia(
			NewJSEPMediaDescription("audio", []string{"111"}).WithValueAttribute(AttrKeyMID, "0"),
		)

		merger := NewTrickleICEMerger()
		if _, err := merger.Merge(desc, data); err != nil {
			return
		}

		assert.NotEmpty(t, frag.Sections)
	})
}
