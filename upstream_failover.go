package goproxy

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"
)

// UpstreamPreferenceHeader is the request header used to select the preferred
// upstream line for a single request. Its value is a comma-separated list of
// line names in descending order of preference; unknown names are ignored.
// The header is a control header for this proxy and is never forwarded to the
// upstream proxy or the destination server.
const UpstreamPreferenceHeader = "X-Upstream-Preferred"

const (
	defaultDialTimeout      = 10 * time.Second
	defaultAuthProbeTimeout = 2 * time.Second
	// defaultMaxBufferedBody is the largest request body the proxy is willing
	// to buffer fully. A fully buffered body is replayable: if the preferred
	// line fails before consuming any of it, the exact same bytes can be sent
	// over a backup line. Larger bodies are streamed and therefore can never
	// be replayed on another line.
	defaultMaxBufferedBody int64 = 1 << 20
)

// Failover phases, reported as part of a line attempt error. They describe how
// far the request got on a given line and never contain credentials.
const (
	phaseDial          = "dial"
	phaseRequestHead   = "request-head"
	phaseAuthHandshake = "auth-handshake"
	phaseRequestBody   = "request-body"
	phaseResponse      = "response"
)

// UpstreamLine describes one upstream ("next-hop") proxy line with its own
// endpoint and its own proxy credentials. Credentials are sent to this line
// only, using a Proxy-Authorization header scoped to that single hop.
type UpstreamLine struct {
	// Name uniquely identifies the line in selectors and in error messages.
	Name string
	// Addr is the upstream proxy endpoint in host:port form. Only plain HTTP
	// proxies are supported.
	Addr string
	// Username/Password are sent as HTTP Basic proxy credentials to this line.
	Username string
	Password string
}

func (l *UpstreamLine) proxyAuthorization() string {
	token := l.Username + ":" + l.Password
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(token))
}

// LineSelector returns the names of the lines to try for a request, in order.
// The first name is the preferred line; following names are fallbacks.
type LineSelector func(req *http.Request) []string

// FailoverOption customizes an UpstreamFailover.
type FailoverOption func(*UpstreamFailover)

// WithLineSelector overrides the default ordering of upstream lines.
func WithLineSelector(selector LineSelector) FailoverOption {
	return func(f *UpstreamFailover) { f.selector = selector }
}

// WithMaxBufferedBody sets the largest request body that may be fully buffered
// (and thus replayed on a fallback). Bodies larger than this are streamed and
// cannot be retried on another line.
func WithMaxBufferedBody(max int64) FailoverOption {
	return func(f *UpstreamFailover) { f.maxBufferedBody = max }
}

// WithDialTimeout sets the timeout for establishing a TCP connection to a
// single upstream line.
func WithDialTimeout(d time.Duration) FailoverOption {
	return func(f *UpstreamFailover) { f.dialTimeout = d }
}

// WithAuthProbeTimeout sets how long the proxy waits, after sending a request
// head that carries "Expect: 100-continue", for the upstream to challenge the
// request (e.g. 407) before the request body is transmitted.
func WithAuthProbeTimeout(d time.Duration) FailoverOption {
	return func(f *UpstreamFailover) { f.authProbeTimeout = d }
}

// UpstreamFailover routes every request through an ordered list of upstream
// proxy lines, starting with the preferred line and failing over to backup
// lines only while the request is still safely replayable.
//
// It implements both forward proxying (plain HTTP requests, used as a
// RoundTripper) and CONNECT tunneling (used as ConnectDialWithReq).
type UpstreamFailover struct {
	lines            map[string]*UpstreamLine
	order            []string
	selector         LineSelector
	maxBufferedBody  int64
	dialTimeout      time.Duration
	authProbeTimeout time.Duration
}

// NewUpstreamFailover builds a failover over the given lines. The slice order
// is the default preference order; the first line is the default preferred
// line unless a LineSelector or the UpstreamPreferenceHeader says otherwise.
func NewUpstreamFailover(lines []UpstreamLine, opts ...FailoverOption) *UpstreamFailover {
	f := &UpstreamFailover{
		lines:            make(map[string]*UpstreamLine, len(lines)),
		maxBufferedBody:  defaultMaxBufferedBody,
		dialTimeout:      defaultDialTimeout,
		authProbeTimeout: defaultAuthProbeTimeout,
	}
	for i := range lines {
		line := lines[i]
		f.lines[line.Name] = &line
		f.order = append(f.order, line.Name)
	}
	f.selector = defaultLineSelector(append([]string(nil), f.order...))
	for _, opt := range opts {
		opt(f)
	}
	return f
}

