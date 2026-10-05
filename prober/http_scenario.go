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
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/textproto"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	pconfig "github.com/prometheus/common/config"
	"golang.org/x/net/publicsuffix"

	"github.com/prometheus/blackbox_exporter/config"
)

// maxScenarioRedirects bounds the redirect chain of a single scenario
// step. A redirect loop therefore fails the step instead of spinning
// until the probe timeout.
const maxScenarioRedirects = 10

// scenarioVariable is a value captured during a scenario run. It only
// lives for the duration of a single probe.
type scenarioVariable struct {
	value  string
	secret bool
	// origin is the scheme://host[:port] the value was captured from.
	// Secret variables are only ever sent back to this origin.
	origin string
}

// templateContext selects the encoding rules applied to interpolated
// variable values.
type templateContext int

const (
	templateURL templateContext = iota
	templateHeader
	templateBody
)

// scenarioOrigin returns the origin (scheme://host[:port]) of a URL.
func scenarioOrigin(u *url.URL) string {
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host)
}

// interpolateScenarioTemplate expands `${name}` references in a
// template using the encoding rules of the given context:
//   - URLs: values are percent-encoded (query-component rules).
//   - Headers: values must not contain CR/LF, otherwise the expansion
//     fails to prevent header injection.
//   - Bodies: values are inserted verbatim.
//
// It never exposes variable values in its errors.
func interpolateScenarioTemplate(template string, vars map[string]scenarioVariable, ctx templateContext) (string, error) {
	var interpErr error
	out := config.ScenarioVariableRefRegexp.ReplaceAllStringFunc(template, func(match string) string {
		if interpErr != nil {
			return match
		}
		name := config.ScenarioVariableRefRegexp.FindStringSubmatch(match)[1]
		v, ok := vars[name]
		if !ok {
			interpErr = fmt.Errorf("variable %q is not defined", name)
			return match
		}
		switch ctx {
		case templateURL:
			return url.QueryEscape(v.value)
		case templateHeader:
			if strings.ContainsAny(v.value, "\r\n") {
				interpErr = fmt.Errorf("variable %q contains CR/LF and cannot be used in a header", name)
				return match
			}
			return v.value
		default:
			return v.value
		}
	})
	if interpErr != nil {
		return "", interpErr
	}
	return out, nil
}

// checkSecretOrigins enforces the cross-origin rule for secrets: a
// secret variable captured from one origin must not be sent to a
// different one. The secret is dropped by failing the step before any
// request is made.
func checkSecretOrigins(step *config.ScenarioStep, vars map[string]scenarioVariable, requestOrigin string) error {
	refs := config.FindScenarioVariableRefs(step.URL)
	refs = append(refs, config.FindScenarioVariableRefs(step.Body)...)
	for _, h := range step.Headers {
		refs = append(refs, config.FindScenarioVariableRefs(h)...)
	}
	for _, name := range refs {
		v, ok := vars[name]
		if !ok {
			continue // Missing variables are reported during interpolation.
		}
		if v.secret && v.origin != requestOrigin {
			return fmt.Errorf("refusing to send secret variable %q to origin %q: it was captured from a different origin", name, requestOrigin)
		}
	}
	return nil
}

// extractScenarioVariables captures the configured variables from a
// step response. Extracted values are never logged.
func extractScenarioVariables(step *config.ScenarioStep, resp *http.Response, body []byte, origin string, vars map[string]scenarioVariable, logger *slog.Logger) error {
	for _, e := range step.Extract {
		var source string
		switch e.Source {
		case "body":
			source = string(body)
		case "header":
			source = resp.Header.Get(e.Key)
			if source == "" {
				return fmt.Errorf("header %q not found, cannot extract variable %q", e.Key, e.Name)
			}
		case "cookie":
			idx := slices.IndexFunc(resp.Cookies(), func(c *http.Cookie) bool { return c.Name == e.Key })
			if idx < 0 {
				return fmt.Errorf("cookie %q not found, cannot extract variable %q", e.Key, e.Name)
			}
			source = resp.Cookies()[idx].Value
		}

		value := source
		if e.Regexp.Regexp != nil {
			m := e.Regexp.FindStringSubmatch(source)
			if m == nil {
				return fmt.Errorf("regexp did not match %s, cannot extract variable %q", e.Source, e.Name)
			}
			if len(m) > 1 {
				value = m[1]
			} else {
				value = m[0]
			}
		}

		vars[e.Name] = scenarioVariable{value: value, secret: e.Secret, origin: origin}
		logger.Debug("Extracted scenario variable", "step", step.Name, "variable", e.Name, "secret", e.Secret)
	}
	return nil
}

