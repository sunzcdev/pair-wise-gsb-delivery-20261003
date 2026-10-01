package goproxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// UpstreamLine describes one upstream ("next hop") proxy line that may be
// used to forward a request.
//
// Credentials may be embedded directly in URL as userinfo
// (e.g. "http://user:pass@proxy-a.example:3128") or supplied separately via
// Username/Password. Either way, authentication is attached on the hop to
// this proxy only: it is sent in a Proxy-Authorization header that the
// standard transport writes on the wire without touching req.Header, and the
// line's URL is redacted in any returned error so credentials never leak.
type UpstreamLine struct {
	// Name identifies the line in logs and in UpstreamFailoverError. It must
	// be non-empty and unique within a failover configuration.
	Name string

	// URL is the upstream proxy address. Supported schemes are "http" and
	// "https". Userinfo, if present, is used as proxy credentials.
	URL string

	// Username/Password optionally provide proxy credentials separately from
	// URL. They take precedence over userinfo embedded in URL.
	Username string
	Password string

	parsedURL *url.URL
	authValue string
}

// FailoverPhase identifies at which stage an upstream line gave up.
type FailoverPhase string

const (
	// FailoverPhaseDial covers TCP dial (and, for https:// upstream proxies,
	// the TLS handshake to the proxy). A failure here means the request never
	// reached the upstream proxy and is always safe to replay on another line.
	FailoverPhaseDial FailoverPhase = "dial"

	// FailoverPhaseAuth means the upstream proxy answered 407 Proxy
	// Authentication Required. The proxy rejected the request before
	// contacting the destination, so it may be replayed on another line as
	// long as the request body can be reproduced.
	FailoverPhaseAuth FailoverPhase = "proxy-auth"

	// FailoverPhaseConnect means an upstream proxy answered a non-2xx status
	// to a CONNECT request (other than 407, which is reported separately as
	// FailoverPhaseAuth). No tunnel was established and no bytes reached the
	// destination, so trying another line is always safe.
	FailoverPhaseConnect FailoverPhase = "connect"

	// FailoverPhaseRequest means an error occurred while writing the request
	// (headers or body) to a forwarding upstream proxy, after at least one body
	// byte may have been consumed. Such a request may already have been relayed
	// towards the destination and must not be blindly replayed.
	FailoverPhaseRequest FailoverPhase = "request"
)

// UpstreamAttempt records the outcome of trying a single upstream line.
type UpstreamAttempt struct {
	// Line is the name of the line that was tried.
	Line string
	// Phase is the stage at which the line failed.
	Phase FailoverPhase
	// StatusCode is the upstream proxy's response status, when applicable
	// (407 for auth, or a non-2xx CONNECT status). Zero for transport errors.
	StatusCode int
	// Err is the underlying error. It never contains credentials: proxy URLs
	// are redacted and upstream response bodies are not included.
	Err error
}

// UpstreamSkip records a line that was never tried once failover had to stop.
type UpstreamSkip struct {
	// Line is the name of the line that was not attempted.
	Line string
	// Reason explains why the line was not tried (for example, the request
	// body could not be replayed).
	Reason string
}

// UpstreamFailoverError is returned when no upstream line could serve a
// request. It lists every line actually attempted (in order) and any lines
// left untried, without exposing credentials.
type UpstreamFailoverError struct {
	Attempts []UpstreamAttempt
	Skipped  []UpstreamSkip
}

func (e *UpstreamFailoverError) Error() string {
	var b strings.Builder
	b.WriteString("upstream failover: all proxy lines failed")
	for _, a := range e.Attempts {
		fmt.Fprintf(&b, "; %s (%s", a.Line, a.Phase)
		if a.StatusCode != 0 {
			fmt.Fprintf(&b, ", status %d", a.StatusCode)
		}
		if a.Err != nil {
			fmt.Fprintf(&b, ": %s", a.Err.Error())
		}
		b.WriteString(")")
	}
	for _, s := range e.Skipped {
		fmt.Fprintf(&b, "; %s not tried (%s)", s.Line, s.Reason)
	}
	return b.String()
}

