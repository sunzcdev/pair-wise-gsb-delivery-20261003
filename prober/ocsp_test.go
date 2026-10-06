// Copyright The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package prober

import (
	"context"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	pconfig "github.com/prometheus/common/config"
	"github.com/prometheus/common/promslog"
	"golang.org/x/crypto/ocsp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"

	"github.com/prometheus/blackbox_exporter/config"
)

// newOCSPTestPKI returns a CA and a leaf certificate signed by it.
func newOCSPTestPKI(t *testing.T) (ca, leaf *x509.Certificate, caKey, leafKey *rsa.PrivateKey) {
	t.Helper()
	ca, caKey = generateCRLTestCert(t, crlCertOptions{CommonName: "Test CA", Serial: 1, IsCA: true}, nil, nil)
	leaf, leafKey = generateCRLTestCert(t, crlCertOptions{CommonName: "Test Leaf", Serial: 100}, ca, caKey)
	return ca, leaf, caKey, leafKey
}

// createOCSPResponse builds a DER-encoded OCSP response for template, issued
// and signed by issuer.
func createOCSPResponse(t *testing.T, issuer *x509.Certificate, issuerKey *rsa.PrivateKey, template ocsp.Response) []byte {
	t.Helper()
	der, err := ocsp.CreateResponse(issuer, issuer, template, issuerKey)
	if err != nil {
		t.Fatalf("creating OCSP response: %s", err)
	}
	return der
}

// newOCSPTLSServer starts a TLS test server presenting cert. The caller sets
// cert.OCSPStaple to control the stapled response.
func newOCSPTLSServer(t *testing.T, cert tls.Certificate) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// tlsConnectionState handshakes with the server and returns the connection
// state as the prober would see it, including any stapled OCSP response.
func tlsConnectionState(t *testing.T, srv *httptest.Server) *tls.ConnectionState {
	t.Helper()
	conn, err := tls.Dial("tcp", srv.Listener.Addr().String(), &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("dialing test TLS server: %s", err)
	}
	defer conn.Close()
	state := conn.ConnectionState()
	return &state
}

func validOCSPWindow() (time.Time, time.Time) {
	return time.Now().Add(-1 * time.Hour).Truncate(time.Second),
		time.Now().Add(24 * time.Hour).Truncate(time.Second)
}

