// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: 2022 mochi-mqtt, mochi-co
// SPDX-FileContributor: mochi-co

// Package scram provides a SCRAM-SHA-256 (RFC 5802 / RFC 7677) authentication
// hook for the MQTT v5 enhanced authentication exchange. Channel binding
// (SCRAM-SHA-256-PLUS) is not supported.
package scram

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Method is the MQTT v5 authentication method name used for the exchange.
const Method = "SCRAM-SHA-256"

var (
	// ErrMalformedMessage indicates a SCRAM message could not be parsed.
	ErrMalformedMessage = errors.New("malformed scram message")

	// ErrChannelBindingUnsupported indicates the client requested channel
	// binding (p= gs2 flag), which is not supported by this hook.
	ErrChannelBindingUnsupported = errors.New("channel binding is not supported")

	// ErrChannelBindingMismatch indicates the c= value of the client-final-message
	// did not match the gs2 header of the client-first-message.
	ErrChannelBindingMismatch = errors.New("channel binding data mismatch")

	// ErrNonceMismatch indicates the nonce of the client-final-message did not
	// match the nonce issued in the server-first-message.
	ErrNonceMismatch = errors.New("nonce mismatch")

	// ErrInvalidProof indicates the client proof did not verify against the
	// stored credentials.
	ErrInvalidProof = errors.New("invalid client proof")
)

// Credentials contains the server-side verification material for a user.
// It contains no plaintext password.
type Credentials struct {
	StoredKey  []byte // H(ClientKey), used to verify the client proof
	ServerKey  []byte // used to generate the server signature
	Salt       []byte // the user's salt
	Iterations int    // the PBKDF2 iteration count
}

// hmacSHA256 returns HMAC-SHA-256 of data keyed by key.
func hmacSHA256(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return mac.Sum(nil)
}

// hi is the SCRAM Hi() function (PBKDF2-HMAC-SHA256 with a single block).
func hi(password, salt []byte, iterations int) []byte {
	mac := hmac.New(sha256.New, password)
	mac.Write(salt)
	mac.Write([]byte{0, 0, 0, 1})
	u := mac.Sum(nil)
	out := append([]byte(nil), u...)
	for i := 1; i < iterations; i++ {
		mac.Reset()
		mac.Write(u)
		u = mac.Sum(nil)
		for j := range out {
			out[j] ^= u[j]
		}
	}
	return out
}

// DeriveCredentials derives the server-side credentials for a password. It is
// intended for user enrolment; the plaintext password is never stored.
func DeriveCredentials(password, salt []byte, iterations int) Credentials {
	salted := hi(password, salt, iterations)
	clientKey := hmacSHA256(salted, []byte("Client Key"))
	storedKey := sha256.Sum256(clientKey)
	serverKey := hmacSHA256(salted, []byte("Server Key"))
	return Credentials{
		StoredKey:  storedKey[:],
		ServerKey:  serverKey,
		Salt:       append([]byte(nil), salt...),
		Iterations: iterations,
	}
}

// GenerateSalt returns a cryptographically random salt of n bytes.
func GenerateSalt(n int) ([]byte, error) {
	salt := make([]byte, n)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	return salt, nil
}

// EscapeUsername escapes a username for use in a SCRAM n= attribute (RFC 5802 section 5.1).
func EscapeUsername(username string) string {
	r := strings.NewReplacer("=", "=3D", ",", "=2C")
	return r.Replace(username)
}

// unescapeUsername reverses EscapeUsername.
func unescapeUsername(username string) string {
	r := strings.NewReplacer("=2C", ",", "=3D", "=")
	return r.Replace(username)
}

// splitAttributes splits a SCRAM message into its comma separated attributes.
// Attribute values never contain raw commas (usernames escape them), so a
// plain split is safe.
func splitAttributes(msg string) []string {
	return strings.Split(msg, ",")
}

