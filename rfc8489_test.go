// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package stun

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rfc8489LongTermRequest is a Binding Request built deterministically from
// the long-term credentials and encoding rules of RFC 8489 Appendix B.2. It
// carries USERHASH, PASSWORD-ALGORITHMS (SHA-256) and
// MESSAGE-INTEGRITY-SHA256. The USERHASH and the SHA-256 key derivation use
// the RFC's username "Bob@example.org", realm "example.org" and password
// "TheMatrIX"; the HMAC is verified against an independent crypto/hmac
// computation as well as the library's Check method.
const rfc8489LongTermRequest = "" +
	"000100bc2112a44278207479666d74757963696b" +
	"802200105354554e207465737420636c69656e74" +
	"0006000f426f62406578616d706c652e6f726700" +
	"0015001a454d41396b39366f6d394a424d4b326b3170646c4943356832360000" +
	"0014000b6578616d706c652e6f726700" +
	"001e00204707006dde4bccad9c5f23f4e4fc738937a45a6974ac4182894cee2c7f77ddfd" +
	"80020018000200140001001000000000000000000000000000000000" +
	"001c00205724939fc4e27c3b61f792d3d8630d53f7454b59b5ade4ef12866cb247fc7d2f"

const (
	rfc8489Username = "Bob@example.org"
	rfc8489Realm    = "example.org"
	rfc8489Password = "TheMatrIX"
	rfc8489Nonce    = "EMA9k96om9JBMK2k1pdlIC5h26"
)

func TestRFC8489_LongTermRequest(t *testing.T) {
	raw, err := hex.DecodeString(rfc8489LongTermRequest)
	require.NoError(t, err)

	msg := new(Message)
	require.NoError(t, Decode(raw, msg))

	t.Run("USERHASH", func(t *testing.T) {
		var userhash Userhash
		require.NoError(t, userhash.GetFrom(msg))
		assert.Equal(t, NewUserhash(rfc8489Username, rfc8489Realm), userhash)
		assert.NoError(t, UserhashChecker{
			Username: rfc8489Username,
			Realm:    rfc8489Realm,
		}.Check(msg))
	})

	t.Run("PASSWORD-ALGORITHMS", func(t *testing.T) {
		var algorithms PasswordAlgorithms
		require.NoError(t, algorithms.GetFrom(msg))
		require.Len(t, algorithms, 1)
		assert.Equal(t, PasswordAlgorithmSHA256, algorithms[0].Algorithm)
		// SHA-256 entry advertises a nested MD5 fallback in its parameters.
		assert.Equal(t,
			[]byte{
				0x00, 0x01, 0x00, 0x10,
				0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
			},
			algorithms[0].Parameters,
		)
	})

	t.Run("MESSAGE-INTEGRITY-SHA256", func(t *testing.T) {
		integrity := NewLongTermIntegritySHA256(rfc8489Username, rfc8489Realm, rfc8489Password)

		before := append([]byte(nil), msg.Raw...)

		// Independently verify the HMAC with crypto/hmac over the bytes up
		// to MESSAGE-INTEGRITY-SHA256, with the length field adjusted to
		// cover it.
		attrStart := len(before) - (attributeHeaderSize + messageIntegritySHA256Size)
		independent := append([]byte(nil), before[:attrStart]...)
		bin.PutUint16(independent[2:4], uint16(attrStart-messageHeaderSize+
			attributeHeaderSize+messageIntegritySHA256Size))
		mac := hmac.New(sha256.New, integrity)
		mac.Write(independent)
		wireMI, getErr := msg.Get(AttrMessageIntegritySHA256)
		require.NoError(t, getErr)
		assert.Equal(t, mac.Sum(nil)[:len(wireMI)], wireMI)

		assert.NoError(t, integrity.Check(msg))
		assert.True(t, bytes.Equal(before, msg.Raw), "Raw changed after successful Check")

		// Corrupting one authenticated byte must fail the check.
		corrupted := append([]byte(nil), before...)
		corrupted[24]++
		badMsg := new(Message)
		require.NoError(t, Decode(corrupted, badMsg))
		assert.ErrorIs(t, integrity.Check(badMsg), ErrIntegrityMismatch)
		assert.True(t, bytes.Equal(corrupted, badMsg.Raw), "Raw changed after failed Check")
	})

	t.Run("nonce and realm decode", func(t *testing.T) {
		var realm Realm
		require.NoError(t, realm.GetFrom(msg))
		assert.Equal(t, rfc8489Realm, realm.String())

		var nonce Nonce
		require.NoError(t, nonce.GetFrom(msg))
		assert.Equal(t, rfc8489Nonce, nonce.String())
	})
}

