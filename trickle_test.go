// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package sdp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTrickleTestDescription(t *testing.T) *SessionDescription {
	t.Helper()

	desc := &SessionDescription{}
	require.NoError(t, desc.UnmarshalString(`v=0
o=- 4611731400430051336 2 IN IP4 127.0.0.1
s=-
t=0 0
m=audio 9 UDP/TLS/RTP/SAVPF 111
c=IN IP4 0.0.0.0
a=mid:0
a=ice-ufrag:base-ufrag
a=ice-pwd:base-pwd
m=video 9 UDP/TLS/RTP/SAVPF 96
c=IN IP4 0.0.0.0
a=mid:1
a=ice-ufrag:base-ufrag
a=ice-pwd:base-pwd
`))

	return desc
}

func candidateValues(media *MediaDescription) []string {
	candidates := []string{}
	for _, attr := range media.Attributes {
		if attr.Key == AttrKeyCandidate {
			candidates = append(candidates, attr.Value)
		}
	}

	return candidates
}

func TestUnmarshalTrickleICESDPFrag(t *testing.T) {
	t.Run("single section with candidates", func(t *testing.T) {
		frag, err := UnmarshalTrickleICESDPFrag(`m=audio 9 UDP/TLS/RTP/SAVPF 111
a=mid:0
a=candidate:842163049 1 udp 1677729535 203.0.113.5 60000 typ srflx
a=end-of-candidates
`)
		require.NoError(t, err)
		require.Len(t, frag.Sections, 1)

		section := frag.Sections[0]
		assert.Equal(t, "0", section.MID)
		assert.True(t, section.EndOfCandidates)
		require.Len(t, section.Candidates, 1)
		assert.Contains(t, section.Candidates[0], "203.0.113.5 60000")
		assert.False(t, section.HasCredentials())
	})

	t.Run("multiple interleaved sections and credentials", func(t *testing.T) {
		frag, err := UnmarshalTrickleICESDPFrag(`m=audio 9 UDP/TLS/RTP/SAVPF 111
a=mid:0
a=ice-ufrag:u1
a=ice-pwd:p1
a=candidate:1 1 udp 1 192.0.2.1 10000 typ host
m=video 9 UDP/TLS/RTP/SAVPF 96
a=mid:1
a=ice-ufrag:u2
a=ice-pwd:p2
a=candidate:2 1 udp 1 192.0.2.2 10002 typ host
a=end-of-candidates
`)
		require.NoError(t, err)
		require.Len(t, frag.Sections, 2)
		assert.Equal(t, "0", frag.Sections[0].MID)
		assert.Equal(t, "u1", frag.Sections[0].Ufrag)
		assert.Equal(t, "p1", frag.Sections[0].Pwd)
		assert.Len(t, frag.Sections[0].Candidates, 1)
		assert.False(t, frag.Sections[0].EndOfCandidates)
		assert.Equal(t, "1", frag.Sections[1].MID)
		assert.Len(t, frag.Sections[1].Candidates, 1)
		assert.True(t, frag.Sections[1].EndOfCandidates)
	})

	t.Run("missing mid does not guess an index", func(t *testing.T) {
		_, err := UnmarshalTrickleICESDPFrag(`m=audio 9 UDP/TLS/RTP/SAVPF 111
a=candidate:1 1 udp 1 192.0.2.1 10000 typ host
`)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrTrickleICESectionMIDMissing)
	})

	t.Run("full SDP is rejected instead of parsed as a fragment", func(t *testing.T) {
		_, err := UnmarshalTrickleICESDPFrag(`v=0
o=- 1 2 IN IP4 127.0.0.1
s=-
t=0 0
m=audio 9 UDP/TLS/RTP/SAVPF 111
a=mid:0
`)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrTrickleICESDPFragInvalid)
	})

	t.Run("empty fragment is invalid", func(t *testing.T) {
		_, err := UnmarshalTrickleICESDPFrag("")
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrTrickleICESDPFragInvalid)
	})
}

