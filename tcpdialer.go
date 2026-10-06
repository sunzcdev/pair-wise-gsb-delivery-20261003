package fasthttp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Dial dials the given TCP addr using tcp4.
//
// This function has the following additional features comparing to net.Dial:
//
//   - It reduces load on DNS resolver by caching resolved TCP addressed
//     for DNSCacheDuration.
//   - It dials all the resolved TCP addresses in round-robin manner until
//     connection is established. This may be useful if certain addresses
//     are temporarily unreachable.
//   - It returns ErrDialTimeout if connection cannot be established during
//     DefaultDialTimeout seconds. Use DialTimeout for customizing dial timeout.
//
// This dialer is intended for custom code wrapping before passing
// to Client.Dial or HostClient.Dial.
//
// For instance, per-host counters and/or limits may be implemented
// by such wrappers.
//
// The addr passed to the function must contain port. Example addr values:
//
//   - foobar.baz:443
//   - foo.bar:80
//   - aaa.com:8080
func Dial(addr string) (net.Conn, error) {
	return defaultDialer.Dial(addr)
}

// DialTimeout dials the given TCP addr using tcp4 using the given timeout.
//
// This function has the following additional features comparing to net.Dial:
//
//   - It reduces load on DNS resolver by caching resolved TCP addressed
//     for DNSCacheDuration.
//   - It dials all the resolved TCP addresses in round-robin manner until
//     connection is established. This may be useful if certain addresses
//     are temporarily unreachable.
//
// This dialer is intended for custom code wrapping before passing
// to Client.DialTimeout or HostClient.DialTimeout.
//
// For instance, per-host counters and/or limits may be implemented
// by such wrappers.
//
// The addr passed to the function must contain port. Example addr values:
//
//   - foobar.baz:443
//   - foo.bar:80
//   - aaa.com:8080
func DialTimeout(addr string, timeout time.Duration) (net.Conn, error) {
	return defaultDialer.DialTimeout(addr, timeout)
}

// DialDualStack dials the given TCP addr using both tcp4 and tcp6.
//
// This function has the following additional features comparing to net.Dial:
//
//   - It reduces load on DNS resolver by caching resolved TCP addressed
//     for DNSCacheDuration.
//   - It dials all the resolved TCP addresses in round-robin manner until
//     connection is established. This may be useful if certain addresses
//     are temporarily unreachable.
//   - It returns ErrDialTimeout if connection cannot be established during
//     DefaultDialTimeout seconds. Use DialDualStackTimeout for custom dial
//     timeout.
//
// This dialer is intended for custom code wrapping before passing
// to Client.Dial or HostClient.Dial.
//
// For instance, per-host counters and/or limits may be implemented
// by such wrappers.
//
// The addr passed to the function must contain port. Example addr values:
//
//   - foobar.baz:443
//   - foo.bar:80
//   - aaa.com:8080
func DialDualStack(addr string) (net.Conn, error) {
	return defaultDialer.DialDualStack(addr)
}

// DialDualStackTimeout dials the given TCP addr using both tcp4 and tcp6
// using the given timeout.
//
// This function has the following additional features comparing to net.Dial:
//
//   - It reduces load on DNS resolver by caching resolved TCP addressed
//     for DNSCacheDuration.
//   - It dials all the resolved TCP addresses in round-robin manner until
//     connection is established. This may be useful if certain addresses
//     are temporarily unreachable.
//
// This dialer is intended for custom code wrapping before passing
// to Client.DialTimeout or HostClient.DialTimeout.
//
// For instance, per-host counters and/or limits may be implemented
// by such wrappers.
//
// The addr passed to the function must contain port. Example addr values:
//
//   - foobar.baz:443
//   - foo.bar:80
//   - aaa.com:8080
func DialDualStackTimeout(addr string, timeout time.Duration) (net.Conn, error) {
	return defaultDialer.DialDualStackTimeout(addr, timeout)
}

var defaultDialer = &TCPDialer{Concurrency: 1000}