func TestRFC8489_PasswordAlgorithmRoundTrip(t *testing.T) {
	m := New()
	m.WriteHeader()
	algorithms := PasswordAlgorithms{
		{Algorithm: PasswordAlgorithmSHA256, Parameters: []byte{0x00, 0x01, 0x00, 0x10}},
		{Algorithm: PasswordAlgorithmMD5},
	}
	require.NoError(t, algorithms.AddTo(m))

	var decoded PasswordAlgorithms
	require.NoError(t, decoded.GetFrom(m))
	assert.Equal(t, algorithms, decoded)

	// Single PASSWORD-ALGORITHM attribute.
	server := New()
	server.WriteHeader()
	selected := NewPasswordAlgorithm(PasswordAlgorithmSHA256, nil)
	require.NoError(t, selected.AddTo(server))

	var got PasswordAlgorithmValue
	require.NoError(t, got.GetFrom(server))
	assert.Equal(t, PasswordAlgorithmSHA256, got.Algorithm)
	assert.Empty(t, got.Parameters)
}

func TestRFC8489_PasswordAlgorithmTruncated(t *testing.T) {
	m := New()
	m.WriteHeader()
	// Declare parameters longer than the actual value.
	m.Add(AttrPasswordAlgorithms, []byte{0x00, 0x02, 0x00, 0x08, 0x00})

	var algorithms PasswordAlgorithms
	assert.Error(t, algorithms.GetFrom(m))
}

func TestRFC8489_LongTermIntegrityForAlgorithm(t *testing.T) {
	md5Key, _, err := LongTermIntegrityForAlgorithm(
		PasswordAlgorithmMD5, "user", "realm", "pass",
	)
	require.NoError(t, err)
	assert.Equal(t, NewLongTermIntegrity("user", "realm", "pass"), md5Key)

	_, sha256Key, err := LongTermIntegrityForAlgorithm(
		PasswordAlgorithmSHA256, "user", "realm", "pass",
	)
	require.NoError(t, err)
	assert.Equal(t, NewLongTermIntegritySHA256("user", "realm", "pass"), sha256Key)

	_, _, err = LongTermIntegrityForAlgorithm(
		PasswordAlgorithmReserved, "user", "realm", "pass",
	)
	assert.ErrorIs(t, err, ErrNoPasswordAlgorithm)
}

// buildDualIntegrityMessage crafts a message carrying both MESSAGE-INTEGRITY
// (SHA-1) and MESSAGE-INTEGRITY-SHA256 attributes, in the order mandated by
// RFC 8489 (MI first, MI-SHA256 after), optionally followed by FINGERPRINT.
func buildDualIntegrityMessage(t *testing.T, withFingerprint bool) *Message {
	t.Helper()

	m := New()
	m.TransactionID = [TransactionIDSize]byte{
		0x78, 0x20, 0x74, 0x79, 0x66, 0x6d, 0x74, 0x75,
		0x79, 0x63, 0x69, 0x6b,
	}
	m.WriteHeader()
	require.NoError(t, NewSoftware("STUN test client").AddTo(m))

	sha1Key := NewLongTermIntegrity(rfc8489Username, rfc8489Realm, rfc8489Password)
	sha256Key := NewLongTermIntegritySHA256(rfc8489Username, rfc8489Realm, rfc8489Password)
	require.NoError(t, sha1Key.AddTo(m))
	require.NoError(t, sha256Key.AddTo(m))
	if withFingerprint {
		require.NoError(t, Fingerprint.AddTo(m))
	}

	return m
}

func TestDualIntegrity_VerifyBoth(t *testing.T) {
	for _, withFingerprint := range []bool{false, true} {
		name := "no fingerprint"
		if withFingerprint {
			name = "with fingerprint"
		}

		t.Run(name, func(t *testing.T) {
			original := buildDualIntegrityMessage(t, withFingerprint)

			// Decode the wire bytes into a fresh message.
			decoded := New()
			require.NoError(t, Decode(original.Raw, decoded))

			sha1Key := NewLongTermIntegrity(rfc8489Username, rfc8489Realm, rfc8489Password)
			sha256Key := NewLongTermIntegritySHA256(rfc8489Username, rfc8489Realm, rfc8489Password)

			snapshot := append([]byte(nil), decoded.Raw...)

			assert.NoError(t, sha1Key.Check(decoded), "SHA-1 integrity must verify")
			assert.True(t, bytes.Equal(snapshot, decoded.Raw), "Raw changed after SHA-1 Check")

			assert.NoError(t, sha256Key.Check(decoded), "SHA-256 integrity must verify")
			assert.True(t, bytes.Equal(snapshot, decoded.Raw), "Raw changed after SHA-256 Check")

			if withFingerprint {
				assert.NoError(t, Fingerprint.Check(decoded))
				assert.True(t, bytes.Equal(snapshot, decoded.Raw), "Raw changed after FINGERPRINT Check")
			}

			// Both attributes are present and ordered MI before MI-SHA256.
			miIdx, mi256Idx := -1, -1
			for i, a := range decoded.Attributes {
				switch a.Type {
				case AttrMessageIntegrity:
					miIdx = i
				case AttrMessageIntegritySHA256:
					mi256Idx = i
				}
			}
			require.GreaterOrEqual(t, miIdx, 0)
			require.GreaterOrEqual(t, mi256Idx, 0)
			assert.Less(t, miIdx, mi256Idx)
		})
	}
}

