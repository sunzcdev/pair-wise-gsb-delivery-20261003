package goproxy

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// requestBody captures how a client request body can be consumed across
// attempts. A body is either fully buffered (replayable) or streamed from the
// client as a single reader (not replayable).
type requestBody struct {
	buffered []byte
	// prefix plus reader reconstruct the complete body when it exceeded the
	// buffer cap. reader is the untouched tail after prefix.
	reader io.ReadCloser
	prefix []byte
	// replayable reports whether the body can be sent again on another line.
	replayable bool
	// knownLength is true when the original body carried Content-Length.
	knownLength bool
}

// newReader returns a reader over the body. For a buffered body this can be
// called repeatedly; for a streamed body it reconstructs the one-shot reader
// exactly once and returns false thereafter.
func (b *requestBody) newReader() (io.Reader, bool) {
	if b.replayable {
		return bytes.NewReader(b.buffered), true
	}
	if b.reader == nil {
		return nil, false
	}
	return io.MultiReader(bytes.NewReader(b.prefix), b.reader), true
}

// prepareBody reads enough of req.Body to decide replayability. Bodies at or
// below maxBuffered are fully buffered and replayable; larger bodies keep their
// unconsumed tail in a one-shot reader and are not replayable.
func prepareBody(req *http.Request, maxBuffered int64) (*requestBody, error) {
	if req.Body == nil || req.Body == http.NoBody {
		return &requestBody{replayable: true}, nil
	}

	if req.ContentLength >= 0 && req.ContentLength <= maxBuffered {
		data, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		_ = req.Body.Close()
		return &requestBody{buffered: data, replayable: true, knownLength: true}, nil
	}

	// Unknown length or larger than the cap: peek up to cap+1 bytes to tell
	// apart "fits" and "does not fit".
	prefix, err := io.ReadAll(io.LimitReader(req.Body, maxBuffered+1))
	if err != nil {
		return nil, err
	}

	if int64(len(prefix)) <= maxBuffered {
		if _, err := req.Body.Read(make([]byte, 1)); err != io.EOF {
			if err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("request body larger than advertised")
		}
		_ = req.Body.Close()
		return &requestBody{buffered: prefix, replayable: true, knownLength: req.ContentLength >= 0}, nil
	}

	return &requestBody{
		prefix:      prefix,
		reader:      req.Body,
		replayable:  false,
		knownLength: req.ContentLength >= 0,
	}, nil
}

