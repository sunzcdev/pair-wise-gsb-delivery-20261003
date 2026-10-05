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
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"

	"github.com/prometheus/client_golang/prometheus"
	pconfig "github.com/prometheus/common/config"

	"github.com/prometheus/blackbox_exporter/config"
)

func probeExpectInfo(registry *prometheus.Registry, qr *config.QueryResponse, bytes []byte, match []int) {
	var names []string
	var values []string
	for _, s := range qr.Labels {
		names = append(names, s.Name)
		values = append(values, string(qr.Expect.Expand(nil, []byte(s.Value), bytes, match)))
	}
	metric := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "probe_expect_info",
			Help: "Explicit content matched",
		},
		names,
	)
	registry.MustRegister(metric)
	metric.WithLabelValues(values...).Set(1)
}

func probeQueryResponses(ctx context.Context, target string, conn net.Conn, module config.Module, proberName string, registry *prometheus.Registry, logger *slog.Logger) bool {
	probeSSLEarliestCertExpiry := prometheus.NewGauge(sslEarliestCertExpiryGaugeOpts)
	probeSSLLastChainExpiryTimestampSeconds := prometheus.NewGauge(sslChainExpiryInTimeStampGaugeOpts)
	probeSSLLastInformation := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "probe_ssl_last_chain_info",
			Help: "Contains SSL leaf certificate information",
		},
		[]string{"fingerprint_sha256", "subject", "issuer", "subjectalternative", "serialnumber"},
	)
	probeTLSVersion := prometheus.NewGaugeVec(
		probeTLSInfoGaugeOpts,
		[]string{"version"},
	)
	probeTLSCipher := prometheus.NewGaugeVec(
		probeTLSCipherGaugeOpts,
		[]string{"cipher"},
	)
	probeFailedDueToRegex := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "probe_failed_due_to_regex",
		Help: "Indicates if probe failed due to regex",
	})
	probeFailedDueToBytes := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "probe_failed_due_to_bytes",
		Help: "Indicates if probe failed due to bytes",
	})
	registry.MustRegister(probeFailedDueToRegex)
	registry.MustRegister(probeFailedDueToBytes)

	var queryResponses []config.QueryResponse
	var tlsConfig *pconfig.TLSConfig
	var checkRevoked bool
	var useTLS bool

	switch proberName {
	case "tcp":
		queryResponses = module.TCP.QueryResponse
		tlsConfig = &module.TCP.TLSConfig
		checkRevoked = module.TCP.CheckRevoked
		useTLS = module.TCP.TLS
	case "unix":
		queryResponses = module.Unix.QueryResponse
		tlsConfig = &module.Unix.TLSConfig
		checkRevoked = module.Unix.CheckRevoked
		useTLS = module.Unix.TLS
	}

	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		logger.Error("Error setting deadline", "err", err)
		return false
	}

	if useTLS {
		state := conn.(*tls.Conn).ConnectionState()
		registry.MustRegister(probeSSLEarliestCertExpiry, probeTLSVersion, probeTLSCipher, probeSSLLastChainExpiryTimestampSeconds, probeSSLLastInformation)
		probeSSLEarliestCertExpiry.Set(float64(getEarliestCertExpiry(&state).Unix()))
		probeTLSVersion.WithLabelValues(getTLSVersion(&state)).Set(1)
		probeTLSCipher.WithLabelValues(getTLSCipher(&state)).Set(1)
		probeSSLLastChainExpiryTimestampSeconds.Set(float64(getLastChainExpiry(&state).Unix()))
		probeSSLLastInformation.WithLabelValues(getFingerprint(&state), getSubject(&state), getIssuer(&state), getDNSNames(&state), getSerialNumber(&state)).Set(1)
		checkCRL(ctx, &state, checkRevoked, nil, registry, logger)
	}

	// All reads go through a single buffered reader so that text steps
	// (line oriented) and binary frame steps consume exactly the bytes
	// belonging to each step, regardless of how the peer segments its
	// writes into packets.
	reader := bufio.NewReader(conn)
	for i, qr := range queryResponses {
		logger.Debug("Processing query response entry", "entry_number", i)
		send := qr.Send
		if qr.Expect.Regexp != nil {
			var match []int
			var line []byte
			// Read lines until one of them matches the configured regexp.
			for match == nil {
				l, err := reader.ReadBytes('\n')
				if len(l) > 0 {
					line = bytes.TrimSuffix(bytes.TrimSuffix(l, []byte("\n")), []byte("\r"))
					logger.Debug("Read line", "line", string(line))
					match = qr.Expect.FindSubmatchIndex(line)
				}
				if err != nil {
					if err == io.EOF {
						// The peer closed the connection without
						// sending a matching line.
						break
					}
					logger.Error("Error reading from connection", "err", err.Error())
					return false
				}
			}
			if match == nil {
				probeFailedDueToRegex.Set(1)
				logger.Error("Regexp did not match", "regexp", qr.Expect.Regexp, "line", string(line))
				return false
			}
			logger.Debug("Regexp matched", "regexp", qr.Expect.Regexp, "line", string(line))
			probeFailedDueToRegex.Set(0)
			send = string(qr.Expect.Expand(nil, []byte(send), line, match))
			if qr.Labels != nil {
				probeExpectInfo(registry, &qr, line, match)
			}
		}
		if qr.ExpectBytes != "" {
			expectBytes := []byte(qr.ExpectBytes)

			// Read exactly the number of bytes expected.
			data := make([]byte, len(expectBytes))
			if _, err := io.ReadFull(reader, data); err != nil {
				logger.Error("Error reading from connection", "err", err)
				return false
			}

			logger.Debug("Read bytes", "bytes", data)

			if !bytes.Equal(expectBytes, data) {
				probeFailedDueToBytes.Set(1)
				logger.Error("Bytes did not match", "expected", expectBytes, "bytes", data)
				return false
			}
			logger.Debug("Bytes matched", "expected", expectBytes, "bytes", data)
			probeFailedDueToBytes.Set(0)
		}
		if qr.Frame != nil {
			frame, err := readFrame(reader, qr.Frame)
			if err != nil {
				logger.Error("Error reading frame", "err", err)
				return false
			}
			logger.Debug("Read frame", "bytes", frame)
			if err := checkFrameFields(frame, qr.Frame.ExpectFields); err != nil {
				probeFailedDueToBytes.Set(1)
				logger.Error("Frame fields did not match", "err", err)
				return false
			}
			probeFailedDueToBytes.Set(0)
		}
		if send != "" {
			logger.Debug("Sending line", "line", send)
			if _, err := fmt.Fprintf(conn, "%s\n", send); err != nil {
				logger.Error("Failed to send", "err", err)
				return false
			}
		}
		if qr.StartTLS {
			// Upgrade TCP connection to TLS.
			tlsUpgradeConfig, err := pconfig.NewTLSConfig(tlsConfig)
			if err != nil {
				logger.Error("Failed to create TLS configuration", "err", err)
				return false
			}
			if proberName == "tcp" && tlsUpgradeConfig.ServerName == "" {
				// Use target-hostname as default for TLS-servername.
				targetAddress, _, _ := net.SplitHostPort(target) // Had succeeded in dialTCP already.
				tlsUpgradeConfig.ServerName = targetAddress
			}

			tlsConn := tls.Client(conn, tlsUpgradeConfig)
			defer tlsConn.Close()

			// Initiate TLS handshake (required here to get TLS state).
			if err := tlsConn.Handshake(); err != nil {
				logger.Error("TLS Handshake (client) failed", "err", err)
				return false
			}
			logger.Debug("TLS Handshake (client) succeeded.")
			conn = net.Conn(tlsConn)
			reader = bufio.NewReader(conn)

			// Get certificate expiry.
			state := tlsConn.ConnectionState()
			registry.MustRegister(probeSSLEarliestCertExpiry, probeTLSVersion, probeTLSCipher, probeSSLLastChainExpiryTimestampSeconds, probeSSLLastInformation)
			probeSSLEarliestCertExpiry.Set(float64(getEarliestCertExpiry(&state).Unix()))
			probeTLSVersion.WithLabelValues(getTLSVersion(&state)).Set(1)
			probeTLSCipher.WithLabelValues(getTLSCipher(&state)).Set(1)
			probeSSLLastChainExpiryTimestampSeconds.Set(float64(getLastChainExpiry(&state).Unix()))
			probeSSLLastInformation.WithLabelValues(getFingerprint(&state), getSubject(&state), getIssuer(&state), getDNSNames(&state), getSerialNumber(&state)).Set(1)
			checkCRL(ctx, &state, checkRevoked, nil, registry, logger)
		}
	}
	return true
}
