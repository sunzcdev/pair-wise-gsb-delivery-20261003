// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package sdp

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// CodecCapability describes a single codec the local endpoint is able to
// send and receive. It is the local side input of NegotiateCapabilities.
type CodecCapability struct {
	// MimeType is the full MIME type of the codec, e.g. "audio/opus",
	// "video/H264" or "video/rtx". Matching is case-insensitive.
	MimeType string

	// ClockRate is the RTP clock rate in Hz, e.g. 48000 for Opus or
	// 90000 for H264/RTX.
	ClockRate uint32

	// Channels is the number of audio channels. Zero means the codec
	// default of one channel.
	Channels uint16

	// FMTP holds the local format-specific parameters in the same syntax
	// as an a=fmtp line, e.g.
	// "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f".
	// It may be empty.
	FMTP string
}

// RejectedCodec describes a codec of the remote offer that was not selected
// during negotiation, together with the reason why.
type RejectedCodec struct {
	// MediaType is the media kind of the m= section the codec appeared in,
	// e.g. "audio" or "video".
	MediaType string

	// PayloadType is the payload type number used by the remote offer.
	PayloadType uint8

	// Codec is the RTP encoding name of the rejected codec, e.g. "H264".
	Codec string

	// Reason is a human-readable explanation of why the codec was not
	// selected.
	Reason string
}

// NegotiationResult is the outcome of NegotiateCapabilities.
type NegotiationResult struct {
	// MediaDescriptions holds one m= section per offer media section that
	// has at least one selected codec. Payload type numbers are taken from
	// the remote offer, never from the local capabilities, so the
	// descriptions can be placed into an answer SessionDescription and
	// marshaled as-is.
	MediaDescriptions []*MediaDescription

	// Rejected lists every offered codec that was not selected, with the
	// reason for its rejection.
	Rejected []RejectedCodec
}

// errNilOffer is returned by NegotiateCapabilities when the offer is nil.
var errNilOffer = errors.New("offer must not be nil")

// NegotiateCapabilities matches the local codec capabilities against a
// remote offer and returns the media descriptions that can be used in an
// answer, plus the reasons for every offered codec that was not selected.
//
// The first batch of supported codecs is Opus, H264 and RTX. Clock rates
// and channel counts must match exactly. For H264 the packetization-mode
// must be identical and the profile-level-id must either be identical or,
// when both sides allow level asymmetry, share the same profile_idc and
// constraint bits. RTX is only selected when the primary codec referenced
// by its apt parameter was selected in the same media section.
//
// Unknown fmtp parameters are handled with a conservative policy: they are
// ignored while deciding compatibility and are never copied into the
// answer. Only parameters the library understands are echoed back, taken
// from the local capability.
func NegotiateCapabilities(local []CodecCapability, offer *SessionDescription) (*NegotiationResult, error) {
	if offer == nil {
		return nil, errNilOffer
	}

	result := &NegotiationResult{}
	for i := range offer.MediaDescriptions {
		negotiateMediaSection(local, offer.MediaDescriptions[i], result)
	}

	return result, nil
}

// offerCodec is a single payload type of a remote offer media section.
type offerCodec struct {
	payloadType uint8
	rtpmapValue string // rtpmap attribute value without the "<pt> " prefix
	name        string // lowercased encoding name
	clockRate   uint32
	channels    uint16
	fmtp        map[string]string
}

// selectedCodec pairs an offered codec with the fmtp line to answer.
type selectedCodec struct {
	offerCodec
	answerFMTP string
}