// serverSession tracks the state of a single server-side SCRAM conversation.
type serverSession struct {
	creds           Credentials
	username        string // unescaped username from the client-first-message
	gs2Header       string // the gs2 header of the client-first-message
	clientFirstBare string // client-first-message-bare, used in the auth message
	serverFirst     string // the server-first-message
	nonce           string // combined client and server nonce
	serverNonceLen  int    // number of random bytes used for the server nonce
	authenticated   bool   // true once the client proof has verified
}

// newServerSession creates a new server-side conversation for the given credentials.
func newServerSession(creds Credentials, serverNonceLen int) *serverSession {
	return &serverSession{
		creds:          creds,
		serverNonceLen: serverNonceLen,
	}
}

// Username returns the unescaped username presented in the client-first-message.
func (s *serverSession) Username() string {
	return s.username
}

// handleClientFirst parses a client-first-message and returns the
// server-first-message.
func (s *serverSession) handleClientFirst(data []byte) ([]byte, error) {
	msg := string(data)
	if !strings.HasPrefix(msg, "n,,") && !strings.HasPrefix(msg, "y,,") {
		if strings.HasPrefix(msg, "p=") {
			return nil, ErrChannelBindingUnsupported
		}
		return nil, ErrMalformedMessage
	}

	s.gs2Header = msg[:strings.Index(msg, ",")+2]
	bare := msg[len(s.gs2Header):]
	attrs := splitAttributes(bare)
	if len(attrs) < 2 || !strings.HasPrefix(attrs[0], "n=") || !strings.HasPrefix(attrs[1], "r=") {
		return nil, ErrMalformedMessage
	}

	username := unescapeUsername(strings.TrimPrefix(attrs[0], "n="))
	clientNonce := strings.TrimPrefix(attrs[1], "r=")
	if username == "" || clientNonce == "" {
		return nil, ErrMalformedMessage
	}

	serverNonceRaw := make([]byte, s.serverNonceLen)
	if _, err := rand.Read(serverNonceRaw); err != nil {
		return nil, err
	}

	s.username = username
	s.clientFirstBare = bare
	s.nonce = clientNonce + base64.RawStdEncoding.EncodeToString(serverNonceRaw)
	s.serverFirst = "r=" + s.nonce +
		",s=" + base64.StdEncoding.EncodeToString(s.creds.Salt) +
		",i=" + strconv.Itoa(s.creds.Iterations)

	return []byte(s.serverFirst), nil
}

// handleClientFinal parses a client-final-message, verifies the client proof,
// and returns the server-final-message containing the server signature.
func (s *serverSession) handleClientFinal(data []byte) ([]byte, error) {
	msg := string(data)
	attrs := splitAttributes(msg)
	if len(attrs) < 3 || !strings.HasPrefix(attrs[0], "c=") || !strings.HasPrefix(attrs[1], "r=") {
		return nil, ErrMalformedMessage
	}

	cbind := strings.TrimPrefix(attrs[0], "c=")
	if cbind != base64.StdEncoding.EncodeToString([]byte(s.gs2Header)) {
		return nil, ErrChannelBindingMismatch
	}

	if strings.TrimPrefix(attrs[1], "r=") != s.nonce {
		return nil, ErrNonceMismatch
	}

	proofAttr := attrs[len(attrs)-1]
	if !strings.HasPrefix(proofAttr, "p=") {
		return nil, ErrMalformedMessage
	}

	proof, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(proofAttr, "p="))
	if err != nil || len(proof) != sha256.Size {
		return nil, ErrMalformedMessage
	}

	// The auth message is computed over the client-final-message without the proof.
	withoutProof := strings.Join(attrs[:len(attrs)-1], ",")
	authMessage := s.clientFirstBare + "," + s.serverFirst + "," + withoutProof

	clientSignature := hmacSHA256(s.creds.StoredKey, []byte(authMessage))
	recoveredClientKey := make([]byte, sha256.Size)
	for i := range proof {
		recoveredClientKey[i] = proof[i] ^ clientSignature[i]
	}

	recoveredStoredKey := sha256.Sum256(recoveredClientKey)
	if !hmac.Equal(recoveredStoredKey[:], s.creds.StoredKey) {
		return nil, ErrInvalidProof
	}

	serverSignature := hmacSHA256(s.creds.ServerKey, []byte(authMessage))
	s.authenticated = true

	return []byte("v=" + base64.StdEncoding.EncodeToString(serverSignature)), nil
}

