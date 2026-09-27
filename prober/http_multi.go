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
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/prometheus/blackbox_exporter/config"
)

// addressLookupFunc resolves a hostname to all of its IP addresses. It is a
// variable so tests can provide a fixed candidate set.
type addressLookupFunc func(ctx context.Context, host string) ([]net.IPAddr, error)

// lookupIPAddrs resolves all A and AAAA records of the host in a single
// lookup, producing the fixed candidate set for a multi-address probe.
func lookupIPAddrs(ctx context.Context, host string) ([]net.IPAddr, error) {
	resolver := &net.Resolver{}
	addrs, err := resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	ips := make([]net.IPAddr, 0, len(addrs))
	for _, addr := range addrs {
		ips = append(ips, net.IPAddr{IP: addr.IP})
	}
	return ips, nil
}

// addressResult is the outcome of probing a single candidate address.
type addressResult int

const (
	// addressUntested means the address was not (fully) probed, e.g. the
	// probe was cancelled after an early success with the "any" strategy or
	// the overall time budget was exhausted. Untested addresses are never
	// counted as successes.
	addressUntested addressResult = iota
	addressSucceeded
	addressFailed
)

func (r addressResult) String() string {
	switch r {
	case addressSucceeded:
		return "succeeded"
	case addressFailed:
		return "failed"
	default:
		return "untested"
	}
}

// filterCandidateAddresses reduces the resolved addresses to the ones
// matching the configured IP protocol preference, deduplicates and sorts
// them so the candidate set is stable.
func filterCandidateAddresses(addrs []net.IPAddr, ipProtocol string, fallback bool) []net.IPAddr {
	seen := make(map[string]struct{}, len(addrs))
	candidates := make([]net.IPAddr, 0, len(addrs))
	for _, addr := range addrs {
		if addr.IP == nil {
			continue
		}
		if !fallback && ipProtocol != "" {
			is4 := addr.IP.To4() != nil
			if (ipProtocol == "ip4") != is4 {
				continue
			}
		}
		key := addr.IP.String()
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		candidates = append(candidates, addr)
	}
	slices.SortFunc(candidates, func(a, b net.IPAddr) int {
		return bytes.Compare(a.IP.To16(), b.IP.To16())
	})
	return candidates
}

