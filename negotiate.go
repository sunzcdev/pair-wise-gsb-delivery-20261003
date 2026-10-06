// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package sdp

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// MediaCapability describes one locally supported codec configuration.
//
// Name is the RTP encoding name (e.g. "opus", "H264", "rtx") and is matched
// case-insensitively. ClockRate is the RTP clock rate in Hz. Channels is the
// number of audio channels; zero means unspecified, which is treated as 1.
// Fmtp holds the local a=fmtp parameters as key/value pairs.
//
// An "rtx" capability is associated with the nearest preceding non-rtx
// capability in the local capability list, mirroring how a remote rtx codec
// references its primary codec through the "apt" fmtp parameter.
type MediaCapability struct {
	Name      string
	ClockRate uint32
	Channels  uint16
	Fmtp      map[string]string
}

// RejectedCapability is a local capability that was not selected during
// negotiation, together with the reason it was not usable.
type RejectedCapability struct {
	Capability MediaCapability
	Reason     string
}

// MediaNegotiation is the result of negotiating one media section. Media is
// the usable media description: it carries the remote offer's payload type
// numbers and may be marshaled directly. Rejected lists the local
// capabilities that were not selected and why.
type MediaNegotiation struct {
	Media    *MediaDescription
	Rejected []RejectedCapability
}

var errNilMediaDescription = errors.New("offer media description is nil")

// NegotiateMedia matches local capabilities against one media section of a
// remote offer and returns the usable media description plus the reasons for
// every local capability that was not selected.
//
// The negotiation understands opus, H264 and rtx semantics: clock rates and
// channel counts must match, H264 profile-level-id, packetization-mode and
// level-asymmetry-allowed are evaluated per RFC 6184, and an rtx codec is
// only usable when the primary codec referenced by its "apt" parameter was
// also negotiated. fmtp parameters that are not understood are handled with
// the declared conservative policy: they must be present with identical
// values on both sides, otherwise the codec is rejected.
//
// The returned media description always uses the remote offer's payload type
// numbers; local numbering is never carried into the answer.
//
// Offers with multiple media sections are negotiated by calling
// NegotiateMedia once per section. Payload type numbers are interpreted per
// section, so different sections may reuse the same numbers.
func NegotiateMedia(local []MediaCapability, offer *MediaDescription) (*MediaNegotiation, error) {
	if offer == nil {
		return nil, errNilMediaDescription
	}

	remote, order := remoteCodecs(offer)

	used := make([]bool, len(local))
	selected := map[uint8]int{} // remote primary payload type -> local capability index
	selectedRTX := map[uint8]struct{}{}

	// Primary codecs first, in the offer's preference order.
	for _, pt := range order {
		remoteCodec := remote[pt]
		if isRTXCodec(remoteCodec.Name) {
			continue
		}
		for i := range local {
			if used[i] || isRTXCodec(local[i].Name) {
				continue
			}
			if ok, _ := matchCapability(local[i], remoteCodec); ok {
				used[i] = true
				selected[pt] = i

				break
			}
		}
	}

	// RTX codecs depend on the fate of their primary codec via "apt".
	for _, pt := range order {
		remoteCodec := remote[pt]
		if !isRTXCodec(remoteCodec.Name) {
			continue
		}
		apt, ok := parseFmtpParameters(remoteCodec.Fmtp)["apt"]
		if !ok {
			continue
		}
		aptPT, err := strconv.ParseUint(apt, 10, 8)
		if err != nil {
			continue
		}
		primary, ok := selected[uint8(aptPT)]
		if !ok {
			continue
		}
		for _, rtxIdx := range associatedRTX(local, primary) {
			if used[rtxIdx] {
				continue
			}
			if ok, _ := matchCapability(local[rtxIdx], remoteCodec); ok {
				used[rtxIdx] = true
				selectedRTX[pt] = struct{}{}

				break
			}
		}
	}

	inAnswer := func(pt uint8) bool {
		if _, ok := selected[pt]; ok {
			return true
		}
		_, ok := selectedRTX[pt]

		return ok
	}

	media := &MediaDescription{
		MediaName: MediaName{
			Media:  offer.MediaName.Media,
			Port:   offer.MediaName.Port,
			Protos: append([]string(nil), offer.MediaName.Protos...),
		},
	}
	for _, pt := range order {
		if inAnswer(pt) {
			media.MediaName.Formats = append(media.MediaName.Formats, strconv.Itoa(int(pt)))
		}
	}
	for _, attr := range offer.Attributes {
		switch attr.Key {
		case "rtpmap", "fmtp":
			if pt, ok := attributePayloadType(attr.Value); ok && inAnswer(pt) {
				media.Attributes = append(media.Attributes, attr)
			}
		case "rtcp-fb":
			if strings.HasPrefix(attr.Value, "*") {
				media.Attributes = append(media.Attributes, attr)
			} else if pt, ok := attributePayloadType(attr.Value); ok && inAnswer(pt) {
				media.Attributes = append(media.Attributes, attr)
			}
		}
	}

	result := &MediaNegotiation{Media: media}
	for i := range local {
		if used[i] {
			continue
		}
		result.Rejected = append(result.Rejected, RejectedCapability{
			Capability: local[i],
			Reason:     rejectionReason(local, i, remote, order, selected),
		})
	}

	return result, nil
}

