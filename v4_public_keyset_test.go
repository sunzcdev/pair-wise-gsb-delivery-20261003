package paseto_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"aidanwoods.dev/go-paseto"
	"github.com/stretchr/testify/require"
)

var b64 = base64.RawURLEncoding.Strict()

type testKey struct {
	id     string
	secret paseto.V4AsymmetricSecretKey
	pub    paseto.V4AsymmetricPublicKey
}

func newTestKey(t *testing.T, id string) testKey {
	t.Helper()
	secret := paseto.NewV4AsymmetricSecretKey()
	return testKey{id: id, secret: secret, pub: secret.Public()}
}

func kidFooter(t *testing.T, id string) []byte {
	t.Helper()
	footer, err := json.Marshal(map[string]string{"kid": id})
	require.NoError(t, err)
	return footer
}

func signedWithFooter(t *testing.T, secret paseto.V4AsymmetricSecretKey, footer []byte) string {
	t.Helper()
	token := paseto.NewToken()
	token.SetString("data", "rotation test")
	token.SetFooter(footer)
	return token.V4Sign(secret, nil)
}

func signedV2(t *testing.T, secret paseto.V2AsymmetricSecretKey) string {
	t.Helper()
	token := paseto.NewToken()
	token.SetString("data", "v2 fallback probe")
	return token.V2Sign(secret)
}

func alwaysWindow() paseto.V4KeyWindow {
	return paseto.V4KeyWindow{}
}

func requireData(t *testing.T, token *paseto.Token) {
	t.Helper()
	data, err := token.GetString("data")
	require.NoError(t, err)
	require.Equal(t, "rotation test", data)
}

// During rotation the old and new key's enabled windows overlap, so a token
// signed by either key verifies and the actually-verifying ID is reported.
func TestKeySetRotationOverlappingWindows(t *testing.T) {
	old := newTestKey(t, "old")
	new := newTestKey(t, "new")

	now := time.Now()
	set, err := paseto.NewV4PublicKeySet([]paseto.V4KeyEntry{
		{ID: old.id, Key: old.pub, Window: paseto.V4KeyWindow{Expires: now.Add(time.Hour)}},
		{ID: new.id, Key: new.pub, Window: paseto.V4KeyWindow{NotBefore: now.Add(-time.Hour)}},
	})
	require.NoError(t, err)

	parser := paseto.NewParserWithoutExpiryCheck()

	oldToken := signedWithFooter(t, old.secret, kidFooter(t, old.id))
	token, id, err := parser.ParseV4PublicWithKeySet(set, oldToken, nil)
	require.NoError(t, err)
	require.Equal(t, old.id, id)
	requireData(t, token)

	newToken := signedWithFooter(t, new.secret, kidFooter(t, new.id))
	token, id, err = parser.ParseV4PublicWithKeySet(set, newToken, nil)
	require.NoError(t, err)
	require.Equal(t, new.id, id)
	require.NotEmpty(t, token.Footer())
}

// A hint is only a routing hint: with no (or a foreign) footer, verification
// still finds the signing key by trying every enabled candidate.
func TestKeySetVerifiesWithoutHint(t *testing.T) {
	first := newTestKey(t, "first")
	second := newTestKey(t, "second")

	set, err := paseto.NewV4PublicKeySet([]paseto.V4KeyEntry{
		{ID: first.id, Key: first.pub, Window: alwaysWindow()},
		{ID: second.id, Key: second.pub, Window: alwaysWindow()},
	})
	require.NoError(t, err)

	parser := paseto.NewParserWithoutExpiryCheck()

	tokenString := signedWithFooter(t, second.secret, nil)
	token, id, err := parser.ParseV4PublicWithKeySet(set, tokenString, nil)
	require.NoError(t, err)
	require.Equal(t, second.id, id)

	// A hint naming some unknown third key must not affect verification.
	foreignHint := signedWithFooter(t, second.secret, kidFooter(t, "totally-unknown"))
	token, id, err = parser.ParseV4PublicWithKeySet(set, foreignHint, nil)
	require.NoError(t, err)
	require.Equal(t, second.id, id)
	require.NotNil(t, token)
}