// TestCheckStapledOCSP exercises stapled OCSP verification against a local CA
// and a real TLS test server, covering good, revoked, unknown, expired,
// missing and invalid responses.
func TestCheckStapledOCSP(t *testing.T) {
	thisUpdate, nextUpdate := validOCSPWindow()

	tests := []struct {
		name string
		// staple returns the DER response to staple; nil staples nothing.
		staple      func(t *testing.T, ca, leaf *x509.Certificate, caKey *rsa.PrivateKey) []byte
		leafOnly    bool // present only the leaf, without the issuer in the chain
		wantStatus  string
		wantStapled bool
		wantErr     bool
	}{
		{
			name: "good response",
			staple: func(t *testing.T, ca, leaf *x509.Certificate, caKey *rsa.PrivateKey) []byte {
				return createOCSPResponse(t, ca, caKey, ocsp.Response{
					Status: ocsp.Good, SerialNumber: leaf.SerialNumber,
					ThisUpdate: thisUpdate, NextUpdate: nextUpdate,
				})
			},
			wantStatus:  ocspStatusGood,
			wantStapled: true,
		},
		{
			name: "revoked response",
			staple: func(t *testing.T, ca, leaf *x509.Certificate, caKey *rsa.PrivateKey) []byte {
				return createOCSPResponse(t, ca, caKey, ocsp.Response{
					Status: ocsp.Revoked, SerialNumber: leaf.SerialNumber,
					ThisUpdate: thisUpdate, NextUpdate: nextUpdate,
					RevokedAt: thisUpdate, RevocationReason: ocsp.KeyCompromise,
				})
			},
			wantStatus:  ocspStatusRevoked,
			wantStapled: true,
		},
		{
			name: "unknown response",
			staple: func(t *testing.T, ca, leaf *x509.Certificate, caKey *rsa.PrivateKey) []byte {
				return createOCSPResponse(t, ca, caKey, ocsp.Response{
					Status: ocsp.Unknown, SerialNumber: leaf.SerialNumber,
					ThisUpdate: thisUpdate, NextUpdate: nextUpdate,
				})
			},
			wantStatus:  ocspStatusUnknown,
			wantStapled: true,
		},
		{
			name: "expired response",
			staple: func(t *testing.T, ca, leaf *x509.Certificate, caKey *rsa.PrivateKey) []byte {
				return createOCSPResponse(t, ca, caKey, ocsp.Response{
					Status: ocsp.Good, SerialNumber: leaf.SerialNumber,
					ThisUpdate: time.Now().Add(-48 * time.Hour), NextUpdate: time.Now().Add(-24 * time.Hour),
				})
			},
			wantStatus:  ocspStatusExpired,
			wantStapled: true,
		},
		{
			name: "response not yet valid",
			staple: func(t *testing.T, ca, leaf *x509.Certificate, caKey *rsa.PrivateKey) []byte {
				return createOCSPResponse(t, ca, caKey, ocsp.Response{
					Status: ocsp.Good, SerialNumber: leaf.SerialNumber,
					ThisUpdate: time.Now().Add(24 * time.Hour), NextUpdate: time.Now().Add(48 * time.Hour),
				})
			},
			wantStatus:  ocspStatusFailed,
			wantStapled: true,
			wantErr:     true,
		},
		{
			name:       "no stapled response",
			staple:     func(_ *testing.T, _, _ *x509.Certificate, _ *rsa.PrivateKey) []byte { return nil },
			wantStatus: ocspStatusNotStapled,
		},
		{
			name: "unparsable response",
			staple: func(_ *testing.T, _, _ *x509.Certificate, _ *rsa.PrivateKey) []byte {
				return []byte("not an OCSP response")
			},
			wantStatus:  ocspStatusFailed,
			wantStapled: true,
			wantErr:     true,
		},
		{
			name: "response for a different serial number",
			staple: func(t *testing.T, ca, _ *x509.Certificate, caKey *rsa.PrivateKey) []byte {
				return createOCSPResponse(t, ca, caKey, ocsp.Response{
					Status: ocsp.Good, SerialNumber: big.NewInt(999),
					ThisUpdate: thisUpdate, NextUpdate: nextUpdate,
				})
			},
			wantStatus:  ocspStatusFailed,
			wantStapled: true,
			wantErr:     true,
		},
		{
			name: "response signed by a different issuer",
			staple: func(t *testing.T, _, leaf *x509.Certificate, _ *rsa.PrivateKey) []byte {
				otherCA, otherKey := generateCRLTestCert(t, crlCertOptions{CommonName: "Other CA", Serial: 1, IsCA: true}, nil, nil)
				return createOCSPResponse(t, otherCA, otherKey, ocsp.Response{
					Status: ocsp.Good, SerialNumber: leaf.SerialNumber,
					ThisUpdate: thisUpdate, NextUpdate: nextUpdate,
				})
			},
			wantStatus:  ocspStatusFailed,
			wantStapled: true,
			wantErr:     true,
		},
		{
			name: "issuer missing from the chain",
			staple: func(t *testing.T, ca, leaf *x509.Certificate, caKey *rsa.PrivateKey) []byte {
				return createOCSPResponse(t, ca, caKey, ocsp.Response{
					Status: ocsp.Good, SerialNumber: leaf.SerialNumber,
					ThisUpdate: thisUpdate, NextUpdate: nextUpdate,
				})
			},
			leafOnly:    true,
			wantStatus:  ocspStatusFailed,
			wantStapled: true,
			wantErr:     true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ca, leaf, caKey, leafKey := newOCSPTestPKI(t)
			staple := test.staple(t, ca, leaf, caKey)

			chain := []*x509.Certificate{leaf, ca}
			if test.leafOnly {
				chain = []*x509.Certificate{leaf}
			}
			cert := serverTLSCert(leafKey, chain...)
			cert.OCSPStaple = staple

			srv := newOCSPTLSServer(t, cert)
			state := tlsConnectionState(t, srv)

			result := checkStapledOCSP(state, promslog.NewNopLogger())

			if result.Status != test.wantStatus {
				t.Errorf("Status = %q, want %q (err: %v)", result.Status, test.wantStatus, result.Err)
			}
			if result.Stapled != test.wantStapled {
				t.Errorf("Stapled = %v, want %v", result.Stapled, test.wantStapled)
			}
			if gotErr := result.Err != nil; gotErr != test.wantErr {
				t.Errorf("Err = %v, want error: %v", result.Err, test.wantErr)
			}
		})
	}
}