// dialDefaultTrace dials through the package default TCPDialer while emitting
// DNS and connect phases on trace. It is used by the client when no custom
// Dial/DialTimeout function is configured. A zero timeout falls back to the
// default dial timeout, matching the untraced path.
func dialDefaultTrace(addr string, dualStack, isTLS bool, timeout time.Duration, trace phaseTracker) (net.Conn, bool, error) {
	if timeout <= 0 {
		timeout = DefaultDialTimeout
	}
	conn, err := defaultDialer.dialTrace(AddMissingPort(addr, isTLS), dualStack, timeout, trace)
	if err != nil {
		return nil, false, err
	}
	_, isTLSAlready := conn.(interface{ Handshake() error })
	return conn, isTLSAlready, nil
}

// Resolver represents interface of the tcp resolver.
type Resolver interface {
	LookupIPAddr(context.Context, string) (names []net.IPAddr, err error)
}

// TCPDialer contains options to control a group of Dial calls.
type TCPDialer struct {
	// This may be used to override DNS resolving policy, like this:
	// var dialer = &fasthttp.TCPDialer{
	// 	Resolver: &net.Resolver{
	// 		PreferGo:     true,
	// 		StrictErrors: false,
	// 		Dial: func (ctx context.Context, network, address string) (net.Conn, error) {
	// 			d := net.Dialer{}
	// 			return d.DialContext(ctx, "udp", "8.8.8.8:53")
	// 		},
	// 	},
	// }
	Resolver Resolver

	// LocalAddr is the local address to use when dialing an
	// address.
	// If nil, a local address is automatically chosen.
	LocalAddr *net.TCPAddr

	concurrencyCh chan struct{}

	tcpAddrsMap    sync.Map
	cleanerRunning atomic.Bool

	// Concurrency controls the maximum number of concurrent Dials
	// that can be performed using this object.
	// Setting this to 0 means unlimited.
	//
	// WARNING: This can only be changed before the first Dial.
	// Changes made after the first Dial will not affect anything.
	Concurrency int

	// DNSCacheDuration may be used to override the default DNS cache duration (DefaultDNSCacheDuration)
	DNSCacheDuration time.Duration

	once sync.Once

	// DisableDNSResolution may be used to disable DNS resolution
	DisableDNSResolution bool

	// NetDial optionally overrides the low-level TCP connection establishment
	// after addresses are resolved. It receives the resolved address (already
	// containing host and port) and must return a connected TCP connection.
	//
	// It is useful for controlling dial behavior in tests or for wrapping the
	// underlying net.Dialer; DNS resolution timing is still reported
	// separately. The default net.Dialer is used when NetDial is nil.
	NetDial func(ctx context.Context, network, address string) (net.Conn, error)
}

// Dial dials the given TCP addr using tcp4.
//
// This function has the following additional features comparing to net.Dial:
//
//   - It reduces load on DNS resolver by caching resolved TCP addressed
//     for DNSCacheDuration.
//   - It dials all the resolved TCP addresses in round-robin manner until
//     connection is established. This may be useful if certain addresses
//     are temporarily unreachable.
//   - It returns ErrDialTimeout if connection cannot be established during
//     DefaultDialTimeout seconds. Use DialTimeout for customizing dial timeout.
//
// This dialer is intended for custom code wrapping before passing
// to Client.Dial or HostClient.Dial.
//
// For instance, per-host counters and/or limits may be implemented
// by such wrappers.
//
// The addr passed to the function must contain port. Example addr values:
//
//   - foobar.baz:443
//   - foo.bar:80
//   - aaa.com:8080
func (d *TCPDialer) Dial(addr string) (net.Conn, error) {
	return d.dialTrace(addr, false, DefaultDialTimeout, phaseTracker{})
}

