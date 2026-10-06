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
	"encoding/asn1"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"golang.org/x/crypto/ocsp"

	"github.com/prometheus/blackbox_exporter/config"
	"github.com/prometheus/client_golang/prometheus"
	pconfig "github.com/prometheus/common/config"
	"github.com/prometheus/common/promslog"
)

// ocspTestChain holds a local CA and a leaf signed by it.
type ocspTestChain struct {
	ca    *x509.Certificate
	caKey *rsa.PrivateKey
	leaf  *x509.Certificate
}

func newOCSPTestChain(t *testing.T, leafSerial int64) ocspTestChain {
	t.Helper()
	ca, caKey := generateCRLTestCert(t, crlCertOptions{CommonName: "OCSP Test CA", Serial: 1, IsCA: true}, nil, nil)
	leaf, _ := generateCRLTestCert(t, crlCertOptions{
		CommonName: "OCSP Test Leaf",
		Serial:     leafSerial,
		OCSPServer: "http://ocsp.invalid/",
	}, ca, caKey)
	return ocspTestChain{ca: ca, caKey: caKey, leaf: leaf}
}

// createOCSPResponse builds a DER-encoded OCSP response with the given status
// and validity window, signed by the CA for the leaf's serial. The response
// therefore describes the current certificate: serial, issuer CertID,
// signature and time fields all match.
func createOCSPResponse(t *testing.T, leaf, ca *x509.Certificate, caKey *rsa.PrivateKey, status int, thisUpdate, nextUpdate time.Time) []byte {
	t.Helper()
	raw, err := ocsp.CreateResponse(ca, ca, ocsp.Response{
		Status:       status,
		SerialNumber: leaf.SerialNumber,
		ThisUpdate:   thisUpdate,
		NextUpdate:   nextUpdate,
		RevokedAt:    thisUpdate,
	}, caKey)
	if err != nil {
		t.Fatalf("creating OCSP response: %s", err)
	}
	return raw
}

func validOCSPWindow() (time.Time, time.Time) {
	return time.Now().Add(-1 * time.Hour), time.Now().Add(24 * time.Hour)
}

func TestEvaluateStapledOCSP(t *testing.T) {
	chain := newOCSPTestChain(t, 100)
	thisUpdate, nextUpdate := validOCSPWindow()

	otherCA, otherCAKey := generateCRLTestCert(t, crlCertOptions{CommonName: "Other CA", Serial: 2, IsCA: true}, nil, nil)

	tests := []struct {
		name       string
		raw        []byte
		peerChain  []*x509.Certificate
		wantStatus string
	}{
		{
			name:       "no staple",
			peerChain:  []*x509.Certificate{chain.leaf, chain.ca},
			wantStatus: ocspStatusNotProvided,
		},
		{
			name:       "good response",
			raw:        createOCSPResponse(t, chain.leaf, chain.ca, chain.caKey, ocsp.Good, thisUpdate, nextUpdate),
			peerChain:  []*x509.Certificate{chain.leaf, chain.ca},
			wantStatus: ocspStatusGood,
		},
		{
			name:       "revoked response",
			raw:        createOCSPResponse(t, chain.leaf, chain.ca, chain.caKey, ocsp.Revoked, thisUpdate, nextUpdate),
			peerChain:  []*x509.Certificate{chain.leaf, chain.ca},
			wantStatus: ocspStatusRevoked,
		},
		{
			name:       "unknown response",
			raw:        createOCSPResponse(t, chain.leaf, chain.ca, chain.caKey, ocsp.Unknown, thisUpdate, nextUpdate),
			peerChain:  []*x509.Certificate{chain.leaf, chain.ca},
			wantStatus: ocspStatusUnknown,
		},
		{
			name:       "expired response",
			raw:        createOCSPResponse(t, chain.leaf, chain.ca, chain.caKey, ocsp.Good, time.Now().Add(-48*time.Hour), time.Now().Add(-24*time.Hour)),
			peerChain:  []*x509.Certificate{chain.leaf, chain.ca},
			wantStatus: ocspStatusExpired,
		},
		{
			name:       "not yet valid response",
			raw:        createOCSPResponse(t, chain.leaf, chain.ca, chain.caKey, ocsp.Good, time.Now().Add(24*time.Hour), time.Now().Add(48*time.Hour)),
			peerChain:  []*x509.Certificate{chain.leaf, chain.ca},
			wantStatus: ocspStatusExpired,
		},
		{
			name:       "garbage bytes",
			raw:        []byte("not an OCSP response"),
			peerChain:  []*x509.Certificate{chain.leaf, chain.ca},
			wantStatus: ocspStatusFailed,
		},
		{
			name:       "response for a different serial",
			raw:        createOCSPResponseForSerial(t, new(big.Int).SetInt64(999), chain.ca, chain.caKey, thisUpdate, nextUpdate),
			peerChain:  []*x509.Certificate{chain.leaf, chain.ca},
			wantStatus: ocspStatusFailed,
		},
		{
			name:       "response signed by a different CA",
			raw:        createOCSPResponse(t, chain.leaf, otherCA, otherCAKey, ocsp.Good, thisUpdate, nextUpdate),
			peerChain:  []*x509.Certificate{chain.leaf, chain.ca},
			wantStatus: ocspStatusFailed,
		},
		{
			name:       "leaf presented without issuer",
			raw:        createOCSPResponse(t, chain.leaf, chain.ca, chain.caKey, ocsp.Good, thisUpdate, nextUpdate),
			peerChain:  []*x509.Certificate{chain.leaf},
			wantStatus: ocspStatusFailed,
		},
		{
			name:       "CertID issuer hashes do not match the issuer",
			raw:        tamperedOCSPIssuerHashes(t, createOCSPResponse(t, chain.leaf, chain.ca, chain.caKey, ocsp.Good, thisUpdate, nextUpdate)),
			peerChain:  []*x509.Certificate{chain.leaf, chain.ca},
			wantStatus: ocspStatusFailed,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := &tls.ConnectionState{
				PeerCertificates: test.peerChain,
				OCSPResponse:     test.raw,
			}
			result := evaluateStapledOCSP(state, time.Now(), promslog.NewNopLogger())
			if result.status != test.wantStatus {
				t.Fatalf("status = %q, want %q (reason: %s)", result.status, test.wantStatus, result.reason)
			}
		})
	}
}

