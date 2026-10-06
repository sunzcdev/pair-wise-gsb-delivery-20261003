// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package sdp

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func rtpmapAttr(pt, value string) Attribute {
	return NewAttribute("rtpmap", pt+" "+value)
}

func fmtpAttr(pt, value string) Attribute {
	return NewAttribute("fmtp", pt+" "+value)
}

func videoSection(formats []string, attrs ...Attribute) *MediaDescription {
	return &MediaDescription{
		MediaName: MediaName{
			Media:   "video",
			Port:    RangedPort{Value: 4000},
			Protos:  []string{"UDP", "TLS", "RTP", "SAVPF"},
			Formats: formats,
		},
		Attributes: attrs,
	}
}

func audioSection(formats []string, attrs ...Attribute) *MediaDescription {
	return &MediaDescription{
		MediaName: MediaName{
			Media:   "audio",
			Port:    RangedPort{Value: 4000},
			Protos:  []string{"UDP", "TLS", "RTP", "SAVPF"},
			Formats: formats,
		},
		Attributes: attrs,
	}
}

type expectedRejection struct {
	payloadType uint8
	codec       string
	reason      string // substring match
}

func TestNegotiateCapabilities(t *testing.T) {
	localOpus := CodecCapability{MimeType: "audio/opus", ClockRate: 48000, Channels: 2}
	localH264 := CodecCapability{
		MimeType:  "video/H264",
		ClockRate: 90000,
		FMTP:      "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
	}
	localRTX := CodecCapability{MimeType: "video/rtx", ClockRate: 90000}

	tests := []struct {
		name            string
		local           []CodecCapability
		offer           []*MediaDescription
		wantFormats     [][]string // per answer media section
		wantAnswerFMTP  map[uint8]string
		wantRejections  []expectedRejection
		wantNoMediaDesc bool
	}{
		{
			name:  "opus selected with remote payload type",
			local: []CodecCapability{localOpus},
			offer: []*MediaDescription{
				audioSection([]string{"111"}, rtpmapAttr("111", "opus/48000/2")),
			},
			wantFormats: [][]string{{"111"}},
		},
		{
			name:  "opus channel mismatch",
			local: []CodecCapability{{MimeType: "audio/opus", ClockRate: 48000, Channels: 1}},
			offer: []*MediaDescription{
				audioSection([]string{"111"}, rtpmapAttr("111", "opus/48000/2")),
			},
			wantNoMediaDesc: true,
			wantRejections: []expectedRejection{
				{payloadType: 111, codec: "opus", reason: "channel count mismatch"},
			},
		},
		{
			name:  "h264 same name but incompatible profile-level-id",
			local: []CodecCapability{localH264},
			offer: []*MediaDescription{
				videoSection([]string{"96"},
					rtpmapAttr("96", "H264/90000"),
					fmtpAttr("96", "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=64001f")),
			},
			wantNoMediaDesc: true,
			wantRejections: []expectedRejection{
				{payloadType: 96, codec: "h264", reason: "incompatible profile-level-id"},
			},
		},
		{
			name:  "h264 packetization-mode mismatch",
			local: []CodecCapability{localH264},
			offer: []*MediaDescription{
				videoSection([]string{"96"},
					rtpmapAttr("96", "H264/90000"),
					fmtpAttr("96", "profile-level-id=42e01f")),
			},
			wantNoMediaDesc: true,
			wantRejections: []expectedRejection{
				{payloadType: 96, codec: "h264", reason: "packetization-mode mismatch"},
			},
		},
		{
			name:  "h264 level asymmetry selects with local fmtp",
			local: []CodecCapability{localH264},
			offer: []*MediaDescription{
				videoSection([]string{"102"},
					rtpmapAttr("102", "H264/90000"),
					fmtpAttr("102", "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e032")),
			},
			wantFormats: [][]string{{"102"}},
			// The answer carries the local profile-level-id, not the remote one.
			wantAnswerFMTP: map[uint8]string{
				102: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
			},
		},
		{
			name:  "h264 unknown fmtp parameters ignored and not echoed",
			local: []CodecCapability{localH264},
			offer: []*MediaDescription{
				videoSection([]string{"96"},
					rtpmapAttr("96", "H264/90000"),
					fmtpAttr("96", "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f;"+
						"max-mbps=108000;sprop-parameter-sets=Z0LAHtoCgPaE,aM4wpIA=")),
			},
			wantFormats: [][]string{{"96"}},
			wantAnswerFMTP: map[uint8]string{
				96: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
			},
		},
		{
			name:  "rtx selected with apt of primary codec",
			local: []CodecCapability{localH264, localRTX},
			offer: []*MediaDescription{
				videoSection([]string{"96", "97"},
					rtpmapAttr("96", "H264/90000"),
					fmtpAttr("96", "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f"),
					rtpmapAttr("97", "rtx/90000"),
					fmtpAttr("97", "apt=96")),
			},
			wantFormats: [][]string{{"96", "97"}},
			wantAnswerFMTP: map[uint8]string{
				96: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
				97: "apt=96",
			},
		},
		{
			name:  "rtx rejected after primary codec eliminated",
			local: []CodecCapability{localH264, localRTX},
			offer: []*MediaDescription{
				videoSection([]string{"96", "97"},
					rtpmapAttr("96", "H264/90000"),
					fmtpAttr("96", "packetization-mode=0;profile-level-id=4d001f"),
					rtpmapAttr("97", "rtx/90000"),
					fmtpAttr("97", "apt=96")),
			},
			wantNoMediaDesc: true,
			wantRejections: []expectedRejection{
				{payloadType: 96, codec: "h264", reason: "packetization-mode mismatch"},
				{payloadType: 97, codec: "rtx", reason: "associated codec with payload type 96 was not selected"},
			},
		},
		{
			name:  "rtx without apt rejected",
			local: []CodecCapability{localH264, localRTX},
			offer: []*MediaDescription{
				videoSection([]string{"96", "97"},
					rtpmapAttr("96", "H264/90000"),
					fmtpAttr("96", "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f"),
					rtpmapAttr("97", "rtx/90000")),
			},
			wantFormats: [][]string{{"96"}},
			wantRejections: []expectedRejection{
				{payloadType: 97, codec: "rtx", reason: "missing apt parameter"},
			},
		},
		{
			name:  "multiple media sections reuse the same payload type",
			local: []CodecCapability{localOpus, localH264},
			offer: []*MediaDescription{
				audioSection([]string{"111"}, rtpmapAttr("111", "opus/48000/2")),
				videoSection([]string{"111"},
					rtpmapAttr("111", "H264/90000"),
					fmtpAttr("111", "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f")),
			},
			wantFormats: [][]string{{"111"}, {"111"}},
		},
		{
			name:  "unsupported codec rejected",
			local: []CodecCapability{localOpus},
			offer: []*MediaDescription{
				audioSection([]string{"0"}, rtpmapAttr("0", "PCMU/8000")),
			},
			wantNoMediaDesc: true,
			wantRejections: []expectedRejection{
				{payloadType: 0, codec: "pcmu", reason: "unsupported codec"},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			offer := &SessionDescription{MediaDescriptions: test.offer}

			result, err := NegotiateCapabilities(test.local, offer)
			require.NoError(t, err)

			if test.wantNoMediaDesc {
				assert.Empty(t, result.MediaDescriptions)
			} else {
				require.Len(t, result.MediaDescriptions, len(test.wantFormats))
				for i, wantFormats := range test.wantFormats {
					assert.Equal(t, wantFormats, result.MediaDescriptions[i].MediaName.Formats)
				}
			}

			for pt, wantFMTP := range test.wantAnswerFMTP {
				value, ok := findFMTP(result.MediaDescriptions, pt)
				require.True(t, ok, "answer fmtp for pt %d not found", pt)
				assert.Equal(t, wantFMTP, value)
			}

			require.Len(t, result.Rejected, len(test.wantRejections))
			for i, want := range test.wantRejections {
				got := result.Rejected[i]
				assert.Equal(t, want.payloadType, got.PayloadType)
				assert.Equal(t, want.codec, got.Codec)
				assert.Contains(t, got.Reason, want.reason)
			}
		})
	}
}