func TestDualIntegrity_FailureKeepsRaw(t *testing.T) {
	original := buildDualIntegrityMessage(t, true)

	// Tamper with an authenticated byte (SOFTWARE value, byte 24).
	tampered := append([]byte(nil), original.Raw...)
	tampered[24] ^= 0xFF

	decoded := New()
	require.NoError(t, Decode(tampered, decoded))

	wrongSHA1 := NewShortTermIntegrity("wrong-password")
	wrongSHA256 := NewShortTermIntegritySHA256("wrong-password")

	assert.ErrorIs(t, wrongSHA1.Check(decoded), ErrIntegrityMismatch)
	assert.True(t, bytes.Equal(tampered, decoded.Raw), "Raw changed after failed SHA-1 Check")

	assert.ErrorIs(t, wrongSHA256.Check(decoded), ErrIntegrityMismatch)
	assert.True(t, bytes.Equal(tampered, decoded.Raw), "Raw changed after failed SHA-256 Check")
}

func TestMessageIntegritySHA256_Truncated(t *testing.T) {
	m := New()
	m.WriteHeader()
	key := NewShortTermIntegritySHA256("password")
	require.NoError(t, key.AddTo(m))

	// Build a message carrying a 16-byte truncated HMAC. The sender
	// computes the HMAC with the length field adjusted to cover the
	// truncated attribute (20 bytes), then emits only the first 16 bytes.
	wire := append([]byte(nil), m.Raw...)
	attrStart := len(wire) - (attributeHeaderSize + messageIntegritySHA256Size)

	// Shrink message length to cover the truncated TLV (4 + 16).
	truncLen := uint16((attrStart - messageHeaderSize) +
		attributeHeaderSize + messageIntegritySHA256TruncSize)
	wire[2] = byte(truncLen >> 8)
	wire[3] = byte(truncLen)

	expected := newHMACSHA256(key, wire[:attrStart], nil)

	wire[attrStart+2] = 0x00
	wire[attrStart+3] = byte(messageIntegritySHA256TruncSize)
	copy(wire[attrStart+4:attrStart+4+messageIntegritySHA256TruncSize],
		expected[:messageIntegritySHA256TruncSize])
	wire = wire[:attrStart+attributeHeaderSize+messageIntegritySHA256TruncSize]

	decoded := New()
	require.NoError(t, Decode(wire, decoded))
	assert.NoError(t, key.Check(decoded))
}

func TestMessageIntegritySHA256_InvalidLength(t *testing.T) {
	m := New()
	m.WriteHeader()
	m.Add(AttrMessageIntegritySHA256, make([]byte, 10))

	key := NewShortTermIntegritySHA256("password")
	assert.Error(t, key.Check(m))
}

func TestMessageIntegritySHA256_AfterFingerprint(t *testing.T) {
	m := New()
	m.WriteHeader()
	require.NoError(t, Fingerprint.AddTo(m))

	key := NewShortTermIntegritySHA256("password")
	assert.ErrorIs(t, key.AddTo(m), ErrFingerprintBeforeIntegrity)
}

func TestUserhash_AddAndCheck(t *testing.T) {
	m := New()
	m.WriteHeader()
	require.NoError(t, NewUserhash(rfc8489Username, rfc8489Realm).AddTo(m))

	var userhash Userhash
	require.NoError(t, userhash.GetFrom(m))
	assert.Len(t, userhash, userhashSize)
	assert.NoError(t, UserhashChecker{
		Username: rfc8489Username,
		Realm:    rfc8489Realm,
	}.Check(m))

	assert.ErrorIs(t, UserhashChecker{
		Username: "wrong",
		Realm:    rfc8489Realm,
	}.Check(m), ErrIntegrityMismatch)
}

func TestUserhash_BadSize(t *testing.T) {
	m := New()
	m.WriteHeader()
	m.Add(AttrUserhash, make([]byte, 10))

	var userhash Userhash
	assert.Error(t, userhash.GetFrom(m))
}
