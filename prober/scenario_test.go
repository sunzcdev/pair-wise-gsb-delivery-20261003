package prober

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	pconfig "github.com/prometheus/common/config"

	"github.com/prometheus/blackbox_exporter/config"
)

// newScenarioModule builds a module for a scenario.
func newScenarioModule(steps []config.HTTPScenarioStep) config.Module {
	return config.Module{
		Timeout: 5 * time.Second,
		HTTP: config.HTTPProbe{
			IPProtocolFallback: true,
			HTTPClientConfig:   pconfig.DefaultHTTPClientConfig,
			Scenario:           &config.HTTPScenario{Steps: steps},
		},
	}
}

func mustRegexp(s string) config.Regexp {
	re, err := config.NewRegexp(s)
	if err != nil {
		panic(err)
	}
	return re
}

// captureLogger returns a logger writing to a buffer so tests can assert on
// redaction.
func captureLogger() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return logger, buf
}

func gatherMetric(t *testing.T, registry *prometheus.Registry, name string) *dto.MetricFamily {
	t.Helper()
	mfs, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() == name {
			return mf
		}
	}
	return nil
}

// labelValue returns the value of label labelName for the first metric that
// has it, or "".
func labelValue(mf *dto.MetricFamily, labelName string) string {
	for _, m := range mf.GetMetric() {
		for _, l := range m.GetLabel() {
			if l.GetName() == labelName {
				return l.GetValue()
			}
		}
	}
	return ""
}

// failedStepLabel returns the step label of the metric marked failed=1, "".
func failedStepLabel(mf *dto.MetricFamily) string {
	for _, m := range mf.GetMetric() {
		if m.GetGauge().GetValue() == 1 {
			return labelValueFor(m, "step")
		}
	}
	return ""
}

// TestProbeHTTPScenarioHappyPath exercises a login step that sets a cookie and
// returns a token, followed by a resource step that must present both. A
// non-secret token is also sent to a second origin while the secret cookie is
// withheld.
func TestProbeHTTPScenarioHappyPath(t *testing.T) {
	const secret = "super-secret-session"
	const csrf = "csrf-token-123"

	var origin2TokenSeen atomic.Bool

	// Origin 1: /login issues cookie + token, /resource requires them.
	origin1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login":
			http.SetCookie(w, &http.Cookie{Name: "session", Value: secret, Path: "/"})
			w.Header().Set("X-CSRF", csrf)
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "ok")
		case "/resource":
			cookie, err := r.Cookie("session")
			if err != nil || cookie.Value != secret {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if r.Header.Get("X-CSRF") != csrf {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "resource ok")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer origin1.Close()

	// Origin 2 accepts the non-secret token in the query string and must
	// never receive the session cookie.
	origin2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("session"); err == nil && c.Value != "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.URL.Query().Get("token") != csrf {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		origin2TokenSeen.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	defer origin2.Close()
	// Go's cookie jar does not distinguish ports, so use a distinct hostname
	// for the second origin (localhost resolves to the same loopback address).
	origin2URL := strings.Replace(origin2.URL, "127.0.0.1", "localhost", 1)

	module := newScenarioModule([]config.HTTPScenarioStep{
		{
			Name: "login",
			URL:  origin1.URL + "/login",
			Extract: []config.HTTPScenarioExtract{
				{Name: "session", Source: "cookie", Key: "session"},
				{Name: "token", Source: "header", Key: "X-CSRF"},
			},
		},
		{
			Name:    "resource",
			URL:     origin1.URL + "/resource",
			Headers: map[string]string{"X-CSRF": "${token}"},
		},
		{
			Name: "cross_origin_non_secret",
			URL:  origin2URL + "/echo?token=${token}",
		},
	})

	registry := prometheus.NewRegistry()
	logger, buf := captureLogger()
	result := ProbeHTTP(context.Background(), origin1.URL, module, registry, logger)

	if !result {
		t.Fatalf("scenario should succeed, logs:\n%s", buf.String())
	}
	if !origin2TokenSeen.Load() {
		t.Fatal("origin 2 never received the non-secret token")
	}
	if strings.Contains(buf.String(), secret) {
		t.Fatalf("secret cookie value leaked into logs:\n%s", buf.String())
	}

	mfs, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	checkRegistryResults(map[string]float64{
		"probe_http_scenario_steps": 3,
	}, mfs, t)
	failed := gatherMetric(t, registry, "probe_http_scenario_step_failed")
	for _, m := range failed.GetMetric() {
		if m.GetGauge().GetValue() != 0 {
			t.Fatalf("expected no failed step, got %v", m)
		}
	}
}