// DialTimeout dials the given TCP addr using tcp4 using the given timeout.
//
// This function has the following additional features comparing to net.Dial:
//
//   - It reduces load on DNS resolver by caching resolved TCP addressed
//     for DNSCacheDuration.
//   - It dials all the resolved TCP addresses in round-robin manner until
//     connection is established. This may be useful if certain addresses
//     are temporarily unreachable.
//
// This dialer is intended for custom code wrapping before passing
// to Client.DialTimeout or HostClient.DialTimeout.
//
// For instance, per-host counters and/or limits may be implemented
// by such wrappers.
//
// The addr passed to the function must contain port. Example addr values:
//
//   - foobar.baz:443
//   - foo.bar:80
//   - aaa.com:8080
func (d *TCPDialer) DialTimeout(addr string, timeout time.Duration) (net.Conn, error) {
	return d.dialTrace(addr, false, timeout, phaseTracker{})
}

// DialDualStack dials the given TCP addr using both tcp4 and tcp6.
//
// This function has the following additional features comparing to net.Dial:
//
//   - It reduces load on DNS resolver by caching resolved TCP addressed
//     for DNSCacheDuration.
//   - It dials all the resolved TCP addresses in round-robin manner until
//     connection is established. This may be useful if certain addresses
//     are temporarily unreachable.
//   - It returns ErrDialTimeout if connection cannot be established during
//     DefaultDialTimeout seconds. Use DialDualStackTimeout for custom dial
//     timeout.
//
// This dialer is intended for custom code wrapping before passing
// to Client.Dial or HostClient.Dial.
//
// For instance, per-host counters and/or limits may be implemented
// by such wrappers.
//
// The addr passed to the function must contain port. Example addr values:
//
//   - foobar.baz:443
//   - foo.bar:80
//   - aaa.com:8080
func (d *TCPDialer) DialDualStack(addr string) (net.Conn, error) {
	return d.dialTrace(addr, true, DefaultDialTimeout, phaseTracker{})
}

// DialDualStackTimeout dials the given TCP addr using both tcp4 and tcp6
// using the given timeout.
//
// This function has the following additional features comparing to net.Dial:
//
//   - It reduces load on DNS resolver by caching resolved TCP addressed
//     for DNSCacheDuration.
//   - It dials all the resolved TCP addresses in round-robin manner until
//     connection is established. This may be useful if certain addresses
//     are temporarily unreachable.
//
// This dialer is intended for custom code wrapping before passing
// to Client.DialTimeout or HostClient.DialTimeout.
//
// For instance, per-host counters and/or limits may be implemented
// by such wrappers.
//
// The addr passed to the function must contain port. Example addr values:
//
//   - foobar.baz:443
//   - foo.bar:80
//   - aaa.com:8080
func (d *TCPDialer) DialDualStackTimeout(addr string, timeout time.Duration) (net.Conn, error) {
	return d.dialTrace(addr, true, timeout, phaseTracker{})
}

// dialTrace is dial with per-request phase tracing. When trace is disabled it
// behaves exactly like dial.
func (d *TCPDialer) dialTrace(addr string, dualStack bool, timeout time.Duration, trace phaseTracker) (net.Conn, error) {
	d.once.Do(func() {
		if d.Concurrency > 0 {
			d.concurrencyCh = make(chan struct{}, d.Concurrency)
		}

		if d.DNSCacheDuration == 0 {
			d.DNSCacheDuration = DefaultDNSCacheDuration
		}
	})
	deadline := time.Now().Add(timeout)
	network := "tcp4"
	if dualStack {
		network = "tcp"
	}
	if d.DisableDNSResolution {
		return d.tryDialTrace(network, addr, deadline, d.concurrencyCh, trace)
	}
	dnsStart := trace.start(PhaseDNS)
	addrs, idx, err := d.getTCPAddrs(addr, dualStack, deadline)
	trace.end(PhaseDNS, dnsStart, err)
	if err != nil {
		return nil, err
	}
	d.startTCPAddrsClean()
	var conn net.Conn
	n := uint32(len(addrs)) // #nosec G115
	for range n {
		conn, err = d.tryDialTrace(network, addrs[idx%n].String(), deadline, d.concurrencyCh, trace)
		if err == nil {
			return conn, nil
		}
		if errors.Is(err, ErrDialTimeout) {
			return nil, err
		}
		idx++
	}
	return nil, err
}

