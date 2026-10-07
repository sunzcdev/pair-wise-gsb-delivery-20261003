// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package sdp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTrickleICEFragmentMergerInterleavedMedia(t *testing.T) {
	original := trickleTestDescription(t)
	merger := NewTrickleICEFragmentMerger(original)

	messages := []struct {
		name string
		body string
	}{
		{
			name: "audio first candidate",
			body: "a=mid:0\r\n" +
				"a=candidate:1 1 UDP 2122252543 198.51.100.1 10000 typ host\r\n",
		},
		{
			name: "video first candidate",
			body: "a=mid:1\r\n" +
				"a=candidate:1 1 UDP 2122252543 198.51.100.2 20000 typ host\r\n",
		},
		{
			name: "audio duplicate and second candidate",
			body: "a=mid:0\r\n" +
				"a=candidate:1 1 UDP 2122252543 198.51.100.1 10000 typ host\r\n" +
				"a=candidate:2 1 UDP 2122194687 198.51.100.1 10001 typ host\r\n",
		},
		{
			name: "video duplicate only",
			body: "a=mid:1\r\n" +
				"a=candidate:1 1 UDP 2122252543 198.51.100.2 20000 typ host\r\n",
		},
		{
			name: "audio end of candidates",
			body: "a=mid:0\r\na=end-of-candidates\r\n",
		},
		{
			name: "audio late after end",
			body: "a=mid:0\r\n" +
				"a=candidate:3 1 UDP 2122063359 198.51.100.1 10002 typ host\r\n",
		},
		{
			name: "video still accepts candidate",
			body: "a=mid:1\r\n" +
				"a=candidate:2 1 UDP 2122194687 198.51.100.2 20001 typ host\r\n",
		},
	}

	for _, message := range messages {
		if message.name == "audio late after end" {
			_, err := merger.MergeString(message.body)
			require.ErrorIs(t, err, ErrTrickleICELateCandidate, message.name)
			continue
		}

		_, err := merger.MergeString(message.body)
		require.NoError(t, err, message.name)
	}

	merged := merger.SessionDescription()
	audio := findMediaByMID(merged, "0")
	video := findMediaByMID(merged, "1")

	assert.Equal(t, []string{
		"1 1 UDP 2122252543 198.51.100.1 10000 typ host",
		"2 1 UDP 2122194687 198.51.100.1 10001 typ host",
	}, candidateValues(audio.Attributes))
	assert.True(t, hasAttribute(audio.Attributes, AttrKeyEndOfCandidates))

	assert.Equal(t, []string{
		"1 1 UDP 2122252543 198.51.100.2 20000 typ host",
		"2 1 UDP 2122194687 198.51.100.2 20001 typ host",
	}, candidateValues(video.Attributes))
	assert.False(t, hasAttribute(video.Attributes, AttrKeyEndOfCandidates))

	originalAudio := findMediaByMID(original, "0")
	originalVideo := findMediaByMID(original, "1")
	assert.Empty(t, candidateValues(originalAudio.Attributes))
	assert.Empty(t, candidateValues(originalVideo.Attributes))
	assert.False(t, hasAttribute(originalAudio.Attributes, AttrKeyEndOfCandidates))
}

