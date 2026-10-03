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
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/prometheus/blackbox_exporter/config"
)

// RFC 6962 (Certificate Transparency v1) extension OIDs.
var (
	// oidExtensionSCT is the X.509v3 extension carrying the SCT list
	// embedded in a certificate.
	oidExtensionSCT = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 11129, 2, 4, 2}
	// oidExtensionCTPoison is the critical poison extension present in
	// precertificates. It is stripped when rebuilding the precertificate
	// TBSCertificate for signature verification.
	oidExtensionCTPoison = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 11129, 2, 4, 3}
)

// SCT sources, used as the "source" label value on metrics so it is visible
// at which layer an SCT was delivered.
const (
	sctSourceX509Extension = "x509_extension"
	sctSourceTLSExtension  = "tls_extension"
)

// TLS 1.2 hash/signature algorithm identifiers used in DigitallySigned
// (RFC 5246 section 7.4.1.4.1), as embedded in SCTs.
const (
	sctHashSHA256 = 4
	sctHashSHA384 = 5
	sctHashSHA512 = 6

	sctSigRSA   = 1
	sctSigECDSA = 3
)

// ctLog is a configured CT log with its parsed key and derived log ID.
type ctLog struct {
	name      string
	key       crypto.PublicKey
	logID     [32]byte
	notBefore time.Time
	notAfter  time.Time
}

// sctResult holds the verification outcome of a single SCT.
type sctResult struct {
	LogID   string // hex-encoded log ID from the SCT, empty if unparseable
	LogName string // configured log name, empty if the log is unknown
	Source  string // sctSourceX509Extension or sctSourceTLSExtension
	Valid   bool
	Reason  string // rejection reason when Valid is false
}

// signedCertificateTimestamp is a parsed RFC 6962 SignedCertificateTimestamp.
type signedCertificateTimestamp struct {
	logID      [32]byte
	timestamp  uint64 // milliseconds since the Unix epoch
	extensions []byte
	hashAlgo   uint8
	sigAlgo    uint8
	signature  []byte
}

// parseSCT parses a single serialized SCT (RFC 6962 section 3.2).
func parseSCT(data []byte) (*signedCertificateTimestamp, error) {
	// version(1) || log_id(32) || timestamp(8) || extensions<2..2^16-1> ||
	// hash_algo(1) || sig_algo(1) || signature<2..2^16-1>
	if len(data) < 1+32+8+2+2+2 {
		return nil, errors.New("SCT too short")
	}
	if data[0] != 0 {
		return nil, fmt.Errorf("unsupported SCT version %d", data[0])
	}
	sct := &signedCertificateTimestamp{}
	copy(sct.logID[:], data[1:33])
	sct.timestamp = binary.BigEndian.Uint64(data[33:41])
	rest := data[41:]

	extLen := int(binary.BigEndian.Uint16(rest))
	rest = rest[2:]
	if len(rest) < extLen+4 {
		return nil, errors.New("malformed SCT extensions")
	}
	sct.extensions = rest[:extLen]
	rest = rest[extLen:]

	sct.hashAlgo = rest[0]
	sct.sigAlgo = rest[1]
	sigLen := int(binary.BigEndian.Uint16(rest[2:]))
	rest = rest[4:]
	if len(rest) != sigLen {
		return nil, errors.New("malformed SCT signature")
	}
	sct.signature = rest
	return sct, nil
}

// parseSCTList parses a SerializedSCTList as carried by the SCT X.509
// extension (RFC 6962 section 3.3) and returns the individual SCTs.
func parseSCTList(data []byte) ([][]byte, error) {
	if len(data) < 2 {
		return nil, errors.New("SCT list too short")
	}
	total := int(binary.BigEndian.Uint16(data))
	data = data[2:]
	if len(data) != total {
		return nil, fmt.Errorf("SCT list length mismatch: header says %d bytes, got %d", total, len(data))
	}
	var scts [][]byte
	for len(data) > 0 {
		if len(data) < 2 {
			return nil, errors.New("truncated SCT list entry")
		}
		n := int(binary.BigEndian.Uint16(data))
		data = data[2:]
		if len(data) < n {
			return nil, errors.New("truncated SCT list entry")
		}
		scts = append(scts, data[:n])
		data = data[n:]
	}
	return scts, nil
}

