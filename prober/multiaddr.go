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
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/prometheus/blackbox_exporter/config"
)

// lookupIPAddr resolves a hostname to all of its IP addresses. It is a
// package level variable so tests can stub out DNS resolution.
var lookupIPAddr = func(ctx context.Context, host string) ([]net.IPAddr, error) {
	resolver := &net.Resolver{}
	return resolver.LookupIPAddr(ctx, host)
}

// addressProbeState is the final state of a single candidate address.
type addressProbeState string

const (
	addressStateSuccess addressProbeState = "success"
	addressStateFailure addressProbeState = "failure"
	// addressStateUntested marks addresses that were never probed, e.g.
	// because the overall result was already decided or the total time
	// budget ran out. Untested addresses never count as successes.
	addressStateUntested addressProbeState = "untested"
)

type addressProbeResult struct {
	address  string
	state    addressProbeState
	duration float64
}

// ProbeAllAddresses resolves the target hostname once into a fixed set of
// candidate addresses and probes each of them individually within the
// total time budget of the parent context. Every per-address probe dials
// the candidate IP directly while the original hostname is preserved for
// the Host header and the TLS server name. The per-address results are
// aggregated into the overall result according to the configured success
// policy (all, any or a minimum success ratio).
//
// Metrics exposed in the registry are aggregate counts only; per-address
// details are reported through the (bounded) diagnostic log output.
func ProbeAllAddresses(ctx context.Context, target string, module config.Module, registry *prometheus.Registry, prober ProbeFn, logger *slog.Logger) bool {
	addressesTotalGauge := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "probe_addresses_total",
		Help: "Number of resolved addresses selected as probe candidates",
	})
	addressesSucceededGauge := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "probe_addresses_succeeded",
		Help: "Number of candidate addresses that were probed successfully",
	})
	addressesFailedGauge := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "probe_addresses_failed",
		Help: "Number of candidate addresses that were probed and failed",
	})
	addressesUntestedGauge := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "probe_addresses_untested",
		Help: "Number of candidate addresses that were not probed (decided early or out of time budget); they never count as successes",
	})
	dnsLookupTimeGauge := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "probe_dns_lookup_time_seconds",
		Help: "Returns the time taken for probe dns lookup in seconds",
	})
	registry.MustRegister(addressesTotalGauge)
	registry.MustRegister(addressesSucceededGauge)
	registry.MustRegister(addressesFailedGauge)
	registry.MustRegister(addressesUntestedGauge)
	registry.MustRegister(dnsLookupTimeGauge)

	host, err := hostFromTarget(module.Prober, target)
	if err != nil {
		logger.Error("Error extracting hostname from target", "target", target, "err", err)
		return false
	}

	logger.Debug("Resolving all target addresses", "target", host)
	resolveStart := time.Now()
	ips, err := lookupIPAddr(ctx, host)
	dnsLookupTimeGauge.Set(time.Since(resolveStart).Seconds())
	if err != nil {
		logger.Error("Error resolving target address", "target", host, "err", err)
		return false
	}

	candidates := selectCandidates(ips, module)
	if len(candidates) == 0 {
		logger.Error("Resolution returned no usable addresses", "target", host)
		return false
	}
	if len(candidates) > module.ProbeAllAddresses.MaxAddresses {
		logger.Info("Capping candidate addresses", "resolved", len(candidates), "max_addresses", module.ProbeAllAddresses.MaxAddresses)
		candidates = candidates[:module.ProbeAllAddresses.MaxAddresses]
	}
	total := len(candidates)
	addressesTotalGauge.Set(float64(total))

	logger.Info("Probing all resolved addresses",
		"target", host,
		"addresses", total,
		"max_parallel", module.ProbeAllAddresses.MaxParallel,
		"success_policy", module.ProbeAllAddresses.SuccessPolicy)

	results := probeAddresses(ctx, target, module, prober, logger, candidates)

	var succeeded, failed, untested int
	for _, result := range results {
		switch result.state {
		case addressStateSuccess:
			succeeded++
		case addressStateFailure:
			failed++
		default:
			untested++
		}
		// Bounded per-address diagnostics: at most MaxAddresses entries.
		logger.Info("Address probe result",
			"address", result.address,
			"state", string(result.state),
			"duration_seconds", result.duration)
	}
	addressesSucceededGauge.Set(float64(succeeded))
	addressesFailedGauge.Set(float64(failed))
	addressesUntestedGauge.Set(float64(untested))

	success := aggregateResult(module.ProbeAllAddresses, total, succeeded)
	logger.Info("All-addresses probe finished",
		"target", host,
		"succeeded", succeeded,
		"failed", failed,
		"untested", untested,
		"success", success)
	return success
}

