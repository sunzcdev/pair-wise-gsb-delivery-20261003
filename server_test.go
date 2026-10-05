package ws

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gobwas/httphead"
	"github.com/gobwas/pool/pbufio"
)

// TODO(gobwas): upgradeGenericCase with methods like configureUpgrader,
// configureHTTPUpgrader.
type upgradeCase struct {
	label string

	protocol  func(string) bool
	negotiate func(httphead.Option) (httphead.Option, error)
	onRequest func(u []byte) error
	onHost    func(h []byte) error
	onHeader  func(k, v []byte) error

	nonce        []byte
	removeSecKey bool
	badSecKey    bool
	secKeyHeader string

	req *http.Request
	res *http.Response
	hs  Handshake
	err error
}

var upgradeCases = []upgradeCase{
	{
		label: "base",
		nonce: mustMakeNonce(),
		req: mustMakeRequest("GET", "ws://example.org", http.Header{
			headerUpgrade:    []string{"websocket"},
			headerConnection: []string{"Upgrade"},
			headerSecVersion: []string{"13"},
		}),
		res: mustMakeResponse(101, http.Header{
			headerUpgrade:    []string{"websocket"},
			headerConnection: []string{"Upgrade"},
		}),
	},
	{
		label:        "base_canonical",
		nonce:        mustMakeNonce(),
		secKeyHeader: headerSecKeyCanonical,
		req: mustMakeRequest("GET", "ws://example.org", http.Header{
			headerUpgrade:             []string{"websocket"},
			headerConnection:          []string{"Upgrade"},
			headerSecVersionCanonical: []string{"13"},
		}),
		res: mustMakeResponse(101, http.Header{
			headerUpgrade:    []string{"websocket"},
			headerConnection: []string{"Upgrade"},
		}),
	},
	{
		label:        "lowercase_headers",
		nonce:        mustMakeNonce(),
		secKeyHeader: strings.ToLower(headerSecKey),
		req: mustMakeRequest("GET", "ws://example.org", http.Header{
			strings.ToLower(headerUpgrade):    []string{"websocket"},
			strings.ToLower(headerConnection): []string{"Upgrade"},
			strings.ToLower(headerSecVersion): []string{"13"},
		}),
		res: mustMakeResponse(101, http.Header{
			headerUpgrade:    []string{"websocket"},
			headerConnection: []string{"Upgrade"},
		}),
	},
	{
		label:    "uppercase",
		protocol: func(sub string) bool { return true },
		nonce:    mustMakeNonce(),
		req: mustMakeRequest("GET", "ws://example.org", http.Header{
			headerUpgrade:    []string{"WEBSOCKET"},
			headerConnection: []string{"UPGRADE"},
			headerSecVersion: []string{"13"},
		}),
		res: mustMakeResponse(101, http.Header{
			headerUpgrade:    []string{"websocket"},
			headerConnection: []string{"Upgrade"},
		}),
	},
	{
		label:    "subproto",
		protocol: SelectFromSlice([]string{"b", "d"}),
		nonce:    mustMakeNonce(),
		req: mustMakeRequest("GET", "ws://example.org", http.Header{
			headerUpgrade:     []string{"websocket"},
			headerConnection:  []string{"Upgrade"},
			headerSecVersion:  []string{"13"},
			headerSecProtocol: []string{"a", "b", "c", "d"},
		}),
		res: mustMakeResponse(101, http.Header{
			headerUpgrade:     []string{"websocket"},
			headerConnection:  []string{"Upgrade"},
			headerSecProtocol: []string{"b"},
		}),
		hs: Handshake{Protocol: "b"},
	},
	{
		label:        "subproto_lowercase_headers",
		protocol:     SelectFromSlice([]string{"b", "d"}),
		nonce:        mustMakeNonce(),
		secKeyHeader: strings.ToLower(headerSecKey),
		req: mustMakeRequest("GET", "ws://example.org", http.Header{
			strings.ToLower(headerUpgrade):     []string{"websocket"},
			strings.ToLower(headerConnection):  []string{"Upgrade"},
			strings.ToLower(headerSecVersion):  []string{"13"},
			strings.ToLower(headerSecProtocol): []string{"a", "b", "c", "d"},
		}),
		res: mustMakeResponse(101, http.Header{
			headerUpgrade:     []string{"websocket"},
			headerConnection:  []string{"Upgrade"},
			headerSecProtocol: []string{"b"},
		}),
		hs: Handshake{Protocol: "b"},
	},
	{
		label:    "subproto_comma",
		protocol: SelectFromSlice([]string{"b", "d"}),
		nonce:    mustMakeNonce(),
		req: mustMakeRequest("GET", "ws://example.org", http.Header{
			headerUpgrade:     []string{"websocket"},
			headerConnection:  []string{"Upgrade"},
			headerSecVersion:  []string{"13"},
			headerSecProtocol: []string{"a, b, c, d"},
		}),
		res: mustMakeResponse(101, http.Header{
			headerUpgrade:     []string{"websocket"},
			headerConnection:  []string{"Upgrade"},
			headerSecProtocol: []string{"b"},
		}),
		hs: Handshake{Protocol: "b"},
	},
	{
		negotiate: func(opt httphead.Option) (ret httphead.Option, err error) {
			switch string(opt.Name) {
			case "b", "d":
				return opt.Clone(), nil
			default:
				return ret, nil
			}
		},
		nonce: mustMakeNonce(),
		req: mustMakeRequest("GET", "ws://example.org", http.Header{
			headerUpgrade:       []string{"websocket"},
			headerConnection:    []string{"Upgrade"},
			headerSecVersion:    []string{"13"},
			headerSecExtensions: []string{"a;foo=1", "b;bar=2", "c", "d;baz=3"},
		}),
		res: mustMakeResponse(101, http.Header{
			headerUpgrade:       []string{"websocket"},
			headerConnection:    []string{"Upgrade"},
			headerSecExtensions: []string{"b;bar=2,d;baz=3"},
		}),
		hs: Handshake{
			Extensions: []httphead.Option{
				httphead.NewOption("b", map[string]string{
					"bar": "2",
				}),
				httphead.NewOption("d", map[string]string{
					"baz": "3",
				}),
			},
		},
	},

	// Error cases.
	// ------------

	{
		label: "bad_http_method",
		nonce: mustMakeNonce(),
		req: mustMakeRequest("POST", "ws://example.org", http.Header{
			headerUpgrade:    []string{"websocket"},
			headerConnection: []string{"Upgrade"},
			headerSecVersion: []string{"13"},
		}),
		res: mustMakeErrResponse(405, ErrHandshakeBadMethod, nil),
		err: ErrHandshakeBadMethod,
	},
	{
		label: "bad_http_proto",
		nonce: mustMakeNonce(),
		req: setProto(1, 0, mustMakeRequest("GET", "ws://example.org", http.Header{
			headerUpgrade:    []string{"websocket"},
			headerConnection: []string{"Upgrade"},
			headerSecVersion: []string{"13"},
		})),
		res: mustMakeErrResponse(505, ErrHandshakeBadProtocol, nil),
		err: ErrHandshakeBadProtocol,
	},
	{
		label: "bad_host",
		nonce: mustMakeNonce(),
		req: withoutHeader("Host", mustMakeRequest("GET", "ws://example.org", http.Header{
			headerUpgrade:    []string{"websocket"},
			headerConnection: []string{"Upgrade"},
			headerSecVersion: []string{"13"},
		})),
		res: mustMakeErrResponse(400, ErrHandshakeBadHost, nil),
		err: ErrHandshakeBadHost,
	},
	{
		label: "bad_upgrade",
		nonce: mustMakeNonce(),
		req: mustMakeRequest("GET", "ws://example.org", http.Header{
			headerConnection: []string{"Upgrade"},
			headerSecVersion: []string{"13"},
		}),
		res: mustMakeErrResponse(400, ErrHandshakeBadUpgrade, nil),
		err: ErrHandshakeBadUpgrade,
	},
	{
		label: "bad_upgrade",
		nonce: mustMakeNonce(),
		req: mustMakeRequest("GET", "ws://example.org", http.Header{
			"X-Custom-Header": []string{"value"},
			headerConnection:  []string{"Upgrade"},
			headerSecVersion:  []string{"13"},
		}),

		onRequest: func([]byte) error { return nil },
		onHost:    func([]byte) error { return nil },
		onHeader:  func(k, v []byte) error { return nil },

		res: mustMakeErrResponse(400, ErrHandshakeBadUpgrade, nil),
		err: ErrHandshakeBadUpgrade,
	},
	{
		label: "bad_upgrade",
		nonce: mustMakeNonce(),
		req: mustMakeRequest("GET", "ws://example.org", http.Header{
			headerUpgrade:    []string{"not-websocket"},
			headerConnection: []string{"Upgrade"},
			headerSecVersion: []string{"13"},
		}),
		res: mustMakeErrResponse(400, ErrHandshakeBadUpgrade, nil),
		err: ErrHandshakeBadUpgrade,
	},
	{
		label: "bad_connection",
		nonce: mustMakeNonce(),
		req: mustMakeRequest("GET", "ws://example.org", http.Header{
			headerUpgrade:    []string{"websocket"},
			headerSecVersion: []string{"13"},
		}),
		res: mustMakeErrResponse(400, ErrHandshakeBadConnection, nil),
		err: ErrHandshakeBadConnection,
	},
	{
		label: "bad_connection",
		nonce: mustMakeNonce(),
		req: mustMakeRequest("GET", "ws://example.org", http.Header{
			headerUpgrade:    []string{"websocket"},
			headerConnection: []string{"not-upgrade"},
			headerSecVersion: []string{"13"},
		}),
		res: mustMakeErrResponse(400, ErrHandshakeBadConnection, nil),
		err: ErrHandshakeBadConnection,
	},
	{
		label: "bad_sec_version_x",
		nonce: mustMakeNonce(),
		req: mustMakeRequest("GET", "ws://example.org", http.Header{
			headerUpgrade:    []string{"websocket"},
			headerConnection: []string{"Upgrade"},
		}),
		res: mustMakeErrResponse(400, ErrHandshakeBadSecVersion, nil),
		err: ErrHandshakeBadSecVersion,
	},
	{
		label: "bad_sec_version",
		nonce: mustMakeNonce(),
		req: mustMakeRequest("GET", "ws://example.org", http.Header{
			headerUpgrade:    []string{"websocket"},
			headerConnection: []string{"upgrade"},
			headerSecVersion: []string{"15"},
		}),
		res: mustMakeErrResponse(426, ErrHandshakeBadSecVersion, http.Header{
			headerSecVersion: []string{"13"},
		}),
		err: ErrHandshakeUpgradeRequired,
	},
	{
		label:        "bad_sec_key",
		nonce:        mustMakeNonce(),
		removeSecKey: true,
		req: mustMakeRequest("GET", "ws://example.org", http.Header{
			headerUpgrade:    []string{"websocket"},
			headerConnection: []string{"Upgrade"},
			headerSecVersion: []string{"13"},
		}),
		res: mustMakeErrResponse(400, ErrHandshakeBadSecKey, nil),
		err: ErrHandshakeBadSecKey,
	},
	{
		label:     "bad_sec_key",
		nonce:     mustMakeNonce(),
		badSecKey: true,
		req: mustMakeRequest("GET", "ws://example.org", http.Header{
			headerUpgrade:    []string{"websocket"},
			headerConnection: []string{"Upgrade"},
			headerSecVersion: []string{"13"},
		}),
		res: mustMakeErrResponse(400, ErrHandshakeBadSecKey, nil),
		err: ErrHandshakeBadSecKey,
	},
	{
		label: "bad_ws_extension",
		nonce: mustMakeNonce(),
		req: mustMakeRequest("GET", "ws://example.org", http.Header{
			headerUpgrade:       []string{"websocket"},
			headerConnection:    []string{"Upgrade"},
			headerSecVersion:    []string{"13"},
			headerSecExtensions: []string{"=["},
		}),
		negotiate: func(opt httphead.Option) (ret httphead.Option, err error) {
			return ret, nil
		},
		res: mustMakeErrResponse(400, ErrMalformedRequest, nil),
		err: ErrMalformedRequest,
	},
	{
		label: "bad_subprotocol",
		nonce: mustMakeNonce(),
		req: mustMakeRequest("GET", "ws://example.org", http.Header{
			headerUpgrade:     []string{"websocket"},
			headerConnection:  []string{"Upgrade"},
			headerSecVersion:  []string{"13"},
			headerSecProtocol: []string{"=["},
		}),
		protocol: func(string) bool {
			return false
		},
		res: mustMakeErrResponse(400, ErrMalformedRequest, nil),
		err: ErrMalformedRequest,
	},
}