// VerifyServerFinal verifies a server-final-message against the expected server
// signature. It is provided for clients and tests.
func VerifyServerFinal(data, expectedSignature []byte) error {
	msg := string(data)
	if !strings.HasPrefix(msg, "v=") {
		return fmt.Errorf("%w: missing v= attribute", ErrMalformedMessage)
	}

	signature, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(msg, "v="))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrMalformedMessage, err)
	}

	if !hmac.Equal(signature, expectedSignature) {
		return errors.New("invalid server signature")
	}

	return nil
}

// ClientConversation is a minimal client-side SCRAM-SHA-256 conversation,
// provided for tests and for verifying server behaviour with a local client.
type ClientConversation struct {
	username        string
	password        []byte
	clientNonce     string
	ClientFirstBare string // client-first-message-bare, used in the auth message
	authMessage     string
	serverSignature []byte
}

// NewClientConversation creates a client-side conversation for a username and password.
func NewClientConversation(username string, password []byte) (*ClientConversation, error) {
	nonceRaw := make([]byte, 18)
	if _, err := rand.Read(nonceRaw); err != nil {
		return nil, err
	}

	c := &ClientConversation{
		username:    username,
		password:    password,
		clientNonce: base64.RawStdEncoding.EncodeToString(nonceRaw),
	}
	c.ClientFirstBare = "n=" + EscapeUsername(username) + ",r=" + c.clientNonce
	return c, nil
}

// ClientFirst returns the client-first-message.
func (c *ClientConversation) ClientFirst() []byte {
	return []byte("n,," + c.ClientFirstBare)
}

// ClientFinal processes the server-first-message and returns the
// client-final-message containing the client proof.
func (c *ClientConversation) ClientFinal(serverFirst []byte) ([]byte, error) {
	attrs := splitAttributes(string(serverFirst))
	if len(attrs) < 3 || !strings.HasPrefix(attrs[0], "r=") ||
		!strings.HasPrefix(attrs[1], "s=") || !strings.HasPrefix(attrs[2], "i=") {
		return nil, ErrMalformedMessage
	}

	nonce := strings.TrimPrefix(attrs[0], "r=")
	if !strings.HasPrefix(nonce, c.clientNonce) {
		return nil, ErrNonceMismatch
	}

	salt, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(attrs[1], "s="))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformedMessage, err)
	}

	iterations, err := strconv.Atoi(strings.TrimPrefix(attrs[2], "i="))
	if err != nil || iterations <= 0 {
		return nil, ErrMalformedMessage
	}

	salted := hi(c.password, salt, iterations)
	clientKey := hmacSHA256(salted, []byte("Client Key"))
	storedKey := sha256.Sum256(clientKey)

	withoutProof := "c=biws,r=" + nonce // biws is base64("n,,")
	c.authMessage = c.ClientFirstBare + "," + string(serverFirst) + "," + withoutProof

	clientSignature := hmacSHA256(storedKey[:], []byte(c.authMessage))
	proof := make([]byte, sha256.Size)
	for i := range clientKey {
		proof[i] = clientKey[i] ^ clientSignature[i]
	}

	serverKey := hmacSHA256(salted, []byte("Server Key"))
	c.serverSignature = hmacSHA256(serverKey, []byte(c.authMessage))

	return []byte(withoutProof + ",p=" + base64.StdEncoding.EncodeToString(proof)), nil
}

// VerifyServerFinal verifies a server-final-message against the expected
// server signature for this conversation.
func (c *ClientConversation) VerifyServerFinal(data []byte) error {
	if c.serverSignature == nil {
		return errors.New("client-final-message has not been generated")
	}
	return VerifyServerFinal(data, c.serverSignature)
}
