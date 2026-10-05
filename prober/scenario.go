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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/textproto"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	pconfig "github.com/prometheus/common/config"
	"golang.org/x/net/publicsuffix"

	"github.com/prometheus/blackbox_exporter/config"
)

// maxScenarioRedirects is the per-step redirect limit.
const maxScenarioRedirects = 10

// errTooManyRedirects marks a redirect loop or a chain that is too long.
var errTooManyRedirects = errors.New("stopped after too many redirects")

var (
	scenarioVarNameRE = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
	placeholderRE     = regexp.MustCompile(`\$\{[a-zA-Z_][a-zA-Z0-9_]*\}`)
	urlSchemeRE       = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://`)
)

// variable holds a value extracted during a scenario together with the
// origin it was extracted from. Secret variables are only sent back to that
// same origin and are redacted from log output.
type variable struct {
	value  string
	secret bool
	origin string
}

// scenarioVars is the variable table of a single probe run. It is created
// per probe and is never shared between probes.
type scenarioVars struct {
	vars map[string]variable
}

func newScenarioVars() *scenarioVars {
	return &scenarioVars{vars: map[string]variable{}}
}

func (sv *scenarioVars) set(name, value, origin string, secret bool) {
	sv.vars[name] = variable{value: value, secret: secret, origin: origin}
}

// expand replaces ${name} placeholders in tmpl using encode. A secret
// referenced by a request to an origin other than the one it was extracted
// from is dropped silently, and dropped is reported so the caller can apply
// an explicit policy for the insertion location.
func (sv *scenarioVars) expand(tmpl, requestOrigin string, encode func(string) (string, error)) (out string, dropped bool, err error) {
	var b strings.Builder
	for {
		start := strings.Index(tmpl, "${")
		if start < 0 {
			b.WriteString(tmpl)
			return b.String(), dropped, nil
		}
		b.WriteString(tmpl[:start])
		rel := tmpl[start+2:]
		end := strings.IndexByte(rel, '}')
		if end < 0 {
			return "", false, errors.New(`unterminated variable placeholder, missing "}"`)
		}
		name := rel[:end]
		if !scenarioVarNameRE.MatchString(name) {
			return "", false, fmt.Errorf("invalid variable reference %q", name)
		}
		v, ok := sv.vars[name]
		if !ok {
			return "", false, fmt.Errorf("undefined variable %q", name)
		}
		if v.secret && v.origin != requestOrigin {
			dropped = true
		} else {
			encoded, encErr := encode(v.value)
			if encErr != nil {
				return "", false, fmt.Errorf("encoding variable %q: %w", name, encErr)
			}
			b.WriteString(encoded)
		}
		tmpl = rel[end+1:]
	}
}

// expandURL expands placeholders independently in the path, query and
// fragment of rawURL, applying the escaping rules of each location.
func (sv *scenarioVars) expandURL(rawURL, requestOrigin string) (string, bool, error) {
	if !strings.Contains(rawURL, "${") {
		return rawURL, false, nil
	}
	q := strings.IndexByte(rawURL, '?')
	f := strings.IndexByte(rawURL, '#')
	pathEnd := len(rawURL)
	switch {
	case q >= 0 && f >= 0:
		pathEnd = min(q, f)
	case q >= 0:
		pathEnd = q
	default:
		pathEnd = f
	}

	pathPart := rawURL[:pathEnd]
	var queryPart, fragmentPart string
	switch {
	case q >= 0 && f >= 0 && q < f:
		queryPart = rawURL[q:f]
		fragmentPart = rawURL[f:]
	case q >= 0 && f >= 0:
		fragmentPart = rawURL[f:]
		queryPart = ""
	case q >= 0:
		queryPart = rawURL[q:]
	default:
		fragmentPart = rawURL[f:]
	}

	pathExp, pathDropped, err := sv.expand(pathPart, requestOrigin, encodeURLPath)
	if err != nil {
		return "", false, err
	}
	queryExp, queryDropped, err := sv.expand(queryPart, requestOrigin, encodeURLQuery)
	if err != nil {
		return "", false, err
	}
	fragmentExp, fragmentDropped, err := sv.expand(fragmentPart, requestOrigin, encodeURLPath)
	if err != nil {
		return "", false, err
	}
	return pathExp + queryExp + fragmentExp, pathDropped || queryDropped || fragmentDropped, nil
}

// encoders implement the encoding rules of every insertion location.

// encodeURLPath percent-escapes a value for a URL path or fragment.
func encodeURLPath(v string) (string, error) {
	return url.PathEscape(v), nil
}