// TestCheckStapledOCSPFields asserts the parsed response fields correspond to
// the leaf certificate and the response template: serial number, issuer and
// the time fields.
func TestCheckStapledOCSPFields(t *testing.T) {
	ca, leaf, caKey, leafKey := newOCSPTestPKI(t)
	thisUpdate, nextUpdate := validOCSPWindow()

	staple := createOCSPResponse(t, ca, caKey, ocsp.Response{
		Status: ocsp.Good, SerialNumber: leaf.SerialNumber,
		ThisUpdate: thisUpdate, NextUpdate: nextUpdate,
	})

	cert := serverTLSCert(leafKey, leaf, ca)
	cert.OCSPStaple = staple
	srv := newOCSPTLSServer(t, cert)
	state := tlsConnectionState(t, srv)

	// The staple must arrive on the connection state and describe the leaf.
	if len(state.OCSPResponse) == 0 {
		t.Fatal("Expected the server to staple the OCSP response")
	}
	resp, err := ocsp.ParseResponseForCert(state.OCSPResponse, leaf, ca)
	if err != nil {
		t.Fatalf("parsing stapled response: %s", err)
	}
	if resp.SerialNumber.Cmp(leaf.SerialNumber) != 0 {
		t.Errorf("Response serial %x does not match leaf serial %x", resp.SerialNumber, leaf.SerialNumber)
	}

	result := checkStapledOCSP(state, promslog.NewNopLogger())
	if result.Status != ocspStatusGood {
		t.Fatalf("Status = %q, want %q (err: %v)", result.Status, ocspStatusGood, result.Err)
	}
	if !result.ThisUpdate.Equal(thisUpdate) {
		t.Errorf("ThisUpdate = %s, want %s", result.ThisUpdate, thisUpdate)
	}
	if !result.NextUpdate.Equal(nextUpdate) {
		t.Errorf("NextUpdate = %s, want %s", result.NextUpdate, nextUpdate)
	}
	// CreateResponse sets ProducedAt to the current time truncated to minutes.
	if result.ProducedAt.IsZero() || time.Since(result.ProducedAt) > 2*time.Minute {
		t.Errorf("ProducedAt = %s, want a recent timestamp", result.ProducedAt)
	}
}

