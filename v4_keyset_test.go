package paseto_test

import (
	"encoding/base64"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"aidanwoods.dev/go-paseto"
	"github.com/stretchr/testify/require"
)

func v4KeySetEntry(id string, secretKey paseto.V4AsymmetricSecretKey, notBefore, notAfter time.Time) paseto.V4PublicKeySetKey {
	return paseto.V4PublicKeySetKey{
		ID:        id,
		Key:       secretKey.Public(),
		NotBefore: notBefore,
		NotAfter:  notAfter,
	}
}

func activeV4KeySetEntry(id string, secretKey paseto.V4AsymmetricSecretKey) paseto.V4PublicKeySetKey {
	now := time.Now()
	return v4KeySetEntry(id, secretKey, now.Add(-time.Hour), now.Add(time.Hour))
}

func signV4WithFooter(secretKey paseto.V4AsymmetricSecretKey, footer string) string {
	token := paseto.NewToken()
	token.SetString("test", "data")
	token.SetExpiration(time.Now().Add(time.Hour))
	if footer != "" {
		token.SetFooter([]byte(footer))
	}

	return token.V4Sign(secretKey, nil)
}

func TestV4KeySetRotationOverlap(t *testing.T) {
	oldSecret := paseto.NewV4AsymmetricSecretKey()
	newSecret := paseto.NewV4AsymmetricSecretKey()

	now := time.Now()
	keySet, err := paseto.NewV4PublicKeySet(4)
	require.NoError(t, err)

	// Old and new keys have overlapping validity windows, so tokens signed
	// by either key verify during the rotation period.
	require.NoError(t, keySet.AddKey(v4KeySetEntry("old", oldSecret, now.Add(-2*time.Hour), now.Add(time.Hour))))
	require.NoError(t, keySet.AddKey(v4KeySetEntry("new", newSecret, now.Add(-time.Hour), now.Add(2*time.Hour))))

	parser := paseto.NewParser()

	oldToken := signV4WithFooter(oldSecret, `{"kid":"old"}`)
	parsed, keyID, err := parser.ParseV4PublicWithKeySet(keySet, oldToken, nil)
	require.NoError(t, err)
	require.Equal(t, "old", keyID)

	value, err := parsed.GetString("test")
	require.NoError(t, err)
	require.Equal(t, "data", value)

	newToken := signV4WithFooter(newSecret, `{"kid":"new"}`)
	_, keyID, err = parser.ParseV4PublicWithKeySet(keySet, newToken, nil)
	require.NoError(t, err)
	require.Equal(t, "new", keyID)

	// Tokens without any footer hint also verify against the active set.
	hintlessToken := signV4WithFooter(newSecret, "")
	_, keyID, err = parser.ParseV4PublicWithKeySet(keySet, hintlessToken, nil)
	require.NoError(t, err)
	require.Equal(t, "new", keyID)
}

func TestV4KeySetDeactivatedKey(t *testing.T) {
	retiredSecret := paseto.NewV4AsymmetricSecretKey()
	activeSecret := paseto.NewV4AsymmetricSecretKey()

	now := time.Now()
	keySet, err := paseto.NewV4PublicKeySet(4)
	require.NoError(t, err)

	require.NoError(t, keySet.AddKey(v4KeySetEntry("retired", retiredSecret, now.Add(-2*time.Hour), now.Add(-time.Hour))))
	require.NoError(t, keySet.AddKey(activeV4KeySetEntry("active", activeSecret)))

	parser := paseto.NewParser()

	// A token signed by the retired key must not verify, even though the
	// key is still present in the set and the footer points at it.
	retiredToken := signV4WithFooter(retiredSecret, `{"kid":"retired"}`)
	_, _, err = parser.ParseV4PublicWithKeySet(keySet, retiredToken, nil)
	require.Error(t, err)

	_, keyID, err := parser.ParseV4PublicWithKeySet(keySet, signV4WithFooter(activeSecret, `{"kid":"active"}`), nil)
	require.NoError(t, err)
	require.Equal(t, "active", keyID)
}