// encodeURLQuery percent-escapes a value for a URL query component.
func encodeURLQuery(v string) (string, error) {
	return url.QueryEscape(v), nil
}

// encodeHeader inserts a value verbatim but rejects CR/LF bytes so a
// variable cannot inject additional headers or split the request.
func encodeHeader(v string) (string, error) {
	if strings.ContainsAny(v, "\r\n") {
		return "", errors.New("value contains a line break and cannot be placed in an HTTP header")
	}
	return v, nil
}

// encodeBodyJSON inserts a value escaped for embedding inside a JSON string
// literal; the surrounding quotes belong to the template.
func encodeBodyJSON(v string) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(raw[1 : len(raw)-1]), nil
}

// encodeBodyForm escapes a value for an application/x-www-form-urlencoded
// body.
func encodeBodyForm(v string) (string, error) {
	return url.QueryEscape(v), nil
}

// encodeBodyRaw inserts a value without transformation.
func encodeBodyRaw(v string) (string, error) {
	return v, nil
}

// bodyEncoder selects body escaping rules from the request Content-Type.
func bodyEncoder(contentType string) func(string) (string, error) {
	mediaType := contentType
	if i := strings.IndexByte(mediaType, ';'); i >= 0 {
		mediaType = mediaType[:i]
	}
	switch strings.ToLower(strings.TrimSpace(mediaType)) {
	case "application/json":
		return encodeBodyJSON
	case "application/x-www-form-urlencoded":
		return encodeBodyForm
	default:
		return encodeBodyRaw
	}
}

// redactingHandler is a slog.Handler that removes every live secret value
// (in raw, query-escaped and path-escaped form) as well as userinfo and
// query strings of URLs from emitted records. Secret variables therefore
// never reach the logs, regardless of where a library placed them in an
// error message.
type redactingHandler struct {
	next slog.Handler
	vars *scenarioVars
}

func (h *redactingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *redactingHandler) Handle(ctx context.Context, r slog.Record) error {
	rec := slog.NewRecord(r.Time, r.Level, h.scrub(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		rec.AddAttrs(h.scrubAttr(a))
		return true
	})
	return h.next.Handle(ctx, rec)
}

func (h *redactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	scrubbed := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		scrubbed[i] = h.scrubAttr(a)
	}
	return &redactingHandler{next: h.next.WithAttrs(scrubbed), vars: h.vars}
}

func (h *redactingHandler) WithGroup(name string) slog.Handler {
	return &redactingHandler{next: h.next.WithGroup(name), vars: h.vars}
}

func (h *redactingHandler) scrub(s string) string {
	for name, v := range h.vars.vars {
		if !v.secret || v.value == "" {
			continue
		}
		for _, form := range []string{v.value, url.QueryEscape(v.value), url.PathEscape(v.value)} {
			if form == "" {
				continue
			}
			s = strings.ReplaceAll(s, form, "REDACTED["+name+"]")
		}
	}
	return scrubURLSecrets(s)
}

func (h *redactingHandler) scrubAttr(a slog.Attr) slog.Attr {
	switch v := a.Value.Any().(type) {
	case string:
		a.Value = slog.StringValue(h.scrub(v))
	case error:
		a.Value = slog.StringValue(h.scrub(v.Error()))
	default:
		a.Value = slog.StringValue(h.scrub(a.Value.String()))
	}
	return a
}

// scrubURLSecrets removes userinfo passwords and query/fragment strings from
// any absolute URL appearing in s, which are common places for secrets to
// leak through transport error messages.
func scrubURLSecrets(s string) string {
	var b strings.Builder
	for {
		loc := urlSchemeRE.FindStringIndex(s)
		if loc == nil {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:loc[0]])
		s = s[loc[0]:]

		// Consume the URL up to the first run of whitespace or a common
		// delimiter that terminates it.
		end := 0
		for end < len(s) {
			c := s[end]
			if c <= ' ' || c == '"' || c == '\'' || c == ')' || c == ']' {
				break
			}
			end++
		}
		match, rest := s[:end], s[end:]
		b.WriteString(scrubOneURL(match))
		s = rest
	}
}

func scrubOneURL(match string) string {
	u, err := url.Parse(match)
	if err != nil || u.Host == "" {
		return match
	}
	if u.User != nil {
		if _, set := u.User.Password(); set {
			u.User = url.UserPassword(u.User.Username(), "REDACTED")
		}
	}
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	u.RawFragment = ""
	return u.String()
}

