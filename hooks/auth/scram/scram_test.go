// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: 2022 mochi-mqtt, mochi-co
// SPDX-FileContributor: mochi-co

package scram

import (
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/require"
)

// RFC 7677 section 3 example exchange for SCRAM-SHA-256, password "pencil".
var (
	rfcClientFirst   = "n,,n=user,r=rOprNGfwEbeRWgbNEkqO"
	rfcServerFirst   = "r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0,s=W22ZaJ0SNY7soEsUEjb6gQ==,i=4096"
	rfcClientFinal   = "c=biws,r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0,p=dHzbZapWIk4jUhN+Ute9ytag9zjfMHgsqmmiz7AndVQ="
	rfcServerFinal   = "v=6rriTRBi23WpRR/wtup+mMhUZUn/dB5nLTJRsjl95G4="
	rfcClientNonce   = "rOprNGfwEbeRWgbNEkqO"
	rfcCombinedNonce = "rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0"
)

func TestClientConversationRFC7677(t *testing.T) {
	c := &ClientConversation{
		username:    "user",
		password:    []byte("pencil"),
		clientNonce: rfcClientNonce,
	}
	c.ClientFirstBare = "n=user,r=" + rfcClientNonce

	require.Equal(t, rfcClientFirst, string(c.ClientFirst()))

	clientFinal, err := c.ClientFinal([]byte(rfcServerFirst))
	require.NoError(t, err)
	require.Equal(t, rfcClientFinal, string(clientFinal))

	require.NoError(t, c.VerifyServerFinal([]byte(rfcServerFinal)))
	require.Error(t, c.VerifyServerFinal([]byte("v="+base64.StdEncoding.EncodeToString([]byte("bogus-signature-bogus-sign")))))
}

func TestServerSessionRFC7677(t *testing.T) {
	salt, err := base64.StdEncoding.DecodeString("W22ZaJ0SNY7soEsUEjb6gQ==")
	require.NoError(t, err)

	creds := DeriveCredentials([]byte("pencil"), salt, 4096)
	s := newServerSession(creds, 24)

	_, err = s.handleClientFirst([]byte(rfcClientFirst))
	require.NoError(t, err)
	require.Equal(t, "user", s.Username())

	// Pin the nonce and server-first-message to the RFC values.
	s.nonce = rfcCombinedNonce
	s.serverFirst = rfcServerFirst

	serverFinal, err := s.handleClientFinal([]byte(rfcClientFinal))
	require.NoError(t, err)
	require.Equal(t, rfcServerFinal, string(serverFinal))
	require.True(t, s.authenticated)
}

func TestConversationRoundTrip(t *testing.T) {
	salt, err := GenerateSalt(16)
	require.NoError(t, err)
	require.Len(t, salt, 16)

	creds := DeriveCredentials([]byte("melon"), salt, 4096)
	require.Len(t, creds.StoredKey, 32)
	require.Len(t, creds.ServerKey, 32)

	s := newServerSession(creds, 24)
	c, err := NewClientConversation("mochi", []byte("melon"))
	require.NoError(t, err)

	serverFirst, err := s.handleClientFirst(c.ClientFirst())
	require.NoError(t, err)
	require.Equal(t, "mochi", s.Username())

	clientFinal, err := c.ClientFinal(serverFirst)
	require.NoError(t, err)

	serverFinal, err := s.handleClientFinal(clientFinal)
	require.NoError(t, err)
	require.NoError(t, c.VerifyServerFinal(serverFinal))
}

func TestConversationRoundTripWrongPassword(t *testing.T) {
	salt, err := GenerateSalt(16)
	require.NoError(t, err)

	creds := DeriveCredentials([]byte("melon"), salt, 4096)
	s := newServerSession(creds, 24)

	c, err := NewClientConversation("mochi", []byte("wrong"))
	require.NoError(t, err)

	serverFirst, err := s.handleClientFirst(c.ClientFirst())
	require.NoError(t, err)

	clientFinal, err := c.ClientFinal(serverFirst)
	require.NoError(t, err)

	_, err = s.handleClientFinal(clientFinal)
	require.ErrorIs(t, err, ErrInvalidProof)
}

func TestServerSessionHandleClientFirstMalformed(t *testing.T) {
	s := newServerSession(Credentials{}, 24)

	for _, msg := range []string{
		"",
		"x,,n=user,r=abc",
		"n,,",
		"n,,r=abc",
		"n,,n=,r=abc",
		"n,,n=user,r=",
	} {
		_, err := s.handleClientFirst([]byte(msg))
		require.ErrorIs(t, err, ErrMalformedMessage, "message %q", msg)
	}

	_, err := s.handleClientFirst([]byte("p=tls-server-end-point,,n=user,r=abc"))
	require.ErrorIs(t, err, ErrChannelBindingUnsupported)
}