// tbsCertificate mirrors the TBSCertificate ASN.1 structure closely enough to
// strip the CT extensions and re-encode it for precertificate signature
// verification. RawValue fields keep the original DER encoding of the
// components that are not inspected.
type tbsCertificate struct {
	Raw                asn1.RawContent
	Version            int `asn1:"optional,explicit,tag:0,default:0"`
	SerialNumber       *big.Int
	SignatureAlgorithm asn1.RawValue
	Issuer             asn1.RawValue
	Validity           asn1.RawValue
	Subject            asn1.RawValue
	PublicKey          asn1.RawValue
	UniqueID           asn1.BitString   `asn1:"optional,tag:1"`
	SubjectUniqueID    asn1.BitString   `asn1:"optional,tag:2"`
	Extensions         []pkix.Extension `asn1:"optional,explicit,tag:3"`
}

// buildPrecertTBS reconstructs the TBSCertificate of the precertificate that
// an embedded SCT was issued for: the TBSCertificate of the final certificate
// with the SCT list extension (and the CT poison extension, should it be
// present) removed. See RFC 6962 section 3.2.
func buildPrecertTBS(cert *x509.Certificate) ([]byte, error) {
	var tbs tbsCertificate
	rest, err := asn1.Unmarshal(cert.RawTBSCertificate, &tbs)
	if err != nil {
		return nil, fmt.Errorf("parsing TBSCertificate: %w", err)
	}
	if len(rest) > 0 {
		return nil, errors.New("trailing data after TBSCertificate")
	}
	var extensions []pkix.Extension
	for _, ext := range tbs.Extensions {
		if ext.Id.Equal(oidExtensionSCT) || ext.Id.Equal(oidExtensionCTPoison) {
			continue
		}
		extensions = append(extensions, ext)
	}
	tbs.Extensions = extensions
	// A non-empty RawContent would make asn1.Marshal emit the original bytes
	// instead of re-encoding the modified structure.
	tbs.Raw = nil
	raw, err := asn1.Marshal(tbs)
	if err != nil {
		return nil, fmt.Errorf("re-encoding TBSCertificate: %w", err)
	}
	return raw, nil
}

// sctTimestamp returns the SCT timestamp as a time.Time.
func (sct *signedCertificateTimestamp) sctTimestamp() time.Time {
	return time.UnixMilli(int64(sct.timestamp))
}

// signatureInput builds the digitally-signed structure the SCT signature
// covers (RFC 6962 section 3.2). For SCTs embedded in the certificate the
// entry is the precertificate (issuer key hash plus the rebuilt
// precertificate TBSCertificate); for SCTs delivered in the TLS handshake it
// is the final certificate itself.
func (sct *signedCertificateTimestamp) signatureInput(leaf *x509.Certificate, issuer *x509.Certificate, embedded bool) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte(0) // v1
	buf.WriteByte(0) // signature_type = certificate_timestamp
	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], sct.timestamp)
	buf.Write(ts[:])

	if embedded {
		if issuer == nil {
			return nil, errors.New("issuer certificate unavailable")
		}
		tbs, err := buildPrecertTBS(leaf)
		if err != nil {
			return nil, err
		}
		issuerKeyHash := sha256.Sum256(issuer.RawSubjectPublicKeyInfo)
		buf.Write([]byte{0, 1}) // entry_type = precert_entry
		buf.Write(issuerKeyHash[:])
		buf.Write(tbs)
	} else {
		buf.Write([]byte{0, 0}) // entry_type = x509_entry
		if len(leaf.Raw) > 0xFFFFFF {
			return nil, errors.New("certificate too large")
		}
		buf.Write([]byte{byte(len(leaf.Raw) >> 16), byte(len(leaf.Raw) >> 8), byte(len(leaf.Raw))})
		buf.Write(leaf.Raw)
	}

	var extLen [2]byte
	binary.BigEndian.PutUint16(extLen[:], uint16(len(sct.extensions)))
	buf.Write(extLen[:])
	buf.Write(sct.extensions)
	return buf.Bytes(), nil
}