func TestTrickleICEFragmentMergerCredentialGeneration(t *testing.T) {
	original := trickleTestDescription(t)
	merger := NewTrickleICEFragmentMerger(original)

	_, err := merger.MergeString("a=mid:1\r\n" +
		"a=candidate:1 1 UDP 2122252543 198.51.100.2 20000 typ host\r\n")
	require.NoError(t, err)

	restart := "a=mid:1\r\n" +
		"a=ice-ufrag:newfrag\r\n" +
		"a=ice-pwd:newpassword\r\n" +
		"a=candidate:10 1 UDP 2122252543 203.0.113.2 30000 typ host\r\n"
	_, err = merger.MergeString(restart)
	require.NoError(t, err)

	_, err = merger.MergeString("a=mid:0\r\n" +
		"a=candidate:4 1 UDP 2121932799 198.51.100.1 10003 typ host\r\n")
	require.NoError(t, err)

	_, err = merger.MergeString("a=mid:1\r\n" +
		"a=candidate:11 1 UDP 2122194687 203.0.113.2 30001 typ host\r\n")
	require.NoError(t, err)

	_, err = merger.MergeString("a=mid:1\r\n" +
		"a=ice-ufrag:ufrag2\r\n" +
		"a=ice-pwd:pwd2\r\n" +
		"a=candidate:20 1 UDP 2122063359 203.0.113.2 30002 typ host\r\n")
	require.NoError(t, err)

	_, err = merger.MergeString("a=mid:0\r\n" +
		"a=candidate:5 1 UDP 2121867263 198.51.100.1 10004 typ host\r\n")
	require.NoError(t, err)

	_, err = merger.MergeString("a=mid:1\r\n" +
		"a=ice-ufrag:newfrag\r\n" +
		"a=ice-pwd:newpassword\r\n" +
		"a=candidate:12 1 UDP 2121998335 203.0.113.2 30003 typ host\r\n")
	require.ErrorIs(t, err, ErrTrickleICEStaleFragment)

	_, err = merger.MergeString("a=mid:1\r\na=end-of-candidates\r\n")
	require.NoError(t, err)
	_, err = merger.MergeString("a=mid:1\r\n" +
		"a=ice-ufrag:newfrag\r\n" +
		"a=ice-pwd:newpassword\r\n" +
		"a=candidate:13 1 UDP 2121867263 203.0.113.2 30005 typ host\r\n")
	require.ErrorIs(t, err, ErrTrickleICEStaleFragment)

	_, err = merger.MergeString("a=mid:1\r\n" +
		"a=candidate:21 1 UDP 2121932799 203.0.113.2 30004 typ host\r\n")
	require.ErrorIs(t, err, ErrTrickleICELateCandidate)

	merged := merger.SessionDescription()
	video := findMediaByMID(merged, "1")
	ufrag, ok := video.Attribute(AttrKeyICEUfrag)
	require.True(t, ok)
	password, ok := video.Attribute(AttrKeyICEPassword)
	require.True(t, ok)
	assert.Equal(t, "ufrag2", ufrag)
	assert.Equal(t, "pwd2", password)
	assert.Equal(t, []string{
		"20 1 UDP 2122063359 203.0.113.2 30002 typ host",
	}, candidateValues(video.Attributes))
	assert.True(t, hasAttribute(video.Attributes, AttrKeyEndOfCandidates))

	audio := findMediaByMID(merged, "0")
	assert.Equal(t, []string{
		"4 1 UDP 2121932799 198.51.100.1 10003 typ host",
		"5 1 UDP 2121867263 198.51.100.1 10004 typ host",
	}, candidateValues(audio.Attributes))
}

func TestTrickleICEFragmentMissingContext(t *testing.T) {
	merger := NewTrickleICEFragmentMerger(trickleTestDescription(t))

	t.Run("empty fragment", func(t *testing.T) {
		_, err := merger.MergeString("")
		var missing TrickleICEMissingContextError
		require.ErrorAs(t, err, &missing)
		assert.True(t, missing.MissingMID)
	})

	t.Run("missing mid", func(t *testing.T) {
		_, err := merger.MergeString("a=candidate:1 1 UDP 2122252543 198.51.100.1 10000 typ host\r\n")
		var missing TrickleICEMissingContextError
		require.ErrorAs(t, err, &missing)
		assert.True(t, missing.MissingMID)
	})

	t.Run("unknown mid", func(t *testing.T) {
		_, err := merger.MergeString("a=mid:2\r\na=end-of-candidates\r\n")
		var missing TrickleICEMissingContextError
		require.ErrorAs(t, err, &missing)
		assert.Equal(t, "2", missing.MID)
		assert.True(t, missing.MissingMedia)
	})

	t.Run("missing password when description has no credentials", func(t *testing.T) {
		sd := trickleTestDescription(t)
		for _, media := range sd.MediaDescriptions {
			media.Attributes = removeAttributes(media.Attributes, AttrKeyICEUfrag, AttrKeyICEPassword)
		}
		noCredentials := NewTrickleICEFragmentMerger(sd)

		_, err := noCredentials.MergeString("a=mid:0\r\n" +
			"a=ice-ufrag:onlyufrag\r\n")
		var missing TrickleICEMissingContextError
		require.ErrorAs(t, err, &missing)
		assert.Equal(t, "0", missing.MID)
		assert.True(t, missing.MissingPassword)

	})
}

func TestTrickleICEFragmentParserRejectsMultipleMediaSDP(t *testing.T) {
	fragment := new(TrickleICESDPFragment)
	err := fragment.UnmarshalString("v=0\r\n" +
		"o=- 0 0 IN IP4 127.0.0.1\r\n" +
		"s=-\r\n" +
		"m=audio 9 UDP/TLS/RTP/SAVPF 111\r\n" +
		"a=mid:0\r\n" +
		"m=video 9 UDP/TLS/RTP/SAVPF 96\r\n" +
		"a=mid:1\r\n")
	require.ErrorIs(t, err, ErrTrickleICEInvalidFragment)
}

