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
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"time"

	"golang.org/x/crypto/ocsp"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/prometheus/blackbox_exporter/config"
)

// Stapled OCSP status values reported in the probe_ssl_ocsp_status_info
// "status" label. They deliberately do not carry certificates or the
// responder URL, which can be sensitive and has unbounded cardinality.
const (
	// ocspStatusNotProvided: the server did not staple a response.
	ocspStatusNotProvided = "not_provided"
	// ocspStatusGood: a valid response certifies the certificate as good.
	ocspStatusGood = "good"
	// ocspStatusRevoked: a valid response certifies the certificate as revoked.
	ocspStatusRevoked = "revoked"
	// ocspStatusUnknown: a valid response reports an unknown certificate status.
	ocspStatusUnknown = "unknown"
	// ocspStatusExpired: the response was parsed and verified but is outside
	// its thisUpdate/nextUpdate validity window.
	ocspStatusExpired = "expired"
	// ocspStatusFailed: the response could not be parsed or verified against
	// the current certificate (wrong serial/issuer, bad signature, ...).
	ocspStatusFailed = "failed"
)

// ocspStatuses is the fixed set of values exported by the status metric, so
// every probe produces the same time series regardless of outcome.
var ocspStatuses = []string{
	ocspStatusNotProvided,
	ocspStatusGood,
	ocspStatusRevoked,
	ocspStatusUnknown,
	ocspStatusExpired,
	ocspStatusFailed,
}

// ocspResult is the outcome of inspecting the stapled OCSP response.
type ocspResult struct {
	status     string
	reason     string    // human-readable detail, logged only (never a label)
	thisUpdate time.Time // zero when no usable response was parsed
	nextUpdate time.Time
	revokedAt  time.Time // zero unless the certificate is revoked
}

// ocspValid is true when the response is acceptable for a "required" probe:
// current and cryptographically valid, with a good certificate status.
func (r ocspResult) ocspValid() bool {
	return r.status == ocspStatusGood
}

// evaluateStapledOCSP inspects the OCSP response stapled to the TLS
// handshake. It performs no network I/O: the responder URL from the
// certificate's AIA extension is never contacted.
//
// The response is verified against the presented leaf certificate and its
// issuer, which checks the serial, the issuer name/key hashes (CertID), the
// responder signature, and that the response time fields cover "now".
func evaluateStapledOCSP(state *tls.ConnectionState, now time.Time, logger *slog.Logger) ocspResult {
	if state == nil || len(state.PeerCertificates) == 0 {
		return ocspResult{status: ocspStatusNotProvided}
	}

	raw := state.OCSPResponse
	if len(raw) == 0 {
		return ocspResult{status: ocspStatusNotProvided}
	}

	cert := state.PeerCertificates[0]
	issuer := ocspIssuer(state, cert)
	if issuer == nil {
		return ocspResult{status: ocspStatusFailed, reason: "no issuer certificate available to verify the stapled OCSP response"}
	}

	// ParseResponseForCert verifies the responder signature (against the
	// issuer, or an embedded, issuer-signed delegated responder cert), and
	// rejects responses whose CertID serial does not match the leaf.
	response, err := ocsp.ParseResponseForCert(raw, cert, issuer)
	if err != nil {
		reason := fmt.Sprintf("stapled OCSP response failed verification: %s", err)
		logger.Warn(reason, "subject", cert.Subject.String())
		return ocspResult{status: ocspStatusFailed, reason: reason}
	}

	// ParseResponseForCert does not compare the CertID issuer name/key
	// hashes, so enforce them explicitly. This ties the response to the
	// actual issuer rather than to any cert with the same serial.
	if err := verifyOCSPIssuer(raw, cert, issuer, response.IssuerHash); err != nil {
		reason := fmt.Sprintf("stapled OCSP response issuer mismatch: %s", err)
		logger.Warn(reason, "subject", cert.Subject.String())
		return ocspResult{status: ocspStatusFailed, reason: reason}
	}

	result := ocspResult{
		thisUpdate: response.ThisUpdate,
		nextUpdate: response.NextUpdate,
		revokedAt:  response.RevokedAt,
	}

	if !response.ThisUpdate.IsZero() && now.Before(response.ThisUpdate) {
		reason := fmt.Sprintf("stapled OCSP response is not yet valid (thisUpdate %s)", response.ThisUpdate)
		logger.Warn(reason, "subject", cert.Subject.String())
		result.status = ocspStatusExpired
		result.reason = reason
		return result
	}
	if !response.NextUpdate.IsZero() && now.After(response.NextUpdate) {
		reason := fmt.Sprintf("stapled OCSP response is expired (nextUpdate %s)", response.NextUpdate)
		logger.Warn(reason, "subject", cert.Subject.String())
		result.status = ocspStatusExpired
		result.reason = reason
		return result
	}

	switch response.Status {
	case ocsp.Good:
		result.status = ocspStatusGood
	case ocsp.Revoked:
		result.status = ocspStatusRevoked
		logger.Warn("Stapled OCSP response reports the certificate as revoked",
			"subject", cert.Subject.String(), "revoked_at", response.RevokedAt)
	case ocsp.Unknown:
		result.status = ocspStatusUnknown
	default:
		result.status = ocspStatusFailed
		result.reason = fmt.Sprintf("stapled OCSP response carries an unrecognized certificate status %d", response.Status)
	}
	return result
}