func TestMergeTrickleICESDPFrag(t *testing.T) {
	const candidateA = "1 1 udp 2122252543 192.0.2.1 50000 typ host"
	const candidateB = "2 1 udp 2122252542 192.0.2.2 50001 typ host"

	t.Run("merges interleaved sections by mid", func(t *testing.T) {
		desc := newTrickleTestDescription(t)

		merged, err := desc.MergeTrickleICESDPFrag(`m=audio 9 UDP/TLS/RTP/SAVPF 111
a=mid:0
a=candidate:` + candidateA + `
m=video 9 UDP/TLS/RTP/SAVPF 96
a=mid:1
a=candidate:` + candidateB + `
a=end-of-candidates
`)
		require.NoError(t, err)

		audio := findMediaDescriptionByMID(merged, "0")
		video := findMediaDescriptionByMID(merged, "1")
		assert.Equal(t, []string{candidateA}, candidateValues(audio))
		assert.False(t, hasPropertyAttribute(audio.Attributes, AttrKeyEndOfCandidates))
		assert.Equal(t, []string{candidateB}, candidateValues(video))
		assert.True(t, hasPropertyAttribute(video.Attributes, AttrKeyEndOfCandidates))
	})

	t.Run("de-duplicates repeated candidates", func(t *testing.T) {
		desc := newTrickleTestDescription(t)

		frag := `m=audio 9 UDP/TLS/RTP/SAVPF 111
a=mid:0
a=candidate:` + candidateA + `
a=candidate:` + candidateA + `
`
		merged, err := desc.MergeTrickleICESDPFrag(frag)
		require.NoError(t, err)
		merged, err = merged.MergeTrickleICESDPFrag(frag)
		require.NoError(t, err)

		assert.Equal(t, []string{candidateA},
			candidateValues(findMediaDescriptionByMID(merged, "0")))
	})

	t.Run("rejects candidates after end-of-candidates", func(t *testing.T) {
		desc := newTrickleTestDescription(t)

		ended, err := desc.MergeTrickleICESDPFrag(`m=audio 9 UDP/TLS/RTP/SAVPF 111
a=mid:0
a=candidate:` + candidateA + `
a=end-of-candidates
`)
		require.NoError(t, err)

		before, err := ended.Marshal()
		require.NoError(t, err)

		_, err = ended.MergeTrickleICESDPFrag(`m=audio 9 UDP/TLS/RTP/SAVPF 111
a=mid:0
a=candidate:` + candidateB + `
`)
		require.Error(t, err)

		after, err := ended.Marshal()
		require.NoError(t, err)
		assert.Equal(t, before, after, "failed merge must not modify the description")
		assert.Equal(t, []string{candidateA},
			candidateValues(findMediaDescriptionByMID(ended, "0")))
	})

	t.Run("unknown mid reports available mids and does not mutate", func(t *testing.T) {
		desc := newTrickleTestDescription(t)
		before, err := desc.Marshal()
		require.NoError(t, err)

		_, err = desc.MergeTrickleICESDPFrag(`m=audio 9 UDP/TLS/RTP/SAVPF 111
a=mid:9
a=candidate:` + candidateA + `
`)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrTrickleICEMIDNotFound)
		assert.Contains(t, err.Error(), "0")

		after, err := desc.Marshal()
		require.NoError(t, err)
		assert.Equal(t, before, after)
	})
}