// createOCSPResponseForSerial signs a response for an arbitrary serial.
func createOCSPResponseForSerial(t *testing.T, serial *big.Int, ca *x509.Certificate, caKey *rsa.PrivateKey, thisUpdate, nextUpdate time.Time) []byte {
	t.Helper()
	raw, err := ocsp.CreateResponse(ca, ca, ocsp.Response{
		Status:       ocsp.Good,
		SerialNumber: serial,
		ThisUpdate:   thisUpdate,
		NextUpdate:   nextUpdate,
	}, caKey)
	if err != nil {
		t.Fatalf("creating OCSP response: %s", err)
	}
	return raw
}

// tamperedOCSPIssuerHashes flips a byte in the CertID issuer name hash and
// re-encodes the response, simulating a response built for a different issuer
// (the signature is unchanged, so parsing alone may still accept it).
func tamperedOCSPIssuerHashes(t *testing.T, raw []byte) []byte {
	t.Helper()

	var response ocspResponseASN1
	if _, err := asn1.Unmarshal(raw, &response); err != nil {
		t.Fatalf("unmarshalling OCSP response: %s", err)
	}
	var basic ocspBasicResponseASN1
	if _, err := asn1.Unmarshal(response.Response.Response, &basic); err != nil {
		t.Fatalf("unmarshalling basic response: %s", err)
	}
	if len(basic.TBSResponseData.Responses) == 0 ||
		len(basic.TBSResponseData.Responses[0].CertID.NameHash) == 0 {
		t.Fatal("response has no CertID name hash to tamper with")
	}
	basic.TBSResponseData.Responses[0].CertID.NameHash[0] ^= 0xFF

	// Re-marshal the full basic response with the tampered TBS data, keeping
	// the original signature in place.
	type basicResponseForMarshal struct {
		TBSResponseData    ocspResponseDataASN1
		SignatureAlgorithm pkixAlgorithmIdentifier
		Signature          asn1.BitString
	}
	rebuilt, err := asn1.Marshal(basicResponseForMarshal{
		TBSResponseData:    basic.TBSResponseData,
		SignatureAlgorithm: basic.SignatureAlgorithm,
		Signature:          basic.Signature,
	})
	if err != nil {
		t.Fatalf("marshalling basic response: %s", err)
	}

	outer, err := asn1.Marshal(ocspResponseASN1{
		Status:   0,
		Response: ocspResponseBytesASN1{ResponseType: idPKIXOCSPBasic, Response: rebuilt},
	})
	if err != nil {
		t.Fatalf("marshalling response: %s", err)
	}
	return outer
}