func TestV4KeySetNotYetActiveKey(t *testing.T) {
	secret := paseto.NewV4AsymmetricSecretKey()

	now := time.Now()
	keySet, err := paseto.NewV4PublicKeySet(2)
	require.NoError(t, err)
	require.NoError(t, keySet.AddKey(v4KeySetEntry("future", secret, now.Add(time.Hour), now.Add(2*time.Hour))))

	parser := paseto.NewParser()
	_, _, err = parser.ParseV4PublicWithKeySet(keySet, signV4WithFooter(secret, `{"kid":"future"}`), nil)
	require.Error(t, err)
}

func TestV4KeySetTamperedFooter(t *testing.T) {
	secret := paseto.NewV4AsymmetricSecretKey()

	keySet, err := paseto.NewV4PublicKeySet(2)
	require.NoError(t, err)
	require.NoError(t, keySet.AddKey(activeV4KeySetEntry("good", secret)))

	parser := paseto.NewParser()

	token := signV4WithFooter(secret, `{"kid":"good"}`)
	_, keyID, err := parser.ParseV4PublicWithKeySet(keySet, token, nil)
	require.NoError(t, err)
	require.Equal(t, "good", keyID)

	// Swap the footer for one pointing at a different key ID. The footer is
	// covered by the signature, so this must fail verification.
	parts := strings.Split(token, ".")
	require.Len(t, parts, 4)
	parts[3] = base64.RawURLEncoding.EncodeToString([]byte(`{"kid":"evil"}`))
	tampered := strings.Join(parts, ".")

	_, _, err = parser.ParseV4PublicWithKeySet(keySet, tampered, nil)
	require.Error(t, err)

	// A truncated token (footer dropped entirely) must also fail.
	_, _, err = parser.ParseV4PublicWithKeySet(keySet, strings.Join(parts[:3], "."), nil)
	require.Error(t, err)
}

func TestV4KeySetUnknownHintDoesNotFetch(t *testing.T) {
	secret := paseto.NewV4AsymmetricSecretKey()

	keySet, err := paseto.NewV4PublicKeySet(2)
	require.NoError(t, err)
	require.NoError(t, keySet.AddKey(activeV4KeySetEntry("known", secret)))

	parser := paseto.NewParser()

	// The footer hint names a key ID which is not in the set. The hint is
	// only an untrusted routing hint: it must not trigger any external key
	// lookup, and the token still verifies against the known active keys.
	token := signV4WithFooter(secret, `{"kid":"does-not-exist"}`)
	_, keyID, err := parser.ParseV4PublicWithKeySet(keySet, token, nil)
	require.NoError(t, err)
	require.Equal(t, "known", keyID)
}

func TestV4KeySetNoProtocolFallback(t *testing.T) {
	secret := paseto.NewV4AsymmetricSecretKey()

	keySet, err := paseto.NewV4PublicKeySet(2)
	require.NoError(t, err)
	require.NoError(t, keySet.AddKey(activeV4KeySetEntry("good", secret)))

	parser := paseto.NewParser()

	// A v4.local token must not be accepted by the v4.public key set parser.
	localToken := paseto.NewToken()
	localToken.SetString("test", "data")
	_, _, err = parser.ParseV4PublicWithKeySet(keySet, localToken.V4Encrypt(paseto.NewV4SymmetricKey(), nil), nil)
	require.Error(t, err)

	// A v2.public token must not be accepted either: no protocol fallback.
	v2Token := paseto.NewToken()
	v2Token.SetString("test", "data")
	_, _, err = parser.ParseV4PublicWithKeySet(keySet, v2Token.V2Sign(paseto.NewV2AsymmetricSecretKey()), nil)
	require.Error(t, err)
}

