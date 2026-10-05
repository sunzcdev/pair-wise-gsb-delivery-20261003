package ws

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gobwas/httphead"
	"github.com/gobwas/pool/pbufio"
)

// Constants used by ConnUpgrader.
const (
	DefaultServerReadBufferSize  = 4096
	DefaultServerWriteBufferSize = 512
)

// Errors used by both client and server when preparing WebSocket handshake.
var (
	ErrHandshakeBadProtocol = RejectConnectionError(
		RejectionStatus(http.StatusHTTPVersionNotSupported),
		RejectionReason("handshake error: bad HTTP protocol version"),
	)
	ErrHandshakeBadMethod = RejectConnectionError(
		RejectionStatus(http.StatusMethodNotAllowed),
		RejectionReason("handshake error: bad HTTP request method"),
	)
	ErrHandshakeBadHost = RejectConnectionError(
		RejectionStatus(http.StatusBadRequest),
		RejectionReason(fmt.Sprintf("handshake error: bad %q header", headerHost)),
	)
	ErrHandshakeBadUpgrade = RejectConnectionError(
		RejectionStatus(http.StatusBadRequest),
		RejectionReason(fmt.Sprintf("handshake error: bad %q header", headerUpgrade)),
	)
	ErrHandshakeBadConnection = RejectConnectionError(
		RejectionStatus(http.StatusBadRequest),
		RejectionReason(fmt.Sprintf("handshake error: bad %q header", headerConnection)),
	)
	ErrHandshakeBadSecAccept = RejectConnectionError(
		RejectionStatus(http.StatusBadRequest),
		RejectionReason(fmt.Sprintf("handshake error: bad %q header", headerSecAccept)),
	)
	ErrHandshakeBadSecKey = RejectConnectionError(
		RejectionStatus(http.StatusBadRequest),
		RejectionReason(fmt.Sprintf("handshake error: bad %q header", headerSecKey)),
	)
	ErrHandshakeBadSecVersion = RejectConnectionError(
		RejectionStatus(http.StatusBadRequest),
		RejectionReason(fmt.Sprintf("handshake error: bad %q header", headerSecVersion)),
	)
)

// ErrMalformedResponse is returned by Dialer to indicate that server response
// can not be parsed.
var ErrMalformedResponse = fmt.Errorf("malformed HTTP response")

// ErrMalformedRequest is returned when HTTP request can not be parsed.
var ErrMalformedRequest = RejectConnectionError(
	RejectionStatus(http.StatusBadRequest),
	RejectionReason("malformed HTTP request"),
)

// ErrHandshakeUpgradeRequired is returned by Upgrader to indicate that
// connection is rejected because given WebSocket version is malformed.
//
// According to RFC6455:
// If this version does not match a version understood by the server, the
// server MUST abort the WebSocket handshake described in this section and
// instead send an appropriate HTTP error code (such as 426 Upgrade Required)
// and a |Sec-WebSocket-Version| header field indicating the version(s) the
// server is capable of understanding.
var ErrHandshakeUpgradeRequired = RejectConnectionError(
	RejectionStatus(http.StatusUpgradeRequired),
	RejectionHeader(HandshakeHeaderString(headerSecVersion+": 13\r\n")),
	RejectionReason(fmt.Sprintf("handshake error: bad %q header", headerSecVersion)),
)

// ErrNotHijacker is an error returned when http.ResponseWriter does not
// implement http.Hijacker interface.
var ErrNotHijacker = RejectConnectionError(
	RejectionStatus(http.StatusInternalServerError),
	RejectionReason("given http.ResponseWriter is not a http.Hijacker"),
)

// ErrHandshakePlanMismatch is returned by HTTPUpgrader.UpgradeWithPlan when
// the given request does not match the request fields the HandshakePlan was
// prepared for.
var ErrHandshakePlanMismatch = RejectConnectionError(
	RejectionStatus(http.StatusBadRequest),
	RejectionReason("handshake error: request does not match the handshake plan"),
)

// ErrHandshakePlanConsumed is returned by HTTPUpgrader.UpgradeWithPlan when
// the given HandshakePlan was already consumed by a previous upgrade.
var ErrHandshakePlanConsumed = RejectConnectionError(
	RejectionStatus(http.StatusBadRequest),
	RejectionReason("handshake error: handshake plan is already consumed"),
)

