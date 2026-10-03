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
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/binary"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/promslog"

	"github.com/prometheus/blackbox_exporter/config"
)

// sctTestLog is a throwaway CT log signing key for tests.
type sctTestLog struct {
	name  string
	key   *ecdsa.PrivateKey
	logID [32]byte
}

func newSCTTestLog(t *testing.T, name string) sctTestLog {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	spki, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return sctTestLog{name: name, key: key, logID: sha256.Sum256(spki)}
}

func (l sctTestLog) pemKey(t *testing.T) string {
	t.Helper()
	spki, err := x509.MarshalPKIXPublicKey(&l.key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: spki}))
}

func (l sctTestLog) ctLog(notBefore, notAfter time.Time) ctLog {
	return ctLog{name: l.name, key: &l.key.PublicKey, logID: l.logID, notBefore: notBefore, notAfter: notAfter}
}

func newSCTTestCA(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Test CA", Organization: []string{"Example Org"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		SubjectKeyId:          []byte{1, 2, 3, 4},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, key
}

func sctTestLeafTemplate() *x509.Certificate {
	return &x509.Certificate{
		SerialNumber:          big.NewInt(100),
		Subject:               pkix.Name{CommonName: "Test Leaf", Organization: []string{"Example Org"}},
		DNSNames:              []string{"example.com"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
}

func createSCTTestCert(t *testing.T, template, parent *x509.Certificate, pub any, signer *ecdsa.PrivateKey) *x509.Certificate {
	t.Helper()
	der, err := x509.CreateCertificate(rand.Reader, template, parent, pub, signer)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

// testPrecertSignatureInput builds the RFC 6962 digitally-signed structure
// for a precertificate entry, independently of the prober implementation.
func testPrecertSignatureInput(ts time.Time, issuerKeyHash [32]byte, tbs []byte) []byte {
	var buf bytes.Buffer
	buf.WriteByte(0) // v1
	buf.WriteByte(0) // certificate_timestamp
	writeUint64(&buf, uint64(ts.UnixMilli()))
	buf.Write([]byte{0, 1}) // precert_entry
	buf.Write(issuerKeyHash[:])
	buf.Write(tbs)
	writeUint16(&buf, 0) // no extensions
	return buf.Bytes()
}

// testX509SignatureInput builds the RFC 6962 digitally-signed structure for
// an X.509 entry (used for SCTs delivered in the TLS handshake).
func testX509SignatureInput(ts time.Time, certDER []byte) []byte {
	var buf bytes.Buffer
	buf.WriteByte(0) // v1
	buf.WriteByte(0) // certificate_timestamp
	writeUint64(&buf, uint64(ts.UnixMilli()))
	buf.Write([]byte{0, 0}) // x509_entry
	buf.Write([]byte{byte(len(certDER) >> 16), byte(len(certDER) >> 8), byte(len(certDER))})
	buf.Write(certDER)
	writeUint16(&buf, 0) // no extensions
	return buf.Bytes()
}

func writeUint16(buf *bytes.Buffer, v uint16) {
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], v)
	buf.Write(b[:])
}

func writeUint64(buf *bytes.Buffer, v uint64) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	buf.Write(b[:])
}

