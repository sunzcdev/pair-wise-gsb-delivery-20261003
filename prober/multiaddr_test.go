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
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/prometheus/blackbox_exporter/config"
	"github.com/prometheus/common/promslog"
)

// stubLookupIPAddr replaces DNS resolution with a fixed candidate set.
func stubLookupIPAddr(t *testing.T, ips ...string) {
	t.Helper()
	old := lookupIPAddr
	lookupIPAddr = func(ctx context.Context, host string) ([]net.IPAddr, error) {
		addrs := make([]net.IPAddr, 0, len(ips))
		for _, ip := range ips {
			addrs = append(addrs, net.IPAddr{IP: net.ParseIP(ip)})
		}
		return addrs, nil
	}
	t.Cleanup(func() { lookupIPAddr = old })
}

func getGaugeValue(t *testing.T, registry *prometheus.Registry, name string) float64 {
	t.Helper()
	mfs, err := registry.Gather()
	if err != nil {
		t.Fatalf("Error gathering metrics: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		if len(mf.GetMetric()) != 1 {
			t.Fatalf("Expected exactly one metric for %s, got %d", name, len(mf.GetMetric()))
		}
		return mf.GetMetric()[0].GetGauge().GetValue()
	}
	t.Fatalf("Metric %s not found", name)
	return 0
}

// closeListener accepts connections and closes them right away. It is
// used as the "bad" address: it proves the dial reached its IP while
// making the probe fail.
type closeListener struct {
	net.Listener
	mu   sync.Mutex
	hits int
}

func newCloseListener(t *testing.T, addr string) *closeListener {
	t.Helper()
	ln, err := net.Listen("tcp4", addr)
	if err != nil {
		t.Fatalf("Error starting close listener on %s: %v", addr, err)
	}
	cl := &closeListener{Listener: ln}
	t.Cleanup(func() { cl.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			cl.mu.Lock()
			cl.hits++
			cl.mu.Unlock()
			conn.Close()
		}
	}()
	return cl
}

func (cl *closeListener) hitCount() int {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	return cl.hits
}

func testAllAddressesModule(policy string, ratio float64, maxParallel int) config.Module {
	module := config.DefaultModule
	module.Prober = "http"
	module.ProbeAllAddresses = config.ProbeAllAddresses{
		Enabled:         true,
		MaxAddresses:    16,
		MaxParallel:     maxParallel,
		SuccessPolicy:   policy,
		MinSuccessRatio: ratio,
	}
	return module
}

// TestProbeAllAddressesHTTPAllPolicy checks that every resolved address
// is dialed individually (one good, one bad local address) while the
// Host header keeps using the original hostname, and that the "all"
// policy fails the probe when any single address fails.
func TestProbeAllAddressesHTTPAllPolicy(t *testing.T) {
	var hostMu sync.Mutex
	var gotHosts []string
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hostMu.Lock()
		gotHosts = append(gotHosts, r.Host)
		hostMu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer good.Close()
	_, port, err := net.SplitHostPort(good.Listener.Addr().String())
	if err != nil {
		t.Fatalf("Error getting test server port: %v", err)
	}

	// The bad address listens on the same port on a different loopback
	// IP and kills every connection.
	bad := newCloseListener(t, net.JoinHostPort("127.0.0.2", port))

	stubLookupIPAddr(t, "127.0.0.1", "127.0.0.2")

	module := testAllAddressesModule(config.SuccessPolicyAll, 0.5, 1)
	registry := prometheus.NewRegistry()
	logger := promslog.NewNopLogger()
	target := fmt.Sprintf("http://example.com:%s/", port)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if ProbeAllAddresses(ctx, target, module, registry, ProbeHTTP, logger) {
		t.Fatal("Expected overall failure with the all policy when one address is bad")
	}

	if got := getGaugeValue(t, registry, "probe_addresses_total"); got != 2 {
		t.Errorf("Expected probe_addresses_total=2, got %v", got)
	}
	if got := getGaugeValue(t, registry, "probe_addresses_succeeded"); got != 1 {
		t.Errorf("Expected probe_addresses_succeeded=1, got %v", got)
	}
	if got := getGaugeValue(t, registry, "probe_addresses_failed"); got != 1 {
		t.Errorf("Expected probe_addresses_failed=1, got %v", got)
	}

	// The connection must have reached each address individually.
	hostMu.Lock()
	defer hostMu.Unlock()
	if len(gotHosts) != 1 {
		t.Fatalf("Expected exactly one request on the good address, got %d", len(gotHosts))
	}
	if gotHosts[0] != "example.com:"+port {
		t.Errorf("Expected Host header to keep the original hostname, got %q", gotHosts[0])
	}
	if bad.hitCount() == 0 {
		t.Error("Expected the bad address to be dialed, but it saw no connection")
	}
}