// TestStapledOCSPMatchesCurrentCertificate asserts the parsed response fields
// genuinely describe the presented certificate: serial, issuer hashes,
// signature (accepted by ParseResponseForCert) and the time window.
func TestStapledOCSPMatchesCurrentCertificate(t *testing.T) {
	chain := newOCSPTestChain(t, 4242)
	thisUpdate, nextUpdate := validOCSPWindow()
	raw := createOCSPResponse(t, chain.leaf, chain.ca, chain.caKey, ocsp.Good, thisUpdate, nextUpdate)

	state := &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{chain.leaf, chain.ca},
		OCSPResponse:     raw,
	}
	result := evaluateStapledOCSP(state, time.Now(), promslog.NewNopLogger())
	if result.status != ocspStatusGood {
		t.Fatalf("status = %q, want good (reason: %s)", result.status, result.reason)
	}

	parsed, err := ocsp.ParseResponseForCert(raw, chain.leaf, chain.ca)
	if err != nil {
		t.Fatalf("ParseResponseForCert: %s", err)
	}
	if parsed.SerialNumber.Cmp(chain.leaf.SerialNumber) != 0 {
		t.Errorf("response serial %s != leaf serial %s", parsed.SerialNumber, chain.leaf.SerialNumber)
	}

	certID, err := extractOCSPCertID(raw, chain.leaf.SerialNumber)
	if err != nil {
		t.Fatalf("extracting CertID: %s", err)
	}
	if err := verifyOCSPIssuer(raw, chain.leaf, chain.ca, parsed.IssuerHash); err != nil {
		t.Errorf("CertID issuer verification failed: %s", err)
	}
	if certID.SerialNumber.Cmp(chain.leaf.SerialNumber) != 0 {
		t.Errorf("CertID serial %s != leaf serial %s", certID.SerialNumber, chain.leaf.SerialNumber)
	}

	if !result.thisUpdate.Equal(thisUpdate.Truncate(time.Second)) && !withinSecond(result.thisUpdate, thisUpdate) {
		t.Errorf("thisUpdate = %s, want %s", result.thisUpdate, thisUpdate)
	}
	if !withinSecond(result.nextUpdate, nextUpdate) {
		t.Errorf("nextUpdate = %s, want %s", result.nextUpdate, nextUpdate)
	}
}

func withinSecond(a, b time.Time) bool {
	d := a.Sub(b)
	if d < 0 {
		d = -d
	}
	return d <= 2*time.Second
}

func TestCheckOCSPStaplingRequired(t *testing.T) {
	chain := newOCSPTestChain(t, 200)
	thisUpdate, nextUpdate := validOCSPWindow()

	tests := []struct {
		name      string
		enabled   bool
		required  bool
		raw       []byte
		wantProbe bool
	}{
		{"disabled returns true and registers nothing", false, false, nil, true},
		{"enabled without staple does not fail", true, false, nil, true},
		{"required without staple fails", true, true, nil, false},
		{"required good response passes", true, true, createOCSPResponse(t, chain.leaf, chain.ca, chain.caKey, ocsp.Good, thisUpdate, nextUpdate), true},
		{"required revoked response fails", true, true, createOCSPResponse(t, chain.leaf, chain.ca, chain.caKey, ocsp.Revoked, thisUpdate, nextUpdate), false},
		{"required expired response fails", true, true, createOCSPResponse(t, chain.leaf, chain.ca, chain.caKey, ocsp.Good, time.Now().Add(-48*time.Hour), time.Now().Add(-24*time.Hour)), false},
		{"required invalid bytes fail", true, true, []byte("bad"), false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			registry := prometheus.NewRegistry()
			state := &tls.ConnectionState{
				PeerCertificates: []*x509.Certificate{chain.leaf, chain.ca},
				OCSPResponse:     test.raw,
			}
			cfg := config.OCSPStaplingConfig{Enabled: test.enabled, Required: test.required}
			got := checkOCSPStapling(state, cfg, registry, promslog.NewNopLogger())
			if got != test.wantProbe {
				t.Fatalf("checkOCSPStapling = %v, want %v", got, test.wantProbe)
			}

			mfs, err := registry.Gather()
			if err != nil {
				t.Fatal(err)
			}
			if test.enabled {
				if _, ok := getMetricWithLabels(mfs, "probe_ssl_ocsp_status_info", map[string]string{}); !ok {
					t.Error("expected probe_ssl_ocsp_status_info when enabled")
				}
			} else if len(mfs) != 0 {
				t.Errorf("expected no metrics when disabled, got %d families", len(mfs))
			}
		})
	}
}

