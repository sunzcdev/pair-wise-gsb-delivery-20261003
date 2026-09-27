package smtp_test

import (
	"crypto/tls"
	"errors"
	"io"
	"log"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
)

// PlainBackend is a minimal backend that relies on the server's default auth
// behavior: every mechanism listed by AuthMechanisms is advertised and
// accepted, as long as the connection uses TLS or Server.AllowInsecureAuth
// is set.
type PlainBackend struct{}

func (b *PlainBackend) NewSession(c *smtp.Conn) (smtp.Session, error) {
	return &PlainSession{}, nil
}

type PlainSession struct {
	auth bool
}

func (s *PlainSession) AuthMechanisms() []string {
	return []string{sasl.Plain}
}

func (s *PlainSession) Auth(mech string) (sasl.Server, error) {
	return sasl.NewPlainServer(func(identity, username, password string) error {
		if username != "username" || password != "password" {
			return errors.New("invalid username or password")
		}
		s.auth = true
		return nil
	}), nil
}

func (s *PlainSession) Mail(from string, opts *smtp.MailOptions) error {
	if !s.auth {
		return smtp.ErrAuthRequired
	}
	return nil
}

func (s *PlainSession) Rcpt(to string, opts *smtp.RcptOptions) error {
	if !s.auth {
		return smtp.ErrAuthRequired
	}
	return nil
}

func (s *PlainSession) Data(r io.Reader) error {
	if !s.auth {
		return smtp.ErrAuthRequired
	}
	_, err := io.Copy(io.Discard, r)
	return err
}

func (s *PlainSession) Reset() {}

func (s *PlainSession) Logout() error {
	return nil
}

// ExampleServer_defaultAuthPolicy runs a server with the default auth
// behavior: no AuthPolicy is set, so all backend mechanisms are allowed.
func ExampleServer_defaultAuthPolicy() {
	s := smtp.NewServer(&PlainBackend{})
	s.Addr = "localhost:1025"
	s.Domain = "localhost"
	s.AllowInsecureAuth = true // only for testing without TLS

	log.Println("Starting server at", s.Addr)
	if err := s.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

// ClientCertBackend is a minimal backend that offers ANONYMOUS and PLAIN,
// where PLAIN is only enabled for clients that present a TLS client
// certificate. It must be combined with Server.AuthPolicy, as shown in
// ExampleServer_clientCertAuthPolicy.
type ClientCertBackend struct{}

func (b *ClientCertBackend) NewSession(c *smtp.Conn) (smtp.Session, error) {
	return &ClientCertSession{}, nil
}

type ClientCertSession struct {
	auth bool
}

func (s *ClientCertSession) AuthMechanisms() []string {
	return []string{sasl.Plain, sasl.Anonymous}
}

func (s *ClientCertSession) Auth(mech string) (sasl.Server, error) {
	switch mech {
	case sasl.Anonymous:
		return sasl.NewAnonymousServer(func(trace string) error {
			log.Println("Anonymous trace:", trace)
			return nil
		}), nil
	case sasl.Plain:
		return sasl.NewPlainServer(func(identity, username, password string) error {
			if username != "username" || password != "password" {
				return errors.New("invalid username or password")
			}
			s.auth = true
			return nil
		}), nil
	}
	return nil, smtp.ErrAuthUnknownMechanism
}

func (s *ClientCertSession) Mail(from string, opts *smtp.MailOptions) error {
	if !s.auth {
		return smtp.ErrAuthRequired
	}
	return nil
}

func (s *ClientCertSession) Rcpt(to string, opts *smtp.RcptOptions) error {
	if !s.auth {
		return smtp.ErrAuthRequired
	}
	return nil
}

func (s *ClientCertSession) Data(r io.Reader) error {
	if !s.auth {
		return smtp.ErrAuthRequired
	}
	_, err := io.Copy(io.Discard, r)
	return err
}

func (s *ClientCertSession) Reset() {}

func (s *ClientCertSession) Logout() error {
	return nil
}

// ExampleServer_clientCertAuthPolicy runs a server that only enables the
// PLAIN mechanism for clients that presented a TLS client certificate.
// ANONYMOUS remains available on any connection.
func ExampleServer_clientCertAuthPolicy() {
	s := smtp.NewServer(&ClientCertBackend{})
	s.Addr = "localhost:1025"
	s.Domain = "localhost"
	s.TLSConfig = &tls.Config{
		// Certificates: ...,
		//
		// Request, but do not require, a client certificate: clients
		// without one may still connect, they just don't get PLAIN.
		ClientAuth: tls.RequestClientCert,
	}
	s.AuthPolicy = func(state *tls.ConnectionState, mechs []string) []string {
		allowed := []string{sasl.Anonymous}
		if state != nil && len(state.PeerCertificates) > 0 {
			allowed = append(allowed, sasl.Plain)
		}
		return allowed
	}

	log.Println("Starting server at", s.Addr)
	if err := s.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