// FlushDNSCache clears all cached DNS entries, forcing fresh DNS lookups on subsequent dials.
// This is useful when you want to ensure fresh DNS resolution, for example after network changes.
func (d *TCPDialer) FlushDNSCache() {
	d.tcpAddrsMap.Range(func(k, v any) bool {
		d.tcpAddrsMap.Delete(k)
		return true
	})
}

// FlushDNSCache clears all cached DNS entries for the default dialer,
// forcing fresh DNS lookups on subsequent Dial* calls.
// This is useful when you want to ensure fresh DNS resolution, for example after network changes.
func FlushDNSCache() {
	defaultDialer.FlushDNSCache()
}

func (d *TCPDialer) tryDialTrace(
	network string, addr string, deadline time.Time, concurrencyCh chan struct{},
	trace phaseTracker,
) (net.Conn, error) {
	timeout := time.Until(deadline)
	if timeout <= 0 {
		return nil, wrapDialWithUpstream(ErrDialTimeout, addr)
	}

	if concurrencyCh != nil {
		select {
		case concurrencyCh <- struct{}{}:
		default:
			tc := AcquireTimer(timeout)
			isTimeout := false
			select {
			case concurrencyCh <- struct{}{}:
			case <-tc.C:
				isTimeout = true
			}
			ReleaseTimer(tc)
			if isTimeout {
				return nil, wrapDialWithUpstream(ErrDialTimeout, addr)
			}
		}
		defer func() { <-concurrencyCh }()
	}

	ctx, cancelCtx := context.WithDeadline(context.Background(), deadline)
	defer cancelCtx()
	connectStart := trace.start(PhaseConnect)
	var conn net.Conn
	var err error
	if d.NetDial != nil {
		conn, err = d.NetDial(ctx, network, addr)
	} else {
		dialer := net.Dialer{}
		if d.LocalAddr != nil {
			dialer.LocalAddr = d.LocalAddr
		}
		conn, err = dialer.DialContext(ctx, network, addr)
	}
	trace.end(PhaseConnect, connectStart, err)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, wrapDialWithUpstream(ErrDialTimeout, addr)
		}
		return nil, wrapDialWithUpstream(err, addr)
	}
	return conn, nil
}

// ErrDialTimeout is returned when TCP dialing is timed out.
var ErrDialTimeout = errors.New("fasthttp: dialing to the given tcp address timed out")

// ErrDialWithUpstream wraps dial error with upstream info.
//
// Use errors.AsType to get upstream information from the error:
//
//	hc := fasthttp.HostClient{Addr: "foo.com,bar.com"}
//	err := hc.Do(req, res)
//
//	if dialErr, ok := errors.AsType[*fasthttp.ErrDialWithUpstream](err); ok {
//		upstream = dialErr.Upstream // 34.206.39.153:80
//	}
type ErrDialWithUpstream struct {
	wrapErr  error
	Upstream string
}

func (e *ErrDialWithUpstream) Error() string {
	return fmt.Sprintf("error when dialing %s: %s", e.Upstream, e.wrapErr.Error())
}

func (e *ErrDialWithUpstream) Unwrap() error {
	return e.wrapErr
}

func wrapDialWithUpstream(err error, upstream string) error {
	return &ErrDialWithUpstream{
		Upstream: upstream,
		wrapErr:  err,
	}
}

// DefaultDialTimeout is timeout used by Dial and DialDualStack
// for establishing TCP connections.
const DefaultDialTimeout = 3 * time.Second

type tcpAddrEntry struct {
	resolveTime time.Time
	addrs       []net.TCPAddr
	addrsIdx    uint32

	pending int32
}

// DefaultDNSCacheDuration is the duration for caching resolved TCP addresses
// by Dial* functions.
const DefaultDNSCacheDuration = time.Minute