func negotiateMediaSection(local []CodecCapability, md *MediaDescription, result *NegotiationResult) {
	mediaType := md.MediaName.Media

	var offered []offerCodec
	for _, format := range md.MediaName.Formats {
		pt, err := strconv.ParseUint(format, 10, 8)
		if err != nil {
			result.Rejected = append(result.Rejected, RejectedCodec{
				MediaType: mediaType,
				Codec:     format,
				Reason:    fmt.Sprintf("invalid payload type %q", format),
			})

			continue
		}

		codec, reason := parseOfferCodec(md, uint8(pt))
		if reason != "" {
			result.Rejected = append(result.Rejected, RejectedCodec{
				MediaType:   mediaType,
				PayloadType: uint8(pt),
				Reason:      reason,
			})

			continue
		}
		offered = append(offered, codec)
	}

	selected := make(map[uint8]selectedCodec)
	var order []uint8

	// First pass: primary codecs.
	for _, codec := range offered {
		if codec.name == "rtx" {
			continue
		}

		answerFMTP, reason := matchPrimaryCodec(local, mediaType, codec)
		if reason != "" {
			result.Rejected = append(result.Rejected, RejectedCodec{
				MediaType:   mediaType,
				PayloadType: codec.payloadType,
				Codec:       codec.name,
				Reason:      reason,
			})

			continue
		}
		selected[codec.payloadType] = selectedCodec{offerCodec: codec, answerFMTP: answerFMTP}
		order = append(order, codec.payloadType)
	}

	// Second pass: RTX depends on its primary codec via apt.
	for _, codec := range offered {
		if codec.name != "rtx" {
			continue
		}

		answerFMTP, reason := matchRTXCodec(local, mediaType, codec, selected)
		if reason != "" {
			result.Rejected = append(result.Rejected, RejectedCodec{
				MediaType:   mediaType,
				PayloadType: codec.payloadType,
				Codec:       codec.name,
				Reason:      reason,
			})

			continue
		}
		selected[codec.payloadType] = selectedCodec{offerCodec: codec, answerFMTP: answerFMTP}
		order = append(order, codec.payloadType)
	}

	if len(order) == 0 {
		return
	}

	answer := MediaDescription{
		MediaName: MediaName{
			Media:  mediaType,
			Port:   RangedPort{Value: 9},
			Protos: append([]string(nil), md.MediaName.Protos...),
		},
	}
	for _, pt := range order {
		codec := selected[pt]
		answer.MediaName.Formats = append(answer.MediaName.Formats, strconv.Itoa(int(pt)))
		answer.Attributes = append(answer.Attributes,
			NewAttribute("rtpmap", fmt.Sprintf("%d %s", pt, codec.rtpmapValue)))
		if codec.answerFMTP != "" {
			answer.Attributes = append(answer.Attributes,
				NewAttribute("fmtp", fmt.Sprintf("%d %s", pt, codec.answerFMTP)))
		}
	}

	result.MediaDescriptions = append(result.MediaDescriptions, &answer)
}

// parseOfferCodec extracts the rtpmap and fmtp attributes of a single
// payload type from a media section. It returns a rejection reason when
// the payload type cannot be described.
func parseOfferCodec(md *MediaDescription, pt uint8) (offerCodec, string) {
	codec := offerCodec{payloadType: pt, channels: 1}
	prefix := strconv.Itoa(int(pt)) + " "

	var rtpmap string
	for _, attr := range md.Attributes {
		if !strings.HasPrefix(attr.Value, prefix) {
			continue
		}
		value := strings.TrimPrefix(attr.Value, prefix)
		switch attr.Key {
		case "rtpmap":
			rtpmap = value
		case "fmtp":
			codec.fmtp = parseFMTPParams(value)
		}
	}

	if rtpmap == "" {
		return codec, "missing rtpmap attribute"
	}

	parts := strings.Split(rtpmap, "/")
	if len(parts) < 2 {
		return codec, fmt.Sprintf("malformed rtpmap %q", rtpmap)
	}

	clockRate, err := strconv.ParseUint(parts[1], 10, 32)
	if err != nil {
		return codec, fmt.Sprintf("malformed clock rate in rtpmap %q", rtpmap)
	}

	codec.rtpmapValue = rtpmap
	codec.name = strings.ToLower(parts[0])
	codec.clockRate = uint32(clockRate)
	if len(parts) > 2 {
		channels, err := strconv.ParseUint(parts[2], 10, 16)
		if err != nil {
			return codec, fmt.Sprintf("malformed channel count in rtpmap %q", rtpmap)
		}
		codec.channels = uint16(channels)
	}

	return codec, ""
}

// parseFMTPParams parses the parameter part of an a=fmtp value into a
// lowercased key/value map. Parameters without a value keep an empty one.
func parseFMTPParams(s string) map[string]string {
	params := make(map[string]string)
	for _, p := range strings.Split(s, ";") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		key, value, _ := strings.Cut(p, "=")
		params[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
	}

	return params
}