// TestProbeAllAddressesAllCancelledInflight checks that with the "all"
// policy an address whose probe is still in flight when another address
// fails is reported as untested, not as a failure.
func TestProbeAllAddressesAllCancelledInflight(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Slow enough that the bad address fails first and the
		// in-flight request gets cancelled.
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer good.Close()
	_, port, err := net.SplitHostPort(good.Listener.Addr().String())
	if err != nil {
		t.Fatalf("Error getting test server port: %v", err)
	}

	// Nothing listens on the bad address: connections are refused.
	stubLookupIPAddr(t, "127.0.0.1", "127.0.0.2")

	module := testAllAddressesModule(config.SuccessPolicyAll, 0.5, 2)
	registry := prometheus.NewRegistry()
	logger := promslog.NewNopLogger()
	target := fmt.Sprintf("http://example.com:%s/", port)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if ProbeAllAddresses(ctx, target, module, registry, ProbeHTTP, logger) {
		t.Fatal("Expected overall failure with the all policy when one address is bad")
	}

	if got := getGaugeValue(t, registry, "probe_addresses_failed"); got != 1 {
		t.Errorf("Expected probe_addresses_failed=1, got %v", got)
	}
	if got := getGaugeValue(t, registry, "probe_addresses_untested"); got != 1 {
		t.Errorf("Expected probe_addresses_untested=1 (cancelled in-flight probe), got %v", got)
	}
	if got := getGaugeValue(t, registry, "probe_addresses_succeeded"); got != 0 {
		t.Errorf("Expected probe_addresses_succeeded=0, got %v", got)
	}
}

// TestProbeAllAddressesTLSSNI checks that with the "any" policy the
// probe succeeds as long as one address works, and that the TLS server
// name and the Host header still use the original hostname even though
// the connection goes to a resolved IP.
func TestProbeAllAddressesTLSSNI(t *testing.T) {
	var mu sync.Mutex
	var gotSNI, gotHost string
	good := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotHost = r.Host
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	good.TLS = &tls.Config{
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			mu.Lock()
			gotSNI = hello.ServerName
			mu.Unlock()
			return nil, nil
		},
	}
	good.StartTLS()
	defer good.Close()
	_, port, err := net.SplitHostPort(good.Listener.Addr().String())
	if err != nil {
		t.Fatalf("Error getting test server port: %v", err)
	}

	// Nothing listens on the bad address: connections are refused.
	stubLookupIPAddr(t, "127.0.0.1", "127.0.0.2")

	module := testAllAddressesModule(config.SuccessPolicyAny, 0.5, 4)
	module.HTTP.HTTPClientConfig.TLSConfig.InsecureSkipVerify = true
	registry := prometheus.NewRegistry()
	logger := promslog.NewNopLogger()
	target := fmt.Sprintf("https://example.com:%s/", port)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if !ProbeAllAddresses(ctx, target, module, registry, ProbeHTTP, logger) {
		t.Fatal("Expected overall success with the any policy when one address is good")
	}

	mu.Lock()
	defer mu.Unlock()
	if gotSNI != "example.com" {
		t.Errorf("Expected TLS SNI to keep the original hostname, got %q", gotSNI)
	}
	if gotHost != "example.com:"+port {
		t.Errorf("Expected Host header to keep the original hostname, got %q", gotHost)
	}
	if got := getGaugeValue(t, registry, "probe_addresses_succeeded"); got < 1 {
		t.Errorf("Expected at least one succeeded address, got %v", got)
	}
}