func TestHTTPUpgrader(t *testing.T) {
	for _, test := range upgradeCases {
		t.Run(test.label, func(t *testing.T) {
			if !test.removeSecKey {
				nonce := test.nonce
				if test.badSecKey {
					nonce = nonce[:nonceSize-1]
				}
				if test.secKeyHeader == "" {
					test.secKeyHeader = headerSecKey
				}
				test.req.Header[test.secKeyHeader] = []string{string(nonce)}
			}
			if test.err == nil {
				test.res.Header[headerSecAccept] = []string{string(makeAccept(test.nonce))}
			}

			// Need to emulate http server read request for truth test.
			//
			// We use dumpRequest here because test.req.Write is always send
			// http/1.1 proto version, that does not fits all our testing
			// cases.
			reqBytes := dumpRequest(test.req)
			req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(reqBytes)))
			if err != nil {
				t.Fatal(err)
			}

			res := newRecorder()

			u := HTTPUpgrader{
				Protocol:  test.protocol,
				Negotiate: test.negotiate,
			}
			_, _, hs, err := u.Upgrade(req, res)
			if test.err != err {
				t.Fatalf(
					"expected error to be '%v', got '%v';\non request:\n====\n%s\n====",
					test.err, err, dumpRequest(req),
				)
			}

			actRespBts := sortHeaders(res.Bytes())
			expRespBts := sortHeaders(dumpResponse(test.res))
			if !bytes.Equal(actRespBts, expRespBts) {
				t.Fatalf(
					"unexpected http response:\n---- act:\n%s\n---- want:\n%s\n==== on request:\n%s\n====",
					actRespBts, expRespBts, dumpRequest(test.req),
				)
			}

			if act, exp := hs.Protocol, test.hs.Protocol; act != exp {
				t.Errorf("handshake protocol is %q want %q", act, exp)
			}
			if act, exp := len(hs.Extensions), len(test.hs.Extensions); act != exp {
				t.Errorf("handshake got %d extensions; want %d", act, exp)
			} else {
				for i := 0; i < act; i++ {
					if act, exp := hs.Extensions[i], test.hs.Extensions[i]; !act.Equal(exp) {
						t.Errorf("handshake %d-th extension is %s; want %s", i, act, exp)
					}
				}
			}
		})
	}
}