// ProbeHTTPScenario runs a multi-step HTTP scenario against the target.
// Cookies and extracted variables are scoped to a single probe run and
// discarded afterwards.
func ProbeHTTPScenario(ctx context.Context, target string, module config.Module, registry *prometheus.Registry, logger *slog.Logger) (success bool) {
	var (
		stepDurationGaugeVec = prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "probe_http_scenario_step_duration_seconds",
			Help: "Duration of each scenario step in seconds",
		}, []string{"step"})
		stepsCompletedGauge = prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "probe_http_scenario_steps_completed",
			Help: "The number of scenario steps that completed successfully",
		})
		stepFailedGaugeVec = prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "probe_http_scenario_step_failed",
			Help: "Indicates which scenario step failed, if any",
		}, []string{"step"})
	)

	registry.MustRegister(stepDurationGaugeVec)
	registry.MustRegister(stepsCompletedGauge)
	registry.MustRegister(stepFailedGaugeVec)

	scenario := module.HTTPScenario

	targetLower := strings.ToLower(target)
	if !strings.HasPrefix(targetLower, "http://") && !strings.HasPrefix(targetLower, "https://") {
		target = "http://" + target
	}
	targetURL, err := url.Parse(target)
	if err != nil {
		logger.Error("Could not parse target URL", "err", err)
		return false
	}

	client, err := pconfig.NewClientFromConfig(scenario.HTTPClientConfig, "http_scenario_probe", pconfig.WithKeepAlivesDisabled())
	if err != nil {
		logger.Error("Error generating HTTP client", "err", err)
		return false
	}

	// The cookie jar is created per probe run: cookies never leak
	// across probes.
	jar, err := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	if err != nil {
		logger.Error("Error generating cookiejar", "err", err)
		return false
	}
	client.Jar = jar

	client.CheckRedirect = func(_ *http.Request, via []*http.Request) error {
		if !scenario.HTTPClientConfig.FollowRedirects {
			return http.ErrUseLastResponse
		}
		if len(via) >= maxScenarioRedirects {
			return fmt.Errorf("stopped after %d redirects", maxScenarioRedirects)
		}
		return nil
	}

	vars := map[string]scenarioVariable{}
	completed := 0

	failStep := func(stepName string, err error) bool {
		// Only the step name (from the configuration) is logged, never
		// variable values.
		logger.Error("Scenario step failed", "step", stepName, "err", err)
		stepFailedGaugeVec.WithLabelValues(stepName).Set(1)
		stepsCompletedGauge.Set(float64(completed))
		return false
	}

	for i := range scenario.Steps {
		step := &scenario.Steps[i]
		stepLogger := logger.With("step", step.Name)
		stepStart := time.Now()

		method := step.Method
		if method == "" {
			method = http.MethodGet
		}

		// Resolve the step URL against the target. Relative URLs are
		// resolved against the target, absolute URLs may point the
		// step at a different server entirely.
		urlTemplate := step.URL
		if urlTemplate == "" {
			urlTemplate = target
		}
		stepURL, err := interpolateScenarioTemplate(urlTemplate, vars, templateURL)
		if err != nil {
			return failStep(step.Name, fmt.Errorf("error interpolating URL: %w", err))
		}
		requestURL, err := url.Parse(stepURL)
		if err != nil {
			return failStep(step.Name, fmt.Errorf("error parsing step URL: %w", err))
		}
		if !requestURL.IsAbs() {
			requestURL = targetURL.ResolveReference(requestURL)
		}
		requestOrigin := scenarioOrigin(requestURL)

		// Enforce the cross-origin rule for secrets before building
		// the request: a secret captured from one origin is dropped
		// (and the step failed) rather than leaked to another.
		if err := checkSecretOrigins(step, vars, requestOrigin); err != nil {
			return failStep(step.Name, err)
		}

		var body io.Reader
		if step.Body != "" {
			interpolatedBody, err := interpolateScenarioTemplate(step.Body, vars, templateBody)
			if err != nil {
				return failStep(step.Name, fmt.Errorf("error interpolating body: %w", err))
			}
			body = strings.NewReader(interpolatedBody)
		}

		request, err := http.NewRequestWithContext(ctx, method, requestURL.String(), body)
		if err != nil {
			return failStep(step.Name, fmt.Errorf("error creating request: %w", err))
		}

		for key, valueTemplate := range step.Headers {
			value, err := interpolateScenarioTemplate(valueTemplate, vars, templateHeader)
			if err != nil {
				return failStep(step.Name, fmt.Errorf("error interpolating header %q: %w", key, err))
			}
			if textproto.CanonicalMIMEHeaderKey(key) == "Host" {
				request.Host = value
				continue
			}
			request.Header.Set(key, value)
		}
		if _, hasUserAgent := request.Header["User-Agent"]; !hasUserAgent {
			request.Header.Set("User-Agent", userAgentDefaultHeader)
		}

		// Log the template URL, never the interpolated one, so secret
		// values cannot end up in the logs.
		stepLogger.Debug("Making scenario step request", "method", method, "url_template", urlTemplate)

		resp, err := client.Do(request)
		if err != nil {
			return failStep(step.Name, fmt.Errorf("error for HTTP request: %w", err))
		}

		respBody, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return failStep(step.Name, fmt.Errorf("error reading response body: %w", err))
		}

		stepDurationGaugeVec.WithLabelValues(step.Name).Set(time.Since(stepStart).Seconds())

		stepLogger.Debug("Received HTTP response", "status_code", resp.StatusCode)

		if len(step.ValidStatusCodes) != 0 {
			if !slices.Contains(step.ValidStatusCodes, resp.StatusCode) {
				return failStep(step.Name, fmt.Errorf("invalid HTTP response status code %d, valid status codes are %v", resp.StatusCode, step.ValidStatusCodes))
			}
		} else if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return failStep(step.Name, fmt.Errorf("invalid HTTP response status code %d, wanted 2xx", resp.StatusCode))
		}

		for _, headerMatchSpec := range step.FailIfHeaderMatchesRegexp {
			values := resp.Header[textproto.CanonicalMIMEHeaderKey(headerMatchSpec.Header)]
			if len(values) == 0 {
				if !headerMatchSpec.AllowMissing {
					return failStep(step.Name, fmt.Errorf("missing required header %q", headerMatchSpec.Header))
				}
				continue
			}
			if slices.ContainsFunc(values, headerMatchSpec.Regexp.MatchString) {
				return failStep(step.Name, fmt.Errorf("header %q matched regular expression %q", headerMatchSpec.Header, headerMatchSpec.Regexp))
			}
		}

		for _, expression := range step.FailIfBodyMatchesRegexp {
			if expression.Match(respBody) {
				return failStep(step.Name, fmt.Errorf("body matched regular expression %q", expression))
			}
		}
		for _, expression := range step.FailIfBodyNotMatchesRegexp {
			if !expression.Match(respBody) {
				return failStep(step.Name, fmt.Errorf("body did not match regular expression %q", expression))
			}
		}

		if err := extractScenarioVariables(step, resp, respBody, requestOrigin, vars, stepLogger); err != nil {
			return failStep(step.Name, err)
		}

		completed++
	}

	stepsCompletedGauge.Set(float64(completed))
	logger.Info("Scenario completed", "steps_completed", completed)
	return true
}
