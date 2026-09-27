// Copyright 2026 The Prometheus Authors
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
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	pconfig "github.com/prometheus/common/config"
	"github.com/prometheus/common/promslog"

	"github.com/prometheus/blackbox_exporter/config"
)

// fixedLookup returns an addressLookupFunc resolving to a fixed candidate
// set, mimicking a hostname with multiple A/AAAA records.
func fixedLookup(ips ...string) addressLookupFunc {
	return func(_ context.Context, _ string) ([]net.IPAddr, error) {
		addrs := make([]net.IPAddr, 0, len(ips))
		for _, ip := range ips {
			addrs = append(addrs, net.IPAddr{IP: net.ParseIP(ip)})
		}
		return addrs, nil
	}
}

func multiAddressModule(strategy string, minRatio float64, maxAddresses, maxConcurrency int) config.Module {
	return config.Module{
		Timeout: 10 * time.Second,
		HTTP: config.HTTPProbe{
			IPProtocol:         "ip4",
			IPProtocolFallback: true,
			MultiAddress: config.MultiAddressProbe{
				Enabled:         true,
				Strategy:        strategy,
				MinSuccessRatio: minRatio,
				MaxAddresses:    maxAddresses,
				MaxConcurrency:  maxConcurrency,
			},
		},
	}
}

// addressResultCounts collects the aggregate probe_http_addresses{result}
// metrics from the registry.
func addressResultCounts(t *testing.T, registry *prometheus.Registry) map[string]float64 {
	t.Helper()
	mfs, err := registry.Gather()
	if err != nil {
		t.Fatalf("Error gathering metrics: %v", err)
	}
	counts := map[string]float64{}
	for _, mf := range mfs {
		if mf.GetName() != "probe_http_addresses" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "result" {
					counts[l.GetValue()] = m.GetGauge().GetValue()
				}
			}
		}
	}
	return counts
}

func gaugeValue(t *testing.T, registry *prometheus.Registry, name string) (float64, bool) {
	t.Helper()
	mfs, err := registry.Gather()
	if err != nil {
		t.Fatalf("Error gathering metrics: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() == name {
			return mf.GetMetric()[0].GetGauge().GetValue(), true
		}
	}
	return 0, false
}

func serverPort(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("Error parsing test server URL: %v", err)
	}
	_, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("Error splitting test server host: %v", err)
	}
	return port
}

// TestMultiAddressAllStrategy probes one good (127.0.0.1) and one bad
// (127.0.0.2, nothing listening) address of the same fake hostname. The
// "all" strategy must fail the probe, the good address must be reached with
// the original Host header, and the aggregate metrics must reflect both
// outcomes.
func TestMultiAddressAllStrategy(t *testing.T) {
	var mu sync.Mutex
	var gotHost, gotLocalAddr string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotHost = r.Host
		if addr, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
			gotLocalAddr = addr.String()
		}
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	port := serverPort(t, ts.URL)

	module := multiAddressModule(config.MultiAddressStrategyAll, 0, 16, 4)
	registry := prometheus.NewRegistry()
	target := fmt.Sprintf("http://example.com:%s", port)

	ok := probeHTTPMultiAddress(context.Background(), target, module, registry,
		promslog.NewNopLogger(), fixedLookup("127.0.0.1", "127.0.0.2"))
	if ok {
		t.Fatalf("Expected probe with strategy=all and one bad address to fail")
	}

	counts := addressResultCounts(t, registry)
	if counts["succeeded"] != 1 || counts["failed"] != 1 || counts["untested"] != 0 {
		t.Fatalf("Unexpected address counts: %v", counts)
	}

	mu.Lock()
	defer mu.Unlock()
	if want := "example.com:" + port; gotHost != want {
		t.Fatalf("Expected Host header %q, got %q", want, gotHost)
	}
	if host, _, err := net.SplitHostPort(gotLocalAddr); err != nil || host != "127.0.0.1" {
		t.Fatalf("Expected connection to 127.0.0.1, got local addr %q (err: %v)", gotLocalAddr, err)
	}
}

// TestMultiAddressAnyStrategyUntested uses one good address and one address
// that accepts connections but never responds. With the "any" strategy the
// probe succeeds as soon as the good address answers; the hanging address
// is cancelled and must be reported as untested, not as a success.
func TestMultiAddressAnyStrategyUntested(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	port := serverPort(t, ts.URL)

	// Hanging server on the same port but a different loopback address.
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.2", port))
	if err != nil {
		t.Fatalf("Error starting hanging listener: %v", err)
	}
	defer ln.Close()
	var connsMu sync.Mutex
	var conns []net.Conn
	defer func() {
		connsMu.Lock()
		defer connsMu.Unlock()
		for _, c := range conns {
			c.Close()
		}
	}()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			connsMu.Lock()
			conns = append(conns, conn)
			connsMu.Unlock()
			// Never respond; the connection is closed at test cleanup.
		}
	}()

	module := multiAddressModule(config.MultiAddressStrategyAny, 0, 16, 4)
	registry := prometheus.NewRegistry()
	target := fmt.Sprintf("http://example.com:%s", port)

	ok := probeHTTPMultiAddress(context.Background(), target, module, registry,
		promslog.NewNopLogger(), fixedLookup("127.0.0.1", "127.0.0.2"))
	if !ok {
		t.Fatalf("Expected probe with strategy=any and one good address to succeed")
	}

	counts := addressResultCounts(t, registry)
	if counts["succeeded"] != 1 {
		t.Fatalf("Expected exactly one succeeded address, got %v", counts)
	}
	if counts["untested"] != 1 {
		t.Fatalf("Expected the cancelled address to be untested, not successful: %v", counts)
	}
	if counts["failed"] != 0 {
		t.Fatalf("Expected no failed addresses, got %v", counts)
	}
}