// matchPrimaryCodec finds a local capability for a non-RTX offered codec
// and returns the fmtp line to place into the answer, or a rejection
// reason.
func matchPrimaryCodec(local []CodecCapability, mediaType string, codec offerCodec) (string, string) {
	switch codec.name {
	case "opus", "h264":
	default:
		return "", fmt.Sprintf("unsupported codec %q", codec.name)
	}

	mimeType := mediaType + "/" + codec.name
	reason := fmt.Sprintf("no local capability for %s", mimeType)

	for i := range local {
		capability := &local[i]
		if !strings.EqualFold(capability.MimeType, mimeType) {
			continue
		}
		if capability.ClockRate != codec.clockRate {
			reason = fmt.Sprintf("clock rate mismatch: local %d, remote %d",
				capability.ClockRate, codec.clockRate)

			continue
		}
		if localChannels(capability) != codec.channels {
			reason = fmt.Sprintf("channel count mismatch: local %d, remote %d",
				localChannels(capability), codec.channels)

			continue
		}
		if codec.name == "h264" {
			if h264Reason := matchH264FMTP(parseFMTPParams(capability.FMTP), codec.fmtp); h264Reason != "" {
				reason = h264Reason

				continue
			}
		}

		return capability.FMTP, ""
	}

	return "", reason
}

// matchRTXCodec validates an offered RTX codec: a local RTX capability with
// the same clock rate must exist, the apt parameter must reference a
// primary codec selected in the same media section, and the clock rates of
// RTX and its primary codec must agree.
func matchRTXCodec(
	local []CodecCapability, mediaType string, codec offerCodec, selected map[uint8]selectedCodec,
) (string, string) {
	mimeType := mediaType + "/rtx"

	var capability *CodecCapability
	for i := range local {
		if strings.EqualFold(local[i].MimeType, mimeType) && local[i].ClockRate == codec.clockRate {
			capability = &local[i]

			break
		}
	}
	if capability == nil {
		return "", fmt.Sprintf("no local capability for %s with clock rate %d", mimeType, codec.clockRate)
	}

	apt, ok := codec.fmtp["apt"]
	if !ok {
		return "", "missing apt parameter"
	}
	aptPT, err := strconv.ParseUint(apt, 10, 8)
	if err != nil {
		return "", fmt.Sprintf("malformed apt parameter %q", apt)
	}

	primary, ok := selected[uint8(aptPT)]
	if !ok {
		return "", fmt.Sprintf("associated codec with payload type %d was not selected", aptPT)
	}
	if primary.clockRate != codec.clockRate {
		return "", fmt.Sprintf("clock rate mismatch with associated codec: rtx %d, primary %d",
			codec.clockRate, primary.clockRate)
	}

	return fmt.Sprintf("apt=%d", aptPT), ""
}

// matchH264FMTP applies the RFC 6184 offer/answer rules for the parameters
// the library understands. Unknown parameters are ignored here and never
// copied into the answer.
func matchH264FMTP(local, remote map[string]string) string {
	localPM := local["packetization-mode"]
	remotePM := remote["packetization-mode"]
	if localPM == "" {
		localPM = "0"
	}
	if remotePM == "" {
		remotePM = "0"
	}
	if localPM != remotePM {
		return fmt.Sprintf("packetization-mode mismatch: local %s, remote %s", localPM, remotePM)
	}

	localProfile := local["profile-level-id"]
	remoteProfile := remote["profile-level-id"]
	if localProfile == "" || remoteProfile == "" {
		return "profile-level-id missing on one side"
	}
	if strings.EqualFold(localProfile, remoteProfile) {
		return ""
	}

	asymmetryAllowed := local["level-asymmetry-allowed"] == "1" && remote["level-asymmetry-allowed"] == "1"
	sameProfile := len(localProfile) >= 4 && len(remoteProfile) >= 4 &&
		strings.EqualFold(localProfile[:4], remoteProfile[:4])
	if asymmetryAllowed && sameProfile {
		return ""
	}

	return fmt.Sprintf("incompatible profile-level-id: local %s, remote %s (level asymmetry not allowed)",
		localProfile, remoteProfile)
}

func localChannels(capability *CodecCapability) uint16 {
	if capability.Channels == 0 {
		return 1
	}

	return capability.Channels
}
