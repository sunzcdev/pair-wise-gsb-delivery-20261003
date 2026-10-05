package ws

import (
	"bufio"
	"errors"
	"hash/fnv"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// ErrHandshakePlanStale is returned when a handshake plan is applied to a
// request that differs from the request the plan was prepared for.
//
// The plan is bound to the request fields that participate in the handshake
// validation. Any modification of these fields between planning and upgrade
// invalidates the plan.
var ErrHandshakePlanStale = RejectConnectionError(
	RejectionStatus(http.StatusBadRequest),
	RejectionReason("handshake error: plan does not match the request"),
)

// ErrHandshakePlanConsumed is returned when a handshake plan is consumed more
// than once.
//
// A plan is consumed by the first successful HandshakePlan.Upgrade() call and
// must never be reused.
var ErrHandshakePlanConsumed = RejectConnectionError(
	RejectionStatus(http.StatusInternalServerError),
	RejectionReason("handshake error: plan has already been consumed"),
)

// HandshakeRejection describes a structured WebSocket handshake rejection.
//
// It is returned by HTTPUpgrader.Preview() instead of writing anything to the
// connection. The application can inspect it to decide how to reject the
// request on its own. Calling HTTPUpgrader.Upgrade() on the same request
// writes the corresponding HTTP error response.
type HandshakeRejection struct {
	// Status is the HTTP status code that should be sent to the client.
	Status int

	// Header is the optional list of additional HTTP response headers that
	// accompany the rejection (such as Sec-WebSocket-Version on 426).
	Header HandshakeHeader

	// Reason is the human readable rejection reason.
	Reason string

	err error
}

// Error implements the error interface.
func (r *HandshakeRejection) Error() string {
	return r.Reason
}

// Unwrap returns the underlying handshake error, if any. This allows
// errors.Is() matching against the sentinel handshake errors such as
// ErrHandshakeBadUpgrade.
func (r *HandshakeRejection) Unwrap() error {
	return r.err
}

// HandshakePlan is the result of a side-effect free handshake preflight.
//
// It holds everything needed to perform the actual WebSocket upgrade without
// invoking the negotiation callbacks again: the selected subprotocol, the
// negotiated extensions and the additional response headers.
//
// A plan is bound to the request fields that participated in validation. The
// plan is single-use: once Upgrade() succeeds it is marked as consumed, and
// any modification of the bound request fields invalidates it.
type HandshakePlan struct {
	// Handshake holds the negotiated subprotocol and extensions.
	Handshake

	// Header holds the additional headers that will be written to the
	// successful upgrade response.
	Header HandshakeHeader

	nonce    []byte
	finger   uint64
	timeout  time.Duration
	consumed int32
}

// Preview performs a side-effect free WebSocket handshake preflight.
//
// Unlike Upgrade it neither hijacks the connection nor writes any response. On
// success it returns a HandshakePlan describing the final subprotocol,
// extensions and response headers. On failure it returns a
// *HandshakeRejection describing how the request would be rejected.
//
// Note that the configured Protocol, Extension and Negotiate callbacks are
// invoked during Preview. They are not invoked again when the returned plan is
// consumed by Upgrade().
func (u HTTPUpgrader) Preview(r *http.Request) (*HandshakePlan, error) {
	plan, err := u.prepare(r)
	if err != nil {
		return nil, reject(err)
	}
	return plan, nil
}

// PreviewHTTP is like HTTPUpgrader{}.Preview().
func PreviewHTTP(r *http.Request) (*HandshakePlan, error) {
	return DefaultHTTPUpgrader.Preview(r)
}

// Upgrade performs the actual WebSocket upgrade using a plan previously
// returned by Preview.
//
// It hijacks the connection from w, validates that the request fields still
// match the ones the plan was prepared for and that the plan has not been
// consumed yet, and then writes the successful 101 response using the plan
// data. No negotiation callback is invoked again.
//
// If the request has been modified since planning, ErrHandshakePlanStale is
// returned and no response is written to w. If the plan has already been
// consumed, ErrHandshakePlanConsumed is returned.
func (p *HandshakePlan) Upgrade(r *http.Request, w http.ResponseWriter) (net.Conn, *bufio.ReadWriter, Handshake, error) {
	if atomic.LoadInt32(&p.consumed) != 0 {
		return nil, nil, Handshake{}, ErrHandshakePlanConsumed
	}
	if requestFingerprint(r) != p.finger {
		return nil, nil, Handshake{}, ErrHandshakePlanStale
	}

	conn, rw, err := hijack(w)
	if err != nil {
		httpError(w, err.Error(), http.StatusInternalServerError)
		return conn, rw, Handshake{}, err
	}

	// Consume the plan only after the request matched and the connection is
	// ready to be written.
	if !atomic.CompareAndSwapInt32(&p.consumed, 0, 1) {
		return conn, rw, Handshake{}, ErrHandshakePlanConsumed
	}

	conn.SetDeadline(noDeadline)
	if t := p.timeout; t != 0 {
		conn.SetWriteDeadline(time.Now().Add(t))
		defer conn.SetWriteDeadline(noDeadline)
	}

	httpWriteResponseUpgrade(rw.Writer, p.nonce, p.Handshake, p.headerFunc())
	if err := rw.Writer.Flush(); err != nil {
		return conn, rw, p.Handshake, err
	}
	return conn, rw, p.Handshake, nil
}

func (p *HandshakePlan) headerFunc() HandshakeHeaderFunc {
	if p.Header == nil {
		return nil
	}
	return p.Header.WriteTo
}

// prepare runs the handshake validation and negotiation without touching the
// connection. It is the single source of truth shared by Preview() and the
// direct Upgrade() path so that rejection statuses stay consistent.
func (u HTTPUpgrader) prepare(r *http.Request) (plan *HandshakePlan, err error) {
	plan = &HandshakePlan{
		finger:  requestFingerprint(r),
		timeout: u.Timeout,
	}
	if h := u.Header; h != nil {
		plan.Header = HandshakeHeaderHTTP(h)
	}

	// See https://tools.ietf.org/html/rfc6455#section-4.1
	// The method of the request MUST be GET, and the HTTP version MUST be at
	// least 1.1.
	var nonce string
	if r.Method != http.MethodGet {
		err = ErrHandshakeBadMethod
	} else if r.ProtoMajor < 1 || (r.ProtoMajor == 1 && r.ProtoMinor < 1) {
		err = ErrHandshakeBadProtocol
	} else if r.Host == "" {
		err = ErrHandshakeBadHost
	} else if up := httpGetHeader(r.Header, headerUpgradeCanonical); up != "websocket" && !strings.EqualFold(up, "websocket") {
		err = ErrHandshakeBadUpgrade
	} else if c := httpGetHeader(r.Header, headerConnectionCanonical); c != "Upgrade" && !strHasToken(c, "upgrade") {
		err = ErrHandshakeBadConnection
	} else if nonce = httpGetHeader(r.Header, headerSecKeyCanonical); len(nonce) != nonceSize {
		err = ErrHandshakeBadSecKey
	} else if v := httpGetHeader(r.Header, headerSecVersionCanonical); v != "13" {
		// According to RFC6455 an unsupported version results in 426 Upgrade
		// Required, while a missing or empty version is a plain 400.
		if v != "" {
			err = ErrHandshakeUpgradeRequired
		} else {
			err = ErrHandshakeBadSecVersion
		}
	}
	// Copy the nonce so the plan does not retain a reference to the request
	// header backing array and stays usable independently of the request.
	plan.nonce = []byte(nonce)

	var hs Handshake
	if check := u.Protocol; err == nil && check != nil {
		ps := r.Header[headerSecProtocolCanonical]
		for i := 0; i < len(ps) && err == nil && hs.Protocol == ""; i++ {
			var ok bool
			hs.Protocol, ok = strSelectProtocol(ps[i], check)
			if !ok {
				err = ErrMalformedRequest
			}
		}
	}
	if f := u.Negotiate; err == nil && f != nil {
		for _, h := range r.Header[headerSecExtensionsCanonical] {
			hs.Extensions, err = negotiateExtensions(strToBytes(h), hs.Extensions, f)
			if err != nil {
				break
			}
		}
	}
	// DEPRECATED path.
	if check := u.Extension; err == nil && check != nil && u.Negotiate == nil {
		xs := r.Header[headerSecExtensionsCanonical]
		for i := 0; i < len(xs) && err == nil; i++ {
			var ok bool
			hs.Extensions, ok = btsSelectExtensions(strToBytes(xs[i]), hs.Extensions, check)
			if !ok {
				err = ErrMalformedRequest
			}
		}
	}
	if err != nil {
		// Return the plan anyway so callers can still access plan.Header when
		// writing the rejection response. Preview drops it on error.
		return plan, err
	}
	plan.Handshake = hs
	return plan, nil
}

// reject converts a raw handshake error into a structured HandshakeRejection,
// preserving the underlying error for errors.Is() matching.
func reject(err error) *HandshakeRejection {
	rej := &HandshakeRejection{
		Status: http.StatusInternalServerError,
		Reason: err.Error(),
		err:    err,
	}
	var connRej *ConnectionRejectedError
	if errors.As(err, &connRej) {
		rej.Status = connRej.code
		rej.Header = connRej.header
	}
	return rej
}

// writeUpgradeRejection writes a handshake rejection onto an already hijacked
// connection, using the same response format for both the direct Upgrade path
// and any plan-based upgrade.
func writeUpgradeRejection(rw *bufio.ReadWriter, plan *HandshakePlan, err error) {
	status := http.StatusInternalServerError
	if rej, ok := err.(*ConnectionRejectedError); ok {
		status = rej.code
	}
	var hh handshakeHeader
	if plan != nil {
		hh[0] = plan.Header
	}
	if rej, ok := err.(*ConnectionRejectedError); ok && rej.header != nil {
		hh[1] = rej.header
	}
	httpWriteResponseError(rw.Writer, err, status, hh.WriteTo)
	_ = rw.Writer.Flush()
}

// requestFingerprint binds a plan to the request fields that participate in
// handshake validation. Any change of these fields between planning and
// upgrade produces a different fingerprint and invalidates the plan.
func requestFingerprint(r *http.Request) uint64 {
	h := fnv.New64a()
	writeFingerString(h, r.Method)
	writeFingerInt(h, r.ProtoMajor)
	writeFingerInt(h, r.ProtoMinor)
	writeFingerString(h, r.Host)
	for _, key := range []string{
		headerUpgradeCanonical,
		headerConnectionCanonical,
		headerSecKeyCanonical,
		headerSecVersionCanonical,
		headerSecProtocolCanonical,
		headerSecExtensionsCanonical,
	} {
		for _, value := range r.Header[key] {
			writeFingerString(h, key)
			writeFingerString(h, value)
		}
	}
	return h.Sum64()
}

func writeFingerString(h interface{ Write([]byte) (int, error) }, s string) {
	_, _ = h.Write([]byte(s))
	_, _ = h.Write([]byte{0})
}

func writeFingerInt(h interface{ Write([]byte) (int, error) }, n int) {
	var buf [20]byte
	_, _ = h.Write(strconv.AppendInt(buf[:0], int64(n), 10))
	_, _ = h.Write([]byte{0})
}
