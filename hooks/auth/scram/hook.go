// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: 2022 mochi-mqtt, mochi-co
// SPDX-FileContributor: mochi-co

package scram

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	mqtt "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/packets"
)

// Default values for the hook options.
const (
	DefaultMaxRounds     = 4     // default maximum number of messages in the authentication exchange
	DefaultMinIterations = 4096  // default minimum acceptable PBKDF2 iteration count (RFC 7677)
	DefaultMaxIterations = 65536 // default maximum acceptable PBKDF2 iteration count
	DefaultNonceLength   = 24    // default number of random bytes in the server nonce
)

var (
	// ErrNoCredentialStore indicates the hook was initialised without a credential store.
	ErrNoCredentialStore = errors.New("a credential store is required")

	// ErrInvalidOptions indicates the hook options failed validation.
	ErrInvalidOptions = errors.New("invalid scram hook options")
)

// Options contains the configuration for the SCRAM hook.
type Options struct {
	// Store is the backend providing salt, iteration count, and verification
	// credentials for each user. Required.
	Store CredentialStore

	// MaxRounds is the maximum number of client messages allowed in the
	// authentication exchange, including the client-first-message carried in
	// the CONNECT packet. A client restarting the exchange consumes an
	// additional round per attempt. SCRAM-SHA-256 requires at least 2.
	// Defaults to DefaultMaxRounds.
	MaxRounds int

	// MinIterations is the minimum PBKDF2 iteration cost accepted from the
	// backend. Credentials below this cost are rejected. Defaults to
	// DefaultMinIterations.
	MinIterations int

	// MaxIterations is the maximum PBKDF2 iteration cost accepted from the
	// backend, bounding the computational cost of verification. Defaults to
	// DefaultMaxIterations.
	MaxIterations int

	// NonceLength is the number of random bytes used for the server nonce.
	// Defaults to DefaultNonceLength.
	NonceLength int
}

// Hook is an authentication hook implementing SCRAM-SHA-256 over the MQTT v5
// enhanced authentication exchange. The server never sees a plaintext
// password; it verifies the client proof against the stored credentials and
// returns a server signature the client can verify.
type Hook struct {
	mqtt.HookBase
	config *Options
}

// ID returns the ID of the hook.
func (h *Hook) ID() string {
	return "scram-auth"
}

// Provides indicates which hook methods this hook provides.
func (h *Hook) Provides(b byte) bool {
	return bytes.Contains([]byte{
		mqtt.OnConnectAuthenticate,
		mqtt.OnAuthPacket,
	}, []byte{b})
}

// Init configures the hook with the credential store and exchange limits.
func (h *Hook) Init(config any) error {
	if _, ok := config.(*Options); !ok && config != nil {
		return mqtt.ErrInvalidConfigType
	}

	if config == nil {
		config = new(Options)
	}

	h.config = config.(*Options)
	if h.config.MaxRounds == 0 {
		h.config.MaxRounds = DefaultMaxRounds
	}
	if h.config.MinIterations == 0 {
		h.config.MinIterations = DefaultMinIterations
	}
	if h.config.MaxIterations == 0 {
		h.config.MaxIterations = DefaultMaxIterations
	}
	if h.config.NonceLength == 0 {
		h.config.NonceLength = DefaultNonceLength
	}

	if h.config.Store == nil {
		return ErrNoCredentialStore
	}
	if h.config.MaxRounds < 2 {
		return fmt.Errorf("%w: max rounds must allow at least the connect and one auth packet", ErrInvalidOptions)
	}
	if h.config.MinIterations < 1 {
		return fmt.Errorf("%w: min iterations must be at least 1", ErrInvalidOptions)
	}
	if h.config.MaxIterations < h.config.MinIterations {
		return fmt.Errorf("%w: max iterations must be greater than or equal to min iterations", ErrInvalidOptions)
	}
	if h.config.NonceLength < 8 || h.config.NonceLength > 64 {
		return fmt.Errorf("%w: nonce length must be between 8 and 64 bytes", ErrInvalidOptions)
	}

	return nil
}

// OnConnectAuthenticate performs the SCRAM-SHA-256 exchange for clients
// connecting with the SCRAM-SHA-256 authentication method. The exchange runs
// before the server read loop starts, so the hook reads the client-final
// AUTH packet directly from the connection. Clients not requesting the
// SCRAM-SHA-256 method are rejected.
func (h *Hook) OnConnectAuthenticate(cl *mqtt.Client, pk packets.Packet) bool {
	if cl.Properties.ProtocolVersion != 5 || pk.Properties.AuthenticationMethod != Method {
		h.Log.Debug("client did not request scram-sha-256 enhanced authentication",
			"client", cl.ID, "remote", cl.Net.Remote)
		return false
	}

	session, serverFirst, ok := h.beginExchange(cl, pk.Properties.AuthenticationData, pk.Connect.Username)
	if !ok {
		return false
	}

	err := cl.WritePacket(authPacket(packets.CodeContinueAuthentication.Code, serverFirst))
	if err != nil {
		h.Log.Debug("failed writing server-first-message", "error", err, "client", cl.ID)
		return false
	}

	// The client-first-message in the CONNECT packet counts as the first
	// round of the exchange. Each subsequent AUTH packet consumes another
	// round, bounding the total number of exchange messages.
	for rounds := 1; ; {
		if rounds >= h.config.MaxRounds {
			h.Log.Info("scram maximum authentication rounds exceeded",
				"rounds", h.config.MaxRounds, "client", cl.ID, "remote", cl.Net.Remote)
			return false
		}

		ap, err := h.readAuthPacket(cl)
		if err != nil {
			h.Log.Debug("scram exchange interrupted", "error", err, "client", cl.ID, "remote", cl.Net.Remote)
			return false
		}
		rounds++

		// A client may restart the exchange by sending a fresh
		// client-first-message.
		if isClientFirstMessage(ap.Properties.AuthenticationData) {
			session, serverFirst, ok = h.beginExchange(cl, ap.Properties.AuthenticationData, pk.Connect.Username)
			if !ok {
				return false
			}

			if err := cl.WritePacket(authPacket(packets.CodeContinueAuthentication.Code, serverFirst)); err != nil {
				h.Log.Debug("failed writing server-first-message", "error", err, "client", cl.ID)
				return false
			}
			continue
		}

		serverFinal, err := session.handleClientFinal(ap.Properties.AuthenticationData)
		if err != nil {
			h.Log.Info("scram authentication failed",
				"error", err, "username", session.Username(), "client", cl.ID, "remote", cl.Net.Remote)
			return false
		}

		// Stash the server-final-message so the server can include it in the
		// CONNACK authentication data [MQTT-3.2.2.3.18].
		cl.Properties.Props.AuthenticationData = serverFinal
		cl.State.EnhancedAuth = true
		return true
	}
}

