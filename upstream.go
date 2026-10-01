package socks5

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/things-go/go-socks5/statute"
)

// Failure phases reported by ChainError while a chain is being established.
const (
	// PhaseResolve is a local name resolution step.
	PhaseResolve = "resolve"
	// PhaseDial is the transport dial towards the first hop.
	PhaseDial = "dial"
	// PhaseHandshake is the SOCKS5 method negotiation with a hop.
	PhaseHandshake = "handshake"
	// PhaseAuth is the username/password sub-negotiation with a hop.
	PhaseAuth = "auth"
	// PhaseConnect is the CONNECT request/reply exchange with a hop.
	PhaseConnect = "connect"
)

// ChainError describes a failure while establishing an upstream chain.
// It always carries the zero-based hop index and the failed phase,
// and it never renders the hop's password.
type ChainError struct {
	// Hop is the zero-based index of the hop that failed.
	Hop int
	// Phase is the failed stage, one of the Phase* constants.
	Phase string
	// Addr is the address of the failed hop.
	Addr string
	// Err is the underlying error.
	Err error
	// secret is the hop password, kept only to scrub it from the message.
	secret string
}

// Error implements the error interface. The hop password is scrubbed
// from the message even if the underlying error leaked it.
func (e *ChainError) Error() string {
	msg := fmt.Sprintf("upstream chain: hop %d (%s) %s failed: %v", e.Hop, e.Addr, e.Phase, e.Err)
	if e.secret != "" {
		msg = strings.ReplaceAll(msg, e.secret, "***")
	}
	return msg
}

// Unwrap returns the underlying error.
func (e *ChainError) Unwrap() error { return e.Err }

// UpstreamHop describes one SOCKS5 proxy of an upstream chain.
type UpstreamHop struct {
	// Addr is the proxy address in host:port form.
	Addr string
	// Username and Password are the auth material for this hop only.
	Username string
	Password string
	// ResolveLocally makes explicit which side resolves Addr's host name:
	// true resolves it on this host before use, false sends the host name
	// through the tunnel and lets the previous hop resolve it.
	// The first hop is always dialed directly, so its address is always
	// resolved locally regardless of this flag.
	ResolveLocally bool
}

// UpstreamChain dials through an ordered chain of SOCKS5 proxies.
// Each hop is connected inside the tunnel established by the previous
// hops, and every hop uses its own address and auth material.
type UpstreamChain struct {
	// Hops is the ordered list of proxies; Hops[0] is dialed directly.
	Hops []UpstreamHop
	// ResolveDestLocally makes explicit which side resolves the final
	// destination host name: false (the zero value) sends the destination
	// FQDN to the last hop, which resolves it; true resolves it on this
	// host before the CONNECT request is issued. Keep it false for
	// isolated networks so their names are never resolved locally.
	ResolveDestLocally bool
	// Resolver is used for every local resolution. Defaults to DNSResolver.
	Resolver NameResolver
	// Dialer establishes the transport connection to the first hop.
	// Defaults to a zero net.Dialer.
	Dialer interface {
		DialContext(ctx context.Context, network, addr string) (net.Conn, error)
	}
	// HandshakeTimeout bounds each hop's handshake. Zero means no timeout.
	HandshakeTimeout time.Duration
}

// Dial establishes the chain and connects to addr ("host:port").
func (c *UpstreamChain) Dial(network, addr string) (net.Conn, error) {
	return c.DialContext(context.Background(), network, addr)
}

// DialContext establishes the chain and connects to addr ("host:port").
// Only tcp networks are supported. On failure every connection that was
// already opened is closed and the returned error is a *ChainError.
func (c *UpstreamChain) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	switch network {
	case "tcp", "tcp4", "tcp6":
	default:
		return nil, fmt.Errorf("upstream chain: unsupported network %q", network)
	}
	if len(c.Hops) == 0 {
		return nil, errors.New("upstream chain: no hops configured")
	}
	dest, err := statute.ParseAddrSpec(addr)
	if err != nil {
		return nil, fmt.Errorf("upstream chain: bad destination %q: %w", addr, err)
	}

	resolver := c.Resolver
	if resolver == nil {
		resolver = DNSResolver{}
	}
	dialer := c.Dialer
	if dialer == nil {
		dialer = &net.Dialer{}
	}

	var conn net.Conn
	for i := range c.Hops {
		hop := &c.Hops[i]
		if i == 0 {
			// The first hop is dialed directly; its address is resolved locally.
			conn, err = dialer.DialContext(ctx, network, hop.Addr)
			if err != nil {
				return nil, hop.chainError(i, PhaseDial, err)
			}
		}

		// Pick the address this hop is asked to CONNECT to: the next
		// hop's proxy address, or the final destination for the last hop.
		var next statute.AddrSpec
		if i+1 < len(c.Hops) {
			next, err = c.hopTarget(ctx, resolver, i+1)
		} else {
			next, err = c.destTarget(ctx, resolver, &dest)
		}
		if err != nil {
			conn.Close() // nolint: errcheck
			return nil, err
		}

		if err := c.handshake(conn, hop, i, &next); err != nil {
			conn.Close() // nolint: errcheck
			return nil, err
		}
	}
	return conn, nil
}

