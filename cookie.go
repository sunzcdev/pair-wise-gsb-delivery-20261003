package dns

// This file implements the optional server side of the DNS Cookies
// extension, as specified in RFC 7873 and RFC 9018. A ServerCookieHandler
// validates client supplied cookies and mints server cookies without keeping
// any per-client state: authenticity is established with a keyed digest and
// freshness is encoded into the server cookie itself.

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"
)

const (
	// ClientCookieLen is the fixed length, in octets, of a client cookie.
	ClientCookieLen = 8

	// serverCookieFixedLen is the length of the fixed prefix of an RFC 9018
	// server cookie: one version octet, three reserved octets and four
	// timestamp octets.
	serverCookieFixedLen = 8
	// ServerCookieMinLen is the minimum length, in octets, of a server cookie.
	ServerCookieMinLen = 16
	// ServerCookieMaxLen is the maximum length, in octets, of a server cookie.
	ServerCookieMaxLen = 32
	// ServerCookieLen is the length, in octets, of the server cookies minted
	// by this package: an 8 octet prefix and a 16 octet digest.
	ServerCookieLen = 24

	// serverCookieSecretLen is the required length, in octets, of an RFC 9018
	// server secret.
	serverCookieSecretLen = 32

	// serverCookieVersion is the only server cookie version defined by RFC 9018.
	serverCookieVersion byte = 1

	// cookieTimestampUnit is the unit, in seconds, of the server cookie
	// timestamp.
	cookieTimestampUnit = 3600
)

// ServerCookieStatus describes how a ServerCookieHandler handled the cookie
// option in a request. The caller decides which statuses warrant a BADCOOKIE
// response and how the response is sized.
type ServerCookieStatus int

const (
	// ServerCookieNoCookie means the request carried no COOKIE option.
	ServerCookieNoCookie ServerCookieStatus = iota
	// ServerCookieOK means the request carried a valid server cookie minted
	// for the current hour and the client's source address.
	ServerCookieOK
	// ServerCookieStale means the request carried a valid server cookie for
	// the previous hour. It is treated as valid and the received server
	// cookie is echoed; the next response carrying a current-hour cookie
	// rolls the client forward.
	ServerCookieStale
	// ServerCookieMissing means the request carried a well-formed client
	// cookie but no server cookie. The response must carry a freshly minted
	// server cookie, typically with RcodeBadCookie on UDP.
	ServerCookieMissing
	// ServerCookieExpired means the request carried a syntactically valid
	// server cookie with a correct digest that was older than the permitted
	// time window. The response carries a freshly minted server cookie,
	// typically with RcodeBadCookie on UDP.
	ServerCookieExpired
	// ServerCookieBad means the request carried a malformed cookie, a server
	// cookie with an unsupported version, a digest that did not match any
	// configured secret, or a cookie bound to another source address.
	ServerCookieBad
)

// String implements the fmt.Stringer interface.
func (s ServerCookieStatus) String() string {
	switch s {
	case ServerCookieNoCookie:
		return "no-cookie"
	case ServerCookieOK:
		return "ok"
	case ServerCookieStale:
		return "stale"
	case ServerCookieMissing:
		return "missing-server-cookie"
	case ServerCookieExpired:
		return "expired-server-cookie"
	case ServerCookieBad:
		return "bad-server-cookie"
	}
	return fmt.Sprintf("cookie-status-%d", int(s))
}

var (
	// ErrNoCookieSecret is returned when an operation needs a server secret
	// but the key ring is empty.
	ErrNoCookieSecret = errors.New("dns: no server cookie secret configured")
	// ErrBadCookieSecretLen is returned when a server secret does not have the
	// 32 octet length required by RFC 9018.
	ErrBadCookieSecretLen = fmt.Errorf("dns: server cookie secret must be %d octets", serverCookieSecretLen)
	// ErrInvalidClientCookie is returned when a client cookie is not exactly
	// ClientCookieLen octets long.
	ErrInvalidClientCookie = fmt.Errorf("dns: client cookie must be %d octets", ClientCookieLen)
)

// GenerateServerCookieSecret returns a new cryptographically random server
// secret suitable for use with a CookieKeyRing.
func GenerateServerCookieSecret() ([]byte, error) {
	secret := make([]byte, serverCookieSecretLen)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	return secret, nil
}

