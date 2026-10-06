package centrifuge

import (
	"time"

	"github.com/centrifugal/centrifuge/internal/sserecovery"
)

const (
	// sseRecoveryQueryParam carries a recovery cursor on the reconnecting GET.
	sseRecoveryQueryParam = "cf_cursor"
	// sseRecoveryHeader carries a recovery cursor as an alternative to the query
	// param (useful for non-browser clients that can set request headers).
	sseRecoveryHeader = "X-Centrifuge-Recovery-Cursor"

	defaultSSERecoveryMaxChannels      = 128
	defaultSSERecoveryMaxChannelLength = 255
	defaultSSERecoveryMaxTokenLength   = 4096
	defaultSSERecoveryTTL              = 5 * time.Minute
)

// SSERecoveryConfig configures the optional SSE recovery cursor. The zero
// value leaves the feature disabled: SSE then behaves exactly as before and no
// cursor is issued or consumed.
//
// A cursor is a signed, stateless record of the per-channel positions already
// delivered to a connection, bound to the authenticated user and the exact
// subscription set. It is NOT an authorization credential: on reconnect every
// channel it names still passes the normal OnConnecting/OnSubscribe permission
// checks before recovery is attempted.
type SSERecoveryConfig struct {
	// KeyID names the current signing key and is embedded in every issued
	// token so verification can select the right secret.
	KeyID string
	// Key is the HMAC secret for new cursors.
	Key []byte

	// OldKey, when set together with OldKeyID, registers a retired signing key
	// that remains valid for verification during OldKeyMaxAge after startup. Use
	// it to rotate keys without dropping in-flight reconnects. Only one retired
	// key is exposed for the common two-generation rotation; callers needing a
	// longer chain rotate gradually.
	OldKey []byte
	// OldKeyID is the id embedded in cursors signed with OldKey.
	OldKeyID string
	// OldKeyMaxAge bounds how long the retired key remains accepted. Zero means
	// the cursor TTL.
	OldKeyMaxAge time.Duration

	// MaxChannels bounds channels per cursor. Zero means 128.
	MaxChannels int
	// MaxChannelLength bounds a channel name length in a cursor. Zero means 255.
	MaxChannelLength int
	// MaxTokenLength bounds the encoded token accepted on reconnect. Zero means
	// 4096 bytes (keeps it safely within URL and header limits).
	MaxTokenLength int
	// TTL bounds cursor validity. Zero means 5 minutes.
	TTL time.Duration
}

// Enabled reports whether recovery cursors are configured.
func (c SSERecoveryConfig) Enabled() bool {
	return len(c.Key) > 0 && c.KeyID != ""
}

func (c SSERecoveryConfig) limits() sserecovery.Limits {
	l := sserecovery.Limits{
		MaxChannels:      c.MaxChannels,
		MaxChannelLength: c.MaxChannelLength,
		MaxTokenLength:   c.MaxTokenLength,
		TTL:              c.TTL,
	}
	if l.MaxChannels == 0 {
		l.MaxChannels = defaultSSERecoveryMaxChannels
	}
	if l.MaxChannelLength == 0 {
		l.MaxChannelLength = defaultSSERecoveryMaxChannelLength
	}
	if l.MaxTokenLength == 0 {
		l.MaxTokenLength = defaultSSERecoveryMaxTokenLength
	}
	if l.TTL == 0 {
		l.TTL = defaultSSERecoveryTTL
	}
	return l
}

// newRecoverySigner builds the signer, accepting the retired key for a bounded
// window when one is configured.
func (c SSERecoveryConfig) newRecoverySigner() (*sserecovery.Signer, error) {
	if !c.Enabled() {
		return nil, nil
	}
	var opts []sserecovery.Option
	if len(c.OldKey) > 0 && c.OldKeyID != "" {
		maxAge := c.OldKeyMaxAge
		if maxAge <= 0 {
			maxAge = c.limits().TTL
		}
		opts = append(opts, sserecovery.WithOldKey(c.OldKeyID, c.OldKey, maxAge))
	}
	return sserecovery.NewSigner(c.KeyID, c.Key, c.limits(), opts...)
}