func TestRegisterOCSPMetrics(t *testing.T) {
	ca, leaf, caKey, leafKey := newOCSPTestPKI(t)
	thisUpdate, nextUpdate := validOCSPWindow()

	staple := createOCSPResponse(t, ca, caKey, ocsp.Response{
		Status: ocsp.Good, SerialNumber: leaf.SerialNumber,
		ThisUpdate: thisUpdate, NextUpdate: nextUpdate,
	})
	cert := serverTLSCert(leafKey, leaf, ca)
	cert.OCSPStaple = staple
	srv := newOCSPTLSServer(t, cert)
	state := tlsConnectionState(t, srv)

	registry := prometheus.NewRegistry()
	checkOCSP(state, true, registry, promslog.NewNopLogger())

	mfs, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}

	if val, ok := getMetricValue(mfs, "probe_ssl_ocsp_stapled"); !ok || val != 1 {
		t.Errorf("probe_ssl_ocsp_stapled = %v (found=%v), want 1", val, ok)
	}
	for _, status := range ocspStatuses {
		want := 0.0
		if status == ocspStatusGood {
			want = 1
		}
		if val, ok := getMetricWithLabels(mfs, "probe_ssl_ocsp_response_status", map[string]string{"status": status}); !ok || val != want {
			t.Errorf("probe_ssl_ocsp_response_status{status=%q} = %v (found=%v), want %v", status, val, ok, want)
		}
	}
	if val, ok := getMetricValue(mfs, "probe_ssl_ocsp_this_update_timestamp_seconds"); !ok || val != float64(thisUpdate.Unix()) {
		t.Errorf("probe_ssl_ocsp_this_update_timestamp_seconds = %v (found=%v), want %v", val, ok, thisUpdate.Unix())
	}
	if val, ok := getMetricValue(mfs, "probe_ssl_ocsp_next_update_timestamp_seconds"); !ok || val != float64(nextUpdate.Unix()) {
		t.Errorf("probe_ssl_ocsp_next_update_timestamp_seconds = %v (found=%v), want %v", val, ok, nextUpdate.Unix())
	}
	if val, ok := getMetricValue(mfs, "probe_ssl_ocsp_produced_at_timestamp_seconds"); !ok || val <= 0 {
		t.Errorf("probe_ssl_ocsp_produced_at_timestamp_seconds = %v (found=%v), want > 0", val, ok)
	}
}

// TestRegisterOCSPMetricsNoStaple asserts the metrics reported when the server
// staples nothing.
func TestRegisterOCSPMetricsNoStaple(t *testing.T) {
	ca, leaf, _, leafKey := newOCSPTestPKI(t)
	srv := newOCSPTLSServer(t, serverTLSCert(leafKey, leaf, ca))
	state := tlsConnectionState(t, srv)

	registry := prometheus.NewRegistry()
	checkOCSP(state, true, registry, promslog.NewNopLogger())

	mfs, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}

	if val, ok := getMetricValue(mfs, "probe_ssl_ocsp_stapled"); !ok || val != 0 {
		t.Errorf("probe_ssl_ocsp_stapled = %v (found=%v), want 0", val, ok)
	}
	if val, ok := getMetricWithLabels(mfs, "probe_ssl_ocsp_response_status", map[string]string{"status": ocspStatusNotStapled}); !ok || val != 1 {
		t.Errorf("probe_ssl_ocsp_response_status{status=%q} = %v (found=%v), want 1", ocspStatusNotStapled, val, ok)
	}
}

// TestRegisterOCSPMetricsLabels asserts the OCSP metrics carry no certificate
// details or responder URLs in their labels: the only label is the status.
func TestRegisterOCSPMetricsLabels(t *testing.T) {
	ca, leaf, caKey, leafKey := newOCSPTestPKI(t)
	thisUpdate, nextUpdate := validOCSPWindow()
	staple := createOCSPResponse(t, ca, caKey, ocsp.Response{
		Status: ocsp.Good, SerialNumber: leaf.SerialNumber,
		ThisUpdate: thisUpdate, NextUpdate: nextUpdate,
	})
	cert := serverTLSCert(leafKey, leaf, ca)
	cert.OCSPStaple = staple
	srv := newOCSPTLSServer(t, cert)
	state := tlsConnectionState(t, srv)

	registry := prometheus.NewRegistry()
	checkOCSP(state, true, registry, promslog.NewNopLogger())

	mfs, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}

	for _, mf := range mfs {
		if mf.GetName() != "probe_ssl_ocsp_response_status" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, label := range m.GetLabel() {
				if label.GetName() != "status" {
					t.Errorf("Unexpected label %q=%q on probe_ssl_ocsp_response_status", label.GetName(), label.GetValue())
				}
			}
		}
	}
}

