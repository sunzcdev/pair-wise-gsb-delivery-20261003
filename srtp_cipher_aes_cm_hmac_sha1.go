// SPDX-FileCopyrightText: 2026 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package srtp

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec
	"crypto/subtle"
	"encoding/binary"
	"hash"
	"math/bits"

	"github.com/pion/rtp"
)

// maxKDRPeriodsCached bounds the number of key derivation periods for which
// session keys are cached when a non-zero key derivation rate is used.
// Evicted periods are re-derived from the master key on demand.
const maxKDRPeriodsCached = 8

// aesCmPeriodKeys holds the SRTP/SRTCP session keys derived for a single
// key derivation period ("index DIV kdr") as defined in RFC 3711, Section 4.3.
type aesCmPeriodKeys struct {
	srtpSessionSalt []byte
	srtpSessionAuth hash.Hash
	srtpBlock       cipher.Block

	srtcpSessionSalt []byte
	srtcpSessionAuth hash.Hash
	srtcpBlock       cipher.Block
}

// clear releases all references to the derived key material and zeroes the
// session salts, so the material does not linger in memory after the period
// has been evicted from the cache.
func (k *aesCmPeriodKeys) clear() {
	for i := range k.srtpSessionSalt {
		k.srtpSessionSalt[i] = 0
	}
	for i := range k.srtcpSessionSalt {
		k.srtcpSessionSalt[i] = 0
	}
	k.srtpSessionSalt = nil
	k.srtpSessionAuth = nil
	k.srtpBlock = nil
	k.srtcpSessionSalt = nil
	k.srtcpSessionAuth = nil
	k.srtcpBlock = nil
}

type srtpCipherAesCmHmacSha1 struct {
	protectionProfileWithArgs

	// masterKey and masterSalt are retained only when keyDerivationRate is
	// non-zero, to derive session keys for later key derivation periods.
	masterKey  []byte
	masterSalt []byte

	// keyDerivationRate is the kdr from RFC 3711, Section 4.3.1.
	// Zero disables periodic re-derivation (all packets use period 0).
	keyDerivationRate uint64
	// kdrShift is log2(keyDerivationRate), valid when keyDerivationRate != 0.
	kdrShift uint
	// periodKeys caches derived session keys per key derivation period.
	// It is bounded by maxKDRPeriodsCached.
	periodKeys map[uint64]*aesCmPeriodKeys

	srtpEncrypted  bool
	srtcpEncrypted bool

	mki []byte

	useCryptex bool

	// Pre-allocated buffers for auth tag to avoid heap allocation in hot path.
	authBuf     [4 + sha1.Size]byte // 4 bytes ROC + 20 bytes SHA1
	rtcpAuthBuf [sha1.Size]byte
}

func (s *srtpCipherAesCmHmacSha1) setCryptex(useCryptex bool) {
	s.useCryptex = useCryptex
}

//nolint:cyclop
func newSrtpCipherAesCmHmacSha1(
	profile protectionProfileWithArgs,
	masterKey, masterSalt, mki []byte,
	encryptSRTP, encryptSRTCP, useCryptex bool,
	keyDerivationRate uint64,
) (*srtpCipherAesCmHmacSha1, error) {
	switch profile.ProtectionProfile {
	case ProtectionProfileNullHmacSha1_80, ProtectionProfileNullHmacSha1_32:
		encryptSRTP = false
		encryptSRTCP = false
	default:
	}

	srtpCipher := &srtpCipherAesCmHmacSha1{
		protectionProfileWithArgs: profile,
		srtpEncrypted:             encryptSRTP,
		srtcpEncrypted:            encryptSRTCP,
		useCryptex:                useCryptex,
		keyDerivationRate:         keyDerivationRate,
		periodKeys:                map[uint64]*aesCmPeriodKeys{},
	}

	if keyDerivationRate != 0 {
		srtpCipher.kdrShift = uint(bits.TrailingZeros64(keyDerivationRate)) //nolint:gosec // G115
		srtpCipher.masterKey = bytes.Clone(masterKey)
		srtpCipher.masterSalt = bytes.Clone(masterSalt)
	}

	periodKeys, err := srtpCipher.derivePeriodKeys(0, masterKey, masterSalt)
	if err != nil {
		return nil, err
	}
	srtpCipher.periodKeys[0] = periodKeys

	mkiLen := len(mki)
	if mkiLen > 0 {
		srtpCipher.mki = make([]byte, mkiLen)
		copy(srtpCipher.mki, mki)
	}

	return srtpCipher, nil
}