// AsUpstreamFailoverError extracts an *UpstreamFailoverError from err, if any.
func AsUpstreamFailoverError(err error) (*UpstreamFailoverError, bool) {
	var fe *UpstreamFailoverError
	if errors.As(err, &fe) {
		return fe, true
	}
	return nil, false
}

// UpstreamLineSelector picks, per request, which configured upstream lines to
// use and in which order. The returned names must match UpstreamLine.Name
// values. Returning an empty slice means "use the configured default order".
// Returning an unknown name stops the request with an error.
//
// For CONNECT requests the original CONNECT request is passed; for regular
// forwarding the client's absolute-form request is passed.
type UpstreamLineSelector func(req *http.Request) []string

// UpstreamFailover routes requests through one of several upstream proxy
// lines, trying the preferred line first and falling back to the remaining
// lines only while the request/tunnel is still safely replayable.
//
// Fallback rules:
//   - TCP connection refused / reset / timeout, or a failed TLS handshake to
//     the proxy: the request never reached the proxy, so another line is
//     always tried (both for regular forwarding and for CONNECT).
//   - 407 Proxy Authentication Required from a line: another line is tried
//     only if the request body can be replayed (no body, or GetBody
//     available, or BufferBody enabled and the body fits).
//   - Non-2xx (other than 407) answer to CONNECT before the tunnel is
//     established: another line is always tried.
//   - Once part of a request body has been written to an upstream proxy and
//     the connection then breaks, or once the destination tunnel has replied
//     200 to CONNECT, no further line is tried: the request might already be
//     in flight at the destination and cannot be replayed.
type UpstreamFailover struct {
	// Lines are the available upstream lines. Their order is the default
	// order used when SelectLine returns nil.
	Lines []*UpstreamLine

	// SelectLine optionally chooses/reorders lines per request. When nil,
	// every request uses Lines in configuration order (first = preferred).
	SelectLine UpstreamLineSelector

	// BufferBody, when > 0, buffers request bodies up to the given number of
	// bytes so that a request otherwise without GetBody can still be replayed
	// on a second line after a connection or 407 failure. Bodies larger than
	// the limit, and streaming/chunked bodies with unknown length, are not
	// buffered and therefore are not replayed.
	BufferBody int64

	transports map[string]*http.Transport
	byName     map[string]*UpstreamLine
	proxy      *ProxyHttpServer
	initOnce   sync.Once
	initErr    error
}

const upstreamNonReplayableReason = "request body cannot be replayed"

func basicAuthValue(username, password string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(username+":"+password))
}

// init validates the configuration and builds one dedicated http.Transport
// per line. Each transport pins the proxy URL (including that line's
// userinfo), so the standard transport injects the matching
// Proxy-Authorization on this hop only.
func (f *UpstreamFailover) init(proxy *ProxyHttpServer) error {
	f.initOnce.Do(func() {
		if len(f.Lines) == 0 {
			f.initErr = errors.New("upstream failover: no upstream lines configured")
			return
		}
		f.byName = make(map[string]*UpstreamLine, len(f.Lines))
		f.transports = make(map[string]*http.Transport, len(f.Lines))

		for _, line := range f.Lines {
			if line == nil {
				f.initErr = errors.New("upstream failover: nil upstream line")
				return
			}
			if strings.TrimSpace(line.Name) == "" {
				f.initErr = errors.New("upstream failover: upstream line with empty name")
				return
			}
			if _, dup := f.byName[line.Name]; dup {
				f.initErr = fmt.Errorf("upstream failover: duplicate upstream line name %q", line.Name)
				return
			}
			u, err := url.Parse(line.URL)
			if err != nil {
				f.initErr = fmt.Errorf("upstream failover: line %q: invalid URL: %w", line.Name, err)
				return
			}
			if u.Scheme != "http" && u.Scheme != "https" {
				f.initErr = fmt.Errorf("upstream failover: line %q: unsupported proxy scheme %q", line.Name, u.Scheme)
				return
			}
			if u.Host == "" {
				f.initErr = fmt.Errorf("upstream failover: line %q: missing proxy host", line.Name)
				return
			}
			line.parsedURL = u

			// Resolve credentials: explicit fields win over URL userinfo.
			username, password := line.Username, line.Password
			if username == "" && u.User != nil {
				username = u.User.Username()
				password, _ = u.User.Password()
			}
			if username != "" {
				line.authValue = basicAuthValue(username, password)
			}

			proxyURL := redactedURLWithCreds(u, username, password)
			tr := newPerLineTransport(proxy)
			tr.Proxy = http.ProxyURL(proxyURL)
			// For https:// destinations the transport performs CONNECT
			// internally; surface the proxy's status so a 407 / non-200 can be
			// classified precisely and the next line tried when replayable.
			lineName := line.Name
			tr.OnProxyConnectResponse = func(_ context.Context, _ *url.URL, _ *http.Request, connectRes *http.Response) error {
				if connectRes.StatusCode == http.StatusOK {
					return nil
				}
				_, _ = io.Copy(io.Discard, io.LimitReader(connectRes.Body, _errorRespMaxLength))
				return &upstreamConnectStatusError{line: lineName, statusCode: connectRes.StatusCode}
			}
			f.transports[line.Name] = tr
			f.byName[line.Name] = line
		}
		f.proxy = proxy
	})
	return f.initErr
}