func TestServerSessionHandleClientFirstGS2Y(t *testing.T) {
	s := newServerSession(Credentials{Salt: []byte("salt"), Iterations: 4096}, 24)
	serverFirst, err := s.handleClientFirst([]byte("y,,n=user,r=clientnonce"))
	require.NoError(t, err)
	require.Contains(t, string(serverFirst), "r=clientnonce")
	require.Contains(t, string(serverFirst), ",s=c2FsdA==")
	require.Contains(t, string(serverFirst), ",i=4096")
}

func TestServerSessionHandleClientFinalErrors(t *testing.T) {
	salt, _ := GenerateSalt(16)
	creds := DeriveCredentials([]byte("melon"), salt, 4096)

	newSession := func(t *testing.T) (*serverSession, *ClientConversation, []byte) {
		s := newServerSession(creds, 24)
		c, err := NewClientConversation("mochi", []byte("melon"))
		require.NoError(t, err)
		serverFirst, err := s.handleClientFirst(c.ClientFirst())
		require.NoError(t, err)
		return s, c, serverFirst
	}

	t.Run("channel binding mismatch", func(t *testing.T) {
		s, c, serverFirst := newSession(t)
		clientFinal, err := c.ClientFinal(serverFirst)
		require.NoError(t, err)

		tampered := "c=eSws" + string(clientFinal[len("c=biws"):])
		_, err = s.handleClientFinal([]byte(tampered))
		require.ErrorIs(t, err, ErrChannelBindingMismatch)
	})

	t.Run("nonce mismatch", func(t *testing.T) {
		s, c, serverFirst := newSession(t)
		clientFinal, err := c.ClientFinal(serverFirst)
		require.NoError(t, err)

		tampered := "c=biws,r=tampered" + string(clientFinal[len("c=biws,r=")+len(s.nonce):])
		_, err = s.handleClientFinal([]byte(tampered))
		require.ErrorIs(t, err, ErrNonceMismatch)
	})

	t.Run("malformed", func(t *testing.T) {
		s, _, _ := newSession(t)
		for _, msg := range []string{
			"",
			"r=abc",
			"c=biws,r=" + s.nonce, // missing proof
			"c=biws,r=" + s.nonce + ",p=!!!not-base64!!!",
		} {
			_, err := s.handleClientFinal([]byte(msg))
			require.Error(t, err, "message %q", msg)
		}
	})
}

func TestClientFinalErrors(t *testing.T) {
	c, err := NewClientConversation("mochi", []byte("melon"))
	require.NoError(t, err)

	_, err = c.ClientFinal([]byte("bogus"))
	require.ErrorIs(t, err, ErrMalformedMessage)

	_, err = c.ClientFinal([]byte("r=someoneelsenonce,s=c2FsdA==,i=4096"))
	require.ErrorIs(t, err, ErrNonceMismatch)

	_, err = c.ClientFinal([]byte("r=" + c.clientNonce + "srv,s=!!!,i=4096"))
	require.Error(t, err)

	require.Error(t, c.VerifyServerFinal([]byte("v=AAAA")))
}

func TestVerifyServerFinalMalformed(t *testing.T) {
	require.Error(t, VerifyServerFinal([]byte("x=AAAA"), []byte("sig")))
	require.Error(t, VerifyServerFinal([]byte("v=!!!"), []byte("sig")))
}

func TestUsernameEscaping(t *testing.T) {
	require.Equal(t, "user", EscapeUsername("user"))
	require.Equal(t, "u=2Cser", EscapeUsername("u,ser"))
	require.Equal(t, "u=3Dser", EscapeUsername("u=ser"))
	require.Equal(t, "u,ser", unescapeUsername("u=2Cser"))
	require.Equal(t, "u=ser", unescapeUsername("u=3Dser"))
}

func TestPeekUsername(t *testing.T) {
	username, ok := peekUsername([]byte("n,,n=mochi,r=abc"))
	require.True(t, ok)
	require.Equal(t, "mochi", username)

	username, ok = peekUsername([]byte("y,,n=u=2Cser,r=abc"))
	require.True(t, ok)
	require.Equal(t, "u,ser", username)

	_, ok = peekUsername([]byte("p=tls,,n=mochi,r=abc"))
	require.False(t, ok)

	_, ok = peekUsername([]byte("n,,r=abc"))
	require.False(t, ok)

	_, ok = peekUsername([]byte("n,,n=,r=abc"))
	require.False(t, ok)
}