// TestRegisterOCSPMetrics checks the emitted metrics: the single active
// status, the time fields, and that labels never carry the responder URL or
// certificate material.
func TestRegisterOCSPMetrics(t *testing.T) {
	chain := newOCSPTestChain(t, 300)
	thisUpdate := time.Now().Add(-1 * time.Hour).Truncate(time.Hour)
	nextUpdate := time.Now().Add(24 * time.Hour).Truncate(time.Hour)
	revokedAt := time.Now().Add(-2 * time.Hour).Truncate(time.Hour)
	raw, err := ocsp.CreateResponse(chain.ca, chain.ca, ocsp.Response{
		Status:       ocsp.Revoked,
		SerialNumber: chain.leaf.SerialNumber,
		ThisUpdate:   thisUpdate,
		NextUpdate:   nextUpdate,
		RevokedAt:    revokedAt,
	}, chain.caKey)
	if err != nil {
		t.Fatalf("creating OCSP response: %s", err)
	}

	state := &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{chain.leaf, chain.ca},
		OCSPResponse:     raw,
	}
	result := evaluateStapledOCSP(state, time.Now(), promslog.NewNopLogger())
	if result.status != ocspStatusRevoked {
		t.Fatalf("status = %q, want revoked", result.status)
	}

	registry := prometheus.NewRegistry()
	registerOCSPMetrics(registry, result)
	mfs, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}

	if val, ok := getMetricWithLabels(mfs, "probe_ssl_ocsp_status_info", map[string]string{"status": "revoked"}); !ok || val != 1 {
		t.Errorf("status{revoked} = %v (found=%v), want 1", val, ok)
	}
	if val, ok := getMetricWithLabels(mfs, "probe_ssl_ocsp_status_info", map[string]string{"status": "good"}); !ok || val != 0 {
		t.Errorf("status{good} = %v (found=%v), want 0", val, ok)
	}

	// Every other status must exist as a time series, with value 0.
	for _, status := range []string{ocspStatusNotProvided, ocspStatusGood, ocspStatusUnknown, ocspStatusExpired, ocspStatusFailed} {
		if _, ok := getMetricWithLabels(mfs, "probe_ssl_ocsp_status_info", map[string]string{"status": status}); !ok {
			t.Errorf("missing status series %q", status)
		}
	}

	if val, ok := getMetricValue(mfs, "probe_ssl_ocsp_this_update_timestamp_seconds"); !ok || val != float64(thisUpdate.Unix()) {
		t.Errorf("this_update = %v (found=%v), want %d", val, ok, thisUpdate.Unix())
	}
	if val, ok := getMetricValue(mfs, "probe_ssl_ocsp_next_update_timestamp_seconds"); !ok || val != float64(nextUpdate.Unix()) {
		t.Errorf("next_update = %v (found=%v), want %d", val, ok, nextUpdate.Unix())
	}
	if val, ok := getMetricValue(mfs, "probe_ssl_ocsp_revoked_at_timestamp_seconds"); !ok || val != float64(revokedAt.Unix()) {
		t.Errorf("revoked_at = %v (found=%v), want %d", val, ok, revokedAt.Unix())
	}

	// Labels must never expose the responder URL or certificate data.
	for _, mf := range mfs {
		for _, metric := range mf.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetName() != "status" {
					t.Errorf("unexpected label %q on %s", label.GetName(), mf.GetName())
				}
				if label.GetValue() == "http://ocsp.invalid/" || label.GetValue() == chain.leaf.Subject.String() {
					t.Errorf("sensitive value leaked into label: %q", label.GetValue())
				}
			}
		}
	}
}