// TestProbeHTTPScenarioBodyExtractAndJSONBody extracts a value from a JSON
// response body and re-inserts it into a JSON request body, checking that
// JSON escaping is applied.
func TestProbeHTTPScenarioBodyExtractAndJSONBody(t *testing.T) {
	const code = `ab"cd\ef`
	var gotBody atomic.Pointer[string]

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/start" {
			fmt.Fprintf(w, `{"code":"%s"}`, code)
			return
		}
		b, _ := io.ReadAll(r.Body)
		s := string(b)
		gotBody.Store(&s)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	module := newScenarioModule([]config.HTTPScenarioStep{
		{
			Name: "start",
			URL:  ts.URL + "/start",
			Extract: []config.HTTPScenarioExtract{
				{Name: "code", Source: bodySource, Regexp: mustRegexp(`"code":"(.+)"}\s*$`)},
			},
		},
		{
			Name:    "echo",
			URL:     ts.URL + "/echo",
			Headers: map[string]string{"Content-Type": "application/json"},
			Body:    `{"code":"${code}"}`,
		},
	})

	registry := prometheus.NewRegistry()
	logger, buf := captureLogger()
	if !ProbeHTTP(context.Background(), ts.URL, module, registry, logger) {
		t.Fatalf("scenario failed, logs:\n%s", buf.String())
	}
	want := `{"code":"ab\"cd\\ef"}`
	if got := gotBody.Load(); got == nil || *got != want {
		gotStr := ""
		if got != nil {
			gotStr = *got
		}
		t.Fatalf("unexpected body %q, want %q", gotStr, want)
	}
}

const bodySource = "body"

// TestProbeHTTPScenarioMissingVariable: referencing an undefined variable
// must fail at the step that uses it.
func TestProbeHTTPScenarioMissingVariable(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	module := newScenarioModule([]config.HTTPScenarioStep{
		{Name: "first", URL: ts.URL},
		{Name: "second", URL: ts.URL + "/?v=${missing}"},
	})

	registry := prometheus.NewRegistry()
	logger, buf := captureLogger()
	result := ProbeHTTP(context.Background(), ts.URL, module, registry, logger)
	if result {
		t.Fatal("scenario should fail because the variable is undefined")
	}
	if !strings.Contains(buf.String(), "undefined variable") || !strings.Contains(buf.String(), "missing") {
		t.Fatalf("expected undefined variable error, logs:\n%s", buf.String())
	}
	failed := gatherMetric(t, registry, "probe_http_scenario_step_failed")
	if got := failedStepLabel(failed); got != "2" {
		t.Fatalf("failure should be reported on step 2, got label %q", got)
	}
	for _, m := range failed.GetMetric() {
		if labelValueFor(m, "step") == "2" && m.GetGauge().GetValue() != 1 {
			t.Fatal("step 2 should be marked failed")
		}
	}
}