func findFMTP(descriptions []*MediaDescription, pt uint8) (string, bool) {
	prefix := strconv.Itoa(int(pt)) + " "
	for i := range descriptions {
		for _, attr := range descriptions[i].Attributes {
			if attr.Key == "fmtp" && len(attr.Value) > len(prefix) && attr.Value[:len(prefix)] == prefix {
				return attr.Value[len(prefix):], true
			}
		}
	}

	return "", false
}

func TestNegotiateCapabilities_NilOffer(t *testing.T) {
	_, err := NegotiateCapabilities(nil, nil)
	assert.Error(t, err)
}

func TestNegotiateCapabilities_MarshalRoundTrip(t *testing.T) {
	local := []CodecCapability{
		{MimeType: "audio/opus", ClockRate: 48000, Channels: 2},
		{
			MimeType:  "video/H264",
			ClockRate: 90000,
			FMTP:      "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
		},
		{MimeType: "video/rtx", ClockRate: 90000},
	}
	offer := &SessionDescription{
		MediaDescriptions: []*MediaDescription{
			audioSection([]string{"111"}, rtpmapAttr("111", "opus/48000/2")),
			videoSection([]string{"96", "97"},
				rtpmapAttr("96", "H264/90000"),
				fmtpAttr("96", "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f"),
				rtpmapAttr("97", "rtx/90000"),
				fmtpAttr("97", "apt=96")),
		},
	}

	result, err := NegotiateCapabilities(local, offer)
	require.NoError(t, err)
	require.Len(t, result.MediaDescriptions, 2)

	answer := &SessionDescription{
		Origin: Origin{
			Username:       "-",
			SessionID:      1,
			SessionVersion: 1,
			NetworkType:    "IN",
			AddressType:    "IP4",
			UnicastAddress: "0.0.0.0",
		},
		SessionName:       "-",
		TimeDescriptions:  []TimeDescription{{Timing: Timing{}}},
		MediaDescriptions: result.MediaDescriptions,
	}
	marshaled, err := answer.Marshal()
	require.NoError(t, err)

	parsed := &SessionDescription{}
	require.NoError(t, parsed.Unmarshal(marshaled))

	assert.Equal(t, result.MediaDescriptions, parsed.MediaDescriptions)
}
