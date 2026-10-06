package centrifuge

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"github.com/segmentio/encoding/json"
)

// Recovery cursor is an optional mechanism of the SSE transport which lets a
// client carry positions of all its channel subscriptions to the next
// connection - unlike Last-Event-ID which can only hold a single position.
//
// The cursor is a signed token the server emits into the event stream after
// delivered positions advance. The client sends it back in the
// cf_recovery_cursor URL parameter on reconnect, and the server seeds
// per-channel recovery positions from it. A cursor is NOT an authorization
// credential: it only carries stream positions, all the regular subscribe
// permission checks still run for every channel.

const (
	// recoveryCursorVersion is the cursor format version, a mismatch is an error.
	recoveryCursorVersion = "v1"

	defaultRecoveryCursorTTL         = 24 * time.Hour
	defaultRecoveryCursorMaxChannels = 128
	defaultRecoveryCursorMaxSize     = 8 * 1024

	// Hard caps to keep cursor handling bounded even with a loose config.
	maxRecoveryCursorChannels = 4096
	maxRecoveryCursorSize     = 64 * 1024
)

// RecoveryCursorConfig configures the optional recovery cursor of SSEHandler.
// The cursor is disabled unless Secret is set.
type RecoveryCursorConfig struct {
	// Secret is the key used to sign (HMAC-SHA256) emitted cursors and to
	// verify incoming ones. Required to enable the feature.
	Secret string
	// PreviousSecrets allows a bounded verification window for recently
	// rotated-out keys, so cursors issued just before a key rotation stay
	// usable. Previous secrets are never used for signing.
	PreviousSecrets []RotatedSecret
	// TTL is how long a cursor stays valid after being issued.
	// Defaults to 24 hours when not set.
	TTL time.Duration
	// MaxChannels limits the number of channel positions a cursor may carry.
	// Defaults to 128, hard-capped at 4096.
	MaxChannels int
	// MaxSize limits the encoded cursor size in bytes. Cursors which would
	// exceed it are not emitted, oversized incoming cursors are rejected.
	// Defaults to 8KB, hard-capped at 64KB.
	MaxSize int
}

// RotatedSecret is a previously used cursor signing key which may still
// verify cursors for a bounded time window.
type RotatedSecret struct {
	// Secret is the rotated-out key.
	Secret string
	// VerifyUntil bounds the window during which cursors signed with this
	// key are still accepted. Cursors are rejected with it after this time.
	VerifyUntil time.Time
}

func (c *RecoveryCursorConfig) enabled() bool {
	return c != nil && c.Secret != ""
}

func (c *RecoveryCursorConfig) ttl() time.Duration {
	if c.TTL > 0 {
		return c.TTL
	}
	return defaultRecoveryCursorTTL
}

func (c *RecoveryCursorConfig) maxChannels() int {
	if c.MaxChannels > 0 {
		return min(c.MaxChannels, maxRecoveryCursorChannels)
	}
	return defaultRecoveryCursorMaxChannels
}

func (c *RecoveryCursorConfig) maxSize() int {
	if c.MaxSize > 0 {
		return min(c.MaxSize, maxRecoveryCursorSize)
	}
	return defaultRecoveryCursorMaxSize
}

// recoveryCursorPosition is a single channel position inside a cursor.
type recoveryCursorPosition struct {
	Offset uint64 `json:"o"`
	Epoch  string `json:"e,omitempty"`
}

// recoveryCursorPayload is the signed content of a recovery cursor. It binds
// the positions to a user and to the exact set of subscribed channels.
type recoveryCursorPayload struct {
	UserID    string                            `json:"u"`
	ExpiresAt int64                             `json:"x"`
	Positions map[string]recoveryCursorPosition `json:"p"`
}

var (
	errRecoveryCursorMalformed = errors.New("malformed recovery cursor")
	errRecoveryCursorSignature = errors.New("recovery cursor signature mismatch")
	errRecoveryCursorExpired   = errors.New("recovery cursor expired")
	errRecoveryCursorTooLarge  = errors.New("recovery cursor too large")
	errRecoveryCursorTooMany   = errors.New("recovery cursor has too many channels")
)

func recoveryCursorSignature(secret string, signed string) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signed))
	return mac.Sum(nil)
}