// startOCSPTestServerWithLeaf stands up a TLS server on 127.0.0.1 presenting
// a leaf signed by chain.ca and stapling stapleFor(leaf) (nil means no
// staple). It returns the host:port target, the CA file to trust, and the
// served leaf.
func startOCSPTestServerWithLeaf(t *testing.T, chain ocspTestChain, stapleFor func(t *testing.T, leaf *x509.Certificate) []byte) (target, caFile string, serverLeaf *x509.Certificate) {
	t.Helper()

	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: chain.ca.Raw})
	tmpCA, err := os.CreateTemp("", "ocsp-ca-*.pem")
	if err != nil {
		t.Fatalf("creating CA tempfile: %s", err)
	}
	if _, err := tmpCA.Write(caPEM); err != nil {
		t.Fatalf("writing CA tempfile: %s", err)
	}
	if err := tmpCA.Close(); err != nil {
		t.Fatalf("closing CA tempfile: %s", err)
	}
	t.Cleanup(func() { os.Remove(tmpCA.Name()) })

	serverLeaf, serverLeafKey := generateCRLTestCert(t, crlCertOptions{
		CommonName: "localhost",
		Serial:     5001,
		OCSPServer: "http://ocsp.invalid/",
	}, chain.ca, chain.caKey)

	serverCert := tls.Certificate{
		Certificate: [][]byte{serverLeaf.Raw, chain.ca.Raw},
		PrivateKey:  serverLeafKey,
		OCSPStaple:  stapleFor(t, serverLeaf),
	}

	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		MinVersion:   tls.VersionTLS12,
	})
	if err != nil {
		t.Fatalf("listening: %s", err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_ = c.(*tls.Conn).Handshake()
				fmt.Fprintf(c, "bye\n")
			}(conn)
		}
	}()

	return ln.Addr().String(), tmpCA.Name(), serverLeaf
}

func TestProbeTCPOCSPStapling(t *testing.T) {
	chain := newOCSPTestChain(t, 1)
	thisUpdate, nextUpdate := validOCSPWindow()

	tests := []struct {
		name        string
		staple      func(t *testing.T, serverLeaf *x509.Certificate) []byte
		ocsp        config.OCSPStaplingConfig
		wantSuccess bool
		wantStatus  string
	}{
		{
			name:        "disabled does not emit metrics and succeeds without staple",
			staple:      func(t *testing.T, _ *x509.Certificate) []byte { return nil },
			ocsp:        config.OCSPStaplingConfig{},
			wantSuccess: true,
		},
		{
			name:        "enabled observe-only tolerates missing staple",
			staple:      func(t *testing.T, _ *x509.Certificate) []byte { return nil },
			ocsp:        config.OCSPStaplingConfig{Enabled: true},
			wantSuccess: true,
			wantStatus:  ocspStatusNotProvided,
		},
		{
			name:        "required fails without staple",
			staple:      func(t *testing.T, _ *x509.Certificate) []byte { return nil },
			ocsp:        config.OCSPStaplingConfig{Enabled: true, Required: true},
			wantSuccess: false,
			wantStatus:  ocspStatusNotProvided,
		},
		{
			name: "required good staple succeeds",
			staple: func(t *testing.T, serverLeaf *x509.Certificate) []byte {
				return createOCSPResponse(t, serverLeaf, chain.ca, chain.caKey, ocsp.Good, thisUpdate, nextUpdate)
			},
			ocsp:        config.OCSPStaplingConfig{Enabled: true, Required: true},
			wantSuccess: true,
			wantStatus:  ocspStatusGood,
		},
		{
			name: "observe-only with revoked staple still succeeds the probe",
			staple: func(t *testing.T, serverLeaf *x509.Certificate) []byte {
				return createOCSPResponse(t, serverLeaf, chain.ca, chain.caKey, ocsp.Revoked, thisUpdate, nextUpdate)
			},
			ocsp:        config.OCSPStaplingConfig{Enabled: true, Required: false},
			wantSuccess: true,
			wantStatus:  ocspStatusRevoked,
		},
		{
			name: "required expired staple fails",
			staple: func(t *testing.T, serverLeaf *x509.Certificate) []byte {
				return createOCSPResponse(t, serverLeaf, chain.ca, chain.caKey, ocsp.Good, time.Now().Add(-48*time.Hour), time.Now().Add(-24*time.Hour))
			},
			ocsp:        config.OCSPStaplingConfig{Enabled: true, Required: true},
			wantSuccess: false,
			wantStatus:  ocspStatusExpired,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Each case needs its own listener because the staple is fixed per cert.
			target, caFile, _ := startOCSPTestServerWithLeaf(t, chain, test.staple)

			module := config.Module{
				TCP: config.TCPProbe{
					IPProtocol:         "ip4",
					IPProtocolFallback: true,
					TLS:                true,
					TLSConfig:          pconfig.TLSConfig{CAFile: caFile, ServerName: "localhost"},
					OCSPStapling:       test.ocsp,
				},
			}

			registry := prometheus.NewRegistry()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			ok := ProbeTCP(ctx, target, module, registry, promslog.NewNopLogger())
			if ok != test.wantSuccess {
				t.Fatalf("ProbeTCP success = %v, want %v", ok, test.wantSuccess)
			}

			if test.ocsp.Enabled {
				mfs, err := registry.Gather()
				if err != nil {
					t.Fatal(err)
				}
				if val, found := getMetricWithLabels(mfs, "probe_ssl_ocsp_status_info", map[string]string{"status": test.wantStatus}); !found || val != 1 {
					t.Fatalf("status{%s} = %v (found=%v), want 1", test.wantStatus, val, found)
				}
			}
		})
	}
}

