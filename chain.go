package socks5

import (
	"context"
	"fmt"
	"net"

	"github.com/things-go/go-socks5/statute"
)

// ResolveMode decides which end of a connection resolves a hostname.
type ResolveMode int

const (
	// ResolveRemote sends the hostname untouched to the upstream proxy,
	// which resolves it on its own side. This is the zero value and the
	// safe default for isolated networks: local DNS never sees the name.
	ResolveRemote ResolveMode = iota
	// ResolveLocal resolves the hostname locally (via the configured
	// NameResolver) before dialing or before sending it upstream.
	ResolveLocal
)

// UpstreamHop describes a single SOCKS5 proxy in a CONNECT upstream chain.
type UpstreamHop struct {
	// Addr is the host:port of this hop's SOCKS5 listener.
	Addr string
	// Username and Password are the optional auth material for this hop only.
	Username string
	Password string
	// Resolve decides where this hop's own address is resolved.
	// For intermediate hops ResolveRemote means the previous hop resolves
	// the name when it dials out. For the first hop the dial always happens
	// locally; ResolveLocal forces explicit resolution via the configured
	// NameResolver, ResolveRemote leaves it to the system dialer.
	Resolve ResolveMode
}

// Chain is an ordered list of upstream SOCKS5 proxies. A CONNECT request is
// tunneled through every hop in order: each hop is asked to CONNECT to the
// next hop, and the final hop connects to the actual destination.
type Chain struct {
	// Hops is the ordered list of upstream proxies, at least one required.
	Hops []UpstreamHop
	// TargetResolve decides which end resolves the final destination's
	// hostname: locally before the chain is built, or by the last hop.
	TargetResolve ResolveMode
}

// ChainPhase identifies the stage of a hop at which a failure occurred.
type ChainPhase string

// chain phases
const (
	ChainPhaseResolve   ChainPhase = "resolve"
	ChainPhaseDial      ChainPhase = "dial"
	ChainPhaseHandshake ChainPhase = "handshake"
	ChainPhaseAuth      ChainPhase = "auth"
	ChainPhaseConnect   ChainPhase = "connect"
)

// ChainError reports a failure while establishing an upstream chain.
// It carries the hop index, the hop address and the failing phase.
// It never carries credentials.
type ChainError struct {
	Hop   int
	Addr  string
	Phase ChainPhase
	Err   error
}

// Error implement interface error
func (e *ChainError) Error() string {
	return fmt.Sprintf("upstream chain: hop %d (%s) %s failed: %v", e.Hop, e.Addr, e.Phase, e.Err)
}

// Unwrap implement interface errors.Unwrap
func (e *ChainError) Unwrap() error { return e.Err }

// dial establishes the full chain and returns a connection to dest that
// traverses every hop. On failure every connection opened so far is closed
// (closing the outermost tunnel cascades through the upstream hops).
func (c *Chain) dial(ctx context.Context, resolver NameResolver, dest *statute.AddrSpec) (net.Conn, error) {
	if resolver == nil {
		resolver = DNSResolver{}
	}
	var conn net.Conn
	for i := range c.Hops {
		hop := &c.Hops[i]
		if conn == nil {
			addr, err := hop.dialAddr(ctx, resolver)
			if err != nil {
				return nil, &ChainError{Hop: i, Addr: hop.Addr, Phase: ChainPhaseResolve, Err: err}
			}
			d := net.Dialer{}
			conn, err = d.DialContext(ctx, "tcp", addr)
			if err != nil {
				return nil, &ChainError{Hop: i, Addr: hop.Addr, Phase: ChainPhaseDial, Err: err}
			}
		}
		// Handshake with this hop inside the tunnel built by the previous hop.
		phase, err := hop.handshake(conn)
		if err != nil {
			conn.Close() // nolint: errcheck
			return nil, &ChainError{Hop: i, Addr: hop.Addr, Phase: phase, Err: err}
		}
		// Ask this hop to CONNECT to the next hop, or to the final destination.
		next, err := c.nextAddr(ctx, resolver, i, dest)
		if err != nil {
			conn.Close() // nolint: errcheck
			return nil, &ChainError{Hop: i, Addr: hop.Addr, Phase: ChainPhaseResolve, Err: err}
		}
		if err := connectRequest(conn, next); err != nil {
			conn.Close() // nolint: errcheck
			return nil, &ChainError{Hop: i, Addr: hop.Addr, Phase: ChainPhaseConnect, Err: err}
		}
	}
	return conn, nil
}

