package smtp_test

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
)

func generateTestCertificate(t *testing.T) tls.Certificate {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "localhost"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}

	cert, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

// ehloCaps sends EHLO and returns the advertised capabilities.
func ehloCaps(t *testing.T, c net.Conn, scanner *bufio.Scanner) map[string]bool {
	t.Helper()

	io.WriteString(c, "EHLO localhost\r\n")

	caps := make(map[string]bool)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "250 ") {
			caps[strings.TrimPrefix(line, "250 ")] = true
			break
		}
		if !strings.HasPrefix(line, "250-") {
			t.Fatal("Invalid capability response:", line)
		}
		caps[strings.TrimPrefix(line, "250-")] = true
	}
	return caps
}

// hasAuthMech reports whether the capabilities advertise the given AUTH
// mechanism.
func hasAuthMech(caps map[string]bool, mech string) bool {
	for cap := range caps {
		fields := strings.Fields(cap)
		if len(fields) < 2 || fields[0] != "AUTH" {
			continue
		}
		for _, m := range fields[1:] {
			if strings.EqualFold(m, mech) {
				return true
			}
		}
	}
	return false
}

// startTLSClient issues STARTTLS and upgrades the client connection to TLS.
func startTLSClient(t *testing.T, c net.Conn, scanner *bufio.Scanner, cfg *tls.Config) (net.Conn, *bufio.Scanner) {
	t.Helper()

	io.WriteString(c, "STARTTLS\r\n")
	scanner.Scan()
	if !strings.HasPrefix(scanner.Text(), "220 ") {
		t.Fatal("Invalid STARTTLS response:", scanner.Text())
	}

	if cfg == nil {
		cfg = &tls.Config{InsecureSkipVerify: true}
	}
	tlsConn := tls.Client(c, cfg)
	if err := tlsConn.Handshake(); err != nil {
		t.Fatal("TLS handshake failed:", err)
	}
	return tlsConn, bufio.NewScanner(tlsConn)
}

// The policy must filter both the EHLO advertisement and the AUTH command.
func TestServerAuthPolicy(t *testing.T) {
	policy := func(s *smtp.Server) {
		s.Backend.(*backend).authMechs = []string{sasl.Plain, sasl.Anonymous}
		s.AuthPolicy = func(state *tls.ConnectionState, mechs []string) []string {
			// ANONYMOUS is allowed on any connection, PLAIN requires TLS.
			if state == nil {
				return []string{sasl.Anonymous}
			}
			return mechs
		}
	}
	_, s, c, scanner, caps := testServerEhlo(t, policy)
	defer s.Close()

	if !hasAuthMech(caps, sasl.Anonymous) {
		t.Fatal("AUTH ANONYMOUS capability is missing")
	}
	if hasAuthMech(caps, sasl.Plain) {
		t.Fatal("PLAIN must not be advertised without TLS")
	}

	// PLAIN is supported by the backend but disallowed by the policy.
	io.WriteString(c, "AUTH PLAIN AHVzZXJuYW1lAHBhc3N3b3Jk\r\n")
	scanner.Scan()
	if !strings.HasPrefix(scanner.Text(), "504 ") {
		t.Fatal("Invalid AUTH PLAIN response:", scanner.Text())
	}

	// ANONYMOUS is allowed.
	io.WriteString(c, "AUTH ANONYMOUS\r\n")
	scanner.Scan()
	if scanner.Text() != "334 " {
		t.Fatal("Invalid AUTH ANONYMOUS response:", scanner.Text())
	}
	io.WriteString(c, "dHJhY2U=\r\n")
	scanner.Scan()
	if !strings.HasPrefix(scanner.Text(), "235 ") {
		t.Fatal("Invalid AUTH ANONYMOUS response:", scanner.Text())
	}
}