// derivePeriodKeys derives the SRTP/SRTCP session keys for one key derivation
// period from the given master key and salt.
func (s *srtpCipherAesCmHmacSha1) derivePeriodKeys(
	period uint64, masterKey, masterSalt []byte,
) (*aesCmPeriodKeys, error) {
	keys := &aesCmPeriodKeys{}

	srtpSessionKey, err := aesCmKeyDerivation(labelSRTPEncryption, masterKey, masterSalt, period, len(masterKey))
	if err != nil {
		return nil, err
	} else if keys.srtpBlock, err = aes.NewCipher(srtpSessionKey); err != nil {
		return nil, err
	}

	srtcpSessionKey, err := aesCmKeyDerivation(labelSRTCPEncryption, masterKey, masterSalt, period, len(masterKey))
	if err != nil {
		return nil, err
	} else if keys.srtcpBlock, err = aes.NewCipher(srtcpSessionKey); err != nil {
		return nil, err
	}

	if keys.srtpSessionSalt, err = aesCmKeyDerivation(
		labelSRTPSalt, masterKey, masterSalt, period, len(masterSalt),
	); err != nil {
		return nil, err
	} else if keys.srtcpSessionSalt, err = aesCmKeyDerivation(
		labelSRTCPSalt, masterKey, masterSalt, period, len(masterSalt),
	); err != nil {
		return nil, err
	}

	authKeyLen, err := s.AuthKeyLen()
	if err != nil {
		return nil, err
	}

	srtpSessionAuthTag, err := aesCmKeyDerivation(labelSRTPAuthenticationTag, masterKey, masterSalt, period, authKeyLen)
	if err != nil {
		return nil, err
	}

	srtcpSessionAuthTag, err := aesCmKeyDerivation(
		labelSRTCPAuthenticationTag, masterKey, masterSalt, period, authKeyLen,
	)
	if err != nil {
		return nil, err
	}

	keys.srtcpSessionAuth = hmac.New(sha1.New, srtcpSessionAuthTag)
	keys.srtpSessionAuth = hmac.New(sha1.New, srtpSessionAuthTag)

	return keys, nil
}

// keyDerivationPeriod returns the key derivation period ("index DIV kdr")
// for a packet index. The index is 2^16 * ROC + SEQ for SRTP and the
// SRTCP index for SRTCP.
func (s *srtpCipherAesCmHmacSha1) keyDerivationPeriod(index uint64) uint64 {
	if s.keyDerivationRate == 0 {
		return 0
	}

	return index >> s.kdrShift
}

// srtpPeriodKeys returns the session keys for the key derivation period of
// the SRTP packet with the given sequence number and ROC.
func (s *srtpCipherAesCmHmacSha1) srtpPeriodKeys(sequenceNumber uint16, roc uint32) (*aesCmPeriodKeys, error) {
	return s.keysForPeriod(s.keyDerivationPeriod(uint64(roc)<<16 | uint64(sequenceNumber)))
}

// srtcpPeriodKeys returns the session keys for the key derivation period of
// the SRTCP packet with the given index.
func (s *srtpCipherAesCmHmacSha1) srtcpPeriodKeys(srtcpIndex uint32) (*aesCmPeriodKeys, error) {
	return s.keysForPeriod(s.keyDerivationPeriod(uint64(srtcpIndex)))
}

// keysForPeriod returns the cached session keys for a key derivation period,
// deriving and caching them if necessary. The cache is bounded: when it is
// full, the keys of the oldest (lowest) period are released before caching
// the new ones. Evicted periods are transparently re-derived on demand, so
// late out-of-order packets from an older period can still be processed with
// the keys of that period.
func (s *srtpCipherAesCmHmacSha1) keysForPeriod(period uint64) (*aesCmPeriodKeys, error) {
	if keys, ok := s.periodKeys[period]; ok {
		return keys, nil
	}

	keys, err := s.derivePeriodKeys(period, s.masterKey, s.masterSalt)
	if err != nil {
		return nil, err
	}

	if len(s.periodKeys) >= maxKDRPeriodsCached {
		var oldest uint64
		first := true
		for p := range s.periodKeys {
			if first || p < oldest {
				oldest = p
				first = false
			}
		}
		s.periodKeys[oldest].clear()
		delete(s.periodKeys, oldest)
	}

	s.periodKeys[period] = keys

	return keys, nil
}