// TestCheckOCSPDisabled registers no metrics when the check is disabled.
func TestCheckOCSPDisabled(t *testing.T) {
	ca, leaf, _, leafKey := newOCSPTestPKI(t)
	srv := newOCSPTLSServer(t, serverTLSCert(leafKey, leaf, ca))
	state := tlsConnectionState(t, srv)

	registry := prometheus.NewRegistry()
	checkOCSP(state, false, registry, promslog.NewNopLogger())

	mfs, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() == "probe_ssl_ocsp_stapled" {
			t.Error("Expected no OCSP metrics when the check is disabled")
		}
	}
}

// TestProbeHTTPOCSPStapling runs end-to-end HTTP probes against a TLS test
// server, covering collect-only and require modes.
func TestProbeHTTPOCSPStapling(t *testing.T) {
	thisUpdate, nextUpdate := validOCSPWindow()

	tests := []struct {
		name            string
		staple          func(t *testing.T, ca, leaf *x509.Certificate, caKey *rsa.PrivateKey) []byte
		checkStapling   bool
		requireStapling bool
		wantSuccess     bool
	}{
		{
			name: "require stapling with a good response succeeds",
			staple: func(t *testing.T, ca, leaf *x509.Certificate, caKey *rsa.PrivateKey) []byte {
				return createOCSPResponse(t, ca, caKey, ocsp.Response{
					Status: ocsp.Good, SerialNumber: leaf.SerialNumber,
					ThisUpdate: thisUpdate, NextUpdate: nextUpdate,
				})
			},
			requireStapling: true,
			wantSuccess:     true,
		},
		{
			name:            "require stapling without a stapled response fails",
			staple:          func(_ *testing.T, _, _ *x509.Certificate, _ *rsa.PrivateKey) []byte { return nil },
			requireStapling: true,
			wantSuccess:     false,
		},
		{
			name: "require stapling with a revoked response fails",
			staple: func(t *testing.T, ca, leaf *x509.Certificate, caKey *rsa.PrivateKey) []byte {
				return createOCSPResponse(t, ca, caKey, ocsp.Response{
					Status: ocsp.Revoked, SerialNumber: leaf.SerialNumber,
					ThisUpdate: thisUpdate, NextUpdate: nextUpdate, RevokedAt: thisUpdate,
				})
			},
			requireStapling: true,
			wantSuccess:     false,
		},
		{
			name:          "collect only without a stapled response succeeds",
			staple:        func(_ *testing.T, _, _ *x509.Certificate, _ *rsa.PrivateKey) []byte { return nil },
			checkStapling: true,
			wantSuccess:   true,
		},
		{
			name: "collect only with a revoked response succeeds",
			staple: func(t *testing.T, ca, leaf *x509.Certificate, caKey *rsa.PrivateKey) []byte {
				return createOCSPResponse(t, ca, caKey, ocsp.Response{
					Status: ocsp.Revoked, SerialNumber: leaf.SerialNumber,
					ThisUpdate: thisUpdate, NextUpdate: nextUpdate, RevokedAt: thisUpdate,
				})
			},
			checkStapling: true,
			wantSuccess:   true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ca, leaf, caKey, leafKey := newOCSPTestPKI(t)
			cert := serverTLSCert(leafKey, leaf, ca)
			cert.OCSPStaple = test.staple(t, ca, leaf, caKey)
			srv := newOCSPTLSServer(t, cert)

			registry := prometheus.NewRegistry()
			module := config.Module{
				Timeout: time.Second,
				HTTP: config.HTTPProbe{
					IPProtocol:          "ip4",
					IPProtocolFallback:  true,
					CheckOCSPStapling:   test.checkStapling,
					RequireOCSPStapling: test.requireStapling,
					HTTPClientConfig: pconfig.HTTPClientConfig{
						TLSConfig: pconfig.TLSConfig{InsecureSkipVerify: true},
					},
				},
			}

			result := ProbeHTTP(context.Background(), srv.URL, module, registry, promslog.NewNopLogger())
			if result != test.wantSuccess {
				t.Errorf("ProbeHTTP = %v, want %v", result, test.wantSuccess)
			}

			mfs, err := registry.Gather()
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := getMetricValue(mfs, "probe_ssl_ocsp_stapled"); !ok {
				t.Error("Expected probe_ssl_ocsp_stapled to be registered")
			}
		})
	}
}

