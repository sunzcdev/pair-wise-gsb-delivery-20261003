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
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	pconfig "github.com/prometheus/common/config"
	"github.com/prometheus/common/promslog"

	"github.com/prometheus/blackbox_exporter/config"
)

// scenarioServerA plays the role of the login service: it hands out a
// temporary cookie and a token, and protects a resource with them.
func scenarioServerA(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/login", func(w http.ResponseWriter, _ *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "session", Value: "xyz123", Path: "/"})
		w.Write([]byte("token=abc123&region=eu"))
	})
	mux.HandleFunc("/protected", func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("session")
		if err != nil || cookie.Value != "xyz123" {
			http.Error(w, "missing session cookie", http.StatusForbidden)
			return
		}
		if r.Header.Get("X-Token") != "abc123" {
			http.Error(w, "missing token header", http.StatusForbidden)
			return
		}
		w.Write([]byte("protected ok"))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// scenarioServerB plays the role of a second, unrelated service.
func scenarioServerB(t *testing.T, hits *atomic.Int64) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Query().Get("region") != "eu" {
			http.Error(w, "missing region", http.StatusBadRequest)
			return
		}
		w.Write([]byte("status ok"))
	})
	mux.HandleFunc("/loop", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, "/loop", http.StatusFound)
	})
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		select {
		case <-time.After(5 * time.Second):
			w.Write([]byte("slow ok"))
		case <-r.Context().Done():
		}
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func scenarioModule(steps ...config.ScenarioStep) config.Module {
	return config.Module{
		Timeout: 5 * time.Second,
		HTTPScenario: config.HTTPScenarioProbe{
			HTTPClientConfig: pconfig.DefaultHTTPClientConfig,
			Steps:            steps,
		},
	}
}

// scenarioMetric returns the value of a gauge with the given labels,
// and whether it was present in the registry.
func scenarioMetric(t *testing.T, registry *prometheus.Registry, name string, labels map[string]string) (float64, bool) {
	t.Helper()
	mfs, err := registry.Gather()
	if err != nil {
		t.Fatalf("Error gathering metrics: %s", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			matched := true
			for k, v := range labels {
				found := false
				for _, lp := range m.GetLabel() {
					if lp.GetName() == k && lp.GetValue() == v {
						found = true
						break
					}
				}
				if !found {
					matched = false
					break
				}
			}
			if matched {
				return m.GetGauge().GetValue(), true
			}
		}
	}
	return 0, false
}

func failedScenarioStep(t *testing.T, registry *prometheus.Registry) string {
	t.Helper()
	mfs, err := registry.Gather()
	if err != nil {
		t.Fatalf("Error gathering metrics: %s", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "probe_http_scenario_step_failed" {
			continue
		}
		for _, m := range mf.GetMetric() {
			if m.GetGauge().GetValue() != 1 {
				continue
			}
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "step" {
					return lp.GetValue()
				}
			}
		}
	}
	return ""
}

// TestHTTPScenarioSuccess runs the full health-check flow across two
// local servers: obtain a temporary cookie and token from the login
// service, then access the protected resource and a second service.
func TestHTTPScenarioSuccess(t *testing.T) {
	var hitsB atomic.Int64
	serverA := scenarioServerA(t)
	serverB := scenarioServerB(t, &hitsB)

	module := scenarioModule(
		config.ScenarioStep{
			Name: "login",
			URL:  serverA.URL + "/login",
			Extract: []config.ScenarioExtract{
				{Name: "token", Source: "body", Regexp: config.MustNewRegexp("token=([^&]+)"), Secret: true},
				{Name: "region", Source: "body", Regexp: config.MustNewRegexp(`region=(\w+)`)},
			},
		},
		config.ScenarioStep{
			Name:    "fetch_protected",
			URL:     serverA.URL + "/protected",
			Headers: map[string]string{"X-Token": "${token}"},
			FailIfBodyNotMatchesRegexp: []config.Regexp{
				config.MustNewRegexp("protected ok"),
			},
		},
		config.ScenarioStep{
			Name: "check_status",
			URL:  serverB.URL + "/status?region=${region}",
		},
	)

	registry := prometheus.NewRegistry()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if !ProbeHTTPScenario(ctx, serverA.URL, module, registry, promslog.NewNopLogger()) {
		t.Fatal("Scenario probe failed, expected success")
	}

	if completed, _ := scenarioMetric(t, registry, "probe_http_scenario_steps_completed", nil); completed != 3 {
		t.Fatalf("Expected 3 completed steps, got %v", completed)
	}
	for _, step := range []string{"login", "fetch_protected", "check_status"} {
		if _, ok := scenarioMetric(t, registry, "probe_http_scenario_step_duration_seconds", map[string]string{"step": step}); !ok {
			t.Fatalf("Missing duration metric for step %q", step)
		}
	}
	if step := failedScenarioStep(t, registry); step != "" {
		t.Fatalf("Unexpected failed step %q", step)
	}
	if hitsB.Load() != 1 {
		t.Fatalf("Expected server B to be hit once, got %d", hitsB.Load())
	}
}