// TestProbeHTTPScenarioMissingExtraction: a variable that cannot be found in
// the response fails the extracting step itself.
func TestProbeHTTPScenarioMissingExtraction(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	module := newScenarioModule([]config.HTTPScenarioStep{
		{
			Name: "expects_header",
			URL:  ts.URL,
			Extract: []config.HTTPScenarioExtract{
				{Name: "token", Source: "header", Key: "X-Token"},
			},
		},
		{Name: "never", URL: ts.URL},
	})

	registry := prometheus.NewRegistry()
	logger, buf := captureLogger()
	result := ProbeHTTP(context.Background(), ts.URL, module, registry, logger)
	if result {
		t.Fatal("scenario should fail when the extracted header is absent")
	}
	failed := gatherMetric(t, registry, "probe_http_scenario_step_failed")
	if got := failedStepLabel(failed); got != "1" {
		t.Fatalf("failure should be reported on step 1, logs:\n%s", buf.String())
	}
}

// TestProbeHTTPScenarioRedirectLoop: a per-step redirect chain exceeding the
// limit fails that step and reports its index.
func TestProbeHTTPScenarioRedirectLoop(t *testing.T) {
	var hits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ok" {
			w.WriteHeader(http.StatusOK)
			return
		}
		hits.Add(1)
		http.Redirect(w, r, r.URL.Path, http.StatusFound)
	}))
	defer ts.Close()

	module := newScenarioModule([]config.HTTPScenarioStep{
		{Name: "ok", URL: ts.URL + "/ok", ValidStatusCodes: []int{http.StatusOK}},
		{Name: "loop", URL: ts.URL + "/loop"},
	})

	registry := prometheus.NewRegistry()
	logger, buf := captureLogger()
	result := ProbeHTTP(context.Background(), ts.URL, module, registry, logger)
	if result {
		t.Fatal("scenario should fail on the redirect loop")
	}
	if !strings.Contains(buf.String(), "Too many redirects") {
		t.Fatalf("expected redirect-loop error, logs:\n%s", buf.String())
	}
	failed := gatherMetric(t, registry, "probe_http_scenario_step_failed")
	if got := failedStepLabel(failed); got != "2" {
		t.Fatalf("failure should be reported on step 2, got %q", got)
	}
	redirects := gatherMetric(t, registry, "probe_http_scenario_step_redirects")
	var seenLoop bool
	for _, m := range redirects.GetMetric() {
		if labelValueFor(m, "step") == "2" {
			seenLoop = true
			if m.GetGauge().GetValue() != float64(maxScenarioRedirects+1) {
				t.Fatalf("expected %d redirects, got %v", maxScenarioRedirects+1, m.GetGauge().GetValue())
			}
		}
	}
	if !seenLoop {
		t.Fatal("no redirect metric for step 2")
	}
}

// TestProbeHTTPScenarioStepTimeout: the second step exceeding its own
// timeout fails at step 2.
func TestProbeHTTPScenarioStepTimeout(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			select {
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
			}
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	module := newScenarioModule([]config.HTTPScenarioStep{
		{Name: "fast", URL: ts.URL + "/fast"},
		{Name: "slow", URL: ts.URL + "/slow", Timeout: 200 * time.Millisecond},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	registry := prometheus.NewRegistry()
	logger, buf := captureLogger()
	start := time.Now()
	result := ProbeHTTP(ctx, ts.URL, module, registry, logger)
	elapsed := time.Since(start)
	if result {
		t.Fatal("scenario should fail when a step times out")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("step timeout was not honoured, elapsed %v", elapsed)
	}
	if !strings.Contains(buf.String(), "Step timed out") {
		t.Fatalf("expected step timeout log, logs:\n%s", buf.String())
	}
	failed := gatherMetric(t, registry, "probe_http_scenario_step_failed")
	if got := failedStepLabel(failed); got != "2" {
		t.Fatalf("failure should be reported on step 2, got %q", got)
	}
}

// TestProbeHTTPScenarioCrossOriginSecretDropped: a secret extracted at one
// origin must not be sent to another. The step fails before any request is
// made, and the value is absent from logs and metric labels.
func TestProbeHTTPScenarioCrossOriginSecretDropped(t *testing.T) {
	const secret = "credential-abc"

	var origin2Hits atomic.Int32
	origin1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "session", Value: secret, Path: "/"})
		w.WriteHeader(http.StatusOK)
	}))
	defer origin1.Close()
	origin2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		origin2Hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer origin2.Close()

	module := newScenarioModule([]config.HTTPScenarioStep{
		{
			Name: "login",
			URL:  origin1.URL + "/login",
			Extract: []config.HTTPScenarioExtract{
				{Name: "session", Source: "cookie", Key: "session"},
			},
		},
		{
			Name:    "leak_url",
			URL:     origin2.URL + "/?s=${session}",
			Headers: map[string]string{"Authorization": "Bearer ${session}"},
		},
	})

	registry := prometheus.NewRegistry()
	logger, buf := captureLogger()
	result := ProbeHTTP(context.Background(), origin1.URL, module, registry, logger)
	if result {
		t.Fatal("scenario must fail when a secret is sent to another origin")
	}
	if origin2Hits.Load() != 0 {
		t.Fatal("origin 2 must never be contacted")
	}
	logs := buf.String()
	if strings.Contains(logs, secret) {
		t.Fatalf("secret leaked into logs:\n%s", logs)
	}

	mfs, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if strings.Contains(l.GetValue(), secret) {
					t.Fatalf("secret leaked into metric label %s=%s", l.GetName(), l.GetValue())
				}
			}
		}
	}
	failed := gatherMetric(t, registry, "probe_http_scenario_step_failed")
	if got := failedStepLabel(failed); got != "2" {
		t.Fatalf("failure should be reported on step 2, got %q", got)
	}
}