// CookieKeyRing holds the secrets used to mint and validate RFC 9018 server
// cookies. Secrets are ordered newest first: the first secret is used for
// minting while every secret is accepted during validation, which allows a
// server to rotate secrets while still honoring cookies minted earlier. The
// zero value is an empty ring.
//
// A CookieKeyRing may be used concurrently.
type CookieKeyRing struct {
	mu      sync.RWMutex
	secrets [][]byte
}

// NewCookieKeyRing returns a CookieKeyRing initialized with the given secrets,
// in newest first order. Every secret must be 32 octets long.
func NewCookieKeyRing(secrets ...[]byte) (*CookieKeyRing, error) {
	ring := new(CookieKeyRing)
	if err := ring.Replace(secrets...); err != nil {
		return nil, err
	}
	return ring, nil
}

// SecretCount reports the number of secrets currently in the ring.
func (r *CookieKeyRing) SecretCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.secrets)
}

// Rotate prepends secret as the new active secret and retains the previous
// ones for validating existing cookies. The secret must be 32 octets long.
func (r *CookieKeyRing) Rotate(secret []byte) error {
	if len(secret) != serverCookieSecretLen {
		return ErrBadCookieSecretLen
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	next := make([][]byte, 0, len(r.secrets)+1)
	next = append(next, append([]byte(nil), secret...))
	next = append(next, r.secrets...)
	r.secrets = next
	return nil
}

// Replace sets the ring contents to secrets, in newest first order, replacing
// any previously configured secret. Every secret must be 32 octets long.
func (r *CookieKeyRing) Replace(secrets ...[]byte) error {
	next := make([][]byte, 0, len(secrets))
	for _, secret := range secrets {
		if len(secret) != serverCookieSecretLen {
			return ErrBadCookieSecretLen
		}
		next = append(next, append([]byte(nil), secret...))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.secrets = next
	return nil
}

// snapshot returns a copy of the ring contents so callers can mint and
// validate without holding the lock.
func (r *CookieKeyRing) snapshot() [][]byte {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([][]byte, len(r.secrets))
	copy(out, r.secrets)
	return out
}

// ServerCookieHandler is the optional server side of the DNS Cookie protocol.
// It is stateless with respect to clients: server cookies are authenticated
// with secrets from KeyRing and carry their own minting time.
//
// A zero ServerCookieHandler must not be used; create one with
// NewServerCookieHandler. A ServerCookieHandler may be used concurrently.
type ServerCookieHandler struct {
	// KeyRing supplies the secrets used to mint and validate server cookies.
	KeyRing *CookieKeyRing
	// now returns the current time. If nil, time.Now is used. It exists to
	// allow deterministic tests and simulated clock skew.
	now func() time.Time
}

// NewServerCookieHandler returns a ServerCookieHandler using ring. If now is
// nil, wall clock time is used.
func NewServerCookieHandler(ring *CookieKeyRing, now func() time.Time) *ServerCookieHandler {
	if now == nil {
		now = time.Now
	}
	return &ServerCookieHandler{KeyRing: ring, now: now}
}

// cookieTimestamp returns the hour number encoded in a server cookie at the
// given time: Unix time divided by 3600.
func cookieTimestamp(t time.Time) uint32 {
	return uint32(t.Unix() / cookieTimestampUnit)
}

// cookieDigest computes the 16 octet server cookie digest defined by
// RFC 9018 section 4.2.1: the first 16 octets of SHA-256 over the server
// secret, the source address in network byte order (4 octets for IPv4,
// 16 octets for IPv6) and the 8 octet client cookie. The timestamp is not
// part of the digest input.
func cookieDigest(secret, ipBytes, clientCookie []byte) []byte {
	h := sha256.New()
	h.Write(secret)
	h.Write(ipBytes)
	h.Write(clientCookie)
	return h.Sum(nil)[:ServerCookieLen-serverCookieFixedLen]
}

// MintServerCookie returns a freshly minted RFC 9018 server cookie for
// clientCookie and source address addr. clientCookie must be exactly
// ClientCookieLen octets and addr must be an IPv4 or IPv6 address.
func (h *ServerCookieHandler) MintServerCookie(addr netip.Addr, clientCookie []byte) ([]byte, error) {
	if h == nil || h.KeyRing == nil {
		return nil, ErrNoCookieSecret
	}
	if len(clientCookie) != ClientCookieLen {
		return nil, ErrInvalidClientCookie
	}
	secrets := h.KeyRing.snapshot()
	if len(secrets) == 0 {
		return nil, ErrNoCookieSecret
	}
	ipBytes, err := cookieIPBytes(addr)
	if err != nil {
		return nil, err
	}

	serverCookie := make([]byte, ServerCookieLen)
	serverCookie[0] = serverCookieVersion
	// octets 1-3 are reserved and stay zero.
	binary.BigEndian.PutUint32(serverCookie[4:8], cookieTimestamp(h.now()))
	copy(serverCookie[8:], cookieDigest(secrets[0], ipBytes, clientCookie))
	return serverCookie, nil
}

// CheckServerCookie validates serverCookie against clientCookie and source
// address addr. The returned status is ServerCookieOK for a cookie minted in
// the current hour, ServerCookieStale for one minted in the previous hour,
// ServerCookieExpired for a syntactically and cryptographically valid cookie
// outside that window, and ServerCookieBad for malformed cookies, unsupported
// versions, cookies that do not match any configured secret or cookies bound
// to another address.
//
// ErrNoCookieSecret is returned if the key ring has no secret; validation
// errors are reported through ServerCookieBad and never include secret
// material.
func (h *ServerCookieHandler) CheckServerCookie(addr netip.Addr, clientCookie, serverCookie []byte) (ServerCookieStatus, error) {
	if h == nil || h.KeyRing == nil {
		return ServerCookieBad, ErrNoCookieSecret
	}
	if len(clientCookie) != ClientCookieLen {
		return ServerCookieBad, ErrInvalidClientCookie
	}
	secrets := h.KeyRing.snapshot()
	if len(secrets) == 0 {
		return ServerCookieBad, ErrNoCookieSecret
	}
	ipBytes, err := cookieIPBytes(addr)
	if err != nil {
		return ServerCookieBad, err
	}
	if len(serverCookie) < ServerCookieMinLen || len(serverCookie) > ServerCookieMaxLen {
		return ServerCookieBad, nil
	}
	if serverCookie[0] != serverCookieVersion {
		return ServerCookieBad, nil
	}
	if serverCookie[1] != 0 || serverCookie[2] != 0 || serverCookie[3] != 0 {
		return ServerCookieBad, nil
	}

	body := serverCookie[serverCookieFixedLen:]
	matched := false
	for _, secret := range secrets {
		expected := cookieDigest(secret, ipBytes, clientCookie)
		if len(body) > len(expected) {
			continue
		}
		// The body length is public wire data; compare its octets in
		// constant time against the digest prefix of the same length.
		if subtle.ConstantTimeCompare(body, expected[:len(body)]) == 1 {
			matched = true
			break
		}
	}
	if !matched {
		return ServerCookieBad, nil
	}

	current := cookieTimestamp(h.now())
	cookieTime := binary.BigEndian.Uint32(serverCookie[4:8])
	switch {
	case cookieTime == current:
		return ServerCookieOK, nil
	case cookieTime == current-1:
		return ServerCookieStale, nil
	case cookieTime < current-1:
		return ServerCookieExpired, nil
	default:
		// A timestamp in the future only happens with clock skew or a
		// forged-but-expired replay; it is not a valid cookie for this host.
		return ServerCookieBad, nil
	}
}

// cookieIPBytes returns the address bytes used in the cookie digest: 4
// octets for an IPv4 address (including an IPv4-mapped IPv6 address) and
// 16 octets for a native IPv6 address.
func cookieIPBytes(addr netip.Addr) ([]byte, error) {
	if !addr.IsValid() {
		return nil, errors.New("dns: invalid source address")
	}
	addr = addr.Unmap()
	if addr.Is4() {
		b := addr.As4()
		return b[:], nil
	}
	b := addr.As16()
	return b[:], nil
}

// ServerCookieResult is the outcome of checking the COOKIE option in a
// request. It never contains secret material.
type ServerCookieResult struct {
	// Status classifies the received cookie.
	Status ServerCookieStatus
	// ClientCookie is the 8 octet client cookie copied from the request, or
	// nil when the request had no usable client cookie.
	ClientCookie []byte
	// ServerCookie is the server cookie that the response should carry:
	// the received one when Status is ServerCookieOK, otherwise a freshly
	// minted one. It is nil when no server cookie can be minted.
	ServerCookie []byte
}

// Option returns an EDNS0_COOKIE option holding the response cookies, or nil
// when no cookie should be placed on the response.
func (r *ServerCookieResult) Option() *EDNS0_COOKIE {
	if r == nil || len(r.ClientCookie) != ClientCookieLen {
		return nil
	}
	cookie := make([]byte, ClientCookieLen, ClientCookieLen+len(r.ServerCookie))
	copy(cookie, r.ClientCookie)
	cookie = append(cookie, r.ServerCookie...)
	return &EDNS0_COOKIE{Code: EDNS0COOKIE, Cookie: hex.EncodeToString(cookie)}
}

// CheckRequest inspects the COOKIE option of m as if it were received from
// remote and returns the validated cookies to place on the response. It is a
// convenience wrapper around CheckRequestAddr.
//
// The caller remains responsible for building the response: RFC 7873
// recommends RcodeBadCookie on UDP for ServerCookieMissing,
// ServerCookieExpired and ServerCookieBad, a fresh server cookie is provided
// for every case that has a usable client cookie, and the caller controls
// whether the BADCOOKIE response is question-only and how it is padded or
// truncated to avoid amplification.
func (h *ServerCookieHandler) CheckRequest(m *Msg, remote net.Addr) (*ServerCookieResult, error) {
	addr, err := cookieRemoteAddr(remote)
	if err != nil {
		return nil, err
	}
	return h.CheckRequestAddr(m, addr)
}

// CheckRequestAddr is CheckRequest with a concrete source address.
func (h *ServerCookieHandler) CheckRequestAddr(m *Msg, remote netip.Addr) (*ServerCookieResult, error) {
	result := &ServerCookieResult{Status: ServerCookieNoCookie}

	var option *EDNS0_COOKIE
	if opt := m.IsEdns0(); opt != nil {
		for _, o := range opt.Option {
			if c, ok := o.(*EDNS0_COOKIE); ok {
				option = c
				break
			}
		}
	}
	if option == nil {
		return result, nil
	}

	raw, err := hex.DecodeString(option.Cookie)
	if err != nil || len(raw) < ClientCookieLen {
		result.Status = ServerCookieBad
		return result, nil
	}
	result.ClientCookie = append([]byte(nil), raw[:ClientCookieLen]...)

	receivedServerCookie := raw[ClientCookieLen:]
	switch len(receivedServerCookie) {
	case 0:
		result.Status = ServerCookieMissing
	default:
		status, err := h.CheckServerCookie(remote, result.ClientCookie, receivedServerCookie)
		if err != nil {
			return nil, err
		}
		result.Status = status
		if status == ServerCookieOK || status == ServerCookieStale {
			result.ServerCookie = append([]byte(nil), receivedServerCookie...)
		}
	}

	if result.ServerCookie == nil {
		fresh, err := h.MintServerCookie(remote, result.ClientCookie)
		if err != nil {
			return nil, err
		}
		result.ServerCookie = fresh
	}
	return result, nil
}

// cookieRemoteAddr extracts the IP address carried by a network address.
func cookieRemoteAddr(remote net.Addr) (netip.Addr, error) {
	if remote == nil {
		return netip.Addr{}, errors.New("dns: nil source address")
	}
	switch a := remote.(type) {
	case *net.UDPAddr:
		return ipToAddr(a.IP)
	case *net.TCPAddr:
		return ipToAddr(a.IP)
	case *net.IPAddr:
		return ipToAddr(a.IP)
	}
	host := remote.String()
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("dns: cannot derive source address from %T", remote)
	}
	return addr, nil
}

// ipToAddr converts a net.IP to a netip.Addr.
func ipToAddr(ip net.IP) (netip.Addr, error) {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return netip.Addr{}, errors.New("dns: invalid source address")
	}
	return addr, nil
}