// remoteCodecs collects the codecs declared by a single media description.
// It deliberately does not look at other media sections so that payload type
// numbers reused across sections are interpreted correctly.
func remoteCodecs(md *MediaDescription) (map[uint8]Codec, []uint8) {
	codecs := map[uint8]Codec{}
	for _, attr := range md.Attributes {
		var (
			codec Codec
			err   error
		)
		switch attr.Key {
		case "rtpmap":
			codec, err = parseRtpmap(attr.Key + ":" + attr.Value)
		case "fmtp":
			codec, err = parseFmtp(attr.Key + ":" + attr.Value)
		default:
			continue
		}
		if err != nil {
			continue
		}
		mergeCodecs(codec, codecs)
	}

	var order []uint8
	for _, format := range md.MediaName.Formats {
		pt, err := strconv.ParseUint(format, 10, 8)
		if err != nil {
			continue
		}
		if _, ok := codecs[uint8(pt)]; ok {
			order = append(order, uint8(pt))
		}
	}

	return codecs, order
}

func isRTXCodec(name string) bool {
	return strings.EqualFold(name, "rtx")
}

// associatedRTX returns the indices of the local rtx capabilities associated
// with the primary capability at index primary.
func associatedRTX(local []MediaCapability, primary int) []int {
	var indices []int
	for i := primary + 1; i < len(local) && isRTXCodec(local[i].Name); i++ {
		indices = append(indices, i)
	}

	return indices
}

// primaryForRTX returns the index of the primary capability associated with
// the local rtx capability at index idx, or -1 if there is none.
func primaryForRTX(local []MediaCapability, idx int) int {
	for i := idx - 1; i >= 0; i-- {
		if !isRTXCodec(local[i].Name) {
			return i
		}
	}

	return -1
}

func matchCapability(local MediaCapability, remote Codec) (bool, string) {
	if !strings.EqualFold(local.Name, remote.Name) {
		return false, fmt.Sprintf("codec name %q does not match %q", local.Name, remote.Name)
	}
	if local.ClockRate != remote.ClockRate {
		return false, fmt.Sprintf("clock rate %d does not match remote %d", local.ClockRate, remote.ClockRate)
	}

	remoteChannels := uint16(1)
	if remote.EncodingParameters != "" {
		channels, err := strconv.ParseUint(remote.EncodingParameters, 10, 16)
		if err != nil {
			return false, fmt.Sprintf("remote encoding parameters %q are not understood", remote.EncodingParameters)
		}
		remoteChannels = uint16(channels)
	}
	localChannels := local.Channels
	if localChannels == 0 {
		localChannels = 1
	}
	if localChannels != remoteChannels {
		return false, fmt.Sprintf("channel count %d does not match remote %d", localChannels, remoteChannels)
	}

	return matchFmtp(local, remote)
}

func matchFmtp(local MediaCapability, remote Codec) (bool, string) {
	remoteParams := parseFmtpParameters(remote.Fmtp)
	if strings.EqualFold(local.Name, "h264") {
		return matchH264Fmtp(local.Fmtp, remoteParams)
	}

	ignored := map[string]bool{}
	if isRTXCodec(local.Name) {
		// "apt" references payload type numbers and is matched through
		// capability association, not string comparison.
		ignored["apt"] = true
	}

	return matchFmtpConservative(local.Fmtp, remoteParams, ignored)
}