// cleanExpiredDNSEntries removes expired DNS cache entries based on DNSCacheDuration.
// This is the core cleanup logic used by both the background cleaner and manual cleanup.
func (d *TCPDialer) cleanExpiredDNSEntries() bool {
	expireDuration := 2 * d.DNSCacheDuration

	t := time.Now()
	hasEntries := false
	d.tcpAddrsMap.Range(func(k, v any) bool {
		if e, ok := v.(*tcpAddrEntry); ok && t.Sub(e.resolveTime) > expireDuration {
			d.tcpAddrsMap.Delete(k)
		} else {
			hasEntries = true
		}
		return true
	})
	return hasEntries
}

func (d *TCPDialer) startTCPAddrsClean() {
	if d.cleanerRunning.CompareAndSwap(false, true) {
		go d.tcpAddrsClean()
	}
}

func (d *TCPDialer) hasTCPAddrsEntries() bool {
	hasEntries := false
	d.tcpAddrsMap.Range(func(k, v any) bool {
		hasEntries = true
		return false
	})
	return hasEntries
}

var tcpAddrsCleanInterval = int64(time.Second)

func (d *TCPDialer) tcpAddrsClean() {
	t := time.NewTicker(time.Duration(atomic.LoadInt64(&tcpAddrsCleanInterval)))
	defer t.Stop()

	for range t.C {
		if !d.cleanExpiredDNSEntries() {
			d.cleanerRunning.Store(false)
			// A dial may store a new DNS entry while cleanerRunning is still
			// true and skip starting a cleaner. Reclaim ownership in that case.
			if !d.hasTCPAddrsEntries() {
				return
			}
			if d.cleanerRunning.CompareAndSwap(false, true) {
				continue
			}
			// Another dial already claimed ownership and started a cleaner.
			return
		}
	}
}

func (d *TCPDialer) getTCPAddrs(addr string, dualStack bool, deadline time.Time) ([]net.TCPAddr, uint32, error) {
	item, exist := d.tcpAddrsMap.Load(addr)
	e, ok := item.(*tcpAddrEntry)
	if exist && ok && e != nil && time.Since(e.resolveTime) > d.DNSCacheDuration {
		// Only let one goroutine re-resolve at a time.
		if atomic.SwapInt32(&e.pending, 1) == 0 {
			e = nil
		}
	}

	if e == nil {
		addrs, err := resolveTCPAddrs(addr, dualStack, d.Resolver, deadline)
		if err != nil {
			item, exist := d.tcpAddrsMap.Load(addr)
			e, ok = item.(*tcpAddrEntry)
			if exist && ok && e != nil {
				// Set pending to 0 so another goroutine can retry.
				atomic.StoreInt32(&e.pending, 0)
			}
			return nil, 0, err
		}

		e = &tcpAddrEntry{
			addrs:       addrs,
			resolveTime: time.Now(),
		}
		d.tcpAddrsMap.Store(addr, e)
	}

	idx := atomic.AddUint32(&e.addrsIdx, 1)
	return e.addrs, idx, nil
}

func resolveTCPAddrs(addr string, dualStack bool, resolver Resolver, deadline time.Time) ([]net.TCPAddr, error) {
	host, portS, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portS)
	if err != nil {
		return nil, err
	}

	if resolver == nil {
		resolver = net.DefaultResolver
	}

	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	ipaddrs, err := resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}

	n := len(ipaddrs)
	addrs := make([]net.TCPAddr, 0, n)
	for i := range n {
		ip := ipaddrs[i]
		if !dualStack && ip.IP.To4() == nil {
			continue
		}
		addrs = append(addrs, net.TCPAddr{
			IP:   ip.IP,
			Port: port,
			Zone: ip.Zone,
		})
	}
	if len(addrs) == 0 {
		return nil, errNoDNSEntries
	}
	return addrs, nil
}

var errNoDNSEntries = errors.New("couldn't find dns entries for the given domain: try using dual-stack dialing")