// newPerLineTransport builds a fresh http.Transport for one upstream line.
// http.Transport contains locks and must not be copied after use, so we copy
// configuration rather than the value itself, before any request is served.
func newPerLineTransport(proxy *ProxyHttpServer) *http.Transport {
	src := proxy.Tr
	tr := &http.Transport{}
	if src != nil {
		tr.Proxy = nil
		tr.DialContext = src.DialContext
		tr.DialTLSContext = src.DialTLSContext
		tr.DialTLS = src.DialTLS
		tr.TLSClientConfig = src.TLSClientConfig
		tr.TLSHandshakeTimeout = src.TLSHandshakeTimeout
		tr.TLSNextProto = src.TLSNextProto
		tr.DisableKeepAlives = src.DisableKeepAlives
		tr.DisableCompression = src.DisableCompression
		tr.ForceAttemptHTTP2 = src.ForceAttemptHTTP2
		tr.MaxIdleConns = src.MaxIdleConns
		tr.MaxIdleConnsPerHost = src.MaxIdleConnsPerHost
		tr.MaxConnsPerHost = src.MaxConnsPerHost
		tr.IdleConnTimeout = src.IdleConnTimeout
		tr.ResponseHeaderTimeout = src.ResponseHeaderTimeout
		tr.ExpectContinueTimeout = src.ExpectContinueTimeout
		tr.ProxyConnectHeader = src.ProxyConnectHeader
		tr.GetProxyConnectHeader = src.GetProxyConnectHeader
	} else {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	return tr
}

// upstreamConnectStatusError is produced by a line's transport when an
// upstream proxy answers CONNECT with anything but 200.
type upstreamConnectStatusError struct {
	line       string
	statusCode int
}

func (e *upstreamConnectStatusError) Error() string {
	return fmt.Sprintf("proxy refused CONNECT with status %d", e.statusCode)
}

// redactedURLWithCreds returns a copy of u that carries the resolved
// credentials but drops everything transport does not need.
func redactedURLWithCreds(u *url.URL, username, password string) *url.URL {
	out := &url.URL{Scheme: u.Scheme, Host: u.Host}
	if username != "" {
		out.User = url.UserPassword(username, password)
	}
	return out
}

// orderedLines resolves the per-request line order.
func (f *UpstreamFailover) orderedLines(req *http.Request) ([]*UpstreamLine, error) {
	if f.SelectLine == nil {
		return f.Lines, nil
	}
	names := f.SelectLine(req)
	if len(names) == 0 {
		return f.Lines, nil
	}
	out := make([]*UpstreamLine, 0, len(names))
	for _, name := range names {
		line, ok := f.byName[name]
		if !ok {
			return nil, fmt.Errorf("upstream failover: selector returned unknown line %q", name)
		}
		out = append(out, line)
	}
	return out, nil
}

// Enable wires failover into both regular HTTP forwarding (as a per-request
// RoundTripper) and CONNECT (as ConnectDialWithReq). It must be called before
// the proxy starts serving.
func (proxy *ProxyHttpServer) EnableUpstreamFailover(f *UpstreamFailover) error {
	if err := f.init(proxy); err != nil {
		return err
	}
	proxy.OnRequest().DoFunc(func(req *http.Request, ctx *ProxyCtx) (*http.Request, *http.Response) {
		ctx.RoundTripper = f
		return req, nil
	})
	// MITM'd requests inside a CONNECT tunnel inherit RoundTripper from the
	// outer CONNECT context, so install it there as well. The handler is
	// prepended and returns (nil, host) so it always runs regardless of any
	// user-registered connect action (e.g. AlwaysMitm).
	handler := FuncHttpsHandler(func(host string, ctx *ProxyCtx) (*ConnectAction, string) {
		ctx.RoundTripper = f
		return nil, host
	})
	proxy.httpsHandlers = append([]HttpsHandler{handler}, proxy.httpsHandlers...)
	proxy.ConnectDialWithReq = func(req *http.Request, network, addr string) (net.Conn, error) {
		return f.dialConnect(req, network, addr)
	}
	return nil
}

// countingReader tracks how many body bytes have been handed to the
// transport. Once > 0 a subsequent write failure may have already relayed part
// of the body, making the request non-replayable.
type countingReader struct {
	r     io.Reader
	count int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.count += int64(n)
	return n, err
}

type closeReaderFunc func() error

func (f closeReaderFunc) Close() error { return f() }

// RoundTrip implements RoundTripper.
func (f *UpstreamFailover) RoundTrip(req *http.Request, ctx *ProxyCtx) (*http.Response, error) {
	return f.roundTrip(req)
}

func (f *UpstreamFailover) roundTrip(origReq *http.Request) (*http.Response, error) {
	lines, err := f.orderedLines(origReq)
	if err != nil {
		return nil, err
	}

	req := origReq

	// Optionally buffer a bounded, fully-known body so the request can be
	// replayed on the next line even though the server-side request has no
	// GetBody.
	var bufferedBody []byte
	if f.BufferBody > 0 && req.Body != nil && req.Body != http.NoBody &&
		req.GetBody == nil && req.ContentLength >= 0 && req.ContentLength <= f.BufferBody {
		data, err := io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("upstream failover: cannot buffer request body: %w", err)
		}
		bufferedBody = data
		clone := req.Clone(req.Context())
		clone.Body = io.NopCloser(bytes.NewReader(bufferedBody))
		clone.ContentLength = int64(len(bufferedBody))
		req = clone
	}

	replay := func() (*http.Request, error) {
		if bufferedBody != nil {
			clone := req.Clone(req.Context())
			clone.Body = io.NopCloser(bytes.NewReader(bufferedBody))
			return clone, nil
		}
		switch {
		case req.Body == nil || req.Body == http.NoBody:
			return req, nil
		case req.GetBody != nil:
			body, err := req.GetBody()
			if err != nil {
				return nil, err
			}
			clone := req.Clone(req.Context())
			clone.Body = body
			return clone, nil
		case bufferedBody != nil:
			clone := req.Clone(req.Context())
			clone.Body = io.NopCloser(bytes.NewReader(bufferedBody))
			return clone, nil
		default:
			return nil, errors.New(upstreamNonReplayableReason)
		}
	}

	canReplay := func(req *http.Request) bool {
		if req.Body == nil || req.Body == http.NoBody {
			return true
		}
		if req.GetBody != nil {
			return true
		}
		return bufferedBody != nil
	}

	var attempts []UpstreamAttempt

	for i, line := range lines {
		var counter *countingReader
		if req.Body != nil && req.Body != http.NoBody {
			counter = &countingReader{r: req.Body}
			req.Body = struct {
				io.Reader
				io.Closer
			}{counter, closeReaderFunc(req.Body.Close)}
		}

		resp, rtErr := f.transports[line.Name].RoundTrip(req)

		// A 407 response (plain-HTTP forwarding) is an authentication failure
		// at the proxy hop. The proxy never forwarded the request to the
		// destination, so switching lines is safe as long as the body can be
		// replayed.
		if rtErr == nil && resp.StatusCode == http.StatusProxyAuthRequired {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, _errorRespMaxLength))
			_ = resp.Body.Close()
			attempt := UpstreamAttempt{
				Line:       line.Name,
				Phase:      FailoverPhaseAuth,
				StatusCode: http.StatusProxyAuthRequired,
				Err:        errors.New("proxy authentication required"),
			}
			if !canReplay(req) {
				attempts = append(attempts, attempt)
				return nil, f.stop(attempts, lines[i+1:], upstreamNonReplayableReason)
			}
			attempts = append(attempts, attempt)
			next, err := replay()
			if err != nil {
				return nil, f.stop(attempts, lines[i+1:], err.Error())
			}
			req = next
			continue
		}

		// For https:// destinations, CONNECT failure arrives as an error. A
		// 407 or any non-200 CONNECT answer happens before any request byte is
		// sent through the tunnel, so another line is always safe to try.
		var connectStatusErr *upstreamConnectStatusError
		if rtErr != nil && errors.As(rtErr, &connectStatusErr) {
			phase := FailoverPhaseConnect
			if connectStatusErr.statusCode == http.StatusProxyAuthRequired {
				phase = FailoverPhaseAuth
			}
			attempts = append(attempts, UpstreamAttempt{
				Line:       line.Name,
				Phase:      phase,
				StatusCode: connectStatusErr.statusCode,
				Err:        errors.New(http.StatusText(connectStatusErr.statusCode)),
			})
			// CONNECT requests carry no body; req here is the inner MITM
			// request whose body is still untouched, simply retry it.
			req = origReq
			continue
		}

		if rtErr == nil {
			return resp, nil
		}

		// Transport error. Classify it to decide whether another line may
		// still serve the request.
		phase, replayable := classifyForwardError(rtErr)
		attempt := UpstreamAttempt{
			Line:  line.Name,
			Phase: phase,
			Err:   redactURLError(rtErr),
		}

		bodyStarted := counter != nil && counter.count > 0
		if !replayable || bodyStarted || !canReplay(req) {
			attempts = append(attempts, attempt)
			reason := upstreamNonReplayableReason
			if bodyStarted {
				reason = "part of request body already sent"
			}
			return nil, f.stop(attempts, lines[i+1:], reason)
		}

		attempts = append(attempts, attempt)
		next, err := replay()
		if err != nil {
			return nil, f.stop(attempts, lines[i+1:], err.Error())
		}
		req = next
	}

	return nil, &UpstreamFailoverError{Attempts: attempts}
}