// encodeRecoveryCursor builds a signed cursor token for the given user and
// per-channel positions.
func encodeRecoveryCursor(config *RecoveryCursorConfig, userID string, positions map[string]StreamPosition, now time.Time) (string, error) {
	if len(positions) > config.maxChannels() {
		return "", errRecoveryCursorTooMany
	}
	payload := recoveryCursorPayload{
		UserID:    userID,
		ExpiresAt: now.Add(config.ttl()).Unix(),
		Positions: make(map[string]recoveryCursorPosition, len(positions)),
	}
	for ch, sp := range positions {
		payload.Positions[ch] = recoveryCursorPosition{Offset: sp.Offset, Epoch: sp.Epoch}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	encodedPayload := base64.RawURLEncoding.EncodeToString(raw)
	signed := recoveryCursorVersion + "." + encodedPayload
	token := signed + "." + base64.RawURLEncoding.EncodeToString(recoveryCursorSignature(config.Secret, signed))
	if len(token) > config.maxSize() {
		return "", errRecoveryCursorTooLarge
	}
	return token, nil
}

// decodeRecoveryCursor verifies and decodes a cursor token. Verification tries
// the current secret first, then previous secrets within their verification
// window.
func decodeRecoveryCursor(config *RecoveryCursorConfig, token string, now time.Time) (*recoveryCursorPayload, error) {
	if len(token) > config.maxSize() {
		return nil, errRecoveryCursorTooLarge
	}
	version, rest, ok := strings.Cut(token, ".")
	if !ok || version != recoveryCursorVersion {
		return nil, errRecoveryCursorMalformed
	}
	encodedPayload, encodedSig, ok := strings.Cut(rest, ".")
	if !ok || encodedPayload == "" || encodedSig == "" {
		return nil, errRecoveryCursorMalformed
	}
	sig, err := base64.RawURLEncoding.DecodeString(encodedSig)
	if err != nil {
		return nil, errRecoveryCursorMalformed
	}
	signed := version + "." + encodedPayload
	if !recoveryCursorSignatureValid(config, signed, sig, now) {
		return nil, errRecoveryCursorSignature
	}
	raw, err := base64.RawURLEncoding.DecodeString(encodedPayload)
	if err != nil {
		return nil, errRecoveryCursorMalformed
	}
	var payload recoveryCursorPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, errRecoveryCursorMalformed
	}
	if now.Unix() >= payload.ExpiresAt {
		return nil, errRecoveryCursorExpired
	}
	if len(payload.Positions) > config.maxChannels() {
		return nil, errRecoveryCursorTooMany
	}
	return &payload, nil
}

func recoveryCursorSignatureValid(config *RecoveryCursorConfig, signed string, sig []byte, now time.Time) bool {
	if hmac.Equal(sig, recoveryCursorSignature(config.Secret, signed)) {
		return true
	}
	for _, prev := range config.PreviousSecrets {
		if prev.Secret == "" || !now.Before(prev.VerifyUntil) {
			continue
		}
		if hmac.Equal(sig, recoveryCursorSignature(prev.Secret, signed)) {
			return true
		}
	}
	return false
}

// positionFor returns the cursor position for a channel if the cursor is
// bound to the given user, not expired, and contains the channel.
func (p *recoveryCursorPayload) positionFor(userID string, channel string, now time.Time) (StreamPosition, bool) {
	if p.UserID != userID || now.Unix() >= p.ExpiresAt {
		return StreamPosition{}, false
	}
	pos, ok := p.Positions[channel]
	if !ok {
		return StreamPosition{}, false
	}
	return StreamPosition{Offset: pos.Offset, Epoch: pos.Epoch}, true
}

// recoveryCursorContextKeyType is a special type to safely use context for
// passing a verified recovery cursor to the client.
type recoveryCursorContextKeyType int

// recoveryCursorContextKey allows passing a verified recovery cursor payload
// from a transport handler to the Client through context.
var recoveryCursorContextKey recoveryCursorContextKeyType

// withRecoveryCursor returns a context carrying the verified recovery cursor.
func withRecoveryCursor(ctx context.Context, cursor *recoveryCursorPayload) context.Context {
	return context.WithValue(ctx, recoveryCursorContextKey, cursor)
}

// recoveryCursorFromContext extracts a recovery cursor from the context.
func recoveryCursorFromContext(ctx context.Context) (*recoveryCursorPayload, bool) {
	if val := ctx.Value(recoveryCursorContextKey); val != nil {
		cursor, ok := val.(*recoveryCursorPayload)
		return cursor, ok
	}
	return nil, false
}