func TestUpgrader(t *testing.T) {
	for _, test := range upgradeCases {
		t.Run(test.label, func(t *testing.T) {
			if !test.removeSecKey {
				nonce := test.nonce[:]
				if test.badSecKey {
					nonce = nonce[:nonceSize-1]
				}
				test.req.Header[headerSecKey] = []string{string(nonce)}
			}
			if test.err == nil {
				test.res.Header[headerSecAccept] = []string{string(makeAccept(test.nonce))}
			}

			u := Upgrader{
				Protocol: func(p []byte) bool {
					return test.protocol(string(p))
				},
				Negotiate: test.negotiate,
				OnHeader:  test.onHeader,
				OnRequest: test.onRequest,
			}

			// We use dumpRequest here because test.req.Write is always send
			// http/1.1 proto version, that does not fits all our testing
			// cases.
			reqBytes := dumpRequest(test.req)
			conn := bytes.NewBuffer(reqBytes)

			hs, err := u.Upgrade(conn)
			if test.err != err {
				t.Fatalf("expected error to be '%v', got '%v'", test.err, err)
			}

			actRespBts := sortHeaders(conn.Bytes())
			expRespBts := sortHeaders(dumpResponse(test.res))
			if !bytes.Equal(actRespBts, expRespBts) {
				t.Fatalf(
					"unexpected http response:\n---- act:\n%s\n---- want:\n%s\n==== on request:\n%s\n====",
					actRespBts, expRespBts, dumpRequest(test.req),
				)
			}

			if act, exp := hs.Protocol, test.hs.Protocol; act != exp {
				t.Errorf("handshake protocol is %q want %q", act, exp)
			}
			if act, exp := len(hs.Extensions), len(test.hs.Extensions); act != exp {
				t.Errorf("handshake got %d extensions; want %d", act, exp)
			} else {
				for i := 0; i < act; i++ {
					if act, exp := hs.Extensions[i], test.hs.Extensions[i]; !act.Equal(exp) {
						t.Errorf("handshake %d-th extension is %s; want %s", i, act, exp)
					}
				}
			}
		})
	}
}