// probeHTTPScenario runs a finite, ordered list of HTTP requests sharing a
// per-probe cookie jar and a variable table. Each response is checked and
// may contribute variables to subsequent steps. Cookies and variables live
// only for this single probe invocation.
func probeHTTPScenario(ctx context.Context, target string, module config.Module, registry *prometheus.Registry, logger *slog.Logger) (success bool) {
	stepsGauge := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "probe_http_scenario_steps",
		Help: "The total number of steps in the HTTP scenario.",
	})
	stepInfo := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "probe_http_scenario_step_info",
		Help: "Identifies a scenario step. The step label never contains variable values.",
	}, []string{"step", "step_name"})
	stepDuration := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "probe_http_scenario_step_duration_seconds",
		Help: "Duration of each scenario step.",
	}, []string{"step"})
	stepStatusCode := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "probe_http_scenario_step_status_code",
		Help: "Final HTTP status code observed for each scenario step.",
	}, []string{"step"})
	stepRedirects := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "probe_http_scenario_step_redirects",
		Help: "Number of redirects followed by each scenario step.",
	}, []string{"step"})
	stepFailed := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "probe_http_scenario_step_failed",
		Help: "1 for the step that failed the scenario, 0 otherwise.",
	}, []string{"step"})

	registry.MustRegister(stepsGauge, stepInfo, stepDuration, stepStatusCode, stepRedirects, stepFailed)

	targetLower := strings.ToLower(target)
	if !strings.HasPrefix(targetLower, "http://") && !strings.HasPrefix(targetLower, "https://") {
		target = "http://" + target
	}
	baseURL, err := url.Parse(target)
	if err != nil {
		logger.Error("Could not parse target URL", "err", err)
		return false
	}

	client, err := pconfig.NewClientFromConfig(module.HTTP.HTTPClientConfig, "http_scenario_probe", pconfig.WithKeepAlivesDisabled())
	if err != nil {
		logger.Error("Error generating HTTP client", "err", err)
		return false
	}
	jar, err := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	if err != nil {
		logger.Error("Error generating cookie jar", "err", err)
		return false
	}
	client.Jar = jar

	vars := newScenarioVars()
	logger = slog.New(&redactingHandler{next: logger.Handler(), vars: vars})

	steps := module.HTTP.Scenario.Steps
	stepsGauge.Set(float64(len(steps)))

	for i, step := range steps {
		stepLabel := fmt.Sprintf("%d", i+1)
		stepName := step.Name
		stepInfo.WithLabelValues(stepLabel, stepName).Set(1)
		stepLogger := logger.With("step", i+1, "step_name", stepName)

		stepOK := runScenarioStep(ctx, &scenarioStepInput{
			step:      step,
			httpCfg:   module.HTTP,
			baseURL:   baseURL,
			client:    client,
			vars:      vars,
			logger:    stepLogger,
			duration:  stepDuration.WithLabelValues(stepLabel),
			status:    stepStatusCode.WithLabelValues(stepLabel),
			redirects: stepRedirects.WithLabelValues(stepLabel),
		})
		if !stepOK {
			stepFailed.WithLabelValues(stepLabel).Set(1)
			logger.Error("Scenario failed at step", "step", i+1, "step_name", stepName)
			return false
		}
		stepFailed.WithLabelValues(stepLabel).Set(0)
	}

	return true
}

type scenarioStepInput struct {
	step      config.HTTPScenarioStep
	httpCfg   config.HTTPProbe
	baseURL   *url.URL
	client    *http.Client
	vars      *scenarioVars
	logger    *slog.Logger
	duration  prometheus.Gauge
	status    prometheus.Gauge
	redirects prometheus.Gauge
}