// serializeTestSCT signs the input with the log key and serializes the SCT.
func serializeTestSCT(t *testing.T, log sctTestLog, ts time.Time, input []byte) []byte {
	t.Helper()
	digest := sha256.Sum256(input)
	sig, err := ecdsa.SignASN1(rand.Reader, log.key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	buf.WriteByte(0) // v1
	buf.Write(log.logID[:])
	writeUint64(&buf, uint64(ts.UnixMilli()))
	writeUint16(&buf, 0) // no sct extensions
	buf.WriteByte(4)     // sha256
	buf.WriteByte(3)     // ecdsa
	writeUint16(&buf, uint16(len(sig)))
	buf.Write(sig)
	return buf.Bytes()
}

// serializeTestSCTList serializes SCTs as a SerializedSCTList.
func serializeTestSCTList(scts [][]byte) []byte {
	var body bytes.Buffer
	for _, sct := range scts {
		writeUint16(&body, uint16(len(sct)))
		body.Write(sct)
	}
	var buf bytes.Buffer
	writeUint16(&buf, uint16(body.Len()))
	buf.Write(body.Bytes())
	return buf.Bytes()
}

// wrapTestSCTList DER-encodes the SCT list as the OCTET STRING carried by
// the certificate SCT extension.
func wrapTestSCTList(t *testing.T, list []byte) []byte {
	t.Helper()
	der, err := asn1.Marshal(list)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

// signEmbeddedSCT signs the precertificate input for the given template
// (which must not have the SCT extension set yet) with the log key.
func signEmbeddedSCT(t *testing.T, template *x509.Certificate, ca *x509.Certificate, caKey *ecdsa.PrivateKey, pub any, log sctTestLog, ts time.Time) []byte {
	t.Helper()
	precert := createSCTTestCert(t, template, ca, pub, caKey)
	issuerKeyHash := sha256.Sum256(ca.RawSubjectPublicKeyInfo)
	return serializeTestSCT(t, log, ts, testPrecertSignatureInput(ts, issuerKeyHash, precert.RawTBSCertificate))
}

// issueLeafWithSCTs creates the final certificate carrying the given
// serialized SCTs in its SCT extension.
func issueLeafWithSCTs(t *testing.T, template *x509.Certificate, ca *x509.Certificate, caKey *ecdsa.PrivateKey, pub any, scts [][]byte) *x509.Certificate {
	t.Helper()
	template.ExtraExtensions = []pkix.Extension{{
		Id:    oidExtensionSCT,
		Value: wrapTestSCTList(t, serializeTestSCTList(scts)),
	}}
	return createSCTTestCert(t, template, ca, pub, caKey)
}

func newSCTLeafKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestVerifyConnectionSCTs(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name         string
		setup        func(t *testing.T) (*tls.ConnectionState, []ctLog)
		want         []sctResult
		wantDistinct int
	}{
		{
			name: "embedded SCTs from two distinct logs",
			setup: func(t *testing.T) (*tls.ConnectionState, []ctLog) {
				ca, caKey := newSCTTestCA(t)
				log1 := newSCTTestLog(t, "log-1")
				log2 := newSCTTestLog(t, "log-2")
				leafKey := newSCTLeafKey(t)
				template := sctTestLeafTemplate()
				sct1 := signEmbeddedSCT(t, template, ca, caKey, &leafKey.PublicKey, log1, now)
				sct2 := signEmbeddedSCT(t, template, ca, caKey, &leafKey.PublicKey, log2, now)
				leaf := issueLeafWithSCTs(t, template, ca, caKey, &leafKey.PublicKey, [][]byte{sct1, sct2})
				state := &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf, ca}}
				return state, []ctLog{log1.ctLog(time.Time{}, time.Time{}), log2.ctLog(time.Time{}, time.Time{})}
			},
			want: []sctResult{
				{LogName: "log-1", Source: sctSourceX509Extension, Valid: true},
				{LogName: "log-2", Source: sctSourceX509Extension, Valid: true},
			},
			wantDistinct: 2,
		},
		{
			name: "SCT delivered in the TLS handshake",
			setup: func(t *testing.T) (*tls.ConnectionState, []ctLog) {
				ca, caKey := newSCTTestCA(t)
				log1 := newSCTTestLog(t, "log-1")
				leafKey := newSCTLeafKey(t)
				leaf := createSCTTestCert(t, sctTestLeafTemplate(), ca, &leafKey.PublicKey, caKey)
				sct := serializeTestSCT(t, log1, now, testX509SignatureInput(now, leaf.Raw))
				state := &tls.ConnectionState{
					PeerCertificates:            []*x509.Certificate{leaf, ca},
					SignedCertificateTimestamps: [][]byte{sct},
				}
				return state, []ctLog{log1.ctLog(time.Time{}, time.Time{})}
			},
			want: []sctResult{
				{LogName: "log-1", Source: sctSourceTLSExtension, Valid: true},
			},
			wantDistinct: 1,
		},
		{
			name: "duplicate log counts once",
			setup: func(t *testing.T) (*tls.ConnectionState, []ctLog) {
				ca, caKey := newSCTTestCA(t)
				log1 := newSCTTestLog(t, "log-1")
				leafKey := newSCTLeafKey(t)
				template := sctTestLeafTemplate()
				embedded := signEmbeddedSCT(t, template, ca, caKey, &leafKey.PublicKey, log1, now)
				leaf := issueLeafWithSCTs(t, template, ca, caKey, &leafKey.PublicKey, [][]byte{embedded})
				handshake := serializeTestSCT(t, log1, now, testX509SignatureInput(now, leaf.Raw))
				state := &tls.ConnectionState{
					PeerCertificates:            []*x509.Certificate{leaf, ca},
					SignedCertificateTimestamps: [][]byte{handshake},
				}
				return state, []ctLog{log1.ctLog(time.Time{}, time.Time{})}
			},
			want: []sctResult{
				{LogName: "log-1", Source: sctSourceX509Extension, Valid: true},
				{LogName: "log-1", Source: sctSourceTLSExtension, Valid: true},
			},
			wantDistinct: 1,
		},
		{
			name: "bad signature",
			setup: func(t *testing.T) (*tls.ConnectionState, []ctLog) {
				ca, caKey := newSCTTestCA(t)
				log1 := newSCTTestLog(t, "log-1")
				leafKey := newSCTLeafKey(t)
				template := sctTestLeafTemplate()
				sct := signEmbeddedSCT(t, template, ca, caKey, &leafKey.PublicKey, log1, now)
				sct[len(sct)-1] ^= 0xff
				leaf := issueLeafWithSCTs(t, template, ca, caKey, &leafKey.PublicKey, [][]byte{sct})
				state := &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf, ca}}
				return state, []ctLog{log1.ctLog(time.Time{}, time.Time{})}
			},
			want: []sctResult{
				{LogName: "log-1", Source: sctSourceX509Extension, Reason: "bad_signature"},
			},
			wantDistinct: 0,
		},
		{
			name: "SCT signed over the final TBS instead of the precertificate",
			setup: func(t *testing.T) (*tls.ConnectionState, []ctLog) {
				ca, caKey := newSCTTestCA(t)
				log1 := newSCTTestLog(t, "log-1")
				leafKey := newSCTLeafKey(t)
				template := sctTestLeafTemplate()
				// Obtain a TBS that includes the SCT extension, then sign
				// over it, which is what a broken log would produce.
				placeholder := serializeTestSCT(t, log1, now, []byte("placeholder"))
				draft := issueLeafWithSCTs(t, template, ca, caKey, &leafKey.PublicKey, [][]byte{placeholder})
				issuerKeyHash := sha256.Sum256(ca.RawSubjectPublicKeyInfo)
				bad := serializeTestSCT(t, log1, now, testPrecertSignatureInput(now, issuerKeyHash, draft.RawTBSCertificate))
				leaf := issueLeafWithSCTs(t, template, ca, caKey, &leafKey.PublicKey, [][]byte{bad})
				state := &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf, ca}}
				return state, []ctLog{log1.ctLog(time.Time{}, time.Time{})}
			},
			want: []sctResult{
				{LogName: "log-1", Source: sctSourceX509Extension, Reason: "bad_signature"},
			},
			wantDistinct: 0,
		},
		{
			name: "expired log",
			setup: func(t *testing.T) (*tls.ConnectionState, []ctLog) {
				ca, caKey := newSCTTestCA(t)
				log1 := newSCTTestLog(t, "log-1")
				leafKey := newSCTLeafKey(t)
				template := sctTestLeafTemplate()
				sct := signEmbeddedSCT(t, template, ca, caKey, &leafKey.PublicKey, log1, now.Add(-48*time.Hour))
				leaf := issueLeafWithSCTs(t, template, ca, caKey, &leafKey.PublicKey, [][]byte{sct})
				state := &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf, ca}}
				// The log's validity window has passed: the log list is expired.
				return state, []ctLog{log1.ctLog(now.Add(-72*time.Hour), now.Add(-time.Hour))}
			},
			want: []sctResult{
				{LogName: "log-1", Source: sctSourceX509Extension, Reason: "log_expired"},
			},
			wantDistinct: 0,
		},
		{
			name: "log not yet valid",
			setup: func(t *testing.T) (*tls.ConnectionState, []ctLog) {
				ca, caKey := newSCTTestCA(t)
				log1 := newSCTTestLog(t, "log-1")
				leafKey := newSCTLeafKey(t)
				template := sctTestLeafTemplate()
				sct := signEmbeddedSCT(t, template, ca, caKey, &leafKey.PublicKey, log1, now.Add(48*time.Hour))
				leaf := issueLeafWithSCTs(t, template, ca, caKey, &leafKey.PublicKey, [][]byte{sct})
				state := &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf, ca}}
				return state, []ctLog{log1.ctLog(now.Add(time.Hour), now.Add(72*time.Hour))}
			},
			want: []sctResult{
				{LogName: "log-1", Source: sctSourceX509Extension, Reason: "log_not_yet_valid"},
			},
			wantDistinct: 0,
		},
		{
			name: "SCT timestamp outside log validity window",
			setup: func(t *testing.T) (*tls.ConnectionState, []ctLog) {
				ca, caKey := newSCTTestCA(t)
				log1 := newSCTTestLog(t, "log-1")
				leafKey := newSCTLeafKey(t)
				template := sctTestLeafTemplate()
				sct := signEmbeddedSCT(t, template, ca, caKey, &leafKey.PublicKey, log1, now.Add(-72*time.Hour))
				leaf := issueLeafWithSCTs(t, template, ca, caKey, &leafKey.PublicKey, [][]byte{sct})
				state := &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf, ca}}
				return state, []ctLog{log1.ctLog(now.Add(-48*time.Hour), now.Add(24*time.Hour))}
			},
			want: []sctResult{
				{LogName: "log-1", Source: sctSourceX509Extension, Reason: "timestamp_outside_log_validity"},
			},
			wantDistinct: 0,
		},
		{
			name: "unknown log",
			setup: func(t *testing.T) (*tls.ConnectionState, []ctLog) {
				ca, caKey := newSCTTestCA(t)
				log1 := newSCTTestLog(t, "log-1")
				unknown := newSCTTestLog(t, "unknown")
				leafKey := newSCTLeafKey(t)
				template := sctTestLeafTemplate()
				sct := signEmbeddedSCT(t, template, ca, caKey, &leafKey.PublicKey, unknown, now)
				leaf := issueLeafWithSCTs(t, template, ca, caKey, &leafKey.PublicKey, [][]byte{sct})
				state := &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf, ca}}
				return state, []ctLog{log1.ctLog(time.Time{}, time.Time{})}
			},
			want: []sctResult{
				{Source: sctSourceX509Extension, Reason: "unknown_log"},
			},
			wantDistinct: 0,
		},
		{
			name: "issuer unavailable for embedded SCT",
			setup: func(t *testing.T) (*tls.ConnectionState, []ctLog) {
				ca, caKey := newSCTTestCA(t)
				log1 := newSCTTestLog(t, "log-1")
				leafKey := newSCTLeafKey(t)
				template := sctTestLeafTemplate()
				sct := signEmbeddedSCT(t, template, ca, caKey, &leafKey.PublicKey, log1, now)
				leaf := issueLeafWithSCTs(t, template, ca, caKey, &leafKey.PublicKey, [][]byte{sct})
				// No issuer in the chain.
				state := &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}
				return state, []ctLog{log1.ctLog(time.Time{}, time.Time{})}
			},
			want: []sctResult{
				{LogName: "log-1", Source: sctSourceX509Extension, Reason: "issuer_unavailable"},
			},
			wantDistinct: 0,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state, logs := test.setup(t)
			results := verifyConnectionSCTs(state, logs, now, promslog.NewNopLogger())

			if len(results) != len(test.want) {
				t.Fatalf("got %d results, want %d: %+v", len(results), len(test.want), results)
			}
			for i, want := range test.want {
				got := results[i]
				if got.Source != want.Source || got.Valid != want.Valid || got.Reason != want.Reason || got.LogName != want.LogName {
					t.Errorf("result %d: got %+v, want %+v", i, got, want)
				}
				if want.Valid && got.LogID == "" {
					t.Errorf("result %d: valid SCT has empty log ID", i)
				}
			}
			if distinct := countDistinctValidLogs(results); distinct != test.wantDistinct {
				t.Errorf("got %d distinct valid logs, want %d", distinct, test.wantDistinct)
			}
		})
	}
}