// Attach wires the failover into proxy: plain HTTP (and MITM'd HTTPS) requests
// use it as their RoundTripper, and CONNECT requests use it as their dialer.
// Each line's credentials are attached per attempt, so the proxy's own inbound
// Proxy-Authorization header is never reused across lines.
func (f *UpstreamFailover) Attach(proxy *ProxyHttpServer) {
	proxy.ConnectDial = nil
	proxy.ConnectDialWithReq = f.ConnectDial
	proxy.OnRequest().DoFunc(func(req *http.Request, ctx *ProxyCtx) (*http.Request, *http.Response) {
		ctx.RoundTripper = f
		return req, nil
	})
}

// linesFor resolves the ordered, de-duplicated list of configured lines to try.
func (f *UpstreamFailover) linesFor(req *http.Request) []*UpstreamLine {
	names := f.selector(req)
	seen := make(map[string]bool, len(names))
	result := make([]*UpstreamLine, 0, len(f.lines))
	for _, name := range names {
		if line, ok := f.lines[name]; ok && !seen[name] {
			seen[name] = true
			result = append(result, line)
		}
	}
	return result
}

// defaultLineSelector honors UpstreamPreferenceHeader first and otherwise keeps
// the configured order.
func defaultLineSelector(configured []string) LineSelector {
	return func(req *http.Request) []string {
		preferred := strings.TrimSpace(req.Header.Get(UpstreamPreferenceHeader))
		if preferred == "" {
			return configured
		}
		rank := make(map[string]int)
		for i, part := range strings.Split(preferred, ",") {
			name := strings.TrimSpace(part)
			if name != "" {
				rank[name] = i
			}
		}
		ordered := append([]string(nil), configured...)
		sort.SliceStable(ordered, func(i, j int) bool {
			ri, iok := rank[ordered[i]]
			rj, jok := rank[ordered[j]]
			switch {
			case iok && jok:
				return ri < rj
			case iok:
				return true
			case jok:
				return false
			default:
				return false
			}
		})
		return ordered
	}
}

// lineAttemptError records what happened on one line. Its Error text is safe to
// return to clients: it carries only the line name, the phase and a generic
// reason, never credentials, realms or response bodies.
type lineAttemptError struct {
	line      string
	phase     string
	status    int
	bytesSent int64
	detail    string
	retryable bool
}

func (e *lineAttemptError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "line %q: %s", e.line, e.detail)
	if e.phase != "" {
		fmt.Fprintf(&b, " (%s phase)", e.phase)
	}
	if e.bytesSent > 0 {
		fmt.Fprintf(&b, "; %d request-body byte(s) already committed to this line", e.bytesSent)
	}
	return b.String()
}

// failoverError aggregates the attempts made across all tried lines. It never
// contains credentials; it only reports line names and scrubbed per-line
// reasons.
type failoverError struct {
	attempts []*lineAttemptError
	tunnel   bool
}

func (e *failoverError) Error() string {
	var b strings.Builder
	if e.tunnel {
		b.WriteString("CONNECT tunnel failed on every upstream line; tried lines:\n")
	} else {
		b.WriteString("upstream failover exhausted; tried lines:\n")
	}
	for _, attempt := range e.attempts {
		fmt.Fprintf(&b, "  - %s\n", attempt.Error())
	}
	return strings.TrimRight(b.String(), "\n")
}

// badGateway builds a client-visible 502 response whose body lists the lines
// actually attempted. Only scrubbed, credential-free text is included.
func badGateway(req *http.Request, cause *failoverError) *http.Response {
	resp := NewResponse(req, ContentTypeText, http.StatusBadGateway, cause.Error())
	resp.Status = fmt.Sprintf("%d %s", http.StatusBadGateway, http.StatusText(http.StatusBadGateway))
	return resp
}

func (f *UpstreamFailover) dialLine(ctx context.Context, addr string) (net.Conn, error) {
	dialer := net.Dialer{Timeout: f.dialTimeout}
	return dialer.DialContext(ctx, "tcp", addr)
}