func TestProbeHTTPOCSPStapling(t *testing.T) {
	chain := newOCSPTestChain(t, 2)
	thisUpdate, nextUpdate := validOCSPWindow()

	ca, caKey := chain.ca, chain.caKey
	leaf, leafKey := generateCRLTestCert(t, crlCertOptions{
		CommonName: "Test Leaf",
		Serial:     6001,
		OCSPServer: "http://ocsp.invalid/",
	}, ca, caKey)

	tests := []struct {
		name        string
		required    bool
		staple      []byte
		wantSuccess bool
		wantStatus  string
	}{
		{"observe-only good", false, createOCSPResponse(t, leaf, ca, caKey, ocsp.Good, thisUpdate, nextUpdate), true, ocspStatusGood},
		{"observe-only missing never fails", false, nil, true, ocspStatusNotProvided},
		{"required missing fails", true, nil, false, ocspStatusNotProvided},
		{"required good passes", true, createOCSPResponse(t, leaf, ca, caKey, ocsp.Good, thisUpdate, nextUpdate), true, ocspStatusGood},
		{"required revoked fails", true, createOCSPResponse(t, leaf, ca, caKey, ocsp.Revoked, thisUpdate, nextUpdate), false, ocspStatusRevoked},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			serverCert := tls.Certificate{
				Certificate: [][]byte{leaf.Raw, ca.Raw},
				PrivateKey:  leafKey,
				OCSPStaple:  test.staple,
			}
			ts := httptest.NewUnstartedServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
			ts.TLS = &tls.Config{Certificates: []tls.Certificate{serverCert}}
			ts.StartTLS()
			t.Cleanup(ts.Close)

			registry := prometheus.NewRegistry()
			module := config.Module{
				Timeout: 2 * time.Second,
				HTTP: config.HTTPProbe{
					IPProtocolFallback: true,
					OCSPStapling:       config.OCSPStaplingConfig{Enabled: true, Required: test.required},
					HTTPClientConfig: pconfig.HTTPClientConfig{
						TLSConfig: pconfig.TLSConfig{InsecureSkipVerify: true},
					},
				},
			}

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result := ProbeHTTP(ctx, ts.URL, module, registry, promslog.NewNopLogger())
			if result != test.wantSuccess {
				t.Fatalf("ProbeHTTP success = %v, want %v", result, test.wantSuccess)
			}

			mfs, err := registry.Gather()
			if err != nil {
				t.Fatal(err)
			}
			if val, found := getMetricWithLabels(mfs, "probe_ssl_ocsp_status_info", map[string]string{"status": test.wantStatus}); !found || val != 1 {
				t.Fatalf("status{%s} = %v (found=%v), want 1", test.wantStatus, val, found)
			}
		})
	}
}
