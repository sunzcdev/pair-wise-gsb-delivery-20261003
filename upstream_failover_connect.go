package goproxy

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
)

// ConnectDial implements the goproxy ConnectDialWithReq hook. It establishes a
// CONNECT tunnel to target through the preferred upstream line, failing over to
// backup lines while the tunnel handshake is still in progress.
//
// Failover is possible only before the tunnel has been established: a CONNECT
// handshake exchanges no target data, so a refused connection or an upstream
// challenge (407) can be retried on another line. Once an upstream replies 200,
// the tunnel is handed to the caller and no further failover occurs even if the
// connection breaks immediately afterward.
func (f *UpstreamFailover) ConnectDial(req *http.Request, _ string, target string) (net.Conn, error) {
	var attempts []*lineAttemptError
	for _, line := range f.linesFor(req) {
		conn, attemptErr := f.connectOnce(req, line, target)
		if attemptErr == nil {
			return conn, nil
		}
		attempts = append(attempts, attemptErr)
		if !attemptErr.retryable {
			break
		}
	}
	return nil, &failoverError{attempts: attempts, tunnel: true}
}

func (f *UpstreamFailover) connectOnce(req *http.Request, line *UpstreamLine, target string) (net.Conn, *lineAttemptError) {
	conn, err := f.dialLine(req.Context(), line.Addr)
	if err != nil {
		return nil, &lineAttemptError{
			line:      line.Name,
			phase:     phaseDial,
			detail:    "could not establish connection",
			retryable: true, // no target bytes exist yet at CONNECT time
		}
	}

	var head bytes.Buffer
	fmt.Fprintf(&head, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n", target, target)
	headers := http.Header{}
	if line.Username != "" || line.Password != "" {
		headers.Set("Proxy-Authorization", line.proxyAuthorization())
	}
	if ua := req.Header.Get("User-Agent"); ua != "" {
		headers.Set("User-Agent", ua)
	}
	if err := writeHeaders(&head, headers); err != nil {
		_ = conn.Close()
		return nil, &lineAttemptError{
			line:      line.Name,
			phase:     phaseRequestHead,
			detail:    "could not write CONNECT headers",
			retryable: true,
		}
	}
	if _, err := io.WriteString(&head, "\r\n"); err != nil {
		_ = conn.Close()
		return nil, &lineAttemptError{
			line:      line.Name,
			phase:     phaseRequestHead,
			detail:    "could not finish CONNECT head",
			retryable: true,
		}
	}
	if _, err := conn.Write(head.Bytes()); err != nil {
		_ = conn.Close()
		return nil, &lineAttemptError{
			line:      line.Name,
			phase:     phaseRequestHead,
			detail:    "connection refused or reset while sending CONNECT",
			retryable: true,
		}
	}

	br := bufio.NewReader(conn)
	connectReq := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: target},
		Host:   target,
		Header: headers,
	}
	resp, err := http.ReadResponse(br, connectReq)
	if err != nil {
		_ = conn.Close()
		return nil, &lineAttemptError{
			line:      line.Name,
			phase:     phaseResponse,
			detail:    "connection closed or reset before a CONNECT response was received",
			retryable: true,
		}
	}
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		_ = conn.Close()
		retryable := resp.StatusCode == http.StatusProxyAuthRequired ||
			resp.StatusCode == http.StatusUnauthorized
		return nil, &lineAttemptError{
			line:      line.Name,
			phase:     phaseResponse,
			status:    resp.StatusCode,
			detail:    fmt.Sprintf("upstream rejected CONNECT with status %d", resp.StatusCode),
			retryable: retryable,
		}
	}

	// Tunnel established. From here on there is no failover: return a connection
	// that preserves any bytes the reader may have buffered.
	return &readBufferedConn{Conn: conn, r: br}, nil
}