// TestMultiAddressRatioStrategy checks that the overall result follows the
// configured minimum success ratio.
func TestMultiAddressRatioStrategy(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	port := serverPort(t, ts.URL)
	target := fmt.Sprintf("http://example.com:%s", port)

	for _, tc := range []struct {
		name     string
		ratio    float64
		expected bool
	}{
		{"ratio met", 0.5, true},
		{"ratio not met", 0.75, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			module := multiAddressModule(config.MultiAddressStrategyRatio, tc.ratio, 16, 4)
			registry := prometheus.NewRegistry()
			ok := probeHTTPMultiAddress(context.Background(), target, module, registry,
				promslog.NewNopLogger(), fixedLookup("127.0.0.1", "127.0.0.2"))
			if ok != tc.expected {
				t.Fatalf("Expected probe result %v, got %v", tc.expected, ok)
			}
			ratio, found := gaugeValue(t, registry, "probe_http_address_success_ratio")
			if !found {
				t.Fatalf("Expected probe_http_address_success_ratio metric")
			}
			if ratio != 0.5 {
				t.Fatalf("Expected success ratio 0.5, got %v", ratio)
			}
		})
	}
}

// TestMultiAddressTLSKeepsSNIAndHost verifies that per-address probes keep
// the TLS SNI and Host header of the original hostname while connecting to
// the candidate IP.
func TestMultiAddressTLSKeepsSNIAndHost(t *testing.T) {
	certExpiry := time.Now().AddDate(0, 0, 1)
	testCertTmpl := generateCertificateTemplate(certExpiry, false)
	testCertTmpl.IsCA = true
	_, testcertPem, testKey := generateSelfSignedCertificate(testCertTmpl)
	testKeyPem := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(testKey)})
	testcert, err := tls.X509KeyPair(testcertPem, testKeyPem)
	if err != nil {
		t.Fatalf("Failed to decode TLS testing keypair: %v", err)
	}

	var mu sync.Mutex
	var gotSNI, gotHost string
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotHost = r.Host
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	ts.TLS = &tls.Config{
		Certificates: []tls.Certificate{testcert},
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			mu.Lock()
			gotSNI = hello.ServerName
			mu.Unlock()
			return nil, nil
		},
	}
	ts.StartTLS()
	defer ts.Close()
	port := serverPort(t, ts.URL)

	module := multiAddressModule(config.MultiAddressStrategyAll, 0, 16, 4)
	module.HTTP.HTTPClientConfig = pconfig.HTTPClientConfig{
		TLSConfig: pconfig.TLSConfig{InsecureSkipVerify: true},
	}
	registry := prometheus.NewRegistry()
	target := fmt.Sprintf("https://example.com:%s", port)

	ok := probeHTTPMultiAddress(context.Background(), target, module, registry,
		promslog.NewNopLogger(), fixedLookup("127.0.0.1"))
	if !ok {
		t.Fatalf("Expected TLS multi-address probe to succeed")
	}

	mu.Lock()
	defer mu.Unlock()
	if gotSNI != "example.com" {
		t.Fatalf("Expected TLS SNI %q, got %q", "example.com", gotSNI)
	}
	if want := "example.com:" + port; gotHost != want {
		t.Fatalf("Expected Host header %q, got %q", want, gotHost)
	}
}

// TestMultiAddressMaxAddressesCap ensures the candidate set is capped and
// the aggregate metrics only cover the bounded set.
func TestMultiAddressMaxAddressesCap(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	port := serverPort(t, ts.URL)
	target := fmt.Sprintf("http://example.com:%s", port)

	ips := []string{"127.0.0.1"}
	for i := 2; i <= 20; i++ {
		ips = append(ips, fmt.Sprintf("127.0.0.%d", i))
	}

	module := multiAddressModule(config.MultiAddressStrategyAll, 0, 3, 2)
	registry := prometheus.NewRegistry()

	ok := probeHTTPMultiAddress(context.Background(), target, module, registry,
		promslog.NewNopLogger(), fixedLookup(ips...))
	if ok {
		t.Fatalf("Expected probe to fail: capped candidate set contains unreachable addresses")
	}

	counts := addressResultCounts(t, registry)
	total := counts["succeeded"] + counts["failed"] + counts["untested"]
	if total != 3 {
		t.Fatalf("Expected candidate set capped at 3, got %v", counts)
	}
	// 127.0.0.1 sorts first, so it is always part of the capped set.
	if counts["succeeded"] != 1 || counts["failed"] != 2 {
		t.Fatalf("Unexpected address counts: %v", counts)
	}
}

// TestMultiAddressResolutionFailure ensures a resolution error fails the
// probe instead of silently succeeding.
func TestMultiAddressResolutionFailure(t *testing.T) {
	module := multiAddressModule(config.MultiAddressStrategyAll, 0, 16, 4)
	registry := prometheus.NewRegistry()
	lookup := func(_ context.Context, _ string) ([]net.IPAddr, error) {
		return nil, fmt.Errorf("no such host")
	}
	ok := probeHTTPMultiAddress(context.Background(), "http://example.com", module, registry,
		promslog.NewNopLogger(), lookup)
	if ok {
		t.Fatalf("Expected probe to fail when resolution fails")
	}
}