// nextAddr returns the address hop i should connect to, honoring the
// configured resolution side for the next hop or the final destination.
func (c *Chain) nextAddr(ctx context.Context, resolver NameResolver, i int, dest *statute.AddrSpec) (statute.AddrSpec, error) {
	if i+1 < len(c.Hops) {
		next := &c.Hops[i+1]
		host, port, err := splitAddr(next.Addr)
		if err != nil {
			return statute.AddrSpec{}, err
		}
		if next.Resolve == ResolveLocal {
			return resolveAddrSpec(ctx, resolver, host, port)
		}
		return remoteAddrSpec(host, port), nil
	}
	if dest.FQDN != "" && c.TargetResolve == ResolveLocal {
		if len(dest.IP) != 0 {
			// Already resolved locally (e.g. by the server's resolver).
			return ipAddrSpec(dest.IP, dest.Port), nil
		}
		return resolveAddrSpec(ctx, resolver, dest.FQDN, dest.Port)
	}
	if dest.FQDN != "" {
		return statute.AddrSpec{FQDN: dest.FQDN, Port: dest.Port, AddrType: statute.ATYPDomain}, nil
	}
	return ipAddrSpec(dest.IP, dest.Port), nil
}

// dialAddr returns the host:port used to dial this hop directly.
func (hop *UpstreamHop) dialAddr(ctx context.Context, resolver NameResolver) (string, error) {
	host, port, err := splitAddr(hop.Addr)
	if err != nil {
		return "", err
	}
	if hop.Resolve != ResolveLocal || net.ParseIP(host) != nil {
		return hop.Addr, nil
	}
	as, err := resolveAddrSpec(ctx, resolver, host, port)
	if err != nil {
		return "", err
	}
	return as.String(), nil
}

// handshake negotiates the auth method with one hop and, when selected,
// completes username/password authentication. It returns the failing phase.
func (hop *UpstreamHop) handshake(conn net.Conn) (ChainPhase, error) {
	hasAuth := hop.Username != "" || hop.Password != ""
	methods := []byte{statute.MethodNoAuth}
	if hasAuth {
		methods = append(methods, statute.MethodUserPassAuth)
	}
	if _, err := conn.Write(statute.NewMethodRequest(statute.VersionSocks5, methods).Bytes()); err != nil {
		return ChainPhaseHandshake, err
	}
	mr, err := statute.ParseMethodReply(conn)
	if err != nil {
		return ChainPhaseHandshake, err
	}
	if mr.Ver != statute.VersionSocks5 {
		return ChainPhaseHandshake, statute.ErrNotSupportVersion
	}
	switch mr.Method {
	case statute.MethodNoAuth:
		return "", nil
	case statute.MethodUserPassAuth:
		if !hasAuth {
			return ChainPhaseAuth, statute.ErrNotSupportMethod
		}
		req := statute.NewUserPassRequest(statute.UserPassAuthVersion, []byte(hop.Username), []byte(hop.Password))
		if _, err := conn.Write(req.Bytes()); err != nil {
			return ChainPhaseAuth, err
		}
		upr, err := statute.ParseUserPassReply(conn)
		if err != nil {
			return ChainPhaseAuth, err
		}
		if upr.Status != statute.AuthSuccess {
			return ChainPhaseAuth, statute.ErrUserAuthFailed
		}
		return "", nil
	default:
		return ChainPhaseHandshake, statute.ErrNoSupportedAuth
	}
}

// connectRequest asks an upstream hop to CONNECT to addr and validates the reply.
func connectRequest(conn net.Conn, addr statute.AddrSpec) error {
	req := statute.Request{
		Version: statute.VersionSocks5,
		Command: statute.CommandConnect,
		DstAddr: addr,
	}
	if _, err := conn.Write(req.Bytes()); err != nil {
		return err
	}
	rep, err := statute.ParseReply(conn)
	if err != nil {
		return err
	}
	if rep.Response != statute.RepSuccess {
		return fmt.Errorf("upstream replied with status 0x%02x", rep.Response)
	}
	return nil
}

// splitAddr splits host:port into a host and a port number.
func splitAddr(addr string) (string, int, error) {
	as, err := statute.ParseAddrSpec(addr)
	if err != nil {
		return "", 0, err
	}
	if as.FQDN != "" {
		return as.FQDN, as.Port, nil
	}
	return as.IP.String(), as.Port, nil
}

// remoteAddrSpec builds the on-the-wire address for a name the upstream must resolve.
func remoteAddrSpec(host string, port int) statute.AddrSpec {
	if ip := net.ParseIP(host); ip != nil {
		return ipAddrSpec(ip, port)
	}
	return statute.AddrSpec{FQDN: host, Port: port, AddrType: statute.ATYPDomain}
}

// resolveAddrSpec resolves host locally and returns an IP-based address.
func resolveAddrSpec(ctx context.Context, resolver NameResolver, host string, port int) (statute.AddrSpec, error) {
	if ip := net.ParseIP(host); ip != nil {
		return ipAddrSpec(ip, port), nil
	}
	_, ip, err := resolver.Resolve(ctx, host)
	if err != nil {
		return statute.AddrSpec{}, err
	}
	return ipAddrSpec(ip, port), nil
}

// ipAddrSpec builds an IP-based address with the matching address type.
func ipAddrSpec(ip net.IP, port int) statute.AddrSpec {
	as := statute.AddrSpec{IP: ip, Port: port, AddrType: statute.ATYPIPv6}
	if ip.To4() != nil {
		as.AddrType = statute.ATYPIPv4
	}
	return as
}