// TestBuildPrecertTBS checks that rebuilding the precertificate
// TBSCertificate strips exactly the CT extensions and leaves the remaining
// DER untouched.
func TestBuildPrecertTBS(t *testing.T) {
	ca, caKey := newSCTTestCA(t)
	leafKey := newSCTLeafKey(t)
	template := sctTestLeafTemplate()

	plain := createSCTTestCert(t, template, ca, &leafKey.PublicKey, caKey)

	template.ExtraExtensions = []pkix.Extension{
		{Id: oidExtensionCTPoison, Critical: true, Value: []byte{0x05, 0x00}},
		{Id: oidExtensionSCT, Value: wrapTestSCTList(t, serializeTestSCTList([][]byte{{0x01, 0x02}}))},
	}
	withCT := createSCTTestCert(t, template, ca, &leafKey.PublicKey, caKey)

	got, err := buildPrecertTBS(withCT)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plain.RawTBSCertificate) {
		t.Errorf("rebuilt precertificate TBS does not match the certificate issued without CT extensions")
	}
}

func TestParseSCTList(t *testing.T) {
	scts := [][]byte{{0x01, 0x02, 0x03}, {0x04}}
	list := serializeTestSCTList(scts)
	got, err := parseSCTList(list)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !bytes.Equal(got[0], scts[0]) || !bytes.Equal(got[1], scts[1]) {
		t.Errorf("round trip mismatch: %x", got)
	}

	for name, data := range map[string][]byte{
		"empty":           {},
		"length mismatch": {0x00, 0x05, 0x01},
		"truncated entry": {0x00, 0x03, 0x00, 0x05, 0x01},
	} {
		if _, err := parseSCTList(data); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

// findGaugeValue returns the value of the gauge with the given name and
// matching labels.
func findGaugeValue(mfs []*dto.MetricFamily, name string, wantLabels map[string]string) (float64, bool) {
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.Metric {
			match := true
			for k, v := range wantLabels {
				found := false
				for _, l := range m.GetLabel() {
					if l.GetName() == k && l.GetValue() == v {
						found = true
						break
					}
				}
				if !found {
					match = false
					break
				}
			}
			if match {
				return m.GetGauge().GetValue(), true
			}
		}
	}
	return 0, false
}

func mustGather(t *testing.T, registry *prometheus.Registry) []*dto.MetricFamily {
	t.Helper()
	mfs, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	return mfs
}

// TestCheckSCTHandshake runs a full TLS handshake against a test server that
// delivers an SCT through the TLS extension and verifies the resulting
// metrics.
func TestCheckSCTHandshake(t *testing.T) {
	now := time.Now()
	ca, caKey := newSCTTestCA(t)
	log1 := newSCTTestLog(t, "handshake-log")
	leafKey := newSCTLeafKey(t)
	leaf := createSCTTestCert(t, sctTestLeafTemplate(), ca, &leafKey.PublicKey, caKey)
	sct := serializeTestSCT(t, log1, now, testX509SignatureInput(now, leaf.Raw))

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{{
			Certificate:                 [][]byte{leaf.Raw, ca.Raw},
			PrivateKey:                  leafKey,
			SignedCertificateTimestamps: [][]byte{sct},
		}},
	}
	srv.StartTLS()
	defer srv.Close()

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.TLS == nil {
		t.Fatal("no TLS connection state")
	}
	if len(resp.TLS.SignedCertificateTimestamps) != 1 {
		t.Fatalf("expected 1 handshake SCT, got %d", len(resp.TLS.SignedCertificateTimestamps))
	}

	registry := prometheus.NewRegistry()
	cfg := config.SCTVerification{
		MinDistinctLogs: 1,
		Logs:            []config.CTLog{{Name: log1.name, Key: log1.pemKey(t)}},
	}
	checkSCT(resp.TLS, cfg, registry, promslog.NewNopLogger())

	mfs := mustGather(t, registry)
	logID := hex.EncodeToString(log1.logID[:])
	if v, ok := findGaugeValue(mfs, "probe_ssl_ct_sct_valid", map[string]string{
		"log_id": logID, "log_name": "handshake-log", "source": "tls_extension",
	}); !ok || v != 1 {
		t.Errorf("probe_ssl_ct_sct_valid for handshake SCT = %v (found %v), want 1", v, ok)
	}
	if v, ok := findGaugeValue(mfs, "probe_ssl_ct_distinct_valid_logs", nil); !ok || v != 1 {
		t.Errorf("probe_ssl_ct_distinct_valid_logs = %v (found %v), want 1", v, ok)
	}
	if v, ok := findGaugeValue(mfs, "probe_ssl_ct_min_distinct_logs_met", nil); !ok || v != 1 {
		t.Errorf("probe_ssl_ct_min_distinct_logs_met = %v (found %v), want 1", v, ok)
	}
	if v, ok := findGaugeValue(mfs, "probe_ssl_ct_verification_level", map[string]string{"level": "signature"}); !ok || v != 1 {
		t.Errorf("probe_ssl_ct_verification_level = %v (found %v), want 1", v, ok)
	}
}