// verifySignature verifies the SCT signature over the given input against
// the log's public key.
func (sct *signedCertificateTimestamp) verifySignature(input []byte, key crypto.PublicKey) error {
	var hash crypto.Hash
	switch sct.hashAlgo {
	case sctHashSHA256:
		hash = crypto.SHA256
	case sctHashSHA384:
		hash = crypto.SHA384
	case sctHashSHA512:
		hash = crypto.SHA512
	default:
		return fmt.Errorf("unsupported hash algorithm %d", sct.hashAlgo)
	}
	h := hash.New()
	h.Write(input)
	digest := h.Sum(nil)

	switch pub := key.(type) {
	case *ecdsa.PublicKey:
		if sct.sigAlgo != sctSigECDSA {
			return fmt.Errorf("signature algorithm %d does not match ECDSA log key", sct.sigAlgo)
		}
		if !ecdsa.VerifyASN1(pub, digest, sct.signature) {
			return errors.New("invalid ECDSA signature")
		}
	case *rsa.PublicKey:
		if sct.sigAlgo != sctSigRSA {
			return fmt.Errorf("signature algorithm %d does not match RSA log key", sct.sigAlgo)
		}
		if err := rsa.VerifyPKCS1v15(pub, hash, digest, sct.signature); err != nil {
			return fmt.Errorf("invalid RSA signature: %w", err)
		}
	default:
		return fmt.Errorf("unsupported log key type %T", key)
	}
	return nil
}

// verifySCT verifies a single serialized SCT against the configured logs.
func verifySCT(raw []byte, source string, leaf, issuer *x509.Certificate, logs []ctLog, now time.Time) sctResult {
	result := sctResult{Source: source}

	sct, err := parseSCT(raw)
	if err != nil {
		result.Reason = "malformed_sct"
		return result
	}
	result.LogID = hex.EncodeToString(sct.logID[:])

	var log *ctLog
	for i := range logs {
		if logs[i].logID == sct.logID {
			log = &logs[i]
			break
		}
	}
	if log == nil {
		result.Reason = "unknown_log"
		return result
	}
	result.LogName = log.name

	// A log whose validity window has passed (or not started) cannot vouch
	// for anything anymore; this catches an expired log list.
	if !log.notAfter.IsZero() && now.After(log.notAfter) {
		result.Reason = "log_expired"
		return result
	}
	if !log.notBefore.IsZero() && now.Before(log.notBefore) {
		result.Reason = "log_not_yet_valid"
		return result
	}
	// The SCT timestamp must fall inside the log's validity window.
	if ts := sct.sctTimestamp(); (!log.notBefore.IsZero() && ts.Before(log.notBefore)) ||
		(!log.notAfter.IsZero() && ts.After(log.notAfter)) {
		result.Reason = "timestamp_outside_log_validity"
		return result
	}

	if source == sctSourceX509Extension && issuer == nil {
		result.Reason = "issuer_unavailable"
		return result
	}
	input, err := sct.signatureInput(leaf, issuer, source == sctSourceX509Extension)
	if err != nil {
		result.Reason = "invalid_entry"
		return result
	}
	if err := sct.verifySignature(input, log.key); err != nil {
		result.Reason = "bad_signature"
		return result
	}
	result.Valid = true
	return result
}

// verifyConnectionSCTs collects the SCTs presented for the leaf certificate,
// both embedded in the X.509 SCT extension and delivered through the TLS
// handshake, and verifies each of them against the configured logs.
func verifyConnectionSCTs(state *tls.ConnectionState, logs []ctLog, now time.Time, logger *slog.Logger) []sctResult {
	if len(state.PeerCertificates) == 0 {
		return nil
	}
	leaf := state.PeerCertificates[0]

	// The issuer is needed to rebuild the precertificate for embedded SCTs.
	var issuer *x509.Certificate
	if len(state.VerifiedChains) > 0 && len(state.VerifiedChains[0]) > 1 {
		issuer = state.VerifiedChains[0][1]
	} else if len(state.PeerCertificates) > 1 {
		issuer = state.PeerCertificates[1]
	}

	var results []sctResult

	for _, ext := range leaf.Extensions {
		if !ext.Id.Equal(oidExtensionSCT) {
			continue
		}
		// The extension value is an OCTET STRING wrapping the serialized
		// SCT list (RFC 6962 section 3.3).
		var list []byte
		rest, err := asn1.Unmarshal(ext.Value, &list)
		if err != nil || len(rest) > 0 {
			logger.Warn("Failed to unwrap embedded SCT list", "err", err)
			results = append(results, sctResult{Source: sctSourceX509Extension, Reason: "malformed_sct"})
			continue
		}
		scts, err := parseSCTList(list)
		if err != nil {
			logger.Warn("Failed to parse embedded SCT list", "err", err)
			results = append(results, sctResult{Source: sctSourceX509Extension, Reason: "malformed_sct"})
			continue
		}
		for _, raw := range scts {
			results = append(results, verifySCT(raw, sctSourceX509Extension, leaf, issuer, logs, now))
		}
	}

	for _, raw := range state.SignedCertificateTimestamps {
		results = append(results, verifySCT(raw, sctSourceTLSExtension, leaf, nil, logs, now))
	}

	return results
}