func BenchmarkHTTPUpgrader(b *testing.B) {
	for _, bench := range upgradeCases {
		bench.req.Header.Set(headerSecKey, string(bench.nonce[:]))

		u := HTTPUpgrader{
			Protocol:  bench.protocol,
			Negotiate: bench.negotiate,
		}

		b.Run(bench.label, func(b *testing.B) {
			res := make([]http.ResponseWriter, b.N)
			for i := 0; i < b.N; i++ {
				res[i] = newRecorder()
			}

			i := new(int64)

			b.ResetTimer()
			b.ReportAllocs()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					w := res[atomic.AddInt64(i, 1)-1]
					u.Upgrade(bench.req, w)
				}
			})
		})
	}
}

func BenchmarkUpgrader(b *testing.B) {
	for _, bench := range upgradeCases {
		bench.req.Header.Set(headerSecKey, string(bench.nonce[:]))

		u := Upgrader{
			Protocol: func(p []byte) bool {
				return bench.protocol(btsToString(p))
			},
			Negotiate: bench.negotiate,
		}

		reqBytes := dumpRequest(bench.req)

		type benchReadWriter struct {
			io.Reader
			io.Writer
		}

		b.Run(bench.label, func(b *testing.B) {
			conn := make([]io.ReadWriter, b.N)
			for i := 0; i < b.N; i++ {
				conn[i] = benchReadWriter{bytes.NewReader(reqBytes), ioutil.Discard}
			}

			i := new(int64)

			b.ResetTimer()
			b.ReportAllocs()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					c := conn[atomic.AddInt64(i, 1)-1]
					u.Upgrade(c)
				}
			})
		})
	}
}