func (s *srtpCipherAesCmHmacSha1) encryptRTP(
	dst []byte,
	header *rtp.Header,
	headerLen int,
	plaintext []byte,
	roc uint32,
	rocInAuthTag bool,
) (ciphertext []byte, err error) {
	// Grow the given buffer to fit the output.
	authTagLen, err := s.AuthTagRTPLen()
	if err != nil {
		return nil, err
	}
	payloadLen := len(plaintext) - headerLen
	dstLen := headerLen + payloadLen + len(s.mki) + authTagLen

	insertEmptyExtHdr := needsEmptyExtensionHeader(s.useCryptex, header)
	if insertEmptyExtHdr {
		dstLen += extensionHeaderSize
	}

	dst = growBufferSize(dst, dstLen)
	sameBuffer := isSameBuffer(dst, plaintext)

	if insertEmptyExtHdr {
		// Insert an empty extension header to plaintext using dst buffer. After this operation dst is used as the
		// plaintext buffer for next operations.
		plaintext = insertEmptyExtensionHeader(dst, plaintext, sameBuffer, header)
		sameBuffer = true
		headerLen += extensionHeaderSize
	}

	err = s.doEncryptRTP(dst, header, headerLen, plaintext, roc, rocInAuthTag, sameBuffer, payloadLen)
	if err != nil {
		return nil, err
	}

	return dst, nil
}

func (s *srtpCipherAesCmHmacSha1) doEncryptRTP(dst []byte, header *rtp.Header, headerLen int, plaintext []byte,
	roc uint32, rocInAuthTag bool, sameBuffer bool, payloadLen int,
) error {
	keys, err := s.srtpPeriodKeys(header.SequenceNumber, roc)
	if err != nil {
		return err
	}

	encrypt := func(dst, plaintext []byte, headerLen int) error {
		counter := generateCounter(header.SequenceNumber, roc, header.SSRC, keys.srtpSessionSalt)

		return xorBytesCTR(keys.srtpBlock, counter[:], dst[headerLen:], plaintext[headerLen:])
	}

	switch {
	case s.useCryptex && header.Extension:
		err = encryptCryptexRTP(dst, plaintext, sameBuffer, header, encrypt)
	case s.srtpEncrypted:
		// Copy the header unencrypted.
		if !sameBuffer {
			copy(dst, plaintext[:headerLen])
		}
		// Encrypt the payload
		err = encrypt(dst, plaintext, headerLen)
	case !sameBuffer:
		copy(dst, plaintext)
	default:
	}
	if err != nil {
		return err
	}
	n := headerLen + payloadLen

	// Generate the auth tag.
	authTag, err := s.generateSrtpAuthTag(keys, dst[:n], roc, rocInAuthTag)
	if err != nil {
		return err
	}

	// Append the MKI (if used)
	if len(s.mki) > 0 {
		copy(dst[n:], s.mki)
		n += len(s.mki)
	}

	// Write the auth tag to the dest.
	copy(dst[n:], authTag)

	return nil
}

func (s *srtpCipherAesCmHmacSha1) decryptRTP(
	dst, ciphertext []byte,
	header *rtp.Header,
	headerLen int,
	roc uint32,
	rocInAuthTag bool,
) ([]byte, error) {
	// Split the auth tag and the cipher text into two parts.
	authTagLen, err := s.AuthTagRTPLen()
	if err != nil {
		return nil, err
	}

	// Split the auth tag and the cipher text into two parts.
	actualTag := ciphertext[len(ciphertext)-authTagLen:]
	ciphertext = ciphertext[:len(ciphertext)-len(s.mki)-authTagLen]

	// Select the session keys of the key derivation period this packet
	// belongs to. Late out-of-order packets from a previous period are
	// authenticated and decrypted with the keys of that period.
	keys, err := s.srtpPeriodKeys(header.SequenceNumber, roc)
	if err != nil {
		return nil, err
	}

	// Generate the auth tag we expect to see from the ciphertext.
	expectedTag, err := s.generateSrtpAuthTag(keys, ciphertext, roc, rocInAuthTag)
	if err != nil {
		return nil, err
	}

	// See if the auth tag actually matches.
	// We use a constant time comparison to prevent timing attacks.
	if subtle.ConstantTimeCompare(actualTag, expectedTag) != 1 {
		return nil, ErrFailedToVerifyAuthTag
	}

	sameBuffer := isSameBuffer(dst, ciphertext)

	err = s.doDecryptRTP(dst, ciphertext, header, headerLen, roc, sameBuffer, keys)
	if err != nil {
		return nil, err
	}

	return dst, nil
}