// TestHTTPScenarioMissingVariable verifies that a step whose
// extraction does not match fails the probe at that step.
func TestHTTPScenarioMissingVariable(t *testing.T) {
	var hitsB atomic.Int64
	serverA := scenarioServerA(t)
	scenarioServerB(t, &hitsB)

	module := scenarioModule(
		config.ScenarioStep{
			Name: "login",
			URL:  serverA.URL + "/login",
			Extract: []config.ScenarioExtract{
				// The body contains no digits after "token=", so the
				// variable is never extracted.
				{Name: "token", Source: "body", Regexp: config.MustNewRegexp("token=([0-9]+)")},
			},
		},
		config.ScenarioStep{
			Name:    "fetch_protected",
			URL:     serverA.URL + "/protected",
			Headers: map[string]string{"X-Token": "${token}"},
		},
	)

	registry := prometheus.NewRegistry()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if ProbeHTTPScenario(ctx, serverA.URL, module, registry, promslog.NewNopLogger()) {
		t.Fatal("Scenario probe succeeded, expected failure due to missing variable")
	}
	if step := failedScenarioStep(t, registry); step != "login" {
		t.Fatalf("Expected failed step %q, got %q", "login", step)
	}
	if completed, _ := scenarioMetric(t, registry, "probe_http_scenario_steps_completed", nil); completed != 0 {
		t.Fatalf("Expected 0 completed steps, got %v", completed)
	}
}

// TestHTTPScenarioRedirectLoop verifies that a redirect loop fails the
// step quickly instead of spinning until the probe timeout.
func TestHTTPScenarioRedirectLoop(t *testing.T) {
	var hitsB atomic.Int64
	serverB := scenarioServerB(t, &hitsB)

	module := scenarioModule(
		config.ScenarioStep{
			Name: "loop",
			URL:  serverB.URL + "/loop",
		},
	)

	registry := prometheus.NewRegistry()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	start := time.Now()
	if ProbeHTTPScenario(ctx, serverB.URL, module, registry, promslog.NewNopLogger()) {
		t.Fatal("Scenario probe succeeded, expected failure due to redirect loop")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("Redirect loop took too long to fail: %s", elapsed)
	}
	if step := failedScenarioStep(t, registry); step != "loop" {
		t.Fatalf("Expected failed step %q, got %q", "loop", step)
	}
}

// TestHTTPScenarioSecondStepTimeout verifies that a timeout in the
// second step is reported against that step, after the first step has
// completed successfully.
func TestHTTPScenarioSecondStepTimeout(t *testing.T) {
	var hitsB atomic.Int64
	serverA := scenarioServerA(t)
	serverB := scenarioServerB(t, &hitsB)

	module := scenarioModule(
		config.ScenarioStep{
			Name: "login",
			URL:  serverA.URL + "/login",
			Extract: []config.ScenarioExtract{
				{Name: "region", Source: "body", Regexp: config.MustNewRegexp(`region=(\w+)`)},
			},
		},
		config.ScenarioStep{
			Name: "wait_status",
			URL:  serverB.URL + "/slow?region=${region}",
		},
	)

	registry := prometheus.NewRegistry()
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	start := time.Now()
	if ProbeHTTPScenario(ctx, serverA.URL, module, registry, promslog.NewNopLogger()) {
		t.Fatal("Scenario probe succeeded, expected timeout in second step")
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("Probe did not respect the timeout, took %s", elapsed)
	}
	if step := failedScenarioStep(t, registry); step != "wait_status" {
		t.Fatalf("Expected failed step %q, got %q", "wait_status", step)
	}
	if completed, _ := scenarioMetric(t, registry, "probe_http_scenario_steps_completed", nil); completed != 1 {
		t.Fatalf("Expected 1 completed step, got %v", completed)
	}
}

// TestHTTPScenarioCrossOriginSecretDropped verifies that a secret
// variable captured from one origin is never sent to a different
// origin: the step fails before any request is made.
func TestHTTPScenarioCrossOriginSecretDropped(t *testing.T) {
	var hitsB atomic.Int64
	serverA := scenarioServerA(t)
	serverB := scenarioServerB(t, &hitsB)

	module := scenarioModule(
		config.ScenarioStep{
			Name: "login",
			URL:  serverA.URL + "/login",
			Extract: []config.ScenarioExtract{
				{Name: "token", Source: "body", Regexp: config.MustNewRegexp("token=([^&]+)"), Secret: true},
			},
		},
		config.ScenarioStep{
			Name: "exfiltrate",
			URL:  serverB.URL + "/status?token=${token}",
		},
	)

	registry := prometheus.NewRegistry()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if ProbeHTTPScenario(ctx, serverA.URL, module, registry, promslog.NewNopLogger()) {
		t.Fatal("Scenario probe succeeded, expected cross-origin secret to be dropped")
	}
	if step := failedScenarioStep(t, registry); step != "exfiltrate" {
		t.Fatalf("Expected failed step %q, got %q", "exfiltrate", step)
	}
	if hitsB.Load() != 0 {
		t.Fatalf("Server B received %d requests, secret must not be sent cross-origin", hitsB.Load())
	}
}