func TestHttpStrSelectProtocol(t *testing.T) {
	for i, test := range []struct {
		header string
	}{
		{"jsonrpc, soap, grpc"},
	} {
		t.Run(fmt.Sprintf("#%d", i), func(t *testing.T) {
			exp := strings.Split(test.header, ",")
			for i, p := range exp {
				exp[i] = strings.TrimSpace(p)
			}

			var calls []string
			strSelectProtocol(test.header, func(s string) bool {
				calls = append(calls, s)
				return false
			})

			if !reflect.DeepEqual(calls, exp) {
				t.Errorf("selectProtocol(%q, fn); called fn with %v; want %v", test.header, calls, exp)
			}
		})
	}
}

func BenchmarkSelectProtocol(b *testing.B) {
	for _, bench := range []struct {
		label     string
		header    string
		acceptStr func(string) bool
		acceptBts func([]byte) bool
	}{
		{
			label:  "never accept",
			header: "jsonrpc, soap, grpc",
			acceptStr: func(s string) bool {
				return len(s)%2 == 2 // never ok
			},
			acceptBts: func(v []byte) bool {
				return len(v)%2 == 2 // never ok
			},
		},
		{
			label:     "from slice",
			header:    "a, b, c, d, e, f, g",
			acceptStr: SelectFromSlice([]string{"g", "f", "e", "d"}),
		},
		{
			label:     "uniq 1024 from slise",
			header:    strings.Join(randProtocols(1024, 16), ", "),
			acceptStr: SelectFromSlice(randProtocols(1024, 17)),
		},
	} {
		b.Run(fmt.Sprintf("String/%s", bench.label), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				strSelectProtocol(bench.header, bench.acceptStr)
			}
		})
		if bench.acceptBts != nil {
			b.Run(fmt.Sprintf("Bytes/%s", bench.label), func(b *testing.B) {
				h := []byte(bench.header)
				b.StartTimer()

				for i := 0; i < b.N; i++ {
					btsSelectProtocol(h, bench.acceptBts)
				}
			})
		}
	}
}