// TestProbeGRPCOCSPStapling runs end-to-end gRPC probes against a TLS test
// server, covering collect-only and require modes.
func TestProbeGRPCOCSPStapling(t *testing.T) {
	thisUpdate, nextUpdate := validOCSPWindow()

	tests := []struct {
		name            string
		staple          func(t *testing.T, ca, leaf *x509.Certificate, caKey *rsa.PrivateKey) []byte
		requireStapling bool
		wantSuccess     bool
		wantStatus      string
	}{
		{
			name: "require stapling with a good response succeeds",
			staple: func(t *testing.T, ca, leaf *x509.Certificate, caKey *rsa.PrivateKey) []byte {
				return createOCSPResponse(t, ca, caKey, ocsp.Response{
					Status: ocsp.Good, SerialNumber: leaf.SerialNumber,
					ThisUpdate: thisUpdate, NextUpdate: nextUpdate,
				})
			},
			requireStapling: true,
			wantSuccess:     true,
			wantStatus:      ocspStatusGood,
		},
		{
			name:            "require stapling without a stapled response fails",
			staple:          func(_ *testing.T, _, _ *x509.Certificate, _ *rsa.PrivateKey) []byte { return nil },
			requireStapling: true,
			wantSuccess:     false,
			wantStatus:      ocspStatusNotStapled,
		},
		{
			name: "collect only with a revoked response succeeds",
			staple: func(t *testing.T, ca, leaf *x509.Certificate, caKey *rsa.PrivateKey) []byte {
				return createOCSPResponse(t, ca, caKey, ocsp.Response{
					Status: ocsp.Revoked, SerialNumber: leaf.SerialNumber,
					ThisUpdate: thisUpdate, NextUpdate: nextUpdate,
					RevokedAt: thisUpdate, RevocationReason: ocsp.KeyCompromise,
				})
			},
			wantSuccess: true,
			wantStatus:  ocspStatusRevoked,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ca, leaf, caKey, leafKey := newOCSPTestPKI(t)
			cert := serverTLSCert(leafKey, leaf, ca)
			cert.OCSPStaple = test.staple(t, ca, leaf, caKey)

			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("Error listening on socket: %s", err)
			}
			defer ln.Close()

			s := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
				Certificates: []tls.Certificate{cert},
			})))
			healthServer := health.NewServer()
			healthServer.SetServingStatus("service", grpc_health_v1.HealthCheckResponse_SERVING)
			grpc_health_v1.RegisterHealthServer(s, healthServer)
			go func() {
				if err := s.Serve(ln); err != nil {
					t.Errorf("failed to serve: %v", err)
				}
			}()
			defer s.GracefulStop()

			registry := prometheus.NewRegistry()
			module := config.Module{
				Timeout: time.Second,
				GRPC: config.GRPCProbe{
					TLS:                 true,
					TLSConfig:           pconfig.TLSConfig{InsecureSkipVerify: true},
					CheckOCSPStapling:   true,
					RequireOCSPStapling: test.requireStapling,
					PreferredIPProtocol: "ip4",
					IPProtocolFallback:  false,
				},
			}

			result := ProbeGRPC(context.Background(), ln.Addr().String(), module, registry, promslog.NewNopLogger())
			if result != test.wantSuccess {
				t.Errorf("ProbeGRPC = %v, want %v", result, test.wantSuccess)
			}

			mfs, err := registry.Gather()
			if err != nil {
				t.Fatal(err)
			}
			if val, ok := getMetricWithLabels(mfs, "probe_ssl_ocsp_response_status", map[string]string{"status": test.wantStatus}); !ok || val != 1 {
				t.Errorf("probe_ssl_ocsp_response_status{status=%q} = %v (found=%v), want 1", test.wantStatus, val, ok)
			}
		})
	}
}
