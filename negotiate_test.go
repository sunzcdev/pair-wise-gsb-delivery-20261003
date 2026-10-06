// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package sdp

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const negotiateSessionPrefix = "v=0\r\n" +
	"o=- 123 456 IN IP4 0.0.0.0\r\n" +
	"s=-\r\n" +
	"t=0 0\r\n"

func mustParseOffer(t *testing.T, body string) *SessionDescription {
	t.Helper()

	offer := &SessionDescription{}
	require.NoError(t, offer.UnmarshalString(negotiateSessionPrefix+body))

	return offer
}

func TestNegotiateMedia(t *testing.T) {
	type rejection struct {
		name   string
		reason string // matched as substring
	}

	cases := []struct {
		name         string
		local        []MediaCapability
		offer        string
		wantFormats  []string
		wantRejected []rejection
	}{
		{
			name: "opus matched, answer uses remote payload types",
			local: []MediaCapability{
				{
					Name:      "opus",
					ClockRate: 48000,
					Channels:  2,
					Fmtp:      map[string]string{"minptime": "10", "useinbandfec": "1"},
				},
			},
			offer: "m=audio 9 UDP/TLS/RTP/SAVPF 111\r\n" +
				"a=rtpmap:111 opus/48000/2\r\n" +
				"a=fmtp:111 minptime=10;useinbandfec=1\r\n" +
				"a=rtcp-fb:111 transport-cc\r\n",
			wantFormats: []string{"111"},
		},
		{
			name: "opus clock rate mismatch",
			local: []MediaCapability{
				{Name: "opus", ClockRate: 16000, Channels: 2},
			},
			offer: "m=audio 9 UDP/TLS/RTP/SAVPF 111\r\n" +
				"a=rtpmap:111 opus/48000/2\r\n",
			wantRejected: []rejection{
				{"opus", "clock rate 16000 does not match remote 48000"},
			},
		},
		{
			name: "opus channel count mismatch",
			local: []MediaCapability{
				{Name: "opus", ClockRate: 48000, Channels: 1},
			},
			offer: "m=audio 9 UDP/TLS/RTP/SAVPF 111\r\n" +
				"a=rtpmap:111 opus/48000/2\r\n",
			wantRejected: []rejection{
				{"opus", "channel count 1 does not match remote 2"},
			},
		},
		{
			name: "unknown fmtp parameter on remote only is rejected",
			local: []MediaCapability{
				{
					Name:      "opus",
					ClockRate: 48000,
					Channels:  2,
					Fmtp:      map[string]string{"useinbandfec": "1"},
				},
			},
			offer: "m=audio 9 UDP/TLS/RTP/SAVPF 111\r\n" +
				"a=rtpmap:111 opus/48000/2\r\n" +
				"a=fmtp:111 useinbandfec=1;usedtx=1\r\n",
			wantRejected: []rejection{
				{"opus", `fmtp parameter "usedtx" is present on only one side`},
			},
		},
		{
			name: "unknown fmtp parameter value mismatch is rejected",
			local: []MediaCapability{
				{
					Name:      "opus",
					ClockRate: 48000,
					Channels:  2,
					Fmtp:      map[string]string{"useinbandfec": "0"},
				},
			},
			offer: "m=audio 9 UDP/TLS/RTP/SAVPF 111\r\n" +
				"a=rtpmap:111 opus/48000/2\r\n" +
				"a=fmtp:111 useinbandfec=1\r\n",
			wantRejected: []rejection{
				{"opus", `fmtp parameter "useinbandfec" mismatch`},
			},
		},
		{
			name: "h264 same name but incompatible profile",
			local: []MediaCapability{
				{
					Name:      "H264",
					ClockRate: 90000,
					Fmtp: map[string]string{
						"profile-level-id":   "42e01f",
						"packetization-mode": "1",
					},
				},
			},
			offer: "m=video 9 UDP/TLS/RTP/SAVPF 102\r\n" +
				"a=rtpmap:102 H264/90000\r\n" +
				"a=fmtp:102 profile-level-id=640c1f;packetization-mode=1\r\n",
			wantRejected: []rejection{
				{"H264", "h264 profile mismatch"},
			},
		},
		{
			name: "h264 packetization-mode mismatch",
			local: []MediaCapability{
				{
					Name:      "H264",
					ClockRate: 90000,
					Fmtp:      map[string]string{"profile-level-id": "42e01f"},
				},
			},
			offer: "m=video 9 UDP/TLS/RTP/SAVPF 102\r\n" +
				"a=rtpmap:102 H264/90000\r\n" +
				"a=fmtp:102 profile-level-id=42e01f;packetization-mode=1\r\n",
			wantRejected: []rejection{
				{"H264", "h264 packetization-mode mismatch"},
			},
		},
		{
			name: "h264 level asymmetry allowed on both sides",
			local: []MediaCapability{
				{
					Name:      "H264",
					ClockRate: 90000,
					Fmtp: map[string]string{
						"profile-level-id":        "42e01f",
						"level-asymmetry-allowed": "1",
					},
				},
			},
			offer: "m=video 9 UDP/TLS/RTP/SAVPF 102\r\n" +
				"a=rtpmap:102 H264/90000\r\n" +
				"a=fmtp:102 profile-level-id=42e028;level-asymmetry-allowed=1\r\n",
			wantFormats: []string{"102"},
		},
		{
			name: "h264 level asymmetry not allowed",
			local: []MediaCapability{
				{
					Name:      "H264",
					ClockRate: 90000,
					Fmtp:      map[string]string{"profile-level-id": "42e01f"},
				},
			},
			offer: "m=video 9 UDP/TLS/RTP/SAVPF 102\r\n" +
				"a=rtpmap:102 H264/90000\r\n" +
				"a=fmtp:102 profile-level-id=42e028\r\n",
			wantRejected: []rejection{
				{"H264", "h264 level mismatch"},
			},
		},
		{
			name: "rtx kept for surviving primary, dropped for eliminated one",
			local: []MediaCapability{
				{
					Name:      "H264",
					ClockRate: 90000,
					Fmtp:      map[string]string{"profile-level-id": "42e01f"},
				},
				{Name: "rtx", ClockRate: 90000},
			},
			offer: "m=video 9 UDP/TLS/RTP/SAVPF 96 97 98 99\r\n" +
				"a=rtpmap:96 VP8/90000\r\n" +
				"a=rtpmap:97 rtx/90000\r\n" +
				"a=fmtp:97 apt=96\r\n" +
				"a=rtpmap:98 H264/90000\r\n" +
				"a=fmtp:98 profile-level-id=42e01f\r\n" +
				"a=rtpmap:99 rtx/90000\r\n" +
				"a=fmtp:99 apt=98\r\n",
			wantFormats: []string{"98", "99"},
		},
		{
			name: "rtx rejected when its primary codec was eliminated",
			local: []MediaCapability{
				{
					Name:      "H264",
					ClockRate: 90000,
					Fmtp:      map[string]string{"profile-level-id": "42e01f"},
				},
				{Name: "rtx", ClockRate: 90000},
			},
			offer: "m=video 9 UDP/TLS/RTP/SAVPF 96 97\r\n" +
				"a=rtpmap:96 H264/90000\r\n" +
				"a=fmtp:96 profile-level-id=640c1f\r\n" +
				"a=rtpmap:97 rtx/90000\r\n" +
				"a=fmtp:97 apt=96\r\n",
			wantRejected: []rejection{
				{"H264", "h264 profile mismatch"},
				{"rtx", "associated primary codec was not negotiated"},
			},
		},
		{
			name: "codec absent from offer",
			local: []MediaCapability{
				{Name: "opus", ClockRate: 48000, Channels: 2},
			},
			offer: "m=video 9 UDP/TLS/RTP/SAVPF 102\r\n" +
				"a=rtpmap:102 H264/90000\r\n" +
				"a=fmtp:102 profile-level-id=42e01f\r\n",
			wantRejected: []rejection{
				{"opus", "no opus codec in remote offer"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			offer := mustParseOffer(t, tc.offer)
			require.Len(t, offer.MediaDescriptions, 1)

			result, err := NegotiateMedia(tc.local, offer.MediaDescriptions[0])
			require.NoError(t, err)
			require.NotNil(t, result.Media)

			require.Equal(t, tc.wantFormats, result.Media.MediaName.Formats)

			require.Len(t, result.Rejected, len(tc.wantRejected))
			for i, want := range tc.wantRejected {
				got := result.Rejected[i]
				require.Truef(t, strings.EqualFold(got.Capability.Name, want.name),
					"rejected capability %d: got %q, want %q", i, got.Capability.Name, want.name)
				require.Containsf(t, got.Reason, want.reason,
					"rejected capability %d (%s) reason", i, want.name)
			}

			// The negotiated media description must survive a
			// Marshal/Unmarshal round trip unchanged.
			session, err := NewJSEPSessionDescription(false)
			require.NoError(t, err)
			session.WithMedia(result.Media)

			marshaled, err := session.Marshal()
			require.NoError(t, err)

			parsed := &SessionDescription{}
			require.NoError(t, parsed.Unmarshal(marshaled))
			require.Len(t, parsed.MediaDescriptions, 1)
			require.Equal(t, result.Media.MediaName, parsed.MediaDescriptions[0].MediaName)
			require.Equal(t, result.Media.Attributes, parsed.MediaDescriptions[0].Attributes)
		})
	}
}

func TestNegotiateMediaMultipleSectionsReusePayloadTypes(t *testing.T) {
	offer := mustParseOffer(t,
		"m=audio 9 UDP/TLS/RTP/SAVPF 96\r\n"+
			"a=rtpmap:96 opus/48000/2\r\n"+
			"m=video 9 UDP/TLS/RTP/SAVPF 96 97\r\n"+
			"a=rtpmap:96 H264/90000\r\n"+
			"a=fmtp:96 profile-level-id=42e01f\r\n"+
			"a=rtpmap:97 rtx/90000\r\n"+
			"a=fmtp:97 apt=96\r\n",
	)
	require.Len(t, offer.MediaDescriptions, 2)

	audioResult, err := NegotiateMedia([]MediaCapability{
		{Name: "opus", ClockRate: 48000, Channels: 2},
	}, offer.MediaDescriptions[0])
	require.NoError(t, err)
	require.Equal(t, []string{"96"}, audioResult.Media.MediaName.Formats)
	require.Empty(t, audioResult.Rejected)
	rtpmap, ok := audioResult.Media.Attribute("rtpmap")
	require.True(t, ok)
	require.Equal(t, "96 opus/48000/2", rtpmap)

	videoResult, err := NegotiateMedia([]MediaCapability{
		{
			Name:      "H264",
			ClockRate: 90000,
			Fmtp:      map[string]string{"profile-level-id": "42e01f"},
		},
		{Name: "rtx", ClockRate: 90000},
	}, offer.MediaDescriptions[1])
	require.NoError(t, err)
	require.Equal(t, []string{"96", "97"}, videoResult.Media.MediaName.Formats)
	require.Empty(t, videoResult.Rejected)
	rtpmap, ok = videoResult.Media.Attribute("rtpmap")
	require.True(t, ok)
	require.Equal(t, "96 H264/90000", rtpmap)
	var fmtps []string
	for _, attr := range videoResult.Media.Attributes {
		if attr.Key == "fmtp" {
			fmtps = append(fmtps, attr.Value)
		}
	}
	require.Equal(t, []string{"96 profile-level-id=42e01f", "97 apt=96"}, fmtps)
}

func TestNegotiateMediaNilOffer(t *testing.T) {
	_, err := NegotiateMedia(nil, nil)
	require.Error(t, err)
}