// Keys outside their enable/disable window are not candidates at all, even
// when the attacker-supplied footer names them. With no enabled keys the parse
// fails as a configuration error rather than verifying against a retired key.
func TestKeySetRejectsDisabledAndFutureKeys(t *testing.T) {
	old := newTestKey(t, "retired")
	future := newTestKey(t, "future")

	now := time.Now()
	set, err := paseto.NewV4PublicKeySet([]paseto.V4KeyEntry{
		{ID: old.id, Key: old.pub, Window: paseto.V4KeyWindow{
			NotBefore: now.Add(-2 * time.Hour),
			Expires:   now.Add(-time.Hour),
		}},
		{ID: future.id, Key: future.pub, Window: paseto.V4KeyWindow{NotBefore: now.Add(time.Hour)}},
	})
	require.NoError(t, err)

	parser := paseto.NewParserWithoutExpiryCheck()

	for _, key := range []testKey{old, future} {
		tokenString := signedWithFooter(t, key.secret, kidFooter(t, key.id))
		token, id, err := parser.ParseV4PublicWithKeySet(set, tokenString, nil)
		require.Error(t, err)
		require.Contains(t, err.Error(), "no v4.public keys are enabled")
		require.Nil(t, token)
		require.Empty(t, id)
	}
}

// Active keys which simply cannot verify the signature produce an ordinary
// token (signature) error indistinguishable from other bad signatures.
func TestKeySetActiveButNoneVerifies(t *testing.T) {
	signer := newTestKey(t, "signer")
	other := newTestKey(t, "other")

	set, err := paseto.NewV4PublicKeySet([]paseto.V4KeyEntry{
		{ID: other.id, Key: other.pub, Window: alwaysWindow()},
	})
	require.NoError(t, err)

	parser := paseto.NewParserWithoutExpiryCheck()
	tokenString := signedWithFooter(t, signer.secret, kidFooter(t, other.id))

	token, id, err := parser.ParseV4PublicWithKeySet(set, tokenString, nil)
	require.Error(t, err)
	require.ErrorIs(t, err, &paseto.TokenError{})
	require.Nil(t, token)
	require.Empty(t, id)
}