func TestV4KeySetDuplicateIDRejected(t *testing.T) {
	secret := paseto.NewV4AsymmetricSecretKey()
	otherSecret := paseto.NewV4AsymmetricSecretKey()

	keySet, err := paseto.NewV4PublicKeySet(4)
	require.NoError(t, err)

	require.NoError(t, keySet.AddKey(activeV4KeySetEntry("key", secret)))
	require.Error(t, keySet.AddKey(activeV4KeySetEntry("key", otherSecret)))

	// ReplaceKeys with duplicate IDs fails and leaves the set unchanged.
	require.Error(t, keySet.ReplaceKeys(
		activeV4KeySetEntry("dup", secret),
		activeV4KeySetEntry("dup", otherSecret),
	))

	parser := paseto.NewParser()
	_, keyID, err := parser.ParseV4PublicWithKeySet(keySet, signV4WithFooter(secret, ""), nil)
	require.NoError(t, err)
	require.Equal(t, "key", keyID)
}

func TestV4KeySetLimits(t *testing.T) {
	_, err := paseto.NewV4PublicKeySet(0)
	require.Error(t, err)

	keySet, err := paseto.NewV4PublicKeySet(2)
	require.NoError(t, err)

	require.NoError(t, keySet.AddKey(activeV4KeySetEntry("one", paseto.NewV4AsymmetricSecretKey())))
	require.NoError(t, keySet.AddKey(activeV4KeySetEntry("two", paseto.NewV4AsymmetricSecretKey())))
	require.Error(t, keySet.AddKey(activeV4KeySetEntry("three", paseto.NewV4AsymmetricSecretKey())))

	require.Error(t, keySet.ReplaceKeys(
		activeV4KeySetEntry("one", paseto.NewV4AsymmetricSecretKey()),
		activeV4KeySetEntry("two", paseto.NewV4AsymmetricSecretKey()),
		activeV4KeySetEntry("three", paseto.NewV4AsymmetricSecretKey()),
	))

	// Invalid windows and empty IDs are rejected.
	now := time.Now()
	require.Error(t, keySet.AddKey(v4KeySetEntry("bad-window", paseto.NewV4AsymmetricSecretKey(), now, now.Add(-time.Hour))))
	require.Error(t, keySet.AddKey(v4KeySetEntry("", paseto.NewV4AsymmetricSecretKey(), now, now.Add(time.Hour))))
}

func TestV4KeySetConcurrentRotation(t *testing.T) {
	oldSecret := paseto.NewV4AsymmetricSecretKey()
	newSecret := paseto.NewV4AsymmetricSecretKey()

	keySet, err := paseto.NewV4PublicKeySet(2)
	require.NoError(t, err)
	require.NoError(t, keySet.ReplaceKeys(
		activeV4KeySetEntry("old", oldSecret),
		activeV4KeySetEntry("new", newSecret),
	))

	parser := paseto.NewParser()
	oldToken := signV4WithFooter(oldSecret, `{"kid":"old"}`)
	newToken := signV4WithFooter(newSecret, `{"kid":"new"}`)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	errs := make(chan error, 8)

	// Readers continuously parse the token signed by the key which is
	// always present in the set; it must always verify with the same ID.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}

				_, keyID, err := parser.ParseV4PublicWithKeySet(keySet, newToken, nil)
				if err != nil {
					errs <- err
					return
				}
				if keyID != "new" {
					errs <- fmt.Errorf("expected key ID `new', got `%s'", keyID)
					return
				}
			}
		}()
	}

	// Rotator swaps the old key in and out while readers are parsing. The
	// new key is always present, so readers must observe a consistent set.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_ = keySet.ReplaceKeys(activeV4KeySetEntry("new", newSecret))
			_ = keySet.ReplaceKeys(
				activeV4KeySetEntry("old", oldSecret),
				activeV4KeySetEntry("new", newSecret),
			)
		}
		close(stop)
	}()

	// Old-token parses may succeed or fail depending on which snapshot a
	// parse observes, but must never report the wrong key ID.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}

			_, keyID, err := parser.ParseV4PublicWithKeySet(keySet, oldToken, nil)
			if err == nil && keyID != "old" {
				errs <- fmt.Errorf("expected key ID `old', got `%s'", keyID)
				return
			}
		}
	}()

	wg.Wait()
	select {
	case err := <-errs:
		require.NoError(t, err)
	default:
	}
}
