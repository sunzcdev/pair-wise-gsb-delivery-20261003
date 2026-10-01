package goproxy

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net/http"
)

// buildForwardRequest creates the request actually sent to one upstream line.
// It uses the absolute-form request target (as required for forward proxies),
// strips hop-by-hop and proxy control headers, and attaches only this line's
// Proxy-Authorization credential.
func buildForwardRequest(req *http.Request, line *UpstreamLine, body *requestBody) *http.Request {
	out := req.Clone(req.Context())
	out.RequestURI = ""

	for _, h := range hopByHopRequestHeaders {
		out.Header.Del(h)
	}
	// This proxy's control header must never travel beyond this hop.
	out.Header.Del(UpstreamPreferenceHeader)

	if out.Host == "" {
		out.Host = req.Host
	}
	out.Header.Set("Connection", "close")

	if line.Username != "" || line.Password != "" {
		out.Header.Set("Proxy-Authorization", line.proxyAuthorization())
	}

	switch {
	case body.replayable:
		out.ContentLength = int64(len(body.buffered))
		out.TransferEncoding = nil
		if len(body.buffered) > 0 {
			out.Body = io.NopCloser(bytes.NewReader(body.buffered))
			// Request an auth verdict before transmitting the body, so a 407
			// from this line is still a safe point to switch lines.
			out.Header.Set("Expect", "100-continue")
		} else {
			out.Body = http.NoBody
		}
	case body.knownLength:
		out.ContentLength = req.ContentLength
		out.TransferEncoding = nil
		out.Header.Set("Expect", "100-continue")
		out.Body = nil // streamed from the client in forwardOnce
	default:
		out.ContentLength = -1
		out.TransferEncoding = []string{"chunked"}
		out.Header.Set("Expect", "100-continue")
		out.Body = nil // streamed from the client in forwardOnce
	}

	return out
}

// writeRequestHead serializes the request line and headers (without the body)
// in absolute form.
func writeRequestHead(w io.Writer, req *http.Request) error {
	target := req.URL.String()
	if target == "" {
		target = req.RequestURI
	}
	if target == "" {
		return fmt.Errorf("missing request target")
	}

	host := req.Host
	if host == "" {
		host = req.URL.Host
	}

	bw := bufio.NewWriter(w)
	if _, err := fmt.Fprintf(bw, "%s %s HTTP/1.1\r\n", req.Method, target); err != nil {
		return err
	}

	headers := req.Header.Clone()
	if host != "" {
		headers.Set("Host", host)
	}
	if err := writeHeaders(bw, headers); err != nil {
		return err
	}
	if _, err := io.WriteString(bw, "\r\n"); err != nil {
		return err
	}
	return bw.Flush()
}

func writeHeaders(w io.Writer, headers http.Header) error {
	for key, values := range headers {
		for _, value := range values {
			if _, err := fmt.Fprintf(w, "%s: %s\r\n", key, value); err != nil {
				return err
			}
		}
	}
	return nil
}

// writeBody sends the body using the framing advertised in the request head:
// Content-Length framing or chunked transfer encoding.
func writeBody(w io.Writer, req *http.Request, body io.Reader) error {
	if len(req.TransferEncoding) > 0 && req.TransferEncoding[0] == "chunked" {
		return writeChunks(w, body)
	}
	_, err := io.Copy(w, body)
	return err
}

func writeChunks(w io.Writer, body io.Reader) error {
	buf := make([]byte, 32*1024)
	for {
		n, readErr := body.Read(buf)
		if n > 0 {
			if _, err := fmt.Fprintf(w, "%x\r\n", n); err != nil {
				return err
			}
			if _, err := w.Write(buf[:n]); err != nil {
				return err
			}
			if _, err := io.WriteString(w, "\r\n"); err != nil {
				return err
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			return readErr
		}
	}
	_, err := io.WriteString(w, "0\r\n\r\n")
	return err
}