// When no key is enabled at parse time, parsing fails with a configuration
// error rather than attempting any disabled key.
func TestKeySetNoActiveKeys(t *testing.T) {
	retired := newTestKey(t, "retired")

	now := time.Now()
	set, err := paseto.NewV4PublicKeySet([]paseto.V4KeyEntry{
		{ID: retired.id, Key: retired.pub, Window: paseto.V4KeyWindow{Expires: now.Add(-time.Hour)}},
	})
	require.NoError(t, err)

	parser := paseto.NewParserWithoutExpiryCheck()
	tokenString := signedWithFooter(t, retired.secret, kidFooter(t, retired.id))

	token, id, err := parser.ParseV4PublicWithKeySet(set, tokenString, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no v4.public keys are enabled")
	require.Nil(t, token)
	require.Empty(t, id)
}

// The footer is part of the signed data. Tampering with it (e.g. swapping the
// hint to another key's ID) must invalidate the signature.
func TestKeySetTamperedFooter(t *testing.T) {
	first := newTestKey(t, "first")
	second := newTestKey(t, "second")

	set, err := paseto.NewV4PublicKeySet([]paseto.V4KeyEntry{
		{ID: first.id, Key: first.pub, Window: alwaysWindow()},
		{ID: second.id, Key: second.pub, Window: alwaysWindow()},
	})
	require.NoError(t, err)

	original := signedWithFooter(t, first.secret, kidFooter(t, first.id))

	parts := strings.Split(original, ".")
	require.Len(t, parts, 4)
	parts[3] = b64.EncodeToString(kidFooter(t, second.id))
	tampered := strings.Join(parts, ".")

	parser := paseto.NewParserWithoutExpiryCheck()
	token, id, err := parser.ParseV4PublicWithKeySet(set, tampered, nil)
	require.Error(t, err)
	require.ErrorIs(t, err, &paseto.TokenError{})
	require.Nil(t, token)
	require.Empty(t, id)
}

// The parser is fixed to v4.public: a v2.public token must never be accepted
// via protocol downgrade/fallback, nor trigger any other key lookup.
func TestKeySetNoProtocolFallback(t *testing.T) {
	v2Secret := paseto.NewV2AsymmetricSecretKey()

	set, err := paseto.NewV4PublicKeySet(nil)
	require.NoError(t, err)

	parser := paseto.NewParserWithoutExpiryCheck()
	token, id, err := parser.ParseV4PublicWithKeySet(set, signedV2(t, v2Secret), nil)
	require.Error(t, err)
	require.ErrorIs(t, err, &paseto.TokenError{})
	require.Nil(t, token)
	require.Empty(t, id)
}

func TestKeySetRejectsDuplicateIDs(t *testing.T) {
	first := newTestKey(t, "shared-id")
	second := newTestKey(t, "shared-id")

	_, err := paseto.NewV4PublicKeySet([]paseto.V4KeyEntry{
		{ID: first.id, Key: first.pub, Window: alwaysWindow()},
		{ID: second.id, Key: second.pub, Window: alwaysWindow()},
	})
	require.Error(t, err)

	set, err := paseto.NewV4PublicKeySet([]paseto.V4KeyEntry{
		{ID: first.id, Key: first.pub, Window: alwaysWindow()},
	})
	require.NoError(t, err)

	err = set.Add(paseto.V4KeyEntry{ID: second.id, Key: second.pub, Window: alwaysWindow()})
	require.Error(t, err)

	require.Equal(t, 1, set.Len())

	require.True(t, set.Remove(first.id))
	require.NoError(t, set.Add(paseto.V4KeyEntry{ID: second.id, Key: second.pub, Window: alwaysWindow()}))
	require.False(t, set.Remove("never-existed"))
}

func TestKeySetValidation(t *testing.T) {
	key := newTestKey(t, "k")

	require.Error(t, (&paseto.V4PublicKeySet{}).Add(paseto.V4KeyEntry{
		ID: "", Key: key.pub, Window: alwaysWindow(),
	}))

	now := time.Now()
	require.Error(t, (&paseto.V4PublicKeySet{}).Add(paseto.V4KeyEntry{
		ID: "empty-window", Key: key.pub,
		Window: paseto.V4KeyWindow{NotBefore: now, Expires: now},
	}))

	set := &paseto.V4PublicKeySet{}
	for i := 0; i < 8; i++ {
		entry := newTestKey(t, fmt.Sprintf("candidate-%d", i))
		require.NoError(t, set.Add(paseto.V4KeyEntry{
			ID:     entry.id,
			Key:    entry.pub,
			Window: alwaysWindow(),
		}))
	}
	extra := newTestKey(t, "one-too-many")
	require.Error(t, set.Add(paseto.V4KeyEntry{ID: extra.id, Key: extra.pub, Window: alwaysWindow()}))
}

// A token which cryptographically verifies but fails a rule reports the key ID
// alongside the rule error.
func TestKeySetRuleFailureReportsID(t *testing.T) {
	signing := newTestKey(t, "signing")

	set, err := paseto.NewV4PublicKeySet([]paseto.V4KeyEntry{
		{ID: signing.id, Key: signing.pub, Window: alwaysWindow()},
	})
	require.NoError(t, err)

	parser := paseto.MakeParser([]paseto.Rule{paseto.ForAudience("expected-audience")})
	tokenString := signedWithFooter(t, signing.secret, kidFooter(t, signing.id))

	token, id, err := parser.ParseV4PublicWithKeySet(set, tokenString, nil)
	require.Error(t, err)
	require.ErrorIs(t, err, &paseto.RuleError{})
	require.Equal(t, signing.id, id)
	require.NotNil(t, token)
}

// Concurrent verification against rotating Add/Remove must stay consistent:
// every parse observes one set version, never a partial/duplicate state.
func TestKeySetConcurrentRotation(t *testing.T) {
	c := newTestKey(t, "c")

	set, err := paseto.NewV4PublicKeySet([]paseto.V4KeyEntry{
		{ID: c.id, Key: c.pub, Window: alwaysWindow()},
	})
	require.NoError(t, err)

	parser := paseto.NewParserWithoutExpiryCheck()
	cToken := signedWithFooter(t, c.secret, kidFooter(t, c.id))

	const iterations = 200
	var wg sync.WaitGroup
	var failuresMu sync.Mutex
	var failures []string

	wg.Add(1)
	go func() {
		defer wg.Done()
		present := true
		for i := 0; i < iterations; i++ {
			if present {
				set.Remove(c.id)
			} else {
				_ = set.Add(paseto.V4KeyEntry{ID: c.id, Key: c.pub})
			}
			present = !present
		}
		if !present {
			_ = set.Add(paseto.V4KeyEntry{ID: c.id, Key: c.pub})
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			token, id, parseErr := parser.ParseV4PublicWithKeySet(set, cToken, nil)
			if parseErr == nil {
				data, err := token.GetString("data")
				if id != c.id || err != nil || data != "rotation test" {
					failuresMu.Lock()
					failures = append(failures,
						fmt.Sprintf("inconsistent success: id=%q err=%v data=%q", id, err, data))
					failuresMu.Unlock()
				}
			} else {
				// The only permissible failure is the key being momentarily
				// absent/disabled during the rotation.
				if id != "" || token != nil {
					failuresMu.Lock()
					failures = append(failures,
						fmt.Sprintf("failed parse must not report a key: id=%q", id))
					failuresMu.Unlock()
				}
			}
		}
	}()

	wg.Wait()
	require.Empty(t, failures, strings.Join(failures, "; "))
	require.NoError(t, set.Add(paseto.V4KeyEntry{
		ID:  "fresh-after-rotation",
		Key: newTestKey(t, "fresh-after-rotation").pub,
	}))
}