// probeAddresses runs the prober against every candidate address with
// bounded parallelism. All probes share the parent context as the total
// time budget; once the overall outcome is decided the remaining probes
// are cancelled and their addresses stay untested.
func probeAddresses(ctx context.Context, target string, module config.Module, prober ProbeFn, logger *slog.Logger, candidates []net.IPAddr) []addressProbeResult {
	total := len(candidates)
	results := make([]addressProbeResult, total)
	for i, ip := range candidates {
		results[i].address = ip.String()
		results[i].state = addressStateUntested
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	sem := make(chan struct{}, module.ProbeAllAddresses.MaxParallel)
	var wg sync.WaitGroup
	var mu sync.Mutex
	succeeded, failed, completed := 0, 0, 0

	decided := func() bool {
		switch module.ProbeAllAddresses.SuccessPolicy {
		case config.SuccessPolicyAny:
			return succeeded >= 1
		case config.SuccessPolicyAll:
			return failed >= 1
		case config.SuccessPolicyRatio:
			needed := int(math.Ceil(module.ProbeAllAddresses.MinSuccessRatio * float64(total)))
			remaining := total - completed
			return succeeded >= needed || succeeded+remaining < needed
		}
		return false
	}

loop:
	for i, ip := range candidates {
		// Acquire a slot in the main loop so that addresses are
		// started in a deterministic order and remaining addresses
		// stay untested once the outcome is decided.
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			break loop
		}
		wg.Add(1)
		go func(i int, ip net.IPAddr) {
			defer wg.Done()
			defer func() { <-sem }()

			addrLogger := logger.With("address", ip.String())
			addrCtx := WithForcedIPAddr(ctx, &ip)
			addrRegistry := prometheus.NewRegistry()

			start := time.Now()
			ok := prober(addrCtx, target, module, addrRegistry, addrLogger)
			duration := time.Since(start).Seconds()

			mu.Lock()
			defer mu.Unlock()
			results[i].duration = duration
			switch {
			case ok:
				results[i].state = addressStateSuccess
				succeeded++
			case ctx.Err() != nil:
				// The probe was cancelled (outcome already decided
				// or total budget exhausted): its result is not a
				// measured failure, the address stays untested.
				addrLogger.Debug("Address probe cancelled before completing")
			default:
				results[i].state = addressStateFailure
				failed++
			}
			completed++
			if decided() {
				cancel()
			}
		}(i, ip)
	}
	wg.Wait()

	return results
}

// aggregateResult combines the per-address tallies into the overall
// result. Untested addresses never count as successes.
func aggregateResult(cfg config.ProbeAllAddresses, total, succeeded int) bool {
	switch cfg.SuccessPolicy {
	case config.SuccessPolicyAny:
		return succeeded >= 1
	case config.SuccessPolicyRatio:
		return float64(succeeded)/float64(total) >= cfg.MinSuccessRatio
	default: // config.SuccessPolicyAll
		return succeeded == total
	}
}

// selectCandidates filters and deduplicates the resolved addresses
// according to the module's preferred IP protocol configuration.
func selectCandidates(ips []net.IPAddr, module config.Module) []net.IPAddr {
	ipProtocol, fallback := moduleIPProtocol(module)

	var preferred, other []net.IPAddr
	seen := map[string]struct{}{}
	for _, ip := range ips {
		if _, ok := seen[ip.String()]; ok {
			continue
		}
		seen[ip.String()] = struct{}{}
		isV4 := ip.IP.To4() != nil
		if (ipProtocol == "ip4") == isV4 {
			preferred = append(preferred, ip)
		} else {
			other = append(other, ip)
		}
	}
	if len(preferred) > 0 || !fallback {
		return preferred
	}
	return other
}

// moduleIPProtocol returns the preferred IP protocol and whether fallback
// to the other protocol family is allowed for the module's prober.
func moduleIPProtocol(module config.Module) (string, bool) {
	switch module.Prober {
	case "http":
		return module.HTTP.IPProtocol, module.HTTP.IPProtocolFallback
	case "tcp":
		return module.TCP.IPProtocol, module.TCP.IPProtocolFallback
	case "dns":
		return module.DNS.IPProtocol, module.DNS.IPProtocolFallback
	case "grpc":
		return module.GRPC.PreferredIPProtocol, module.GRPC.IPProtocolFallback
	case "icmp":
		return module.ICMP.IPProtocol, module.ICMP.IPProtocolFallback
	case "websocket":
		return module.Websocket.IPProtocol, module.Websocket.IPProtocolFallback
	}
	return "ip6", true
}

// hostFromTarget extracts the hostname part of a target for the given
// prober.
func hostFromTarget(proberName, target string) (string, error) {
	switch proberName {
	case "http", "websocket":
		if !strings.Contains(target, "://") {
			target = "http://" + target
		}
		u, err := url.Parse(target)
		if err != nil {
			return "", err
		}
		if u.Hostname() == "" {
			return "", fmt.Errorf("no hostname in target %q", target)
		}
		return u.Hostname(), nil
	default:
		host, _, err := net.SplitHostPort(target)
		if err != nil {
			if strings.Contains(err.Error(), "missing port") {
				return target, nil
			}
			return "", err
		}
		return host, nil
	}
}