func (f *UpstreamFailover) stop(attempts []UpstreamAttempt, remaining []*UpstreamLine, reason string) error {
	fe := &UpstreamFailoverError{Attempts: attempts}
	for _, line := range remaining {
		fe.Skipped = append(fe.Skipped, UpstreamSkip{Line: line.Name, Reason: reason})
	}
	return fe
}

// classifyForwardError decides whether a transport error happened before the
// request could have been relayed to the destination.
//
// A failure establishing the connection (or TLS) to the proxy is always
// replayable. Any error surfacing while writing the request itself is treated
// as potentially non-replayable, because headers (or part of the body) may
// already have been forwarded.
func classifyForwardError(err error) (FailoverPhase, bool) {
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		// Plain dial failures carry Op "dial"; when connecting through a
		// proxy the transport wraps connection-establishment failures as
		// Op "proxyconnect". Both happen before any request byte is written.
		if opErr.Op == "dial" || strings.HasPrefix(opErr.Op, "dial ") || opErr.Op == "proxyconnect" {
			return FailoverPhaseDial, true
		}
	}

	// url.Error wraps transport errors; inspect its nested error for a dial
	// failure ("dial tcp ...: connect: connection refused").
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		msg := urlErr.Err.Error()
		var innerOp *net.OpError
		if errors.As(urlErr.Err, &innerOp) &&
			(innerOp.Op == "dial" || strings.HasPrefix(innerOp.Op, "dial ") || innerOp.Op == "proxyconnect") {
			return FailoverPhaseDial, true
		}
		if strings.Contains(msg, "dial tcp") || strings.Contains(msg, "proxyconnect") {
			return FailoverPhaseDial, true
		}
	}

	return FailoverPhaseRequest, false
}