// DefaultHTTPUpgrader is an HTTPUpgrader that holds no options and is used by
// UpgradeHTTP function.
var DefaultHTTPUpgrader HTTPUpgrader

// UpgradeHTTP is like HTTPUpgrader{}.Upgrade().
func UpgradeHTTP(r *http.Request, w http.ResponseWriter) (net.Conn, *bufio.ReadWriter, Handshake, error) {
	return DefaultHTTPUpgrader.Upgrade(r, w)
}

// PrepareHTTP is like HTTPUpgrader{}.Prepare().
func PrepareHTTP(r *http.Request) (*HandshakePlan, error) {
	return DefaultHTTPUpgrader.Prepare(r)
}

// DefaultUpgrader is an Upgrader that holds no options and is used by Upgrade
// function.
var DefaultUpgrader Upgrader

// Upgrade is like Upgrader{}.Upgrade().
func Upgrade(conn io.ReadWriter) (Handshake, error) {
	return DefaultUpgrader.Upgrade(conn)
}

// HTTPUpgrader contains options for upgrading connection to websocket from
// net/http Handler arguments.
type HTTPUpgrader struct {
	// Timeout is the maximum amount of time an Upgrade() will spent while
	// writing handshake response.
	//
	// The default is no timeout.
	Timeout time.Duration

	// Header is an optional http.Header mapping that could be used to
	// write additional headers to the handshake response.
	//
	// Note that if present, it will be written in any result of handshake.
	Header http.Header

	// Protocol is the select function that is used to select subprotocol from
	// list requested by client. If this field is set, then the first matched
	// protocol is sent to a client as negotiated.
	Protocol func(string) bool

	// Extension is the select function that is used to select extensions from
	// list requested by client. If this field is set, then the all matched
	// extensions are sent to a client as negotiated.
	//
	// Deprecated: use Negotiate instead.
	Extension func(httphead.Option) bool

	// Negotiate is the callback that is used to negotiate extensions from
	// the client's offer. If this field is set, then the returned non-zero
	// extensions are sent to the client as accepted extensions in the
	// response.
	//
	// The argument is only valid until the Negotiate callback returns.
	//
	// If returned error is non-nil then connection is rejected and response is
	// sent with appropriate HTTP error code and body set to error message.
	//
	// RejectConnectionError could be used to get more control on response.
	Negotiate func(httphead.Option) (httphead.Option, error)
}

// Upgrade upgrades http connection to the websocket connection.
//
// It hijacks net.Conn from w and returns received net.Conn and
// bufio.ReadWriter. On successful handshake it returns Handshake struct
// describing handshake info.
func (u HTTPUpgrader) Upgrade(r *http.Request, w http.ResponseWriter) (conn net.Conn, rw *bufio.ReadWriter, hs Handshake, err error) {
	// Hijack connection first to get the ability to write rejection errors the
	// same way as in Upgrader.
	conn, rw, err = hijack(w)
	if err != nil {
		httpError(w, err.Error(), http.StatusInternalServerError)
		return conn, rw, hs, err
	}

	plan, err := u.prepare(r)
	if plan != nil {
		hs = plan.Handshake
	}
	err = u.respond(conn, rw, plan, err)
	return conn, rw, hs, err
}

// HandshakePlan is a result of a successful HTTPUpgrader.Prepare call. It
// describes the handshake that will be performed when the plan is consumed
// by HTTPUpgrader.UpgradeWithPlan: the negotiated subprotocol, extensions
// and the additional response headers.
//
// A plan is bound to the request fields that took part in its validation
// (method, protocol version, host and the WebSocket handshake headers). If
// any of these fields is modified after the plan was prepared, consuming
// the plan fails with ErrHandshakePlanMismatch.
//
// A plan is single-use: consuming it twice fails with
// ErrHandshakePlanConsumed.
type HandshakePlan struct {
	// Handshake is the handshake info that will be reported after the
	// upgrade: the negotiated subprotocol and extensions.
	Handshake

	// Header is the additional headers that will be written into the
	// handshake response.
	Header http.Header

	// nonce is the Sec-WebSocket-Key value captured during Prepare.
	nonce string

	// bound is the snapshot of the request fields the plan is bound to.
	bound planBinding

	// used reports whether the plan was already consumed.
	used int32
}