// TestCheckSCTMetrics exercises checkSCT with a mix of valid and rejected
// embedded SCTs and checks the metrics, including an unmet distinct-log
// minimum.
func TestCheckSCTMetrics(t *testing.T) {
	now := time.Now()
	ca, caKey := newSCTTestCA(t)
	log1 := newSCTTestLog(t, "log-1")
	log2 := newSCTTestLog(t, "log-2")
	leafKey := newSCTLeafKey(t)
	template := sctTestLeafTemplate()

	good := signEmbeddedSCT(t, template, ca, caKey, &leafKey.PublicKey, log1, now)
	bad := signEmbeddedSCT(t, template, ca, caKey, &leafKey.PublicKey, log2, now)
	bad[len(bad)-1] ^= 0xff
	leaf := issueLeafWithSCTs(t, template, ca, caKey, &leafKey.PublicKey, [][]byte{good, bad})

	state := &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf, ca}}
	registry := prometheus.NewRegistry()
	cfg := config.SCTVerification{
		MinDistinctLogs: 2,
		Logs: []config.CTLog{
			{Name: log1.name, Key: log1.pemKey(t)},
			{Name: log2.name, Key: log2.pemKey(t)},
		},
	}
	checkSCT(state, cfg, registry, promslog.NewNopLogger())

	mfs := mustGather(t, registry)
	log1ID := hex.EncodeToString(log1.logID[:])
	log2ID := hex.EncodeToString(log2.logID[:])

	if v, ok := findGaugeValue(mfs, "probe_ssl_ct_sct_valid", map[string]string{
		"log_id": log1ID, "log_name": "log-1", "source": "x509_extension",
	}); !ok || v != 1 {
		t.Errorf("probe_ssl_ct_sct_valid for log-1 = %v (found %v), want 1", v, ok)
	}
	if v, ok := findGaugeValue(mfs, "probe_ssl_ct_sct_valid", map[string]string{
		"log_id": log2ID, "log_name": "log-2", "source": "x509_extension",
	}); !ok || v != 0 {
		t.Errorf("probe_ssl_ct_sct_valid for log-2 = %v (found %v), want 0", v, ok)
	}
	if v, ok := findGaugeValue(mfs, "probe_ssl_ct_sct_rejected", map[string]string{
		"log_id": log2ID, "log_name": "log-2", "source": "x509_extension", "reason": "bad_signature",
	}); !ok || v != 1 {
		t.Errorf("probe_ssl_ct_sct_rejected for log-2 = %v (found %v), want 1", v, ok)
	}
	if v, ok := findGaugeValue(mfs, "probe_ssl_ct_distinct_valid_logs", nil); !ok || v != 1 {
		t.Errorf("probe_ssl_ct_distinct_valid_logs = %v (found %v), want 1", v, ok)
	}
	if v, ok := findGaugeValue(mfs, "probe_ssl_ct_min_distinct_logs_met", nil); !ok || v != 0 {
		t.Errorf("probe_ssl_ct_min_distinct_logs_met = %v (found %v), want 0", v, ok)
	}
	if v, ok := findGaugeValue(mfs, "probe_ssl_ct_verification_level", map[string]string{"level": "signature"}); !ok || v != 1 {
		t.Errorf("probe_ssl_ct_verification_level = %v (found %v), want 1", v, ok)
	}
}

// TestCheckSCTDisabled makes sure no metrics are registered when SCT
// verification is not configured.
func TestCheckSCTDisabled(t *testing.T) {
	registry := prometheus.NewRegistry()
	checkSCT(&tls.ConnectionState{}, config.SCTVerification{}, registry, promslog.NewNopLogger())
	if mfs := mustGather(t, registry); len(mfs) != 0 {
		t.Errorf("expected no metrics, got %d families", len(mfs))
	}
}