// redactURLError strips credentials out of any URL embedded in an error.
func redactURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		if u, perr := url.Parse(ue.URL); perr == nil && u.User != nil {
			u.User = nil
			return &url.Error{Op: ue.Op, URL: u.String(), Err: ue.Err}
		}
	}
	return err
}

// dialConnect establishes a transparent CONNECT tunnel for the given target,
// trying lines in the per-request order. A line is only considered before it
// answers 200: once a tunnel to the destination is open, the returned
// connection is handed off immediately and no other line is tried, because the
// caller (handleHttps) will then write "200 Connection established" to its own
// client and begin relaying.
func (f *UpstreamFailover) dialConnect(clientReq *http.Request, network, addr string) (net.Conn, error) {
	lines, err := f.orderedLines(clientReq)
	if err != nil {
		return nil, err
	}

	var attempts []UpstreamAttempt
	for _, line := range lines {
		conn, attempt, err := f.dialConnectOne(clientReq, line, network, addr)
		if err == nil {
			return conn, nil
		}
		attempts = append(attempts, attempt)
	}
	return nil, &UpstreamFailoverError{Attempts: attempts}
}

// dialConnectOne performs the full handshake against a single line: TCP dial,
// optional TLS to the proxy, CONNECT request and status read.
func (f *UpstreamFailover) dialConnectOne(
	clientReq *http.Request,
	line *UpstreamLine,
	network, addr string,
) (net.Conn, UpstreamAttempt, error) {
	proxyHost := line.parsedURL.Host
	if !strings.Contains(proxyHost, ":") {
		if line.parsedURL.Scheme == "https" {
			proxyHost += ":443"
		} else {
			proxyHost += ":80"
		}
	}

	ctx := clientReq.Context()
	dialer := &net.Dialer{}
	conn, err := dialer.DialContext(ctx, network, proxyHost)
	if err != nil {
		return nil, UpstreamAttempt{
			Line:  line.Name,
			Phase: FailoverPhaseDial,
			Err:   redactURLError(err),
		}, err
	}

	if line.parsedURL.Scheme == "https" {
		tlsConfig := &tls.Config{}
		if f.proxy != nil && f.proxy.Tr != nil && f.proxy.Tr.TLSClientConfig != nil {
			tlsConfig = f.proxy.Tr.TLSClientConfig.Clone()
		}
		if tlsConfig.ServerName == "" {
			tlsConfig.ServerName = line.parsedURL.Hostname()
		}
		tlsConn := tls.Client(conn, tlsConfig)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, UpstreamAttempt{
				Line:  line.Name,
				Phase: FailoverPhaseDial,
				Err:   redactURLError(err),
			}, err
		}
		conn = tlsConn
	}

	connectReq := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: addr},
		Host:   addr,
		Header: make(http.Header),
	}
	if line.authValue != "" {
		connectReq.Header.Set("Proxy-Authorization", line.authValue)
	}

	if err := connectReq.Write(conn); err != nil {
		_ = conn.Close()
		return nil, UpstreamAttempt{
			Line:  line.Name,
			Phase: FailoverPhaseDial,
			Err:   redactURLError(err),
		}, err
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, connectReq)
	if err != nil {
		_ = conn.Close()
		return nil, UpstreamAttempt{
			Line:  line.Name,
			Phase: FailoverPhaseDial,
			Err:   redactURLError(err),
		}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, _errorRespMaxLength))
		_ = conn.Close()
		phase := FailoverPhaseConnect
		if resp.StatusCode == http.StatusProxyAuthRequired {
			phase = FailoverPhaseAuth
		}
		attempt := UpstreamAttempt{
			Line:       line.Name,
			Phase:      phase,
			StatusCode: resp.StatusCode,
			Err:        errors.New(http.StatusText(resp.StatusCode)),
		}
		return nil, attempt, errors.New(http.StatusText(resp.StatusCode))
	}

	// 200: the tunnel is open. Preserve any buffered bytes the proxy may have
	// sent together with the response head.
	if buffered := br.Buffered(); buffered > 0 {
		peek, _ := br.Peek(buffered)
		conn = &readBufferedConn{Conn: conn, r: io.MultiReader(bytes.NewReader(peek), conn)}
	}
	return conn, UpstreamAttempt{}, nil
}