// hopTarget returns the address used to reach hop idx, resolving it
// locally when that hop opted into local resolution.
func (c *UpstreamChain) hopTarget(ctx context.Context, resolver NameResolver, idx int) (statute.AddrSpec, error) {
	hop := &c.Hops[idx]
	spec, err := statute.ParseAddrSpec(hop.Addr)
	if err != nil {
		return spec, hop.chainError(idx, PhaseDial, fmt.Errorf("bad hop address: %w", err))
	}
	if spec.FQDN != "" && hop.ResolveLocally {
		if _, ip, err := resolver.Resolve(ctx, spec.FQDN); err != nil {
			return spec, hop.chainError(idx, PhaseResolve, fmt.Errorf("resolve hop address %q: %w", spec.FQDN, err))
		} else {
			setSpecIP(&spec, ip)
		}
	}
	return spec, nil
}

// destTarget returns the final destination address, resolving it locally
// only when the chain opted into local destination resolution.
func (c *UpstreamChain) destTarget(ctx context.Context, resolver NameResolver, dest *statute.AddrSpec) (statute.AddrSpec, error) {
	spec := *dest
	if spec.FQDN != "" && c.ResolveDestLocally {
		if _, ip, err := resolver.Resolve(ctx, spec.FQDN); err != nil {
			return spec, c.chainErrorAt(len(c.Hops)-1, PhaseResolve,
				fmt.Errorf("resolve destination %q locally: %w", spec.FQDN, err))
		} else {
			setSpecIP(&spec, ip)
		}
	}
	return spec, nil
}

// handshake performs the SOCKS5 method negotiation, optional
// username/password auth and the CONNECT request to target with the
// proxy reached via conn, using hop's own auth material.
func (c *UpstreamChain) handshake(conn net.Conn, hop *UpstreamHop, idx int, target *statute.AddrSpec) error {
	if c.HandshakeTimeout > 0 {
		conn.SetDeadline(time.Now().Add(c.HandshakeTimeout)) // nolint: errcheck
		defer conn.SetDeadline(time.Time{})                  // nolint: errcheck
	}

	methods := []byte{statute.MethodNoAuth}
	if hop.Username != "" || hop.Password != "" {
		methods = []byte{statute.MethodUserPassAuth, statute.MethodNoAuth}
	}
	if _, err := conn.Write(statute.NewMethodRequest(statute.VersionSocks5, methods).Bytes()); err != nil {
		return hop.chainError(idx, PhaseHandshake, err)
	}
	mr, err := statute.ParseMethodReply(conn)
	if err != nil {
		return hop.chainError(idx, PhaseHandshake, err)
	}
	if mr.Ver != statute.VersionSocks5 {
		return hop.chainError(idx, PhaseHandshake, statute.ErrNotSupportVersion)
	}

	switch mr.Method {
	case statute.MethodNoAuth:
	case statute.MethodUserPassAuth:
		if hop.Username == "" && hop.Password == "" {
			return hop.chainError(idx, PhaseAuth,
				errors.New("hop requires username/password authentication but no credentials are configured"))
		}
		up := statute.NewUserPassRequest(statute.UserPassAuthVersion, []byte(hop.Username), []byte(hop.Password))
		if _, err := conn.Write(up.Bytes()); err != nil {
			return hop.chainError(idx, PhaseAuth, err)
		}
		upr, err := statute.ParseUserPassReply(conn)
		if err != nil {
			return hop.chainError(idx, PhaseAuth, err)
		}
		if upr.Status != statute.AuthSuccess {
			return hop.chainError(idx, PhaseAuth, statute.ErrUserAuthFailed)
		}
	default:
		return hop.chainError(idx, PhaseHandshake, statute.ErrNoSupportedAuth)
	}

	req := statute.Request{
		Version: statute.VersionSocks5,
		Command: statute.CommandConnect,
		DstAddr: *target,
	}
	if _, err := conn.Write(req.Bytes()); err != nil {
		return hop.chainError(idx, PhaseConnect, err)
	}
	rep, err := statute.ParseReply(conn)
	if err != nil {
		return hop.chainError(idx, PhaseConnect, err)
	}
	if rep.Response != statute.RepSuccess {
		return hop.chainError(idx, PhaseConnect, fmt.Errorf("CONNECT rejected with reply code %d", rep.Response))
	}
	return nil
}

// chainError builds a ChainError for hop idx, scrubbing the hop password.
func (c *UpstreamChain) chainErrorAt(idx int, phase string, err error) *ChainError {
	hop := &c.Hops[idx]
	return hop.chainError(idx, phase, err)
}

// chainError builds a ChainError for this hop. The password is never
// rendered; it is only kept to scrub accidental leaks.
func (h *UpstreamHop) chainError(idx int, phase string, err error) *ChainError {
	return &ChainError{
		Hop:    idx,
		Phase:  phase,
		Addr:   h.Addr,
		Err:    err,
		secret: h.Password,
	}
}

// setSpecIP stores a resolved IP in spec and fixes its address type.
func setSpecIP(spec *statute.AddrSpec, ip net.IP) {
	spec.IP = ip
	if ip.To4() != nil {
		spec.AddrType = statute.ATYPIPv4
	} else {
		spec.AddrType = statute.ATYPIPv6
	}
}