// TestProbeAllAddressesAnyUntested checks that with the "any" policy the
// remaining addresses are left untested once one address succeeded, and
// that the cancelled address is neither dialed nor counted as a success.
func TestProbeAllAddressesAnyUntested(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer good.Close()
	_, port, err := net.SplitHostPort(good.Listener.Addr().String())
	if err != nil {
		t.Fatalf("Error getting test server port: %v", err)
	}
	bad := newCloseListener(t, net.JoinHostPort("127.0.0.2", port))

	// The good address comes first; with max_parallel=1 the second
	// address is only considered after the first one succeeded.
	stubLookupIPAddr(t, "127.0.0.1", "127.0.0.2")

	module := testAllAddressesModule(config.SuccessPolicyAny, 0.5, 1)
	registry := prometheus.NewRegistry()
	logger := promslog.NewNopLogger()
	target := fmt.Sprintf("http://example.com:%s/", port)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if !ProbeAllAddresses(ctx, target, module, registry, ProbeHTTP, logger) {
		t.Fatal("Expected overall success with the any policy")
	}

	if got := getGaugeValue(t, registry, "probe_addresses_succeeded"); got != 1 {
		t.Errorf("Expected probe_addresses_succeeded=1, got %v", got)
	}
	if got := getGaugeValue(t, registry, "probe_addresses_untested"); got != 1 {
		t.Errorf("Expected probe_addresses_untested=1 (cancelled addresses are not successes), got %v", got)
	}
	if got := getGaugeValue(t, registry, "probe_addresses_failed"); got != 0 {
		t.Errorf("Expected probe_addresses_failed=0, got %v", got)
	}
	if bad.hitCount() != 0 {
		t.Errorf("Expected the remaining address to stay untested, but it was dialed %d times", bad.hitCount())
	}
}

// TestProbeAllAddressesRatioPolicy checks that the overall result
// follows the configured minimum success ratio.
func TestProbeAllAddressesRatioPolicy(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	}))
	defer good.Close()
	_, port, err := net.SplitHostPort(good.Listener.Addr().String())
	if err != nil {
		t.Fatalf("Error getting test server port: %v", err)
	}

	testcases := []struct {
		name      string
		ratio     float64
		expected  bool
	}{
		{"half is enough", 0.5, true},
		{"more than half is not enough", 0.6, false},
	}
	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			// One good (127.0.0.1) and one bad (127.0.0.2, refused)
			// address: exactly half of the candidates succeed.
			stubLookupIPAddr(t, "127.0.0.1", "127.0.0.2")

			module := testAllAddressesModule(config.SuccessPolicyRatio, tc.ratio, 4)
			registry := prometheus.NewRegistry()
			logger := promslog.NewNopLogger()
			target := fmt.Sprintf("http://example.com:%s/", port)

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			if got := ProbeAllAddresses(ctx, target, module, registry, ProbeHTTP, logger); got != tc.expected {
				t.Errorf("Expected overall result %v with ratio %v, got %v", tc.expected, tc.ratio, got)
			}
			if got := getGaugeValue(t, registry, "probe_addresses_total"); got != 2 {
				t.Errorf("Expected probe_addresses_total=2, got %v", got)
			}
		})
	}
}

// TestProbeAllAddressesMaxAddresses checks that the candidate set is
// capped at the configured maximum.
func TestProbeAllAddressesMaxAddresses(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer good.Close()
	_, port, err := net.SplitHostPort(good.Listener.Addr().String())
	if err != nil {
		t.Fatalf("Error getting test server port: %v", err)
	}

	stubLookupIPAddr(t, "127.0.0.1", "127.0.0.2", "127.0.0.3", "127.0.0.4")

	module := testAllAddressesModule(config.SuccessPolicyAny, 0.5, 2)
	module.ProbeAllAddresses.MaxAddresses = 2
	registry := prometheus.NewRegistry()
	logger := promslog.NewNopLogger()
	target := fmt.Sprintf("http://example.com:%s/", port)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if !ProbeAllAddresses(ctx, target, module, registry, ProbeHTTP, logger) {
		t.Fatal("Expected overall success with the any policy")
	}
	if got := getGaugeValue(t, registry, "probe_addresses_total"); got != 2 {
		t.Errorf("Expected candidate set to be capped at 2, got %v", got)
	}
}