// probeHTTPMultiAddress resolves the target hostname once into a fixed
// candidate set and probes every candidate address within the overall probe
// timeout (the parent context deadline). The overall result is decided by
// the configured strategy: "all", "any" or a minimum success ratio.
//
// Metrics are aggregated counts (probe_http_addresses{result=...}); the
// per-address details are reported through the (bounded) scrape log.
func probeHTTPMultiAddress(ctx context.Context, target string, module config.Module, registry *prometheus.Registry, logger *slog.Logger, lookup addressLookupFunc) bool {
	cfg := module.HTTP.MultiAddress

	addressesGaugeVec := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "probe_http_addresses",
		Help: "Number of resolved IP addresses by probe result (succeeded, failed, untested)",
	}, []string{"result"})
	successRatioGauge := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "probe_http_address_success_ratio",
		Help: "Ratio of successfully probed addresses over all candidate addresses",
	})
	lookupTimeGauge := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "probe_dns_lookup_time_seconds",
		Help: "Returns the time taken for probe dns lookup in seconds",
	})
	registry.MustRegister(addressesGaugeVec, successRatioGauge, lookupTimeGauge)
	for _, result := range []addressResult{addressSucceeded, addressFailed, addressUntested} {
		addressesGaugeVec.WithLabelValues(result.String())
	}

	// Normalize the target the same way the single-address probe does.
	targetLower := strings.ToLower(target)
	if !strings.HasPrefix(targetLower, "http://") && !strings.HasPrefix(targetLower, "https://") {
		target = "http://" + target
	}
	targetURL, err := url.Parse(target)
	if err != nil {
		logger.Error("Could not parse target URL", "err", err)
		return false
	}
	targetHost := targetURL.Hostname()
	if targetHost == "" {
		logger.Error("Target URL has no hostname", "target", target)
		return false
	}

	if !shouldResolveDNSWithProxy(module.HTTP) {
		logger.Error("multi_address requires local DNS resolution, but the module skips the resolve phase when a proxy is configured")
		return false
	}

	// Resolve exactly once: the candidate set stays fixed for the whole
	// probe, so a changing DNS answer cannot make addresses untestable.
	lookupStart := time.Now()
	addrs, err := lookup(ctx, targetHost)
	lookupTimeGauge.Set(time.Since(lookupStart).Seconds())
	if err != nil {
		logger.Error("Error resolving target addresses", "target", targetHost, "err", err)
		return false
	}

	candidates := filterCandidateAddresses(addrs, module.HTTP.IPProtocol, module.HTTP.IPProtocolFallback)
	if len(candidates) == 0 {
		logger.Error("No candidate addresses found", "target", targetHost)
		return false
	}
	if len(candidates) > cfg.MaxAddresses {
		logger.Info("Too many resolved addresses, capping candidate set",
			"resolved", len(candidates), "max_addresses", cfg.MaxAddresses)
		candidates = candidates[:cfg.MaxAddresses]
	}

	logger.Info("Probing all resolved addresses",
		"target", targetHost,
		"candidates", len(candidates),
		"strategy", cfg.Strategy,
		"max_concurrency", cfg.MaxConcurrency,
	)

	// All per-address probes share the parent context, so the module
	// timeout is the total budget for the whole candidate set. The child
	// context additionally allows the "any" strategy to stop early.
	probeCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	results := make([]addressResult, len(candidates))
	concurrency := min(cfg.MaxConcurrency, len(candidates))
	sem := make(chan struct{}, concurrency)
	var anySucceeded atomic.Bool
	var wg sync.WaitGroup

	for i, addr := range candidates {
		wg.Add(1)
		go func(i int, addr net.IPAddr) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-probeCtx.Done():
				// Never started: budget exhausted or an early "any"
				// success. Stays untested.
				return
			}
			if cfg.Strategy == config.MultiAddressStrategyAny && anySucceeded.Load() {
				return
			}

			addrLogger := logger.With("address", addr.String())
			addrRegistry := prometheus.NewRegistry()
			pinned := addr
			ok := probeHTTP(probeCtx, target, module, addrRegistry, addrLogger, &pinned)
			if !ok && probeCtx.Err() != nil {
				// The probe was cancelled while running (early "any"
				// success or exhausted budget). A cancelled probe is
				// untested, not a failure and certainly not a success.
				addrLogger.Info("Address probe cancelled before completion, counting as untested")
				return
			}
			if ok {
				results[i] = addressSucceeded
				if cfg.Strategy == config.MultiAddressStrategyAny && anySucceeded.CompareAndSwap(false, true) {
					// One success is enough; stop the remaining
					// addresses. They are reported as untested.
					cancel()
				}
			} else {
				results[i] = addressFailed
			}
		}(i, addr)
	}
	wg.Wait()

	var succeeded, failed, untested int
	for i, addr := range candidates {
		switch results[i] {
		case addressSucceeded:
			succeeded++
		case addressFailed:
			failed++
		default:
			untested++
		}
		// Bounded per-address diagnostics: at most MaxAddresses lines.
		logger.Info("Address probe result", "address", addr.String(), "result", results[i].String())
	}

	addressesGaugeVec.WithLabelValues(addressSucceeded.String()).Set(float64(succeeded))
	addressesGaugeVec.WithLabelValues(addressFailed.String()).Set(float64(failed))
	addressesGaugeVec.WithLabelValues(addressUntested.String()).Set(float64(untested))
	ratio := float64(succeeded) / float64(len(candidates))
	successRatioGauge.Set(ratio)

	logger.Info("Multi-address probe finished",
		"target", targetHost,
		"candidates", len(candidates),
		"succeeded", succeeded,
		"failed", failed,
		"untested", untested,
		"success_ratio", fmt.Sprintf("%.4f", ratio),
	)

	switch cfg.Strategy {
	case config.MultiAddressStrategyAny:
		if untested > 0 {
			logger.Info("Some addresses were not probed to completion; untested addresses are not counted as successes",
				"untested", untested)
		}
		return succeeded > 0
	case config.MultiAddressStrategyRatio:
		if ratio < cfg.MinSuccessRatio {
			logger.Error("Success ratio below minimum",
				"success_ratio", fmt.Sprintf("%.4f", ratio),
				"min_success_ratio", cfg.MinSuccessRatio)
			return false
		}
		return true
	default: // config.MultiAddressStrategyAll
		if failed > 0 || untested > 0 {
			logger.Error("Not all addresses were successfully probed",
				"failed", failed, "untested", untested)
			return false
		}
		return true
	}
}