func randProtocols(n, m int) []string {
	ret := make([]string, n)
	bts := make([]byte, m)
	uniq := map[string]bool{}
	for i := 0; i < n; i++ {
		for {
			for j := 0; j < m; j++ {
				bts[j] = byte(rand.Intn('x'-'a') + 'a')
			}
			str := string(bts)
			if _, has := uniq[str]; !has {
				ret[i] = str
				break
			}
		}
	}
	return ret
}

func dumpRequest(req *http.Request) []byte {
	bts, err := httputil.DumpRequest(req, true)
	if err != nil {
		panic(err)
	}
	return bts
}

func dumpResponse(res *http.Response) []byte {
	if !res.Close {
		for _, v := range res.Header[headerConnection] {
			if v == "close" {
				res.Close = true
				break
			}
		}
	}
	bts, err := httputil.DumpResponse(res, true)
	if err != nil {
		panic(err)
	}
	if !res.Close {
		bts = bytes.ReplaceAll(bts, []byte("Connection: close\r\n"), nil)
	}

	return bts
}

type headersBytes [][]byte

func (h headersBytes) Len() int           { return len(h) }
func (h headersBytes) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h headersBytes) Less(i, j int) bool { return bytes.Compare(h[i], h[j]) == -1 }

func maskHeader(bts []byte, key, mask string) []byte {
	lines := bytes.Split(bts, []byte("\r\n"))
	for i, line := range lines {
		pair := bytes.Split(line, []byte(": "))
		if string(pair[0]) == key {
			lines[i] = []byte(key + ": " + mask)
		}
	}
	return bytes.Join(lines, []byte("\r\n"))
}

func sortHeaders(bts []byte) []byte {
	lines := bytes.Split(bts, []byte("\r\n"))
	if len(lines) <= 1 {
		return bts
	}
	sort.Sort(headersBytes(lines[1 : len(lines)-2]))
	return bytes.Join(lines, []byte("\r\n"))
}

type recorder struct {
	*httptest.ResponseRecorder
	hijacked bool
	conn     func(*bytes.Buffer) net.Conn
}

func newRecorder() *recorder {
	return &recorder{
		ResponseRecorder: httptest.NewRecorder(),
	}
}

func (r *recorder) Bytes() []byte {
	if r.hijacked {
		return r.ResponseRecorder.Body.Bytes()
	}

	// TODO(gobwas): remove this when support for go 1.7 will end.
	resp := r.Result()
	cs := strings.TrimSpace(resp.Header.Get("Content-Length"))
	if n, err := strconv.ParseInt(cs, 10, 64); err == nil {
		resp.ContentLength = n
	} else {
		resp.ContentLength = -1
	}

	return dumpResponse(resp)
}

func (r *recorder) Hijack() (conn net.Conn, brw *bufio.ReadWriter, err error) {
	if r.hijacked {
		err = fmt.Errorf("already hijacked")
		return conn, brw, err
	}

	r.hijacked = true

	var buf *bytes.Buffer
	if r.ResponseRecorder != nil {
		buf = r.ResponseRecorder.Body
	}

	if r.conn != nil {
		conn = r.conn(buf)
	} else {
		conn = stubConn{
			read:  buf.Read,
			write: buf.Write,
			close: func() error { return nil },
		}
	}

	br := pbufio.GetReader(conn, DefaultClientReadBufferSize)
	bw := pbufio.GetWriter(conn, DefaultClientWriteBufferSize)

	brw = bufio.NewReadWriter(br, bw)

	return conn, brw, err
}

func mustMakeRequest(method, url string, headers http.Header) *http.Request {
	req, err := http.NewRequest(method, url, http.NoBody)
	if err != nil {
		panic(err)
	}
	req.Header = headers
	return req
}

func setProto(major, minor int, req *http.Request) *http.Request {
	req.ProtoMajor = major
	req.ProtoMinor = minor
	return req
}