func TestTrickleICEMergerGenerations(t *testing.T) {
	const (
		firstCandidate  = "1 1 udp 1 192.0.2.1 10000 typ host"
		secondCandidate = "2 1 udp 1 192.0.2.2 10001 typ host"
		staleCandidate  = "9 1 udp 1 192.0.2.9 10099 typ host"
	)

	firstCreds := `a=ice-ufrag:first
a=ice-pwd:first-pwd`
	secondCreds := `a=ice-ufrag:second
a=ice-pwd:second-pwd`

	section := func(mid, body string) string {
		return "m=audio 9 UDP/TLS/RTP/SAVPF 111\r\na=mid:" + mid + "\r\n" + body + "\r\n"
	}

	t.Run("new credentials replace the candidate set", func(t *testing.T) {
		desc := newTrickleTestDescription(t)
		merger := NewTrickleICEMerger()

		desc, err := merger.Merge(desc, section("0", firstCreds+"\r\na=candidate:"+firstCandidate))
		require.NoError(t, err)

		desc, err = merger.Merge(desc, section("0", secondCreds+"\r\na=candidate:"+secondCandidate))
		require.NoError(t, err)

		audio := findMediaDescriptionByMID(desc, "0")
		ufrag, _ := audio.Attribute(AttrKeyICEUfrag)
		pwd, _ := audio.Attribute(AttrKeyICEPwd)
		assert.Equal(t, "second", ufrag)
		assert.Equal(t, "second-pwd", pwd)
		assert.Equal(t, []string{secondCandidate}, candidateValues(audio))
		assert.False(t, hasPropertyAttribute(audio.Attributes, AttrKeyEndOfCandidates))
	})

	t.Run("late fragment of an earlier generation is rejected", func(t *testing.T) {
		desc := newTrickleTestDescription(t)
		merger := NewTrickleICEMerger()

		desc, err := merger.Merge(desc, section("0", firstCreds+"\r\na=candidate:"+firstCandidate))
		require.NoError(t, err)
		desc, err = merger.Merge(desc, section("0", secondCreds+"\r\na=candidate:"+secondCandidate))
		require.NoError(t, err)

		before, err := desc.Marshal()
		require.NoError(t, err)

		_, err = merger.Merge(desc, section("0",
			firstCreds+"\r\na=candidate:"+staleCandidate))
		require.Error(t, err)

		after, err := desc.Marshal()
		require.NoError(t, err)
		assert.Equal(t, before, after)

		audio := findMediaDescriptionByMID(desc, "0")
		assert.Equal(t, []string{secondCandidate}, candidateValues(audio))
	})

	t.Run("restart reopens candidate flow after end-of-candidates", func(t *testing.T) {
		desc := newTrickleTestDescription(t)
		merger := NewTrickleICEMerger()

		desc, err := merger.Merge(desc, section("0", firstCreds+"\r\na=candidate:"+firstCandidate+
			"\r\na=end-of-candidates"))
		require.NoError(t, err)

		// Same generation after the marker is late.
		_, err = merger.Merge(desc, section("0", "a=candidate:"+staleCandidate))
		require.Error(t, err)

		// New generation after the marker is a fresh candidate set.
		desc, err = merger.Merge(desc, section("0", secondCreds+"\r\na=candidate:"+secondCandidate))
		require.NoError(t, err)
		audio := findMediaDescriptionByMID(desc, "0")
		assert.Equal(t, []string{secondCandidate}, candidateValues(audio))
		assert.False(t, hasPropertyAttribute(audio.Attributes, AttrKeyEndOfCandidates))
	})

	t.Run("partial credential change is an error", func(t *testing.T) {
		desc := newTrickleTestDescription(t)
		merger := NewTrickleICEMerger()

		_, err := merger.Merge(desc, section("0", "a=ice-ufrag:new\r\na=ice-pwd:base-pwd"))
		require.Error(t, err)
	})

	t.Run("zero value merger is ready to use", func(t *testing.T) {
		desc := newTrickleTestDescription(t)
		var merger TrickleICEMerger

		merged, err := merger.Merge(desc, section("0", "a=candidate:"+firstCandidate))
		require.NoError(t, err)
		assert.Equal(t, []string{firstCandidate},
			candidateValues(findMediaDescriptionByMID(merged, "0")))
	})
}

func TestSessionDescriptionClone(t *testing.T) {
	desc := newTrickleTestDescription(t)

	clone := desc.Clone()
	clone.Attributes = append(clone.Attributes, NewPropertyAttribute(AttrKeyRTCPMux))
	clone.MediaDescriptions[0].Attributes = append(
		clone.MediaDescriptions[0].Attributes,
		NewAttribute(AttrKeyCandidate, "x"),
	)
	clone.MediaDescriptions[0].Bandwidth = append(clone.MediaDescriptions[0].Bandwidth, Bandwidth{})

	assert.Empty(t, desc.Attributes)
	assert.NotContains(t,
		candidateValues(desc.MediaDescriptions[0]),
		"x")
	assert.Empty(t, desc.MediaDescriptions[0].Bandwidth)

	// The clone remains independently marshalable.
	_, err := clone.Marshal()
	require.NoError(t, err)

	assert.Nil(t, (*SessionDescription)(nil).Clone())
}