// Prepare checks r to be a valid WebSocket upgrade request and runs the
// subprotocol and extensions negotiation, without hijacking the connection
// and without writing anything to the response.
//
// On success it returns a HandshakePlan that could be consumed later by
// UpgradeWithPlan to perform the actual upgrade. The negotiation callbacks
// (Protocol, Negotiate, Extension) are called only here and are never
// called again when the plan is consumed.
//
// On failure it returns an error describing the rejection. If the error is
// a *ConnectionRejectedError, it carries the same HTTP status code, reason
// and headers that Upgrade would write into the rejection response for the
// same request.
func (u HTTPUpgrader) Prepare(r *http.Request) (*HandshakePlan, error) {
	return u.prepare(r)
}

// prepare runs the request validation and negotiation shared by Prepare and
// Upgrade. It returns either a complete HandshakePlan or a rejection error.
func (u HTTPUpgrader) prepare(r *http.Request) (plan *HandshakePlan, err error) {
	var hs Handshake

	// See https://tools.ietf.org/html/rfc6455#section-4.1
	// The method of the request MUST be GET, and the HTTP version MUST be at least 1.1.
	var nonce string
	if r.Method != http.MethodGet {
		err = ErrHandshakeBadMethod
	} else if r.ProtoMajor < 1 || (r.ProtoMajor == 1 && r.ProtoMinor < 1) {
		err = ErrHandshakeBadProtocol
	} else if r.Host == "" {
		err = ErrHandshakeBadHost
	} else if u := httpGetHeader(r.Header, headerUpgradeCanonical); u != "websocket" && !strings.EqualFold(u, "websocket") {
		err = ErrHandshakeBadUpgrade
	} else if c := httpGetHeader(r.Header, headerConnectionCanonical); c != "Upgrade" && !strHasToken(c, "upgrade") {
		err = ErrHandshakeBadConnection
	} else if nonce = httpGetHeader(r.Header, headerSecKeyCanonical); len(nonce) != nonceSize {
		err = ErrHandshakeBadSecKey
	} else if v := httpGetHeader(r.Header, headerSecVersionCanonical); v != "13" {
		// According to RFC6455:
		//
		// If this version does not match a version understood by the server,
		// the server MUST abort the WebSocket handshake described in this
		// section and instead send an appropriate HTTP error code (such as 426
		// Upgrade Required) and a |Sec-WebSocket-Version| header field
		// indicating the version(s) the server is capable of understanding.
		//
		// So we branching here cause empty or not present version does not
		// meet the ABNF rules of RFC6455:
		//
		// version = DIGIT | (NZDIGIT DIGIT) |
		// ("1" DIGIT DIGIT) | ("2" DIGIT DIGIT)
		// ; Limited to 0-255 range, with no leading zeros
		//
		// That is, if version is really invalid – we sent 426 status, if it
		// not present or empty – it is 400.
		if v != "" {
			err = ErrHandshakeUpgradeRequired
		} else {
			err = ErrHandshakeBadSecVersion
		}
	}
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
		return nil, err
	}
	return &HandshakePlan{
		Handshake: hs,
		Header:    u.Header,
		nonce:     nonce,
		bound:     bindHandshakeRequest(r),
	}, nil
}

// UpgradeWithPlan upgrades http connection to the websocket connection
// using the plan previously prepared by Prepare for the same request.
//
// It does not run the negotiation callbacks again: the subprotocol,
// extensions and response headers are taken from the plan as is.
//
// If the request fields that took part in the plan validation were modified
// since Prepare, the connection is rejected with ErrHandshakePlanMismatch.
// If the plan was already consumed, the connection is rejected with
// ErrHandshakePlanConsumed.
func (u HTTPUpgrader) UpgradeWithPlan(r *http.Request, w http.ResponseWriter, plan *HandshakePlan) (conn net.Conn, rw *bufio.ReadWriter, hs Handshake, err error) {
	// Hijack connection first to get the ability to write rejection errors
	// the same way as in Upgrade.
	conn, rw, err = hijack(w)
	if err != nil {
		httpError(w, err.Error(), http.StatusInternalServerError)
		return conn, rw, hs, err
	}

	err = plan.consume(r)
	if err == nil {
		hs = plan.Handshake
	}
	err = u.respond(conn, rw, plan, err)
	return conn, rw, hs, err
}