// OnAuthPacket rejects AUTH packets received after the connection is
// established. Re-authentication and duplicate AUTH packets are not
// supported, so the client is disconnected with a protocol violation.
func (h *Hook) OnAuthPacket(cl *mqtt.Client, pk packets.Packet) (packets.Packet, error) {
	h.Log.Warn("unexpected auth packet; re-authentication is not supported",
		"client", cl.ID, "remote", cl.Net.Remote)
	return pk, packets.ErrProtocolViolation
}

// beginExchange starts a new SCRAM conversation from a client-first-message,
// returning the session and the server-first-message.
func (h *Hook) beginExchange(cl *mqtt.Client, clientFirst, connectUsername []byte) (*serverSession, []byte, bool) {
	creds, ok := h.credentials(clientFirst)
	if !ok {
		return nil, nil, false
	}

	session := newServerSession(creds, h.config.NonceLength)
	serverFirst, err := session.handleClientFirst(clientFirst)
	if err != nil {
		h.Log.Info("scram client-first-message rejected",
			"error", err, "client", cl.ID, "remote", cl.Net.Remote)
		return nil, nil, false
	}

	if len(connectUsername) > 0 && string(connectUsername) != session.Username() {
		h.Log.Info("scram username does not match connect username",
			"username", session.Username(), "client", cl.ID, "remote", cl.Net.Remote)
		return nil, nil, false
	}

	return session, serverFirst, true
}

// credentials parses the username out of a client-first-message and returns
// the backend credentials for the user if they are within the configured
// iteration cost bounds.
func (h *Hook) credentials(clientFirst []byte) (Credentials, bool) {
	username, ok := peekUsername(clientFirst)
	if !ok {
		h.Log.Info("scram client-first-message malformed")
		return Credentials{}, false
	}

	creds, ok := h.config.Store.GetCredentials(username)
	if !ok {
		h.Log.Info("scram user unknown", "username", username)
		return Credentials{}, false
	}

	if creds.Iterations < h.config.MinIterations || creds.Iterations > h.config.MaxIterations {
		h.Log.Warn("scram credentials iteration count out of bounds",
			"username", username, "iterations", creds.Iterations,
			"min", h.config.MinIterations, "max", h.config.MaxIterations)
		return Credentials{}, false
	}

	if len(creds.StoredKey) != 32 || len(creds.ServerKey) != 32 || len(creds.Salt) == 0 {
		h.Log.Warn("scram credentials malformed", "username", username)
		return Credentials{}, false
	}

	return creds, true
}

// readAuthPacket reads the next packet from the client connection and
// requires it to be an AUTH packet continuing the exchange.
func (h *Hook) readAuthPacket(cl *mqtt.Client) (packets.Packet, error) {
	fh := new(packets.FixedHeader)
	if err := cl.ReadFixedHeader(fh); err != nil {
		return packets.Packet{}, err
	}

	pk, err := cl.ReadPacket(fh)
	if err != nil {
		return packets.Packet{}, err
	}

	if pk.FixedHeader.Type != packets.Auth {
		return packets.Packet{}, fmt.Errorf("expected auth packet during authentication exchange, received packet type %d", pk.FixedHeader.Type)
	}

	if pk.ReasonCode != packets.CodeContinueAuthentication.Code {
		return packets.Packet{}, fmt.Errorf("unexpected auth packet reason code %d", pk.ReasonCode)
	}

	return pk, nil
}

// authPacket returns an AUTH packet for the enhanced authentication exchange.
func authPacket(reasonCode byte, data []byte) packets.Packet {
	return packets.Packet{
		FixedHeader: packets.FixedHeader{
			Type: packets.Auth,
		},
		ReasonCode: reasonCode,
		Properties: packets.Properties{
			AuthenticationMethod: Method,
			AuthenticationData:   data,
		},
	}
}

// peekUsername extracts the username from a client-first-message without
// validating the remainder of the message.
func peekUsername(data []byte) (string, bool) {
	msg := string(data)
	if !isClientFirstMessage(data) {
		return "", false
	}

	attrs := splitAttributes(msg[len("n,,"):])
	if len(attrs) < 1 || !strings.HasPrefix(attrs[0], "n=") {
		return "", false
	}

	username := unescapeUsername(strings.TrimPrefix(attrs[0], "n="))
	return username, username != ""
}

// isClientFirstMessage indicates whether data begins with a SCRAM gs2 header
// without channel binding, identifying a client-first-message.
func isClientFirstMessage(data []byte) bool {
	msg := string(data)
	return strings.HasPrefix(msg, "n,,") || strings.HasPrefix(msg, "y,,")
}