// ocspIssuer returns the certificate to verify the stapled response against:
// the issuing certificate from the presented chain, falling back to the leaf
// itself for a self-signed certificate.
func ocspIssuer(state *tls.ConnectionState, leaf *x509.Certificate) *x509.Certificate {
	if len(state.PeerCertificates) > 1 {
		return state.PeerCertificates[1]
	}
	if len(state.VerifiedChains) > 0 {
		chain := state.VerifiedChains[0]
		if len(chain) > 1 {
			return chain[1]
		}
	}
	if leaf.Subject.String() == leaf.Issuer.String() {
		return leaf
	}
	return nil
}

// checkOCSPStapling evaluates the stapled OCSP response when enabled,
// registers the metrics, and reports whether the probe may be considered
// successful. When Required is false probe success is never affected; when
// it is true only a current good response passes.
func checkOCSPStapling(state *tls.ConnectionState, cfg config.OCSPStaplingConfig, registry *prometheus.Registry, logger *slog.Logger) bool {
	if !cfg.Enabled {
		return true
	}

	result := evaluateStapledOCSP(state, time.Now(), logger)
	registerOCSPMetrics(registry, result)

	if cfg.Required && !result.ocspValid() {
		logger.Error("Required stapled OCSP response is missing or invalid",
			"status", result.status, "reason", result.reason)
		return false
	}
	return true
}

// registerOCSPMetrics registers the stapled OCSP metrics. Labels are limited
// to the fixed status vocabulary: no certificate data and no responder URL.
func registerOCSPMetrics(registry *prometheus.Registry, result ocspResult) {
	statusInfo := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "probe_ssl_ocsp_status_info",
			Help: "Stapled OCSP response status; exactly one status is 1. Statuses: not_provided, good, revoked, unknown, expired, failed",
		},
		[]string{"status"},
	)
	thisUpdate := prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "probe_ssl_ocsp_this_update_timestamp_seconds",
			Help: "thisUpdate of the stapled OCSP response as a Unix timestamp in seconds; 0 when absent or unverifiable",
		},
	)
	nextUpdate := prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "probe_ssl_ocsp_next_update_timestamp_seconds",
			Help: "nextUpdate of the stapled OCSP response as a Unix timestamp in seconds; 0 when absent or unverifiable",
		},
	)
	revokedAt := prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "probe_ssl_ocsp_revoked_at_timestamp_seconds",
			Help: "Revocation time reported by the stapled OCSP response as a Unix timestamp in seconds; 0 unless revoked",
		},
	)
	registry.MustRegister(statusInfo, thisUpdate, nextUpdate, revokedAt)

	for _, status := range ocspStatuses {
		value := 0.0
		if status == result.status {
			value = 1
		}
		statusInfo.WithLabelValues(status).Set(value)
	}

	if !result.thisUpdate.IsZero() {
		thisUpdate.Set(float64(result.thisUpdate.Unix()))
	}
	if !result.nextUpdate.IsZero() {
		nextUpdate.Set(float64(result.nextUpdate.Unix()))
	}
	if !result.revokedAt.IsZero() {
		revokedAt.Set(float64(result.revokedAt.Unix()))
	}
}