// consume marks the plan as consumed after checking that the request still
// matches the fields the plan was bound to.
func (p *HandshakePlan) consume(r *http.Request) error {
	if p == nil || !p.bound.matches(r) {
		return ErrHandshakePlanMismatch
	}
	if !atomic.CompareAndSwapInt32(&p.used, 0, 1) {
		return ErrHandshakePlanConsumed
	}
	return nil
}

// respond writes the handshake response into the hijacked connection. On
// nil err it writes the successful upgrade response described by plan,
// otherwise it writes the rejection response for err.
func (u HTTPUpgrader) respond(conn net.Conn, rw *bufio.ReadWriter, plan *HandshakePlan, err error) error {
	// Clear deadlines set by server.
	conn.SetDeadline(noDeadline)
	if t := u.Timeout; t != 0 {
		conn.SetWriteDeadline(time.Now().Add(t))
		defer conn.SetWriteDeadline(noDeadline)
	}

	var header handshakeHeader
	h := u.Header
	if plan != nil {
		h = plan.Header
	}
	if h != nil {
		header[0] = HandshakeHeaderHTTP(h)
	}
	if err == nil {
		httpWriteResponseUpgrade(rw.Writer, strToBytes(plan.nonce), plan.Handshake, header.WriteTo)
		err = rw.Writer.Flush()
	} else {
		var code int
		if rej, ok := err.(*ConnectionRejectedError); ok {
			code = rej.code
			header[1] = rej.header
		}
		if code == 0 {
			code = http.StatusInternalServerError
		}
		httpWriteResponseError(rw.Writer, err, code, header.WriteTo)
		// Do not store Flush() error to not override already existing one.
		_ = rw.Writer.Flush()
	}
	return err
}

// planBinding is a snapshot of the http.Request fields that take part in
// the handshake validation and negotiation. A HandshakePlan is bound to
// these fields and could be consumed only by a request that still matches
// them.
type planBinding struct {
	method        string
	protoMajor    int
	protoMinor    int
	host          string
	upgrade       string
	connection    string
	secKey        string
	secVersion    string
	secProtocol   []string
	secExtensions []string
}

// bindHandshakeRequest snapshots the request fields that take part in the
// handshake validation and negotiation.
func bindHandshakeRequest(r *http.Request) planBinding {
	return planBinding{
		method:        r.Method,
		protoMajor:    r.ProtoMajor,
		protoMinor:    r.ProtoMinor,
		host:          r.Host,
		upgrade:       httpGetHeader(r.Header, headerUpgradeCanonical),
		connection:    httpGetHeader(r.Header, headerConnectionCanonical),
		secKey:        httpGetHeader(r.Header, headerSecKeyCanonical),
		secVersion:    httpGetHeader(r.Header, headerSecVersionCanonical),
		secProtocol:   append([]string(nil), r.Header[headerSecProtocolCanonical]...),
		secExtensions: append([]string(nil), r.Header[headerSecExtensionsCanonical]...),
	}
}

// matches reports whether r still holds the same values of the bound
// fields.
func (b planBinding) matches(r *http.Request) bool {
	return b.method == r.Method &&
		b.protoMajor == r.ProtoMajor &&
		b.protoMinor == r.ProtoMinor &&
		b.host == r.Host &&
		b.upgrade == httpGetHeader(r.Header, headerUpgradeCanonical) &&
		b.connection == httpGetHeader(r.Header, headerConnectionCanonical) &&
		b.secKey == httpGetHeader(r.Header, headerSecKeyCanonical) &&
		b.secVersion == httpGetHeader(r.Header, headerSecVersionCanonical) &&
		stringSliceEqual(b.secProtocol, r.Header[headerSecProtocolCanonical]) &&
		stringSliceEqual(b.secExtensions, r.Header[headerSecExtensionsCanonical])
}

func stringSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Upgrader contains options for upgrading connection to websocket.
type Upgrader struct {
	// ReadBufferSize and WriteBufferSize is an I/O buffer sizes.
	// They used to read and write http data while upgrading to WebSocket.
	// Allocated buffers are pooled with sync.Pool to avoid extra allocations.
	//
	// If a size is zero then default value is used.
	//
	// Usually it is useful to set read buffer size bigger than write buffer
	// size because incoming request could contain long header values, such as
	// Cookie. Response, in other way, could be big only if user write multiple
	// custom headers. Usually response takes less than 256 bytes.
	ReadBufferSize, WriteBufferSize int

	// Protocol is a select function that is used to select subprotocol
	// from list requested by client. If this field is set, then the first matched
	// protocol is sent to a client as negotiated.
	//
	// The argument is only valid until the callback returns.
	Protocol func([]byte) bool

	// ProtocolCustrom allow user to parse Sec-WebSocket-Protocol header manually.
	// Note that returned bytes must be valid until Upgrade returns.
	// If ProtocolCustom is set, it used instead of Protocol function.
	ProtocolCustom func([]byte) (string, bool)

	// Extension is a select function that is used to select extensions
	// from list requested by client. If this field is set, then the all matched
	// extensions are sent to a client as negotiated.
	//
	// Note that Extension may be called multiple times and implementations
	// must track uniqueness of accepted extensions manually.
	//
	// The argument is only valid until the callback returns.
	//
	// According to the RFC6455 order of extensions passed by a client is
	// significant. That is, returning true from this function means that no
	// other extension with the same name should be checked because server
	// accepted the most preferable extension right now:
	// "Note that the order of extensions is significant.  Any interactions between
	// multiple extensions MAY be defined in the documents defining the extensions.
	// In the absence of such definitions, the interpretation is that the header
	// fields listed by the client in its request represent a preference of the
	// header fields it wishes to use, with the first options listed being most
	// preferable."
	//
	// Deprecated: use Negotiate instead.
	Extension func(httphead.Option) bool

	// ExtensionCustom allow user to parse Sec-WebSocket-Extensions header
	// manually.
	//
	// If ExtensionCustom() decides to accept received extension, it must
	// append appropriate option to the given slice of httphead.Option.
	// It returns results of append() to the given slice and a flag that
	// reports whether given header value is wellformed or not.
	//
	// Note that ExtensionCustom may be called multiple times and
	// implementations must track uniqueness of accepted extensions manually.
	//
	// Note that returned options should be valid until Upgrade returns.
	// If ExtensionCustom is set, it used instead of Extension function.
	ExtensionCustom func([]byte, []httphead.Option) ([]httphead.Option, bool)

	// Negotiate is the callback that is used to negotiate extensions from
	// the client's offer. If this field is set, then the returned non-zero
	// extensions are sent to the client as accepted extensions in the
	// response.
	//
	// The argument is only valid until the Negotiate callback returns.
	//
	// If returned error is non-nil then connection is rejected and response is
	// sent with appropriate HTTP error code and body set to error message.
	//
	// RejectConnectionError could be used to get more control on response.
	Negotiate func(httphead.Option) (httphead.Option, error)

	// Header is an optional HandshakeHeader instance that could be used to
	// write additional headers to the handshake response.
	//
	// It used instead of any key-value mappings to avoid allocations in user
	// land.
	//
	// Note that if present, it will be written in any result of handshake.
	Header HandshakeHeader

	// OnRequest is a callback that will be called after request line
	// successful parsing.
	//
	// The arguments are only valid until the callback returns.
	//
	// If returned error is non-nil then connection is rejected and response is
	// sent with appropriate HTTP error code and body set to error message.
	//
	// RejectConnectionError could be used to get more control on response.
	OnRequest func(uri []byte) error

	// OnHost is a callback that will be called after "Host" header successful
	// parsing.
	//
	// It is separated from OnHeader callback because the Host header must be
	// present in each request since HTTP/1.1. Thus Host header is non-optional
	// and required for every WebSocket handshake.
	//
	// The arguments are only valid until the callback returns.
	//
	// If returned error is non-nil then connection is rejected and response is
	// sent with appropriate HTTP error code and body set to error message.
	//
	// RejectConnectionError could be used to get more control on response.
	OnHost func(host []byte) error

	// OnHeader is a callback that will be called after successful parsing of
	// header, that is not used during WebSocket handshake procedure. That is,
	// it will be called with non-websocket headers, which could be relevant
	// for application-level logic.
	//
	// The arguments are only valid until the callback returns.
	//
	// If returned error is non-nil then connection is rejected and response is
	// sent with appropriate HTTP error code and body set to error message.
	//
	// RejectConnectionError could be used to get more control on response.
	OnHeader func(key, value []byte) error

	// OnBeforeUpgrade is a callback that will be called before sending
	// successful upgrade response.
	//
	// Setting OnBeforeUpgrade allows user to make final application-level
	// checks and decide whether this connection is allowed to successfully
	// upgrade to WebSocket.
	//
	// It must return non-nil either HandshakeHeader or error and never both.
	//
	// If returned error is non-nil then connection is rejected and response is
	// sent with appropriate HTTP error code and body set to error message.
	//
	// RejectConnectionError could be used to get more control on response.
	OnBeforeUpgrade func() (header HandshakeHeader, err error)
}