// Mechanism names returned by the policy and sent by the client must be
// matched case-insensitively.
func TestServerAuthPolicy_caseInsensitive(t *testing.T) {
	policy := func(s *smtp.Server) {
		// The policy uses a lower-case name while the backend uses
		// upper-case.
		s.AuthPolicy = func(state *tls.ConnectionState, mechs []string) []string {
			return []string{"plain"}
		}
	}
	_, s, c, scanner, caps := testServerEhlo(t, policy)
	defer s.Close()

	if !hasAuthMech(caps, sasl.Plain) {
		t.Fatal("AUTH PLAIN capability is missing")
	}

	// The client uses a lower-case mechanism name.
	io.WriteString(c, "auth plain AHVzZXJuYW1lAHBhc3N3b3Jk\r\n")
	scanner.Scan()
	if !strings.HasPrefix(scanner.Text(), "235 ") {
		t.Fatal("Invalid AUTH response:", scanner.Text())
	}
}

// Cancelling the SASL exchange must work when a policy is in place, and
// authentication must be retryable afterwards.
func TestServerAuthPolicy_cancelSASL(t *testing.T) {
	policy := func(s *smtp.Server) {
		s.AuthPolicy = func(state *tls.ConnectionState, mechs []string) []string {
			return mechs
		}
	}
	_, s, c, scanner, caps := testServerEhlo(t, policy)
	defer s.Close()

	if !hasAuthMech(caps, sasl.Plain) {
		t.Fatal("AUTH PLAIN capability is missing")
	}

	io.WriteString(c, "AUTH PLAIN\r\n")
	scanner.Scan()
	if scanner.Text() != "334 " {
		t.Fatal("Invalid AUTH response:", scanner.Text())
	}

	io.WriteString(c, "*\r\n")
	scanner.Scan()
	if !strings.HasPrefix(scanner.Text(), "501 ") {
		t.Fatal("Invalid AUTH cancel response:", scanner.Text())
	}

	io.WriteString(c, "AUTH PLAIN AHVzZXJuYW1lAHBhc3N3b3Jk\r\n")
	scanner.Scan()
	if !strings.HasPrefix(scanner.Text(), "235 ") {
		t.Fatal("Invalid AUTH response:", scanner.Text())
	}
}

// Capabilities must be re-computed after STARTTLS completes.
func TestServerAuthPolicy_startTLS(t *testing.T) {
	cert := generateTestCertificate(t)
	policy := func(s *smtp.Server) {
		s.Backend.(*backend).authMechs = []string{sasl.Plain, sasl.Anonymous}
		s.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}}
		s.AuthPolicy = func(state *tls.ConnectionState, mechs []string) []string {
			if state == nil {
				return []string{sasl.Anonymous}
			}
			return mechs
		}
	}
	_, s, c, scanner, caps := testServerEhlo(t, policy)
	defer s.Close()

	if _, ok := caps["STARTTLS"]; !ok {
		t.Fatal("STARTTLS capability is missing")
	}
	if hasAuthMech(caps, sasl.Plain) {
		t.Fatal("PLAIN must not be advertised before STARTTLS")
	}

	c, scanner = startTLSClient(t, c, scanner, nil)

	caps = ehloCaps(t, c, scanner)
	if !hasAuthMech(caps, sasl.Plain) {
		t.Fatal("PLAIN must be advertised after STARTTLS")
	}

	io.WriteString(c, "AUTH PLAIN AHVzZXJuYW1lAHBhc3N3b3Jk\r\n")
	scanner.Scan()
	if !strings.HasPrefix(scanner.Text(), "235 ") {
		t.Fatal("Invalid AUTH response:", scanner.Text())
	}
}