// matchFmtpConservative implements the declared policy for parameters that
// are not otherwise understood: both sides must either omit a parameter or
// carry the identical value.
func matchFmtpConservative(local, remote map[string]string, ignored map[string]bool) (bool, string) {
	keys := map[string]struct{}{}
	for key := range local {
		keys[key] = struct{}{}
	}
	for key := range remote {
		keys[key] = struct{}{}
	}
	sorted := make([]string, 0, len(keys))
	for key := range keys {
		sorted = append(sorted, key)
	}
	sort.Strings(sorted)

	for _, key := range sorted {
		if ignored[key] {
			continue
		}
		localValue, localOK := local[key]
		remoteValue, remoteOK := remote[key]
		if localOK != remoteOK {
			return false, fmt.Sprintf("fmtp parameter %q is present on only one side", key)
		}
		if localOK && localValue != remoteValue {
			return false, fmt.Sprintf("fmtp parameter %q mismatch: local %q, remote %q", key, localValue, remoteValue)
		}
	}

	return true, ""
}

// matchH264Fmtp applies the RFC 6184 parameter rules. profile-level-id and
// packetization-mode are evaluated semantically; every other parameter falls
// back to the conservative policy.
func matchH264Fmtp(local, remote map[string]string) (bool, string) {
	localProfile, localHasProfile := local["profile-level-id"]
	remoteProfile, remoteHasProfile := remote["profile-level-id"]
	if localHasProfile != remoteHasProfile {
		return false, "h264 profile-level-id is present on only one side"
	}
	if localHasProfile {
		if len(localProfile) != 6 || len(remoteProfile) != 6 {
			return false, "h264 profile-level-id is malformed"
		}
		if !strings.EqualFold(localProfile[:4], remoteProfile[:4]) {
			return false, fmt.Sprintf(
				"h264 profile mismatch: local %q, remote %q", localProfile[:4], remoteProfile[:4])
		}
		if !strings.EqualFold(localProfile[4:], remoteProfile[4:]) {
			asymmetryAllowed := local["level-asymmetry-allowed"] == "1" &&
				remote["level-asymmetry-allowed"] == "1"
			if !asymmetryAllowed {
				return false, fmt.Sprintf(
					"h264 level mismatch (%q vs %q) and level-asymmetry-allowed is not set on both sides",
					localProfile[4:], remoteProfile[4:])
			}
		}
	}

	packetizationMode := func(params map[string]string) string {
		if mode, ok := params["packetization-mode"]; ok {
			return mode
		}

		return "0"
	}
	if packetizationMode(local) != packetizationMode(remote) {
		return false, fmt.Sprintf(
			"h264 packetization-mode mismatch: local %q, remote %q",
			packetizationMode(local), packetizationMode(remote))
	}

	return matchFmtpConservative(local, remote, map[string]bool{
		"profile-level-id":        true,
		"packetization-mode":      true,
		"level-asymmetry-allowed": true,
	})
}

func rejectionReason(
	local []MediaCapability,
	idx int,
	remote map[uint8]Codec,
	order []uint8,
	selected map[uint8]int,
) string {
	capability := local[idx]

	if isRTXCodec(capability.Name) {
		primary := primaryForRTX(local, idx)
		if primary < 0 {
			return "rtx capability has no preceding primary codec"
		}
		for _, localIdx := range selected {
			if localIdx == primary {
				return "remote offer has no compatible rtx codec for the associated primary codec"
			}
		}

		return "associated primary codec was not negotiated"
	}

	reason := fmt.Sprintf("no %s codec in remote offer", capability.Name)
	for _, pt := range order {
		remoteCodec := remote[pt]
		if isRTXCodec(remoteCodec.Name) || !strings.EqualFold(remoteCodec.Name, capability.Name) {
			continue
		}
		if _, mismatch := matchCapability(capability, remoteCodec); mismatch != "" {
			reason = mismatch

			break
		}
	}

	return reason
}

func parseFmtpParameters(fmtp string) map[string]string {
	params := map[string]string{}
	for _, part := range strings.Split(fmtp, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if key, value, ok := strings.Cut(part, "="); ok {
			params[strings.TrimSpace(key)] = strings.TrimSpace(value)
		} else {
			params[part] = ""
		}
	}

	return params
}

func attributePayloadType(value string) (uint8, bool) {
	fields := strings.Fields(value)
	if len(fields) == 0 {
		return 0, false
	}
	pt, err := strconv.ParseUint(fields[0], 10, 8)
	if err != nil {
		return 0, false
	}

	return uint8(pt), true
}
