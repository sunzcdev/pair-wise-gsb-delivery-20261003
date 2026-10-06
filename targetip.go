package goproxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// TargetIPPolicy decides whether an outbound connection may proceed, based on
// the IP address the proxy actually connected to. It is invoked after the
// TCP dial completes and before any byte is exchanged with the peer, for both
// plain HTTP requests (through ProxyHttpServer.Tr) and CONNECT tunnels.
//
// Checking the address at dial time (instead of resolving the hostname once,
// up front) closes the DNS-rebinding window where a hostname resolves to an
// allowed IP during validation but to a forbidden IP when the connection is
// really established.
//
// ip is the remote peer address, normalized to its 4-byte form for
// IPv4-mapped IPv6 addresses (e.g. "::ffff:10.0.0.1" is reported as
// "10.0.0.1"), so policies can match plain IPv4 networks.
//
// verified reports whether ip is guaranteed to be the final destination of
// the connection. It is false when the proxy cannot observe the real target:
//   - a custom dialer was used (ProxyCtx.Dialer, ConnectDial or
//     ConnectDialWithReq), which may tunnel through an upstream;
//   - the request is forwarded through an upstream proxy (Tr.Proxy), so the
//     next hop is the proxy, not the target;
//   - the peer address is not an IP address at all.
//
// A policy must not treat an unverified connection as allowed unless it
// explicitly chooses to; NewCIDRBlocklistPolicy rejects them.
//
// Returning a non-nil error closes the connection and fails the request.
type TargetIPPolicy func(ctx *ProxyCtx, ip net.IP, verified bool) error

// NewCIDRBlocklistPolicy returns a TargetIPPolicy that rejects connections
// whose target IP belongs to one of the given networks (e.g. internal ranges
// such as 10.0.0.0/8 or 169.254.0.0/16).
//
// Connections whose target IP cannot be verified (verified == false) are
// rejected as well: an unverified connection is never treated as allowed.
func NewCIDRBlocklistPolicy(networks ...*net.IPNet) TargetIPPolicy {
	return func(_ *ProxyCtx, ip net.IP, verified bool) error {
		if !verified || ip == nil {
			return errors.New("goproxy: target IP could not be verified")
		}
		for _, network := range networks {
			if network.Contains(ip) {
				return fmt.Errorf("goproxy: target IP %s is in blocked network %s", ip, network)
			}
		}
		return nil
	}
}

// dialTargetContextKey carries the expected target of a request down to the
// transport dial wrapper, so it can tell a direct dial apart from a dial to
// an upstream proxy.
type dialTargetContextKey struct{}

type dialTarget struct {
	// addr is the canonical host:port the request is addressed to.
	addr string
	ctx  *ProxyCtx
}

// canonicalTargetAddr makes the implicit default port of a request target
// explicit, so it can be compared with the address the transport dials.
func canonicalTargetAddr(host, scheme string) string {
	if hasPort(host) {
		return host
	}
	switch scheme {
	case "http", "ws":
		return host + ":80"
	case "https", "wss":
		return host + ":443"
	}
	return host
}

// checkTargetIP enforces TargetIPPolicy on an established connection. The
// connection is closed when the policy rejects it.
func (proxy *ProxyHttpServer) checkTargetIP(ctx *ProxyCtx, conn net.Conn, verified bool) error {
	policy := proxy.TargetIPPolicy
	if policy == nil || conn == nil {
		return nil
	}
	var ip net.IP
	if addr := conn.RemoteAddr(); addr != nil {
		if tcpAddr, ok := addr.(*net.TCPAddr); ok {
			ip = tcpAddr.IP
		} else {
			host, _, err := net.SplitHostPort(addr.String())
			if err != nil {
				host = addr.String()
			}
			ip = net.ParseIP(host)
		}
	}
	// Normalize IPv4-mapped IPv6 addresses ("::ffff:10.0.0.1") to their
	// 4-byte form, so policies match plain IPv4 networks.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	if ip == nil {
		// The peer address is not an IP address (e.g. a pipe): the target
		// cannot be considered verified.
		verified = false
	}
	if err := policy(ctx, ip, verified); err != nil {
		_ = conn.Close()
		return fmt.Errorf("goproxy: target IP policy rejected connection to %v: %w", ip, err)
	}
	return nil
}

// ensureTargetIPPolicy wraps the transport dialer of proxy.Tr so that every
// connection opened for plain HTTP requests is checked against
// TargetIPPolicy. It runs lazily on the first request and is a no-op when no
// policy is set. Pooled connections were checked when they were dialed, so
// reuse stays safe: a connection is only ever reused for the same target it
// was verified for.
func (proxy *ProxyHttpServer) ensureTargetIPPolicy() {
	if proxy.TargetIPPolicy == nil || proxy.Tr == nil {
		return
	}
	proxy.trDialMu.Lock()
	defer proxy.trDialMu.Unlock()
	if proxy.trDialWrapped == proxy.Tr {
		return
	}
	base := proxy.Tr.DialContext
	if base == nil {
		// Mirror the dialer configuration of http.DefaultTransport.
		base = (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	}
	proxy.Tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := base(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		target, ok := ctx.Value(dialTargetContextKey{}).(*dialTarget)
		if !ok {
			// The dial cannot be attributed to a request target (e.g. it
			// comes from the CONNECT path, which verifies the connection
			// itself in connectDial).
			return conn, nil
		}
		// When the transport dials the request target directly, the peer
		// address is the real target IP. When an upstream proxy is
		// configured (Tr.Proxy), the transport dials the proxy instead and
		// the real target IP is chosen by the upstream: mark it unverified.
		verified := strings.EqualFold(addr, target.addr)
		if err := proxy.checkTargetIP(target.ctx, conn, verified); err != nil {
			return nil, err
		}
		return conn, nil
	}
	proxy.trDialWrapped = proxy.Tr
}

// withDialTarget attaches the request target to the request context, so the
// transport dial wrapper installed by ensureTargetIPPolicy can verify the
// dialed connection against TargetIPPolicy.
func (proxy *ProxyHttpServer) withDialTarget(ctx *ProxyCtx, req *http.Request) *http.Request {
	if proxy.TargetIPPolicy == nil || req.URL == nil || req.URL.Host == "" {
		return req
	}
	proxy.ensureTargetIPPolicy()
	target := &dialTarget{
		addr: canonicalTargetAddr(req.URL.Host, req.URL.Scheme),
		ctx:  ctx,
	}
	return req.WithContext(context.WithValue(req.Context(), dialTargetContextKey{}, target))
}