// A failed STARTTLS handshake must not retain previously allowed results.
func TestServerAuthPolicy_startTLSFailure(t *testing.T) {
	cert := generateTestCertificate(t)
	policy := func(s *smtp.Server) {
		s.Backend.(*backend).authMechs = []string{sasl.Plain, sasl.Anonymous}
		s.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}}
		s.AuthPolicy = func(state *tls.ConnectionState, mechs []string) []string {
			if state == nil {
				return []string{sasl.Anonymous}
			}
			return mechs
		}
	}
	_, s, c, scanner, _ := testServerEhlo(t, policy)
	defer s.Close()

	io.WriteString(c, "STARTTLS\r\n")
	scanner.Scan()
	if !strings.HasPrefix(scanner.Text(), "220 ") {
		t.Fatal("Invalid STARTTLS response:", scanner.Text())
	}

	// Send exactly 5 garbage bytes (an invalid TLS record header) instead of
	// a ClientHello so that no trailing bytes confuse the command parser.
	io.WriteString(c, "XXXXX")
	scanner.Scan()
	if !strings.HasPrefix(scanner.Text(), "550 ") {
		t.Fatal("Invalid STARTTLS failure response:", scanner.Text())
	}

	caps := ehloCaps(t, c, scanner)
	if hasAuthMech(caps, sasl.Plain) {
		t.Fatal("PLAIN must not be advertised after a failed STARTTLS handshake")
	}
	if !hasAuthMech(caps, sasl.Anonymous) {
		t.Fatal("ANONYMOUS must still be advertised after a failed STARTTLS handshake")
	}

	io.WriteString(c, "AUTH PLAIN AHVzZXJuYW1lAHBhc3N3b3Jk\r\n")
	scanner.Scan()
	if !strings.HasPrefix(scanner.Text(), "504 ") {
		t.Fatal("Invalid AUTH PLAIN response:", scanner.Text())
	}
}

// The policy can require a TLS client certificate before enabling a
// mechanism.
func TestServerAuthPolicy_clientCert(t *testing.T) {
	serverCert := generateTestCertificate(t)
	clientCert := generateTestCertificate(t)

	policy := func(s *smtp.Server) {
		s.TLSConfig = &tls.Config{
			Certificates: []tls.Certificate{serverCert},
			// Request a certificate without requiring it, so that clients
			// without a certificate can still connect.
			ClientAuth: tls.RequestClientCert,
		}
		s.AuthPolicy = func(state *tls.ConnectionState, mechs []string) []string {
			if state == nil || len(state.PeerCertificates) == 0 {
				return nil
			}
			return mechs
		}
	}

	t.Run("missing client certificate", func(t *testing.T) {
		_, s, c, scanner, _ := testServerEhlo(t, policy)
		defer s.Close()

		c, scanner = startTLSClient(t, c, scanner, nil)

		caps := ehloCaps(t, c, scanner)
		if hasAuthMech(caps, sasl.Plain) {
			t.Fatal("PLAIN must not be advertised without a client certificate")
		}

		io.WriteString(c, "AUTH PLAIN AHVzZXJuYW1lAHBhc3N3b3Jk\r\n")
		scanner.Scan()
		if !strings.HasPrefix(scanner.Text(), "504 ") {
			t.Fatal("Invalid AUTH PLAIN response:", scanner.Text())
		}
	})

	t.Run("with client certificate", func(t *testing.T) {
		_, s, c, scanner, _ := testServerEhlo(t, policy)
		defer s.Close()

		clientCfg := &tls.Config{
			InsecureSkipVerify: true,
			Certificates:       []tls.Certificate{clientCert},
		}
		c, scanner = startTLSClient(t, c, scanner, clientCfg)

		caps := ehloCaps(t, c, scanner)
		if !hasAuthMech(caps, sasl.Plain) {
			t.Fatal("PLAIN must be advertised with a client certificate")
		}

		io.WriteString(c, "AUTH PLAIN AHVzZXJuYW1lAHBhc3N3b3Jk\r\n")
		scanner.Scan()
		if !strings.HasPrefix(scanner.Text(), "235 ") {
			t.Fatal("Invalid AUTH response:", scanner.Text())
		}
	})
}