// Upgrade zero-copy upgrades connection to WebSocket. It interprets given conn
// as connection with incoming HTTP Upgrade request.
//
// It is a caller responsibility to manage i/o timeouts on conn.
//
// Non-nil error means that request for the WebSocket upgrade is invalid or
// malformed and usually connection should be closed.
// Even when error is non-nil Upgrade will write appropriate response into
// connection in compliance with RFC.
func (u Upgrader) Upgrade(conn io.ReadWriter) (hs Handshake, err error) {
	// headerSeen constants helps to report whether or not some header was seen
	// during reading request bytes.
	const (
		headerSeenHost = 1 << iota
		headerSeenUpgrade
		headerSeenConnection
		headerSeenSecVersion
		headerSeenSecKey

		// headerSeenAll is the value that we expect to receive at the end of
		// headers read/parse loop.
		headerSeenAll = 0 |
			headerSeenHost |
			headerSeenUpgrade |
			headerSeenConnection |
			headerSeenSecVersion |
			headerSeenSecKey
	)

	// Prepare I/O buffers.
	// TODO(gobwas): make it configurable.
	br := pbufio.GetReader(conn,
		nonZero(u.ReadBufferSize, DefaultServerReadBufferSize),
	)
	bw := pbufio.GetWriter(conn,
		nonZero(u.WriteBufferSize, DefaultServerWriteBufferSize),
	)
	defer func() {
		pbufio.PutReader(br)
		pbufio.PutWriter(bw)
	}()

	// Read HTTP request line like "GET /ws HTTP/1.1".
	rl, err := readLine(br)
	if err != nil {
		return hs, err
	}
	// Parse request line data like HTTP version, uri and method.
	req, err := httpParseRequestLine(rl)
	if err != nil {
		return hs, err
	}

	// Prepare stack-based handshake header list.
	header := handshakeHeader{
		0: u.Header,
	}

	// Parse and check HTTP request.
	// As RFC6455 says:
	//   The client's opening handshake consists of the following parts. If the
	//   server, while reading the handshake, finds that the client did not
	//   send a handshake that matches the description below (note that as per
	//   [RFC2616], the order of the header fields is not important), including
	//   but not limited to any violations of the ABNF grammar specified for
	//   the components of the handshake, the server MUST stop processing the
	//   client's handshake and return an HTTP response with an appropriate
	//   error code (such as 400 Bad Request).
	//
	// See https://tools.ietf.org/html/rfc6455#section-4.2.1

	// An HTTP/1.1 or higher GET request, including a "Request-URI".
	//
	// Even if RFC says "1.1 or higher" without mentioning the part of the
	// version, we apply it only to minor part.
	switch {
	case req.major != 1 || req.minor < 1:
		// Abort processing the whole request because we do not even know how
		// to actually parse it.
		err = ErrHandshakeBadProtocol

	case btsToString(req.method) != http.MethodGet:
		err = ErrHandshakeBadMethod

	default:
		if onRequest := u.OnRequest; onRequest != nil {
			err = onRequest(req.uri)
		}
	}
	// Start headers read/parse loop.
	var (
		// headerSeen reports which header was seen by setting corresponding
		// bit on.
		headerSeen byte

		nonce = make([]byte, nonceSize)
	)
	for err == nil {
		line, e := readLine(br)
		if e != nil {
			return hs, e
		}
		if len(line) == 0 {
			// Blank line, no more lines to read.
			break
		}

		k, v, ok := httpParseHeaderLine(line)
		if !ok {
			err = ErrMalformedRequest
			break
		}

		switch btsToString(k) {
		case headerHostCanonical:
			headerSeen |= headerSeenHost
			if onHost := u.OnHost; onHost != nil {
				err = onHost(v)
			}

		case headerUpgradeCanonical:
			headerSeen |= headerSeenUpgrade
			if !bytes.Equal(v, specHeaderValueUpgrade) && !bytes.EqualFold(v, specHeaderValueUpgrade) {
				err = ErrHandshakeBadUpgrade
			}

		case headerConnectionCanonical:
			headerSeen |= headerSeenConnection
			if !bytes.Equal(v, specHeaderValueConnection) && !btsHasToken(v, specHeaderValueConnectionLower) {
				err = ErrHandshakeBadConnection
			}

		case headerSecVersionCanonical:
			headerSeen |= headerSeenSecVersion
			if !bytes.Equal(v, specHeaderValueSecVersion) {
				err = ErrHandshakeUpgradeRequired
			}

		case headerSecKeyCanonical:
			headerSeen |= headerSeenSecKey
			if len(v) != nonceSize {
				err = ErrHandshakeBadSecKey
			} else {
				copy(nonce, v)
			}

		case headerSecProtocolCanonical:
			if custom, check := u.ProtocolCustom, u.Protocol; hs.Protocol == "" && (custom != nil || check != nil) {
				var ok bool
				if custom != nil {
					hs.Protocol, ok = custom(v)
				} else {
					hs.Protocol, ok = btsSelectProtocol(v, check)
				}
				if !ok {
					err = ErrMalformedRequest
				}
			}

		case headerSecExtensionsCanonical:
			if f := u.Negotiate; err == nil && f != nil {
				hs.Extensions, err = negotiateExtensions(v, hs.Extensions, f)
			}
			// DEPRECATED path.
			if custom, check := u.ExtensionCustom, u.Extension; u.Negotiate == nil && (custom != nil || check != nil) {
				var ok bool
				if custom != nil {
					hs.Extensions, ok = custom(v, hs.Extensions)
				} else {
					hs.Extensions, ok = btsSelectExtensions(v, hs.Extensions, check)
				}
				if !ok {
					err = ErrMalformedRequest
				}
			}

		default:
			if onHeader := u.OnHeader; onHeader != nil {
				err = onHeader(k, v)
			}
		}
	}
	switch {
	case err == nil && headerSeen != headerSeenAll:
		switch {
		case headerSeen&headerSeenHost == 0:
			// As RFC2616 says:
			//   A client MUST include a Host header field in all HTTP/1.1
			//   request messages. If the requested URI does not include an
			//   Internet host name for the service being requested, then the
			//   Host header field MUST be given with an empty value. An
			//   HTTP/1.1 proxy MUST ensure that any request message it
			//   forwards does contain an appropriate Host header field that
			//   identifies the service being requested by the proxy. All
			//   Internet-based HTTP/1.1 servers MUST respond with a 400 (Bad
			//   Request) status code to any HTTP/1.1 request message which
			//   lacks a Host header field.
			err = ErrHandshakeBadHost
		case headerSeen&headerSeenUpgrade == 0:
			err = ErrHandshakeBadUpgrade
		case headerSeen&headerSeenConnection == 0:
			err = ErrHandshakeBadConnection
		case headerSeen&headerSeenSecVersion == 0:
			// In case of empty or not present version we do not send 426 status,
			// because it does not meet the ABNF rules of RFC6455:
			//
			// version = DIGIT | (NZDIGIT DIGIT) |
			// ("1" DIGIT DIGIT) | ("2" DIGIT DIGIT)
			// ; Limited to 0-255 range, with no leading zeros
			//
			// That is, if version is really invalid – we sent 426 status as above, if it
			// not present – it is 400.
			err = ErrHandshakeBadSecVersion
		case headerSeen&headerSeenSecKey == 0:
			err = ErrHandshakeBadSecKey
		default:
			panic("unknown headers state")
		}

	case err == nil && u.OnBeforeUpgrade != nil:
		header[1], err = u.OnBeforeUpgrade()
	}
	if err != nil {
		var code int
		if rej, ok := err.(*ConnectionRejectedError); ok {
			code = rej.code
			header[1] = rej.header
		}
		if code == 0 {
			code = http.StatusInternalServerError
		}
		httpWriteResponseError(bw, err, code, header.WriteTo)
		// Do not store Flush() error to not override already existing one.
		_ = bw.Flush()
		return hs, err
	}

	httpWriteResponseUpgrade(bw, nonce, hs, header.WriteTo)
	err = bw.Flush()

	return hs, err
}

type handshakeHeader [2]HandshakeHeader

func (hs handshakeHeader) WriteTo(w io.Writer) (n int64, err error) {
	for i := 0; i < len(hs) && err == nil; i++ {
		if h := hs[i]; h != nil {
			var m int64
			m, err = h.WriteTo(w)
			n += m
		}
	}
	return n, err
}
