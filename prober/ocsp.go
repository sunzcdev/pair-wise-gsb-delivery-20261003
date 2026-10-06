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
	"fmt"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/crypto/ocsp"
)

// Stapled OCSP response statuses reported via the probe_ssl_ocsp_response_status
// metric's status label.
const (
	ocspStatusNotStapled = "not_stapled"
	ocspStatusGood       = "good"
	ocspStatusRevoked    = "revoked"
	ocspStatusUnknown    = "unknown"
	ocspStatusExpired    = "expired"
	ocspStatusFailed     = "failed"
)

var ocspStatuses = []string{
	ocspStatusNotStapled,
	ocspStatusGood,
	ocspStatusRevoked,
	ocspStatusUnknown,
	ocspStatusExpired,
	ocspStatusFailed,
}

// ocspStaplingResult holds the outcome of verifying the stapled OCSP response
// of a TLS connection.
type ocspStaplingResult struct {
	Stapled    bool      // the server stapled an OCSP response
	Status     string    // one of the ocspStatus* constants
	ThisUpdate time.Time // response ThisUpdate, zero when unparsable
	NextUpdate time.Time // response NextUpdate, zero when absent or unparsable
	ProducedAt time.Time // response ProducedAt, zero when unparsable
	Err        error     // non-nil when verification failed
}

// checkOCSP inspects the stapled OCSP response if enabled in the module config
// and registers the metrics. It only looks at the response stapled during the
// TLS handshake; it never queries the OCSP responder URLs from the certificate.
// The result is returned so callers can enforce require_ocsp_stapling.
func checkOCSP(state *tls.ConnectionState, enabled bool, registry *prometheus.Registry, logger *slog.Logger) ocspStaplingResult {
	if !enabled {
		return ocspStaplingResult{}
	}
	result := checkStapledOCSP(state, logger)
	registerOCSPMetrics(registry, result)
	return result
}

// checkStapledOCSP verifies the stapled OCSP response against the peer's leaf
// certificate: the response must match the leaf's serial number, carry a valid
// signature from the leaf's issuer, and be within its validity window.
func checkStapledOCSP(state *tls.ConnectionState, logger *slog.Logger) ocspStaplingResult {
	result := ocspStaplingResult{Status: ocspStatusNotStapled}

	if len(state.PeerCertificates) == 0 {
		return result
	}
	leaf := state.PeerCertificates[0]

	if len(state.OCSPResponse) == 0 {
		logger.Debug("Server did not staple an OCSP response", "subject", leaf.Subject.String())
		return result
	}
	result.Stapled = true

	// The issuer is needed to verify the response signature; it is the next
	// certificate in the presented chain.
	if len(state.PeerCertificates) < 2 {
		result.Status = ocspStatusFailed
		result.Err = fmt.Errorf("certificate chain has no issuer for the leaf certificate")
		logger.Warn("Cannot verify stapled OCSP response without the issuer certificate", "subject", leaf.Subject.String())
		return result
	}
	issuer := state.PeerCertificates[1]

	// ParseResponseForCert checks that the response covers the leaf's serial
	// number and verifies the response signature against the issuer.
	resp, err := ocsp.ParseResponseForCert(state.OCSPResponse, leaf, issuer)
	if err != nil {
		result.Status = ocspStatusFailed
		result.Err = fmt.Errorf("parsing stapled OCSP response: %w", err)
		logger.Warn("Stapled OCSP response verification failed", "subject", leaf.Subject.String(), "err", err)
		return result
	}

	result.ThisUpdate = resp.ThisUpdate
	result.NextUpdate = resp.NextUpdate
	result.ProducedAt = resp.ProducedAt

	now := time.Now()
	if !resp.ThisUpdate.IsZero() && resp.ThisUpdate.After(now) {
		result.Status = ocspStatusFailed
		result.Err = fmt.Errorf("stapled OCSP response is not yet valid: thisUpdate %s is in the future", resp.ThisUpdate)
		logger.Warn("Stapled OCSP response is not yet valid", "subject", leaf.Subject.String(), "this_update", resp.ThisUpdate)
		return result
	}
	if !resp.NextUpdate.IsZero() && resp.NextUpdate.Before(now) {
		result.Status = ocspStatusExpired
		logger.Warn("Stapled OCSP response is expired", "subject", leaf.Subject.String(), "next_update", resp.NextUpdate)
		return result
	}

	switch resp.Status {
	case ocsp.Good:
		result.Status = ocspStatusGood
	case ocsp.Revoked:
		result.Status = ocspStatusRevoked
		logger.Warn("Certificate is revoked according to the stapled OCSP response", "subject", leaf.Subject.String(), "revoked_at", resp.RevokedAt)
	case ocsp.Unknown:
		result.Status = ocspStatusUnknown
		logger.Warn("Stapled OCSP response reports the certificate status as unknown", "subject", leaf.Subject.String())
	default:
		result.Status = ocspStatusFailed
		result.Err = fmt.Errorf("stapled OCSP response has unrecognized status %d", resp.Status)
	}

	return result
}

// registerOCSPMetrics registers the OCSP stapling metrics and populates them
// from the check result. Labels are limited to the response status so that
// neither certificate contents nor responder URLs end up in the label set.
func registerOCSPMetrics(registry *prometheus.Registry, result ocspStaplingResult) {
	ocspStapled := prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "probe_ssl_ocsp_stapled",
			Help: "Whether the server stapled an OCSP response to the TLS handshake (0/1)",
		},
	)
	ocspResponseStatus := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "probe_ssl_ocsp_response_status",
			Help: "Status of the stapled OCSP response (1 for the current status, 0 otherwise)",
		},
		[]string{"status"},
	)
	ocspThisUpdate := prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "probe_ssl_ocsp_this_update_timestamp_seconds",
			Help: "The thisUpdate field of the stapled OCSP response as a Unix timestamp in seconds",
		},
	)
	ocspNextUpdate := prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "probe_ssl_ocsp_next_update_timestamp_seconds",
			Help: "The nextUpdate field of the stapled OCSP response as a Unix timestamp in seconds",
		},
	)
	ocspProducedAt := prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "probe_ssl_ocsp_produced_at_timestamp_seconds",
			Help: "The producedAt field of the stapled OCSP response as a Unix timestamp in seconds",
		},
	)

	registry.MustRegister(ocspStapled, ocspResponseStatus, ocspThisUpdate, ocspNextUpdate, ocspProducedAt)

	if result.Stapled {
		ocspStapled.Set(1)
	}
	for _, status := range ocspStatuses {
		if status == result.Status {
			ocspResponseStatus.WithLabelValues(status).Set(1)
		} else {
			ocspResponseStatus.WithLabelValues(status).Set(0)
		}
	}
	if !result.ThisUpdate.IsZero() {
		ocspThisUpdate.Set(float64(result.ThisUpdate.Unix()))
	}
	if !result.NextUpdate.IsZero() {
		ocspNextUpdate.Set(float64(result.NextUpdate.Unix()))
	}
	if !result.ProducedAt.IsZero() {
		ocspProducedAt.Set(float64(result.ProducedAt.Unix()))
	}
}