// countDistinctValidLogs returns the number of distinct logs that produced at
// least one valid SCT.
func countDistinctValidLogs(results []sctResult) int {
	seen := map[string]struct{}{}
	for _, r := range results {
		if r.Valid {
			seen[r.LogID] = struct{}{}
		}
	}
	return len(seen)
}

// checkSCT runs SCT verification if enabled in the module config and
// registers the metrics. Only SCT signatures are verified against the
// configured CT logs; inclusion proofs are not checked.
func checkSCT(state *tls.ConnectionState, cfg config.SCTVerification, registry *prometheus.Registry, logger *slog.Logger) {
	if !cfg.Enabled() {
		return
	}

	logs := make([]ctLog, 0, len(cfg.Logs))
	for _, l := range cfg.Logs {
		key, logID, err := config.ParseCTLogKey(l.Key)
		if err != nil {
			// Keys are validated when the configuration is loaded, so this
			// should never happen.
			logger.Error("Failed to parse CT log key", "log", l.Name, "err", err)
			continue
		}
		logs = append(logs, ctLog{
			name:      l.Name,
			key:       key,
			logID:     logID,
			notBefore: l.NotBefore,
			notAfter:  l.NotAfter,
		})
	}

	results := verifyConnectionSCTs(state, logs, time.Now(), logger)
	for _, r := range results {
		if r.Valid {
			logger.Debug("SCT verified", "log", r.LogName, "log_id", r.LogID, "source", r.Source)
		} else {
			logger.Warn("SCT rejected", "log", r.LogName, "log_id", r.LogID, "source", r.Source, "reason", r.Reason)
		}
	}
	registerSCTMetrics(registry, results, cfg.MinDistinctLogs)
}

// registerSCTMetrics registers SCT verification metrics and populates them
// from the check results.
func registerSCTMetrics(registry *prometheus.Registry, results []sctResult, minDistinctLogs int) {
	sctLabels := []string{"log_id", "log_name", "source"}

	sctValid := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "probe_ssl_ct_sct_valid",
			Help: "Whether the SCT signature verified against a configured, currently valid CT log (0/1)",
		},
		sctLabels,
	)
	sctRejected := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "probe_ssl_ct_sct_rejected",
			Help: "SCTs that failed verification, labeled with the rejection reason",
		},
		[]string{"log_id", "log_name", "source", "reason"},
	)
	distinctValidLogs := prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "probe_ssl_ct_distinct_valid_logs",
			Help: "Number of distinct CT logs that provided at least one valid SCT",
		},
	)
	minDistinctLogsMet := prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "probe_ssl_ct_min_distinct_logs_met",
			Help: "Whether the number of distinct CT logs with valid SCTs meets the configured minimum (0/1)",
		},
	)
	verificationLevel := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "probe_ssl_ct_verification_level",
			Help: "Depth of CT verification performed: 'signature' means SCT signatures were verified against configured logs; inclusion proofs are not checked",
		},
		[]string{"level"},
	)

	registry.MustRegister(sctValid, sctRejected, distinctValidLogs, minDistinctLogsMet, verificationLevel)

	for _, r := range results {
		if r.Valid {
			sctValid.WithLabelValues(r.LogID, r.LogName, r.Source).Set(1)
		} else {
			sctValid.WithLabelValues(r.LogID, r.LogName, r.Source).Set(0)
			sctRejected.WithLabelValues(r.LogID, r.LogName, r.Source, r.Reason).Set(1)
		}
	}

	distinct := countDistinctValidLogs(results)
	distinctValidLogs.Set(float64(distinct))
	if distinct >= minDistinctLogs {
		minDistinctLogsMet.Set(1)
	} else {
		minDistinctLogsMet.Set(0)
	}
	verificationLevel.WithLabelValues("signature").Set(1)
}