func (s *srtpCipherAesCmHmacSha1) doDecryptRTP(dst, ciphertext []byte, header *rtp.Header, headerLen int, roc uint32,
	sameBuffer bool, keys *aesCmPeriodKeys,
) error {
	decrypt := func(dst, ciphertext []byte, headerLen int) error {
		counter := generateCounter(header.SequenceNumber, roc, header.SSRC, keys.srtpSessionSalt)

		return xorBytesCTR(keys.srtpBlock, counter[:], dst[headerLen:], ciphertext[headerLen:])
	}

	switch {
	case isCryptexPacket(header):
		err := decryptCryptexRTP(dst, ciphertext, sameBuffer, header, headerLen, decrypt)
		if err != nil {
			return err
		}
	case s.srtpEncrypted:
		// Write the plaintext header to the destination buffer.
		if !sameBuffer {
			copy(dst, ciphertext[:headerLen])
		}

		// Decrypt the ciphertext for the payload.
		err := decrypt(dst, ciphertext, headerLen)
		if err != nil {
			return err
		}
	case !sameBuffer:
		copy(dst, ciphertext)
	default:
	}

	return nil
}

func (s *srtpCipherAesCmHmacSha1) encryptRTCP(dst, decrypted []byte, srtcpIndex uint32, ssrc uint32) ([]byte, error) {
	authTagLen, err := s.AuthTagRTCPLen()
	if err != nil {
		return nil, err
	}
	mkiLen := len(s.mki)
	decryptedLen := len(decrypted)
	encryptedLen := decryptedLen + authTagLen + mkiLen + srtcpIndexSize

	dst = growBufferSize(dst, encryptedLen)
	sameBuffer := isSameBuffer(dst, decrypted)

	keys, err := s.srtcpPeriodKeys(srtcpIndex)
	if err != nil {
		return nil, err
	}

	if !sameBuffer {
		copy(dst, decrypted[:srtcpHeaderSize]) // Copy the first 8 bytes (RTCP header)
	}

	// Encrypt everything after header
	if s.srtcpEncrypted {
		counter := generateCounter(uint16(srtcpIndex&0xffff), srtcpIndex>>16, ssrc, keys.srtcpSessionSalt) //nolint:gosec // G115
		if err = xorBytesCTR(keys.srtcpBlock, counter[:], dst[srtcpHeaderSize:], decrypted[srtcpHeaderSize:]); err != nil {
			return nil, err
		}

		// Add SRTCP Index and set Encryption bit
		binary.BigEndian.PutUint32(dst[decryptedLen:], srtcpIndex)
		dst[decryptedLen] |= srtcpEncryptionFlag
	} else {
		// Copy the decrypted payload as is
		if !sameBuffer {
			copy(dst[srtcpHeaderSize:], decrypted[srtcpHeaderSize:])
		}

		// Add SRTCP Index with Encryption bit cleared
		binary.BigEndian.PutUint32(dst[decryptedLen:], srtcpIndex)
	}

	n := decryptedLen + srtcpIndexSize

	// Generate the authentication tag
	authTag, err := s.generateSrtcpAuthTag(keys, dst[:n])
	if err != nil {
		return nil, err
	}

	// Include the MKI if provided
	if len(s.mki) > 0 {
		copy(dst[n:], s.mki)
		n += mkiLen
	}

	// Append the auth tag at the end of the buffer
	copy(dst[n:], authTag)

	return dst, nil
}

func (s *srtpCipherAesCmHmacSha1) decryptRTCP(dst, encrypted []byte, index, ssrc uint32) ([]byte, error) {
	authTagLen, err := s.AuthTagRTCPLen()
	if err != nil {
		return nil, err
	}
	mkiLen := len(s.mki)
	encryptedLen := len(encrypted)
	decryptedLen := encryptedLen - (authTagLen + mkiLen + srtcpIndexSize)
	if decryptedLen < 8 {
		return nil, errTooShortRTCP
	}

	// Select the session keys of the key derivation period this packet
	// belongs to, so late out-of-order packets from a previous period are
	// authenticated and decrypted with the keys of that period.
	keys, err := s.srtcpPeriodKeys(index)
	if err != nil {
		return nil, err
	}

	expectedTag, err := s.generateSrtcpAuthTag(keys, encrypted[:encryptedLen-mkiLen-authTagLen])
	if err != nil {
		return nil, err
	}

	actualTag := encrypted[encryptedLen-authTagLen:]
	if subtle.ConstantTimeCompare(actualTag, expectedTag) != 1 {
		return nil, ErrFailedToVerifyAuthTag
	}

	dst = growBufferSize(dst, decryptedLen)
	sameBuffer := isSameBuffer(dst, encrypted)

	if !sameBuffer {
		copy(dst, encrypted[:srtcpHeaderSize]) // Copy the first 8 bytes (RTCP header)
	}

	isEncrypted := encrypted[decryptedLen]&srtcpEncryptionFlag != 0
	if isEncrypted {
		counter := generateCounter(uint16(index&0xffff), index>>16, ssrc, keys.srtcpSessionSalt) //nolint:gosec // G115
		err = xorBytesCTR(keys.srtcpBlock, counter[:], dst[srtcpHeaderSize:], encrypted[srtcpHeaderSize:decryptedLen])
	} else if !sameBuffer {
		copy(dst[srtcpHeaderSize:], encrypted[srtcpHeaderSize:])
	}

	return dst, err
}