// hopByHopRequestHeaders are stripped from the request before it is sent to an
// upstream proxy. Proxy-Authorization is removed as well because each line
// attaches its own credential; a client-supplied value must never leak across
// lines or to the destination.
var hopByHopRequestHeaders = []string{
	"Proxy-Connection",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Proxy-Authentication-Info",
	"Connection",
	"Keep-Alive",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

// RoundTrip implements RoundTripper for ordinary forward-proxy requests. It
// tries the preferred line first and only falls over to a backup line while the
// request has not yet committed any body bytes (and, for streamed bodies,
// while the body is still replayable).
func (f *UpstreamFailover) RoundTrip(req *http.Request, ctx *ProxyCtx) (*http.Response, error) {
	body, err := prepareBody(req, f.maxBufferedBody)
	if err != nil {
		return nil, err
	}

	var attempts []*lineAttemptError
	for _, line := range f.linesFor(req) {
		if len(attempts) > 0 && !body.replayable {
			// A non-replayable body must never be sent twice.
			break
		}
		resp, attemptErr := f.forwardOnce(req, line, body)
		if attemptErr == nil {
			return resp, nil
		}
		attempts = append(attempts, attemptErr)
		ctx.Warnf("upstream line %q failed: %s", line.Name, attemptErr.detail)
		if !attemptErr.retryable {
			break
		}
	}

	return badGateway(req, &failoverError{attempts: attempts}), nil
}

// forwardOnce performs a single attempt over one line. The returned error's
// retryable flag tells the caller whether the same request may safely go to
// another line.
func (f *UpstreamFailover) forwardOnce(
	req *http.Request,
	line *UpstreamLine,
	body *requestBody,
) (*http.Response, *lineAttemptError) {
	conn, err := f.dialLine(req.Context(), line.Addr)
	if err != nil {
		return nil, &lineAttemptError{
			line:      line.Name,
			phase:     phaseDial,
			detail:    "could not establish connection",
			retryable: body.replayable,
		}
	}
	connClosed := false
	defer func() {
		if !connClosed {
			_ = conn.Close()
		}
	}()

	out := buildForwardRequest(req, line, body)

	// Send the request head. No body byte is committed yet, so a failure here
	// only abandons a connection on which the request could not have been
	// processed. Retrying another line is safe as long as the body is
	// replayable.
	var head bytes.Buffer
	if err := writeRequestHead(&head, out); err != nil {
		return nil, &lineAttemptError{
			line:      line.Name,
			phase:     phaseRequestHead,
			detail:    "could not serialize request",
			retryable: body.replayable,
		}
	}
	if _, err := conn.Write(head.Bytes()); err != nil {
		return nil, &lineAttemptError{
			line:      line.Name,
			phase:     phaseRequestHead,
			detail:    "connection refused or reset while sending request head",
			retryable: body.replayable,
		}
	}

	br := bufio.NewReader(conn)
	hasBody := body.hasContent()

	// With "Expect: 100-continue", let the upstream challenge proxy
	// authentication before any body byte is transmitted. A 407 seen here proves
	// the upstream did not consume the body, so switching lines is safe.
	if hasBody && expectContinue(out) {
		finalResp, earlyStatus := f.probeContinue(conn, br)
		if earlyStatus != nil {
			earlyStatus.line = line.Name
			earlyStatus.phase = phaseAuthHandshake
			earlyStatus.retryable = earlyStatus.retryable && body.replayable
			return nil, earlyStatus
		}
		if finalResp != nil {
			_ = finalResp.Body.Close()
			retryable := finalResp.StatusCode == http.StatusProxyAuthRequired ||
				finalResp.StatusCode == http.StatusUnauthorized
			return nil, &lineAttemptError{
				line:      line.Name,
				phase:     phaseAuthHandshake,
				status:    finalResp.StatusCode,
				detail:    fmt.Sprintf("upstream rejected request with status %d before request body", finalResp.StatusCode),
				retryable: retryable && body.replayable,
			}
		}
	}

	if hasBody {
		reader, ok := body.newReader()
		if !ok {
			return nil, &lineAttemptError{
				line:   line.Name,
				phase:  phaseRequestBody,
				detail: "request body is not replayable; aborting to avoid a second send",
			}
		}
		counter := &countingWriter{w: conn}
		if err := writeBody(counter, out, reader); err != nil {
			return nil, &lineAttemptError{
				line:      line.Name,
				phase:     phaseRequestBody,
				detail:    "connection broke while writing request body",
				bytesSent: counter.n,
				// Once body bytes are committed the request may have reached the
				// origin; another line must never receive a replay.
				retryable: counter.n == 0 && body.replayable,
			}
		}
	}

	_ = conn.SetReadDeadline(time.Time{})
	resp, err := http.ReadResponse(br, out)
	if err != nil {
		return nil, &lineAttemptError{
			line:   line.Name,
			phase:  phaseResponse,
			detail: "connection closed or reset before an upstream response head was received",
		}
	}

	// The upstream already consumed the request. A proxy auth challenge at this
	// point is terminal: replaying on another line could double-submit. Surface
	// a 502 (via failoverError) instead of silently forwarding the 407.
	if resp.StatusCode == http.StatusProxyAuthRequired || resp.StatusCode == http.StatusUnauthorized {
		_ = resp.Body.Close()
		return nil, &lineAttemptError{
			line:   line.Name,
			phase:  phaseResponse,
			status: resp.StatusCode,
			detail: fmt.Sprintf("upstream proxy authentication failed (status %d) after request was sent", resp.StatusCode),
		}
	}

	resp.Body = &connBody{reader: resp.Body, conn: conn}
	connClosed = true
	return resp, nil
}

func (b *requestBody) hasContent() bool {
	if b.replayable {
		return len(b.buffered) > 0
	}
	return true
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// connBody closes the upstream connection when the response body is closed.
type connBody struct {
	reader io.ReadCloser
	conn   net.Conn
}

func (b *connBody) Read(p []byte) (int, error) { return b.reader.Read(p) }

func (b *connBody) Close() error {
	err := b.reader.Close()
	if b.conn != nil {
		_ = b.conn.Close()
	}
	return err
}

func expectContinue(req *http.Request) bool {
	return strings.EqualFold(req.Header.Get("Expect"), "100-continue")
}

// probeContinue waits briefly for an interim 100 or a final status.
// final is nil when the body may be sent (a 100 arrived, or the upstream did
// not answer within the probe window). earlyStatus describes a transport-level
// failure before any status was received.
func (f *UpstreamFailover) probeContinue(conn net.Conn, br *bufio.Reader) (final *http.Response, earlyStatus *lineAttemptError) {
	_ = conn.SetReadDeadline(time.Now().Add(f.authProbeTimeout))
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()

	if _, err := br.Peek(1); err != nil {
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			return nil, nil // upstream ignores Expect; proceed with the body
		}
		return nil, &lineAttemptError{
			detail:    "connection closed while waiting for proxy authentication verdict",
			retryable: true,
		}
	}

	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodPost})
	if err != nil {
		return nil, &lineAttemptError{
			detail:    "could not parse upstream interim response",
			retryable: true,
		}
	}
	if resp.StatusCode == http.StatusContinue {
		_ = resp.Body.Close()
		return nil, nil
	}
	return resp, nil
}