func runScenarioStep(parentCtx context.Context, in *scenarioStepInput) bool {
	stepCtx := parentCtx
	var cancel context.CancelFunc
	if in.step.Timeout > 0 {
		stepCtx, cancel = context.WithTimeout(parentCtx, in.step.Timeout)
		defer cancel()
	}

	start := time.Now()
	defer func() {
		in.duration.Set(time.Since(start).Seconds())
	}()

	rawURL := in.step.URL
	if rawURL == "" {
		rawURL = in.baseURL.String()
	}

	// The request origin is known for absolute URLs. For relative URLs the
	// target origin is used, as they resolve against it.
	origin := originHint(rawURL, in.baseURL)

	// Variables must not compose the scheme/host/port: the origin used for
	// secret scoping is derived before expansion, so allowing a variable in
	// the authority would let a value change which origin a secret is sent
	// to. Path, query and fragment are safe and get location-specific
	// escaping.
	if authorityHasPlaceholder(rawURL) {
		in.logger.Error("Variables are not allowed in the scheme, host or port of a step URL")
		return false
	}

	expandedURL, urlSecretDropped, err := in.vars.expandURL(rawURL, origin)
	if err != nil {
		in.logger.Error("Failed to expand step URL", "err", err)
		return false
	}
	stepURL, err := in.baseURL.Parse(expandedURL)
	if err != nil {
		in.logger.Error("Failed to parse step URL", "err", err)
		return false
	}
	if urlSecretDropped {
		in.logger.Error("Step URL references a secret extracted from a different origin; refusing to send it", "url", safeURL(stepURL))
		return false
	}

	method := in.step.Method
	if method == "" {
		method = http.MethodGet
	}

	contentType := mergedContentType(in.httpCfg.Headers, in.step.Headers)
	body := in.step.Body
	if strings.Contains(body, "${") {
		var bodyDropped bool
		body, bodyDropped, err = in.vars.expand(body, stepURLOrigin(stepURL), bodyEncoder(contentType))
		if err != nil {
			in.logger.Error("Failed to expand step body", "err", err)
			return false
		}
		if bodyDropped {
			in.logger.Error("Step body references a secret extracted from a different origin; refusing to send it")
			return false
		}
	}

	req, err := http.NewRequestWithContext(stepCtx, method, stepURL.String(), strings.NewReader(body))
	if err != nil {
		in.logger.Error("Error creating request", "err", err)
		return false
	}

	// Module-level headers are defaults; step headers override them.
	for key, value := range in.httpCfg.Headers {
		req.Header.Set(key, value)
	}
	for key, value := range in.step.Headers {
		if textproto.CanonicalMIMEHeaderKey(key) == "Host" {
			req.Host = value
			continue
		}
		if strings.Contains(value, "${") {
			expanded, headerDropped, expandErr := in.vars.expand(value, stepURLOrigin(stepURL), encodeHeader)
			if expandErr != nil {
				in.logger.Error("Failed to expand request header", "header", key, "err", expandErr)
				return false
			}
			if headerDropped {
				// Secrets from another origin must not leak into headers.
				// Drop the whole header and continue.
				in.logger.Debug("Dropped header carrying a secret from another origin", "header", key)
				continue
			}
			value = expanded
		}
		req.Header.Set(key, value)
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", userAgentDefaultHeader)
	}

	var redirects int
	in.client.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		redirects = len(via)
		in.logger.Info("Received redirect", "location", safeURL(r.URL))
		if !in.httpCfg.HTTPClientConfig.FollowRedirects {
			return http.ErrUseLastResponse
		}
		if redirects > maxScenarioRedirects {
			return errTooManyRedirects
		}
		return nil
	}

	in.logger.Debug("Making HTTP request", "method", method, "url", safeURL(stepURL))
	resp, err := in.client.Do(req)
	in.redirects.Set(float64(redirects))

	if resp == nil {
		in.status.Set(0)
		if errors.Is(err, context.DeadlineExceeded) {
			in.logger.Error("Step timed out", "timeout", in.step.Timeout, "err", err)
		} else {
			in.logger.Error("Error executing HTTP request", "err", err)
		}
		return false
	}
	defer resp.Body.Close()
	in.status.Set(float64(resp.StatusCode))

	if err != nil && errors.Is(err, errTooManyRedirects) {
		in.logger.Error("Too many redirects, possibly a redirect loop", "redirects", redirects)
		return false
	}
	if err != nil {
		in.logger.Error("Error executing HTTP request", "err", err)
		return false
	}

	if !statusCodeAllowed(resp.StatusCode, in.step.ValidStatusCodes) {
		if len(in.step.ValidStatusCodes) != 0 {
			in.logger.Error("Invalid HTTP response status code", "status_code", resp.StatusCode, "valid_status_codes", fmt.Sprintf("%v", in.step.ValidStatusCodes))
		} else {
			in.logger.Error("Invalid HTTP response status code, wanted 2xx", "status_code", resp.StatusCode)
		}
		return false
	}

	limit := int64(in.httpCfg.BodySizeLimit)
	var reader io.Reader = resp.Body
	if limit > 0 {
		reader = io.LimitReader(resp.Body, limit)
	}
	bodyBytes, readErr := io.ReadAll(reader)
	if readErr != nil {
		in.logger.Error("Failed to read HTTP response body", "err", readErr)
		return false
	}

	stepProbe := config.HTTPProbe{
		FailIfHeaderMatchesRegexp:    in.step.FailIfHeaderMatchesRegexp,
		FailIfHeaderNotMatchesRegexp: in.step.FailIfHeaderNotMatchesRegexp,
		FailIfBodyMatchesRegexp:      in.step.FailIfBodyMatchesRegexp,
		FailIfBodyNotMatchesRegexp:   in.step.FailIfBodyNotMatchesRegexp,
	}
	if (len(stepProbe.FailIfHeaderMatchesRegexp) > 0 || len(stepProbe.FailIfHeaderNotMatchesRegexp) > 0) &&
		!matchRegularExpressionsOnHeaders(resp.Header, stepProbe, in.logger) {
		return false
	}
	if (len(stepProbe.FailIfBodyMatchesRegexp) > 0 || len(stepProbe.FailIfBodyNotMatchesRegexp) > 0) &&
		!matchRegularExpressions(bytes.NewReader(bodyBytes), stepProbe, in.logger) {
		return false
	}

	finalOrigin := stepURLOrigin(resp.Request.URL)
	for _, extract := range in.step.Extract {
		value, found, extractErr := extractVariable(extract, resp, bodyBytes)
		if extractErr != nil {
			in.logger.Error("Failed to extract variable", "variable", extract.Name, "err", extractErr)
			return false
		}
		if !found {
			in.logger.Error("Variable not found in response", "variable", extract.Name, "source", extract.Source)
			return false
		}
		secret := extract.Secret || extract.Source == "cookie"
		in.vars.set(extract.Name, value, finalOrigin, secret)
		in.logger.Debug("Extracted variable", "variable", extract.Name, "source", extract.Source, "secret", secret)
	}

	return true
}