func (s *srtpCipherAesCmHmacSha1) generateSrtpAuthTag(
	keys *aesCmPeriodKeys, buf []byte, roc uint32, rocInAuthTag bool,
) ([]byte, error) {
	// https://tools.ietf.org/html/rfc3711#section-4.2
	// In the case of SRTP, M SHALL consist of the Authenticated
	// Portion of the packet (as specified in Figure 1) concatenated with
	// the ROC, M = Authenticated Portion || ROC;
	//
	// The pre-defined authentication transform for SRTP is HMAC-SHA1
	// [RFC2104].  With HMAC-SHA1, the SRTP_PREFIX_LENGTH (Figure 3) SHALL
	// be 0.  For SRTP (respectively SRTCP), the HMAC SHALL be applied to
	// the session authentication key and M as specified above, i.e.,
	// HMAC(k_a, M).  The HMAC output SHALL then be truncated to the n_tag
	// left-most bits.
	// - Authenticated portion of the packet is everything BEFORE MKI
	// - k_a is the session message authentication key
	// - n_tag is the bit-length of the output authentication tag
	keys.srtpSessionAuth.Reset()

	if _, err := keys.srtpSessionAuth.Write(buf); err != nil {
		return nil, err
	}

	// For SRTP only, we need to hash the rollover counter as well.
	binary.BigEndian.PutUint32(s.authBuf[:4], roc)

	_, err := keys.srtpSessionAuth.Write(s.authBuf[:4])
	if err != nil {
		return nil, err
	}

	// Truncate the hash to the size indicated by the profile
	authTagLen, err := s.AuthTagRTPLen()
	if err != nil {
		return nil, err
	}

	// Use pre-allocated buffer to avoid allocation in Sum().
	// When rocInAuthTag is true, we prepend the ROC (4 bytes) before the hash.
	var authTag []byte
	if rocInAuthTag {
		// ROC is already in buffer at [0:4], Sum will append hash starting at offset 4
		authTag = keys.srtpSessionAuth.Sum(s.authBuf[:4])[:authTagLen]
	} else {
		// No ROC prefix, Sum appends hash starting at offset 0
		authTag = keys.srtpSessionAuth.Sum(s.authBuf[:0])[:authTagLen]
	}

	return authTag, nil
}

func (s *srtpCipherAesCmHmacSha1) generateSrtcpAuthTag(keys *aesCmPeriodKeys, buf []byte) ([]byte, error) {
	// https://tools.ietf.org/html/rfc3711#section-4.2
	//
	// The pre-defined authentication transform for SRTP is HMAC-SHA1
	// [RFC2104].  With HMAC-SHA1, the SRTP_PREFIX_LENGTH (Figure 3) SHALL
	// be 0.  For SRTP (respectively SRTCP), the HMAC SHALL be applied to
	// the session authentication key and M as specified above, i.e.,
	// HMAC(k_a, M).  The HMAC output SHALL then be truncated to the n_tag
	// left-most bits.
	// - Authenticated portion of the packet is everything BEFORE MKI
	// - k_a is the session message authentication key
	// - n_tag is the bit-length of the output authentication tag
	keys.srtcpSessionAuth.Reset()

	if _, err := keys.srtcpSessionAuth.Write(buf); err != nil {
		return nil, err
	}
	authTagLen, err := s.AuthTagRTCPLen()
	if err != nil {
		return nil, err
	}

	// Use pre-allocated buffer to avoid allocation in Sum()
	return keys.srtcpSessionAuth.Sum(s.rtcpAuthBuf[:0])[:authTagLen], nil
}

func (s *srtpCipherAesCmHmacSha1) getRTCPIndex(in []byte) uint32 {
	authTagLen, _ := s.AuthTagRTCPLen()
	tailOffset := len(in) - (authTagLen + srtcpIndexSize + len(s.mki))
	srtcpIndexBuffer := in[tailOffset : tailOffset+srtcpIndexSize]

	return binary.BigEndian.Uint32(srtcpIndexBuffer) &^ (1 << 31)
}
