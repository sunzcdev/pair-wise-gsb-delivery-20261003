package centrifuge

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRecoveryCursorEncodeDecode(t *testing.T) {
	t.Parallel()
	config := &RecoveryCursorConfig{Secret: "secret"}
	now := time.Now()
	positions := map[string]StreamPosition{
		"ch1": {Offset: 41, Epoch: "epoch1"},
		"ch2": {Offset: 0, Epoch: ""},
	}
	token, err := encodeRecoveryCursor(config, "user1", positions, now)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(token, "v1."))
	payload, err := decodeRecoveryCursor(config, token, now)
	require.NoError(t, err)
	require.Equal(t, "user1", payload.UserID)
	require.Len(t, payload.Positions, 2)
	require.Equal(t, uint64(41), payload.Positions["ch1"].Offset)
	require.Equal(t, "epoch1", payload.Positions["ch1"].Epoch)
	require.Equal(t, uint64(0), payload.Positions["ch2"].Offset)
	sp, ok := payload.positionFor("user1", "ch1", now)
	require.True(t, ok)
	require.Equal(t, uint64(41), sp.Offset)
	require.Equal(t, "epoch1", sp.Epoch)
}

func TestRecoveryCursorDefaults(t *testing.T) {
	t.Parallel()
	config := &RecoveryCursorConfig{}
	require.False(t, config.enabled())
	require.Equal(t, defaultRecoveryCursorTTL, config.ttl())
	require.Equal(t, defaultRecoveryCursorMaxChannels, config.maxChannels())
	require.Equal(t, defaultRecoveryCursorMaxSize, config.maxSize())
	config = &RecoveryCursorConfig{Secret: "secret", MaxChannels: 1000000, MaxSize: 1000000}
	require.True(t, config.enabled())
	require.Equal(t, maxRecoveryCursorChannels, config.maxChannels())
	require.Equal(t, maxRecoveryCursorSize, config.maxSize())
}

func TestRecoveryCursorTampered(t *testing.T) {
	t.Parallel()
	config := &RecoveryCursorConfig{Secret: "secret"}
	now := time.Now()
	token, err := encodeRecoveryCursor(config, "user1", map[string]StreamPosition{
		"ch1": {Offset: 1},
	}, now)
	require.NoError(t, err)
	// Tamper the payload part.
	parts := strings.Split(token, ".")
	require.Len(t, parts, 3)
	payloadChars := []byte(parts[1])
	if payloadChars[0] == 'A' {
		payloadChars[0] = 'B'
	} else {
		payloadChars[0] = 'A'
	}
	tampered := parts[0] + "." + string(payloadChars) + "." + parts[2]
	_, err = decodeRecoveryCursor(config, tampered, now)
	require.ErrorIs(t, err, errRecoveryCursorSignature)
	// Different secret does not verify.
	_, err = decodeRecoveryCursor(&RecoveryCursorConfig{Secret: "other"}, token, now)
	require.ErrorIs(t, err, errRecoveryCursorSignature)
}

func TestRecoveryCursorMalformed(t *testing.T) {
	t.Parallel()
	config := &RecoveryCursorConfig{Secret: "secret"}
	now := time.Now()
	for _, token := range []string{
		"",
		"v1",
		"v1.",
		"v1..sig",
		"v2.abc.def",
		"v1.!!!.def",
		"v1." + strings.Repeat("a", 100) + ".!!!",
	} {
		_, err := decodeRecoveryCursor(config, token, now)
		require.Error(t, err, "token: %q", token)
	}
}

func TestRecoveryCursorExpired(t *testing.T) {
	t.Parallel()
	config := &RecoveryCursorConfig{Secret: "secret", TTL: time.Hour}
	now := time.Now()
	token, err := encodeRecoveryCursor(config, "user1", map[string]StreamPosition{
		"ch1": {Offset: 1},
	}, now)
	require.NoError(t, err)
	_, err = decodeRecoveryCursor(config, token, now.Add(2*time.Hour))
	require.ErrorIs(t, err, errRecoveryCursorExpired)
	payload, err := decodeRecoveryCursor(config, token, now)
	require.NoError(t, err)
	_, ok := payload.positionFor("user1", "ch1", now.Add(2*time.Hour))
	require.False(t, ok)
}