func labelValueFor(m *dto.Metric, name string) string {
	for _, l := range m.GetLabel() {
		if l.GetName() == name {
			return l.GetValue()
		}
	}
	return ""
}

// TestScenarioVariableEncoding verifies the encoding rules for URL, header
// and body insertion.
func TestScenarioVariableEncoding(t *testing.T) {
	sv := newScenarioVars()
	const val = `a b&c=d/e?f#g "h"` + "\t"
	sv.set("v", val, "http://a", false)

	out, dropped, err := sv.expand("/p/${v}", "http://a", encodeURLPath)
	if err != nil || dropped {
		t.Fatalf("path expand: %v %v", err, dropped)
	}
	if strings.ContainsAny(out, " ?#") || !strings.Contains(out, "/p/") {
		t.Fatalf("path value not escaped: %q", out)
	}

	out, _, err = sv.expand("?x=${v}", "http://a", encodeURLQuery)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "?x=a+b%26c%3Dd") && !strings.HasPrefix(out, "?x=a%20b%26c%3Dd") {
		t.Fatalf("query value not escaped: %q", out)
	}

	jsonEnc, err := encodeBodyJSON(val)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(jsonEnc, `"`) && !strings.Contains(jsonEnc, `\"`) {
		t.Fatalf("json value not escaped: %q", jsonEnc)
	}

	if _, err := encodeHeader("x\r\nInjected: y"); err == nil {
		t.Fatal("CR/LF in a header value must be rejected")
	}
}

// TestScenarioSecretScrubbedFromLogs builds a redacting logger and ensures a
// secret value, in raw and URL-escaped form, never appears.
func TestScenarioSecretScrubbedFromLogs(t *testing.T) {
	const secret = "topsecret value"
	sv := newScenarioVars()
	sv.set("token", secret, "http://a", true)

	buf := &bytes.Buffer{}
	base := slog.New(slog.NewTextHandler(buf, nil))
	logger := slog.New(&redactingHandler{next: base.Handler(), vars: sv})

	logger.Error("request failed", "url", "http://a/?t="+url.QueryEscape(secret))
	logger.Error("raw", "err", errString(secret))
	logger.Info("userinfo", "url", "https://user:hunter2@a/path?code=zzz")

	out := buf.String()
	for _, leak := range []string{secret, url.QueryEscape(secret), "hunter2", "code=zzz"} {
		if strings.Contains(out, leak) {
			t.Fatalf("log output contains %q:\n%s", leak, out)
		}
	}
}

type errString string

func (e errString) Error() string { return string(e) }