func withoutHeader(header string, req *http.Request) *http.Request {
	if strings.EqualFold(header, "Host") {
		req.URL.Host = ""
		req.Host = ""
	} else {
		delete(req.Header, header)
	}
	return req
}

func mustMakeResponse(code int, headers http.Header) *http.Response {
	res := &http.Response{
		StatusCode:    code,
		Status:        http.StatusText(code),
		Header:        headers,
		ProtoMajor:    1,
		ProtoMinor:    1,
		ContentLength: -1,
	}
	return res
}

func mustMakeErrResponse(code int, err error, headers http.Header) *http.Response {
	// Body text.
	body := err.Error()

	res := &http.Response{
		StatusCode: code,
		Status:     http.StatusText(code),
		Header: http.Header{
			"Content-Type": []string{"text/plain; charset=utf-8"},
		},
		ProtoMajor:    1,
		ProtoMinor:    1,
		ContentLength: int64(len(body)),
	}
	res.Body = ioutil.NopCloser(
		strings.NewReader(body),
	)
	for k, v := range headers {
		res.Header[k] = v
	}
	return res
}

func mustMakeNonce() (ret []byte) {
	ret = make([]byte, nonceSize)
	initNonce(ret)
	return ret
}

type protocolListCase struct {
	label string

	list     []string
	required bool

	// offer holds the Sec-WebSocket-Protocol header lines sent by the
	// client.
	offer []string

	// protocol is the expected negotiated protocol.
	protocol string
	// err is the expected handshake error.
	err error
}

var protocolListCases = []protocolListCase{
	{
		label:    "server_preference_order",
		list:     []string{"c", "b"},
		offer:    []string{"a, b, c"},
		protocol: "c",
	},
	{
		label:    "multiple_header_lines",
		list:     []string{"d", "b"},
		offer:    []string{"a, b", "c, d"},
		protocol: "d",
	},
	{
		label:    "duplicate_tokens",
		list:     []string{"a", "b"},
		offer:    []string{"b, b, a", "b, a"},
		protocol: "a",
	},
	{
		label:    "empty_tokens",
		list:     []string{"c"},
		offer:    []string{"a,, b,", " , c"},
		protocol: "c",
	},
	{
		label:    "only_empty_tokens",
		list:     []string{"a"},
		offer:    []string{", ,"},
		protocol: "",
	},
	{
		label:    "case_insensitive_match",
		list:     []string{"chat"},
		offer:    []string{"Chat, CHAT"},
		protocol: "chat",
	},
	{
		label:    "server_spelling_wins",
		list:     []string{"Chat"},
		offer:    []string{"chat"},
		protocol: "Chat",
	},
	{
		label:    "empty_list_entries_ignored",
		list:     []string{"", "b"},
		offer:    []string{"b"},
		protocol: "b",
	},
	{
		label:    "no_offer",
		list:     []string{"a"},
		offer:    nil,
		protocol: "",
	},
	{
		label:    "no_common_protocol_accepted",
		list:     []string{"a"},
		offer:    []string{"x, y"},
		protocol: "",
	},
	{
		label:    "no_common_protocol_rejected",
		list:     []string{"a"},
		required: true,
		offer:    []string{"x, y"},
		err:      ErrHandshakeNoCommonProtocol,
	},
	{
		label:    "no_offer_rejected",
		list:     []string{"a"},
		required: true,
		offer:    nil,
		err:      ErrHandshakeNoCommonProtocol,
	},
	{
		label:    "required_without_list_has_no_effect",
		required: true,
		offer:    []string{"x"},
		protocol: "",
	},
}

func makeProtocolListRequest(t *testing.T, offer []string) (*http.Request, []byte) {
	t.Helper()

	header := http.Header{
		headerUpgrade:    []string{"websocket"},
		headerConnection: []string{"Upgrade"},
		headerSecVersion: []string{"13"},
		headerSecKey:     []string{string(mustMakeNonce())},
	}
	for _, line := range offer {
		header[headerSecProtocol] = append(header[headerSecProtocol], line)
	}
	req := mustMakeRequest("GET", "ws://example.org", header)
	return req, dumpRequest(req)
}