func TestTrickleICEFragmentParserAcceptsStandardForms(t *testing.T) {
	t.Run("full media section", func(t *testing.T) {
		fragment, err := NewTrickleICESDPFragment("v=0\r\n" +
			"o=- 0 0 IN IP4 127.0.0.1\r\n" +
			"s=-\r\n" +
			"t=0 0\r\n" +
			"m=audio 9 UDP/TLS/RTP/SAVPF 111\r\n" +
			"a=mid:0\r\n" +
			"a=candidate:1 1 UDP 2122252543 198.51.100.1 10000 typ host\r\n")
		require.NoError(t, err)
		assert.Equal(t, "0", fragment.MID)
		assert.Equal(t, []string{"1 1 UDP 2122252543 198.51.100.1 10000 typ host"}, fragment.Candidates)
	})

	t.Run("compact mid", func(t *testing.T) {
		fragment, err := NewTrickleICESDPFragment("*:1\r\na=end-of-candidates\r\n")
		require.NoError(t, err)
		assert.Equal(t, "1", fragment.MID)
		assert.True(t, fragment.EndOfCandidates)
	})

	t.Run("compact end of candidates", func(t *testing.T) {
		fragment, err := NewTrickleICESDPFragment("*:1 end-of-candidates\r\n")
		require.NoError(t, err)
		assert.Equal(t, "1", fragment.MID)
		assert.True(t, fragment.EndOfCandidates)
	})

	t.Run("multiple media sections rejected", func(t *testing.T) {
		err := new(TrickleICESDPFragment).UnmarshalString(
			"m=audio 9 UDP/TLS/RTP/SAVPF 111\r\na=mid:0\r\n" +
				"m=video 9 UDP/TLS/RTP/SAVPF 96\r\na=mid:1\r\n")
		require.ErrorIs(t, err, ErrTrickleICEInvalidFragment)
	})
}

func TestTrickleICEFragmentMergerFailureDoesNotModifyOutput(t *testing.T) {
	merger := NewTrickleICEFragmentMerger(trickleTestDescription(t))
	_, err := merger.MergeString("a=mid:0\r\n" +
		"a=candidate:1 1 UDP 2122252543 198.51.100.1 10000 typ host\r\n")
	require.NoError(t, err)

	_, err = merger.MergeString("a=mid:0\r\na=end-of-candidates\r\n")
	require.NoError(t, err)

	_, err = merger.MergeString("a=mid:0\r\n" +
		"a=candidate:1 1 UDP 2122252543 198.51.100.1 10000 typ host\r\n")
	require.NoError(t, err)

	before := merger.SessionDescription()
	_, err = merger.MergeString("a=mid:0\r\n" +
		"a=candidate:3 1 UDP 2122063359 198.51.100.1 10002 typ host\r\n")
	require.ErrorIs(t, err, ErrTrickleICELateCandidate)

	after := merger.SessionDescription()
	assert.Equal(t, before, after)
}

func TestMergeTrickleICESDPFragmentConvenience(t *testing.T) {
	sd, err := NewJSEPSessionDescription(false)
	require.NoError(t, err)
	sd.WithMedia(NewJSEPMediaDescription("video", nil).WithValueAttribute(AttrKeyMID, "1"))

	merged, err := MergeTrickleICESDPFragment(sd, "a=mid:1\r\n"+
		"a=ice-ufrag:single\r\n"+
		"a=ice-pwd:singlepwd\r\n"+
		"a=candidate:1 1 UDP 2122252543 198.51.100.2 20000 typ host\r\n")
	require.NoError(t, err)

	video := findMediaByMID(merged, "1")
	assert.Equal(t, []string{"1 1 UDP 2122252543 198.51.100.2 20000 typ host"}, candidateValues(video.Attributes))
	assert.Empty(t, candidateValues(findMediaByMID(sd, "1").Attributes))
}

func trickleTestDescription(t *testing.T) *SessionDescription {
	t.Helper()

	sd, err := NewJSEPSessionDescription(false)
	require.NoError(t, err)

	audio := NewJSEPMediaDescription("audio", nil).
		WithValueAttribute(AttrKeyMID, "0").
		WithICECredentials("ufraga", "pwdsecret12345678901234")
	video := NewJSEPMediaDescription("video", nil).
		WithValueAttribute(AttrKeyMID, "1").
		WithICECredentials("ufragv", "pwdsecret12345678901235")

	return sd.WithMedia(audio).WithMedia(video)
}

func candidateValues(attributes []Attribute) []string {
	candidates := make([]string, 0)
	for _, attribute := range attributes {
		if attribute.Key == AttrKeyCandidate {
			candidates = append(candidates, attribute.Value)
		}
	}

	return candidates
}
