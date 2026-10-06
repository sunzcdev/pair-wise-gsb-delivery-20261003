package goproxy

import (
	"context"
	"crypto/tls"
	"mime"
	"net"
	"net/http"
)

// ProxyCtx is the Proxy context, contains useful information about every request. It is passed to
// every user function. Also used as a logger.
type ProxyCtx struct {
	// Will contain the client request from the proxy
	Req *http.Request
	// Will contain the remote server's response (if available. nil if the request wasn't send yet)
	Resp         *http.Response
	RoundTripper RoundTripper
	// Specify a custom connection dialer that will be used only for the current
	// request, including WebSocket connection upgrades
	Dialer func(ctx context.Context, network string, addr string) (net.Conn, error)
	// will contain the recent error that occurred while trying to send receive or parse traffic
	Error error
	// A handle for the user to keep data in the context, from the call of ReqHandler to the
	// call of RespHandler
	UserData any
	// Will connect a request to a response
	Session   int64
	certStore CertStorage
	Proxy     *ProxyHttpServer
}

type RoundTripper interface {
	RoundTrip(req *http.Request, ctx *ProxyCtx) (*http.Response, error)
}

type CertStorage interface {
	Fetch(hostname string, gen func() (*tls.Certificate, error)) (*tls.Certificate, error)
}

// GenerationCertStorage is an optional extension of CertStorage for caches
// that want to be CA-generation aware. A RotatingCA passes the current issuer
// generation with every request, so the cache can keep certificates signed by
// different CAs in separate slots and drop an old generation wholesale when
// DropGeneration is invoked.
//
// Stores that implement only CertStorage keep working unchanged: the proxy
// transparently prefixes cache keys with the generation number, so a legacy
// store never serves a stale, old-CA certificate after rotation.
type GenerationCertStorage interface {
	FetchGeneration(hostname string, generation uint64, gen func() (*tls.Certificate, error)) (*tls.Certificate, error)
}

// GenerationCertRecycler is implemented by GenerationCertStorage
// implementations that can release all certificates of a retired CA
// generation. It is called once, after every handshake that used that
// generation has finished. Implementations that do not need to reclaim old
// generations may simply omit this method.
type GenerationCertRecycler interface {
	DropGeneration(generation uint64)
}

type RoundTripperFunc func(req *http.Request, ctx *ProxyCtx) (*http.Response, error)

func (f RoundTripperFunc) RoundTrip(req *http.Request, ctx *ProxyCtx) (*http.Response, error) {
	return f(req, ctx)
}

func (ctx *ProxyCtx) RoundTrip(req *http.Request) (*http.Response, error) {
	if ctx.RoundTripper != nil {
		return ctx.RoundTripper.RoundTrip(req, ctx)
	}
	return ctx.Proxy.Tr.RoundTrip(req)
}

func (ctx *ProxyCtx) printf(msg string, argv ...any) {
	ctx.Proxy.Logger.Printf("[%03d] "+msg+"\n", append([]any{ctx.Session & 0xFFFF}, argv...)...)
}

// Logf prints a message to the proxy's log. Should be used in a ProxyHttpServer's filter
// This message will be printed only if the Verbose field of the ProxyHttpServer is set to true
//
//	proxy.OnRequest().DoFunc(func(r *http.Request,ctx *goproxy.ProxyCtx) (*http.Request, *http.Response){
//		nr := atomic.AddInt32(&counter,1)
//		ctx.Printf("So far %d requests",nr)
//		return r, nil
//	})
func (ctx *ProxyCtx) Logf(msg string, argv ...any) {
	if ctx.Proxy.Verbose {
		ctx.printf("INFO: "+msg, argv...)
	}
}

// Warnf prints a message to the proxy's log. Should be used in a ProxyHttpServer's filter
// This message will always be printed.
//
//	proxy.OnRequest().DoFunc(func(r *http.Request,ctx *goproxy.ProxyCtx) (*http.Request, *http.Response){
//		f,err := os.OpenFile(cachedContent)
//		if err != nil {
//			ctx.Warnf("error open file %v: %v",cachedContent,err)
//			return r, nil
//		}
//		return r, nil
//	})
func (ctx *ProxyCtx) Warnf(msg string, argv ...any) {
	ctx.printf("WARN: "+msg, argv...)
}

// Will try to infer the character set of the request from the headers.
// Returns the empty string if we don't know which character set it used.
// Currently it will look for charset=<charset> in the Content-Type header of the request.
func (ctx *ProxyCtx) Charset() string {
	contentType := ctx.Resp.Header.Get("Content-Type")
	if _, params, err := mime.ParseMediaType(contentType); err == nil {
		if cs, ok := params["charset"]; ok {
			return cs
		}
	}
	return ""
}