func checkProtocolListResult(t *testing.T, label string, hs Handshake, err error, response []byte, test protocolListCase) {
	t.Helper()

	if err != test.err {
		t.Fatalf("%s: error is %v; want %v", label, err, test.err)
	}
	if hs.Protocol != test.protocol {
		t.Errorf("%s: handshake protocol is %q; want %q", label, hs.Protocol, test.protocol)
	}
	if test.err != nil {
		if !bytes.HasPrefix(response, []byte("HTTP/1.1 400")) {
			t.Errorf("%s: response does not start with 400 status line:\n%s", label, response)
		}
		return
	}
	if !bytes.HasPrefix(response, []byte("HTTP/1.1 101")) {
		t.Fatalf("%s: response does not start with 101 status line:\n%s", label, response)
	}
	headerLine := []byte(headerSecProtocol + ": " + test.protocol + "\r\n")
	if test.protocol == "" {
		if bytes.Contains(response, []byte(headerSecProtocol+":")) {
			t.Errorf("%s: response contains unexpected %q header:\n%s", label, headerSecProtocol, response)
		}
	} else if !bytes.Contains(response, headerLine) {
		t.Errorf("%s: response does not contain %q header line:\n%s", label, headerLine, response)
	}
}

func TestUpgraderProtocolList(t *testing.T) {
	for _, test := range protocolListCases {
		t.Run(test.label, func(t *testing.T) {
			_, reqBytes := makeProtocolListRequest(t, test.offer)

			u := Upgrader{
				ProtocolList:     test.list,
				ProtocolRequired: test.required,
			}
			conn := bytes.NewBuffer(reqBytes)
			hs, err := u.Upgrade(conn)

			checkProtocolListResult(t, test.label, hs, err, conn.Bytes(), test)
		})
	}
}

func TestHTTPUpgraderProtocolList(t *testing.T) {
	for _, test := range protocolListCases {
		t.Run(test.label, func(t *testing.T) {
			_, reqBytes := makeProtocolListRequest(t, test.offer)
			req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(reqBytes)))
			if err != nil {
				t.Fatal(err)
			}

			u := HTTPUpgrader{
				ProtocolList:     test.list,
				ProtocolRequired: test.required,
			}
			res := newRecorder()
			_, _, hs, err := u.Upgrade(req, res)

			checkProtocolListResult(t, test.label, hs, err, res.Bytes(), test)
		})
	}
}

func TestUpgraderProtocolConfigConflict(t *testing.T) {
	_, reqBytes := makeProtocolListRequest(t, []string{"a"})

	upgraders := map[string]Upgrader{
		"protocol": {
			Protocol:     func([]byte) bool { return true },
			ProtocolList: []string{"a"},
		},
		"protocol_custom": {
			ProtocolCustom: func([]byte) (string, bool) { return "a", true },
			ProtocolList:   []string{"a"},
		},
	}
	for label, u := range upgraders {
		t.Run(label, func(t *testing.T) {
			conn := bytes.NewBuffer(reqBytes)
			_, err := u.Upgrade(conn)
			if err != ErrProtocolConfigConflict {
				t.Fatalf("error is %v; want %v", err, ErrProtocolConfigConflict)
			}
			// The conflict must be reported before any handshake I/O: no
			// request bytes read and no response (in particular no 101)
			// written.
			if !bytes.Equal(conn.Bytes(), reqBytes) {
				t.Errorf("connection buffer was touched before the conflict was reported")
			}
		})
	}
}

func TestHTTPUpgraderProtocolConfigConflict(t *testing.T) {
	_, reqBytes := makeProtocolListRequest(t, []string{"a"})
	req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(reqBytes)))
	if err != nil {
		t.Fatal(err)
	}

	u := HTTPUpgrader{
		Protocol:     func(string) bool { return true },
		ProtocolList: []string{"a"},
	}
	res := newRecorder()
	_, _, _, err = u.Upgrade(req, res)
	if err != ErrProtocolConfigConflict {
		t.Fatalf("error is %v; want %v", err, ErrProtocolConfigConflict)
	}
	if res.hijacked {
		t.Errorf("connection was hijacked before the conflict was reported")
	}
	if act, exp := res.Code, http.StatusInternalServerError; act != exp {
		t.Errorf("response status code is %d; want %d", act, exp)
	}
}