// extractVariable resolves one extraction specification against a response.
// found is false when the named source is absent or its regexp does not
// match.
func extractVariable(spec config.HTTPScenarioExtract, resp *http.Response, body []byte) (value string, found bool, err error) {
	var raw string
	switch spec.Source {
	case "body":
		raw = string(body)
	case "header":
		values := resp.Header[textproto.CanonicalMIMEHeaderKey(spec.Key)]
		if len(values) == 0 {
			return "", false, nil
		}
		raw = values[0]
	case "cookie":
		var rawCookie string
		for _, c := range resp.Cookies() {
			if c.Name == spec.Key {
				rawCookie = c.Value
			}
		}
		if rawCookie == "" {
			return "", false, nil
		}
		raw = rawCookie
	}

	if spec.Regexp.Regexp == nil {
		return raw, true, nil
	}
	match := spec.Regexp.FindStringSubmatchIndex(raw)
	if match == nil {
		return "", false, nil
	}
	if len(match) < 4 || match[2] < 0 {
		return "", false, nil
	}
	return raw[match[2]:match[3]], true, nil
}

func statusCodeAllowed(code int, valid []int) bool {
	if len(valid) != 0 {
		return slices.Contains(valid, code)
	}
	return code >= 200 && code < 300
}

// originHint returns the origin a step URL will belong to. Relative URLs
// resolve against the target base.
func originHint(rawURL string, base *url.URL) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return stepURLOrigin(base)
	}
	resolved := base.ResolveReference(u)
	return stepURLOrigin(resolved)
}

func stepURLOrigin(u *url.URL) string {
	return u.Scheme + "://" + u.Host
}

// authorityHasPlaceholder reports whether any placeholder appears in the
// scheme/host/port portion of a step URL. Relative URLs only carry a path,
// query and fragment and are always safe.
func authorityHasPlaceholder(rawURL string) bool {
	schemeEnd := strings.Index(rawURL, "://")
	if schemeEnd < 0 {
		return false // Relative URL: only path/query/fragment.
	}
	rest := rawURL[schemeEnd+3:]
	pathStart := strings.IndexAny(rest, "/?#")
	authority := rest
	if pathStart >= 0 {
		authority = rest[:pathStart]
	}
	return placeholderRE.MatchString(authority)
}

// safeURL renders a URL without query or fragment, which may contain
// interpolated variables.
func safeURL(u *url.URL) string {
	c := *u
	c.RawQuery = ""
	c.ForceQuery = false
	c.Fragment = ""
	c.RawFragment = ""
	return c.String()
}

// mergedContentType returns the effective request Content-Type. Step headers
// take precedence over module-level headers.
func mergedContentType(moduleHeaders, stepHeaders map[string]string) string {
	for _, headers := range []map[string]string{stepHeaders, moduleHeaders} {
		for name, value := range headers {
			if textproto.CanonicalMIMEHeaderKey(name) == "Content-Type" {
				return value
			}
		}
	}
	return ""
}
