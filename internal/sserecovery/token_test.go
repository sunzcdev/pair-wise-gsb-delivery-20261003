package sserecovery

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func fixedClock() (*time.Time, func() time.Time) {
	base := time.Unix(1_700_000_000, 0)
	return &base, func() time.Time { return base }
}

func testSigner(t *testing.T, opts ...Option) *Signer {
	t.Helper()
	_, now := fixedClock()
	s, err := NewSigner("k1", []byte("current-secret"), Limits{
		MaxTokenLength:   4096,
		MaxChannels:      4,
		MaxChannelLength: 32,
		TTL:              time.Minute,
	}, append([]Option{WithNow(now)}, opts...)...)
	require.NoError(t, err)
	return s
}

func sampleCursor() Cursor {
	return Cursor{
		User: "user-42",
		Channels: []Channel{
			{Channel: "ch:b", Epoch: "ep2", Offset: 55},
			{Channel: "ch:a", Epoch: "ep1", Offset: 11},
		},
	}
}

func TestIssueVerifyRoundTrip(t *testing.T) {
	s := testSigner(t)
	token, err := s.Issue(sampleCursor())
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(token, string(Version)+"k1."))

	got, err := s.Verify(token)
	require.NoError(t, err)
	require.Equal(t, "user-42", got.User)
	// Entries come back sorted and bound to the user.
	require.Equal(t, []Channel{
		{Channel: "ch:a", Epoch: "ep1", Offset: 11},
		{Channel: "ch:b", Epoch: "ep2", Offset: 55},
	}, got.Channels)
	require.True(t, got.ChannelSetEqual([]string{"ch:a", "ch:b"}))

	ch, ok := got.Position("ch:a")
	require.True(t, ok)
	require.Equal(t, uint64(11), ch.Offset)
	_, ok = got.Position("missing")
	require.False(t, ok)
}

func TestVerifyRejectsTampering(t *testing.T) {
	s := testSigner(t)
	token, err := s.Issue(sampleCursor())
	require.NoError(t, err)

	// Flip a character in the encoded payload segment (between the two dots).
	firstDot := strings.IndexByte(token, '.')
	secondDot := firstDot + 1 + strings.IndexByte(token[firstDot+1:], '.')
	body := []byte(token[firstDot+1 : secondDot])
	require.Greater(t, len(body), 2)
	if body[0] == 'A' {
		body[0] = 'B'
	} else {
		body[0] = 'A'
	}
	tampered := token[:firstDot+1] + string(body) + token[secondDot:]
	_, err = s.Verify(tampered)
	require.Error(t, err)
	require.True(t, IsRejected(err))

	// Truncated and malformed shapes.
	for _, bad := range []string{"", "x", "1k1", "1k1.abc", "2" + token[1:], token[:secondDot], token[:len(token)-2]} {
		_, err = s.Verify(bad)
		require.Error(t, err, "token %q", bad)
	}
}

func TestVerifyUnknownKey(t *testing.T) {
	s := testSigner(t)
	token, err := s.Issue(sampleCursor())
	require.NoError(t, err)

	// Rebuild the token with a foreign key id by signing it with another signer
	// whose current id differs.
	other, err := NewSigner("other", []byte("foreign"), Limits{TTL: time.Minute})
	require.NoError(t, err)
	otherToken, err := other.Issue(sampleCursor())
	require.NoError(t, err)
	_, err = s.Verify(otherToken)
	require.ErrorIs(t, err, errUnknownKey)

	require.NotEmpty(t, token)
}

func TestOldKeyBoundedWindow(t *testing.T) {
	clockRef, now := fixedClock()
	s, err := NewSigner("new", []byte("new-secret"), Limits{TTL: time.Minute},
		WithNow(now),
		WithOldKey("old", []byte("old-secret"), time.Hour),
	)
	require.NoError(t, err)

	oldSigner, err := NewSigner("old", []byte("old-secret"), Limits{TTL: time.Minute}, WithNow(now))
	require.NoError(t, err)
	token, err := oldSigner.Issue(sampleCursor())
	require.NoError(t, err)

	// Within the window the retired key still verifies.
	_, err = s.Verify(token)
	require.NoError(t, err)

	// New cursors are always signed with the current key.
	newToken, err := s.Issue(sampleCursor())
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(newToken, string(Version)+"new."))

	// Past the window the retired key is rejected as unknown.
	*clockRef = clockRef.Add(2 * time.Hour)
	_, err = s.Verify(token)
	require.ErrorIs(t, err, errUnknownKey)
}

func TestTTLExpiryAndFutureSkew(t *testing.T) {
	clockRef, now := fixedClock()
	s, err := NewSigner("k1", []byte("secret"), Limits{TTL: time.Minute}, WithNow(now))
	require.NoError(t, err)
	token, err := s.Issue(sampleCursor())
	require.NoError(t, err)

	*clockRef = clockRef.Add(61 * time.Second)
	_, err = s.Verify(token)
	require.ErrorIs(t, err, errExpired)

	*clockRef = clockRef.Add(-(61 + futureSkewSeconds + 1) * time.Second)
	_, err = s.Verify(token)
	require.ErrorIs(t, err, errExpired)
}

func TestLimitsEnforced(t *testing.T) {
	s := testSigner(t)

	tooMany := Cursor{Channels: make([]Channel, 5)}
	for i := range tooMany.Channels {
		tooMany.Channels[i].Channel = "ch"
		tooMany.Channels[i].Channel = string(rune('a' + i))
	}
	_, err := s.Issue(tooMany)
	require.ErrorIs(t, err, errTooManyChannels)

	longName := Cursor{Channels: []Channel{{Channel: strings.Repeat("x", 33)}}}
	_, err = s.Issue(longName)
	require.ErrorIs(t, err, errChannelTooLong)

	dup := Cursor{Channels: []Channel{{Channel: "a"}, {Channel: "a"}}}
	_, err = s.Issue(dup)
	require.Error(t, err)

	empty := Cursor{}
	token, err := s.Issue(empty)
	require.NoError(t, err)
	got, err := s.Verify(token)
	require.NoError(t, err)
	require.Empty(t, got.Channels)

	// Raw token length cap.
	big, err := NewSigner("k1", []byte("secret"), Limits{
		MaxTokenLength:   10,
		MaxChannels:      4,
		MaxChannelLength: 32,
		TTL:              time.Minute,
	})
	require.NoError(t, err)
	_, err = big.Issue(sampleCursor())
	require.ErrorIs(t, err, errTooLarge)
	_, err = big.Verify(strings.Repeat("a", 11))
	require.ErrorIs(t, err, errTooLarge)
}

func TestChannelSetEqual(t *testing.T) {
	c := Cursor{Channels: []Channel{{Channel: "a"}, {Channel: "b"}}}
	require.True(t, c.ChannelSetEqual([]string{"b", "a"}))
	require.False(t, c.ChannelSetEqual([]string{"a"}))
	require.False(t, c.ChannelSetEqual([]string{"a", "c"}))
	require.False(t, c.ChannelSetEqual([]string{"a", "b", "c"}))
}

func TestNewSignerValidation(t *testing.T) {
	_, err := NewSigner("", []byte("k"), Limits{})
	require.Error(t, err)
	_, err = NewSigner("k", nil, Limits{})
	require.Error(t, err)
}