func TestRecoveryCursorPreviousSecrets(t *testing.T) {
	t.Parallel()
	now := time.Now()
	oldConfig := &RecoveryCursorConfig{Secret: "old"}
	token, err := encodeRecoveryCursor(oldConfig, "user1", map[string]StreamPosition{
		"ch1": {Offset: 7, Epoch: "e"},
	}, now)
	require.NoError(t, err)
	// Rotated config verifies cursors signed with the old secret within the window.
	rotated := &RecoveryCursorConfig{
		Secret: "new",
		PreviousSecrets: []RotatedSecret{
			{Secret: "old", VerifyUntil: now.Add(time.Hour)},
		},
	}
	payload, err := decodeRecoveryCursor(rotated, token, now)
	require.NoError(t, err)
	require.Equal(t, uint64(7), payload.Positions["ch1"].Offset)
	// After the verification window the old secret is no longer accepted.
	_, err = decodeRecoveryCursor(rotated, token, now.Add(2*time.Hour))
	require.ErrorIs(t, err, errRecoveryCursorSignature)
	// A rotated config without the previous secret rejects the token.
	_, err = decodeRecoveryCursor(&RecoveryCursorConfig{Secret: "new"}, token, now)
	require.ErrorIs(t, err, errRecoveryCursorSignature)
	// Previous secrets are not used for signing.
	newToken, err := encodeRecoveryCursor(rotated, "user1", map[string]StreamPosition{
		"ch1": {Offset: 1},
	}, now)
	require.NoError(t, err)
	_, err = decodeRecoveryCursor(oldConfig, newToken, now)
	require.ErrorIs(t, err, errRecoveryCursorSignature)
}

func TestRecoveryCursorTooManyChannels(t *testing.T) {
	t.Parallel()
	now := time.Now()
	positions := map[string]StreamPosition{
		"ch1": {Offset: 1},
		"ch2": {Offset: 2},
	}
	config := &RecoveryCursorConfig{Secret: "secret", MaxChannels: 1}
	_, err := encodeRecoveryCursor(config, "user1", positions, now)
	require.ErrorIs(t, err, errRecoveryCursorTooMany)
	// Encoded with a higher limit, decoded with a lower one.
	token, err := encodeRecoveryCursor(&RecoveryCursorConfig{Secret: "secret"}, "user1", positions, now)
	require.NoError(t, err)
	_, err = decodeRecoveryCursor(config, token, now)
	require.ErrorIs(t, err, errRecoveryCursorTooMany)
}

func TestRecoveryCursorTooLarge(t *testing.T) {
	t.Parallel()
	now := time.Now()
	config := &RecoveryCursorConfig{Secret: "secret", MaxSize: 128}
	positions := map[string]StreamPosition{
		"a-very-long-channel-name-to-make-the-cursor-payload-bigger": {Offset: 1, Epoch: "epoch"},
	}
	_, err := encodeRecoveryCursor(config, "user1", positions, now)
	require.ErrorIs(t, err, errRecoveryCursorTooLarge)
	// Oversized incoming tokens are rejected before parsing.
	token, err := encodeRecoveryCursor(&RecoveryCursorConfig{Secret: "secret"}, "user1", positions, now)
	require.NoError(t, err)
	require.Greater(t, len(token), 128)
	_, err = decodeRecoveryCursor(config, token, now)
	require.ErrorIs(t, err, errRecoveryCursorTooLarge)
}

func TestRecoveryCursorPositionFor(t *testing.T) {
	t.Parallel()
	config := &RecoveryCursorConfig{Secret: "secret"}
	now := time.Now()
	token, err := encodeRecoveryCursor(config, "user1", map[string]StreamPosition{
		"ch1": {Offset: 5, Epoch: "e"},
	}, now)
	require.NoError(t, err)
	payload, err := decodeRecoveryCursor(config, token, now)
	require.NoError(t, err)
	// Another user can not use the cursor.
	_, ok := payload.positionFor("user2", "ch1", now)
	require.False(t, ok)
	// Anonymous user can not use a cursor bound to a user.
	_, ok = payload.positionFor("", "ch1", now)
	require.False(t, ok)
	// Unknown channel.
	_, ok = payload.positionFor("user1", "ch2", now)
	require.False(t, ok)
}
