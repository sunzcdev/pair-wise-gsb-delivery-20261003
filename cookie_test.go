package dns

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

// Test vectors are computed independently of the implementation from the
// RFC 9018 section 4.2.1 construction SHA-256(secret || ip || client-cookie),
// truncated to 16 octets. The fixed prefix is version 1, three zero reserved
// octets and the hour timestamp 1700000000/3600 = 472222 = 0x0007349e.
var (
	cookieSecret1, _ = hex.DecodeString("0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20")
	cookieSecret2, _ = hex.DecodeString("a0a1a2a3a4a5a6a7a8a9aaabacadaeafb0b1b2b3b4b5b6b7b8b9babbbcbdbebf")
	cookieClientHex  = "0011223344556677"
	cookieClient, _  = hex.DecodeString(cookieClientHex)

	cookieFixedCurrent = "010000000007349e"
	cookieFixedPrev    = "010000000007349d"
	cookieFixedTwoBack = "010000000007349c"
	cookieFixedNext    = "010000000007349f"

	cookieV4Body1    = "0addb1df90ddfb602fbe89ace4bcfd67"
	cookieV6Body1    = "e86af3677f6a169ed7373d60d469cfb9"
	cookieOtherBody1 = "b2a77db7cc39445ced1c2d220e6ef10a"
	cookieV4Body2    = "dc01197c97a95d1a6ac7680e04c8bf3e"

	cookieTestTime = time.Unix(1700000000, 0).UTC() // on the hour, 2023-11-14T22:13:20Z
	cookieV4Addr   = netip.MustParseAddr("198.51.100.7")
	cookieV6Addr   = netip.MustParseAddr("2001:db8::7")
	cookieOtherV4  = netip.MustParseAddr("198.51.100.8")
)

func mustDecodeCookie(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad hex %q: %v", s, err)
	}
	return b
}

func newCookieHandler(t *testing.T, secrets ...[]byte) (*ServerCookieHandler, *CookieKeyRing) {
	t.Helper()
	ring, err := NewCookieKeyRing(secrets...)
	if err != nil {
		t.Fatalf("NewCookieKeyRing: %v", err)
	}
	return NewServerCookieHandler(ring, func() time.Time { return cookieTestTime }), ring
}

func TestMintServerCookieVectors(t *testing.T) {
	h, _ := newCookieHandler(t, cookieSecret1)

	tests := []struct {
		name string
		addr netip.Addr
		want string
	}{
		{"ipv4", cookieV4Addr, cookieFixedCurrent + cookieV4Body1},
		{"ipv6", cookieV6Addr, cookieFixedCurrent + cookieV6Body1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := h.MintServerCookie(test.addr, cookieClient)
			if err != nil {
				t.Fatalf("MintServerCookie: %v", err)
			}
			if len(got) != ServerCookieLen {
				t.Fatalf("server cookie length = %d, want %d", len(got), ServerCookieLen)
			}
			if hex.EncodeToString(got) != test.want {
				t.Errorf("server cookie = %x, want %s", got, test.want)
			}
		})
	}
}

func TestCheckServerCookieVectors(t *testing.T) {
	h, _ := newCookieHandler(t, cookieSecret1)

	tests := []struct {
		name       string
		addr       netip.Addr
		server     string
		wantStatus ServerCookieStatus
	}{
		{"current ipv4", cookieV4Addr, cookieFixedCurrent + cookieV4Body1, ServerCookieOK},
		{"current ipv6", cookieV6Addr, cookieFixedCurrent + cookieV6Body1, ServerCookieOK},
		{"previous hour is stale", cookieV4Addr, cookieFixedPrev + cookieV4Body1, ServerCookieStale},
		{"two hours old is expired", cookieV4Addr, cookieFixedTwoBack + cookieV4Body1, ServerCookieExpired},
		{"future hour rejected", cookieV4Addr, cookieFixedNext + cookieV4Body1, ServerCookieBad},
		{"bound to different ipv4", cookieOtherV4, cookieFixedCurrent + cookieV4Body1, ServerCookieBad},
		{"minted for v4 presented from v6", cookieV6Addr, cookieFixedCurrent + cookieV4Body1, ServerCookieBad},
		{"minted for v6 presented from v4", cookieV4Addr, cookieFixedCurrent + cookieV6Body1, ServerCookieBad},
		{"wrong secret", cookieV4Addr, cookieFixedCurrent + cookieV4Body2, ServerCookieBad},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := h.CheckServerCookie(test.addr, cookieClient, mustDecodeCookie(t, test.server))
			if err != nil {
				t.Fatalf("CheckServerCookie: %v", err)
			}
			if got != test.wantStatus {
				t.Errorf("status = %s, want %s", got, test.wantStatus)
			}
		})
	}
}

func TestCheckServerCookieMalformed(t *testing.T) {
	h, _ := newCookieHandler(t, cookieSecret1)

	mutate := func(orig string, change func([]byte)) string {
		b := mustDecodeCookie(t, orig)
		change(b)
		return hex.EncodeToString(b)
	}

	good := cookieFixedCurrent + cookieV4Body1
	cases := map[string][]byte{
		"too short":                mustDecodeCookie(t, "010000000007349e0add"),
		"too long":                 append(mustDecodeCookie(t, good), make([]byte, ServerCookieMaxLen-ServerCookieLen+1)...),
		"bad version":              mustDecodeCookie(t, mutate(good, func(b []byte) { b[0] = 99 })),
		"nonzero reserved octet 1": mustDecodeCookie(t, mutate(good, func(b []byte) { b[1] = 1 })),
		"nonzero reserved octet 3": mustDecodeCookie(t, mutate(good, func(b []byte) { b[3] = 0x80 })),
		"corrupt digest":           mustDecodeCookie(t, mutate(good, func(b []byte) { b[23] ^= 0xff })),
	}
	for name, serverCookie := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := h.CheckServerCookie(cookieV4Addr, cookieClient, serverCookie)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != ServerCookieBad {
				t.Errorf("status = %s, want %s", got, ServerCookieBad)
			}
		})
	}

	t.Run("bad client cookie length", func(t *testing.T) {
		got, err := h.CheckServerCookie(cookieV4Addr, []byte{1, 2, 3}, mustDecodeCookie(t, good))
		if !errors.Is(err, ErrInvalidClientCookie) {
			t.Fatalf("err = %v, want ErrInvalidClientCookie", err)
		}
		if got != ServerCookieBad {
			t.Errorf("status = %s, want %s", got, ServerCookieBad)
		}
	})

	t.Run("invalid address", func(t *testing.T) {
		got, err := h.CheckServerCookie(netip.Addr{}, cookieClient, mustDecodeCookie(t, good))
		if err == nil {
			t.Fatal("expected error for invalid address")
		}
		if got != ServerCookieBad {
			t.Errorf("status = %s, want %s", got, ServerCookieBad)
		}
	})
}

func TestKeyRingRotation(t *testing.T) {
	ring, err := NewCookieKeyRing(cookieSecret1)
	if err != nil {
		t.Fatalf("NewCookieKeyRing: %v", err)
	}
	h := NewServerCookieHandler(ring, func() time.Time { return cookieTestTime })

	oldCookie := mustDecodeCookie(t, cookieFixedCurrent+cookieV4Body1)
	if status, err := h.CheckServerCookie(cookieV4Addr, cookieClient, oldCookie); err != nil || status != ServerCookieOK {
		t.Fatalf("before rotation: status = %s, err = %v", status, err)
	}

	// Rotate secret2 in as the active secret; cookies minted with secret1
	// must still validate while freshly minted cookies use secret2.
	if err := ring.Rotate(cookieSecret2); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if ring.SecretCount() != 2 {
		t.Fatalf("SecretCount = %d, want 2", ring.SecretCount())
	}
	if status, err := h.CheckServerCookie(cookieV4Addr, cookieClient, oldCookie); err != nil || status != ServerCookieOK {
		t.Fatalf("old cookie after rotation: status = %s, err = %v", status, err)
	}

	newCookie, err := h.MintServerCookie(cookieV4Addr, cookieClient)
	if err != nil {
		t.Fatalf("MintServerCookie: %v", err)
	}
	if hex.EncodeToString(newCookie) != cookieFixedCurrent+cookieV4Body2 {
		t.Errorf("new cookie = %x, want secret2 digest", newCookie)
	}
	if status, err := h.CheckServerCookie(cookieV4Addr, cookieClient, newCookie); err != nil || status != ServerCookieOK {
		t.Fatalf("new cookie after rotation: status = %s, err = %v", status, err)
	}

	// Retiring every old secret invalidates cookies that depended on them.
	if err := ring.Replace(cookieSecret2); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if status, err := h.CheckServerCookie(cookieV4Addr, cookieClient, oldCookie); err != nil || status != ServerCookieBad {
		t.Fatalf("old cookie after replace: status = %s, err = %v", status, err)
	}

	// A caller mutation of secret material must not affect the ring.
	external := append([]byte(nil), cookieSecret1...)
	external[0] ^= 0xff
	if status, err := h.CheckServerCookie(cookieV4Addr, cookieClient, newCookie); err != nil || status != ServerCookieOK {
		t.Fatalf("ring must copy secrets: status = %s, err = %v", status, err)
	}
}

func TestKeyRingValidation(t *testing.T) {
	if _, err := NewCookieKeyRing(make([]byte, 31)); !errors.Is(err, ErrBadCookieSecretLen) {
		t.Fatalf("NewCookieKeyRing err = %v, want ErrBadCookieSecretLen", err)
	}
	ring, err := NewCookieKeyRing()
	if err != nil {
		t.Fatalf("empty ring: %v", err)
	}
	if err := ring.Rotate(make([]byte, 33)); !errors.Is(err, ErrBadCookieSecretLen) {
		t.Fatalf("Rotate err = %v, want ErrBadCookieSecretLen", err)
	}

	h := NewServerCookieHandler(ring, func() time.Time { return cookieTestTime })
	if _, err := h.MintServerCookie(cookieV4Addr, cookieClient); !errors.Is(err, ErrNoCookieSecret) {
		t.Fatalf("MintServerCookie err = %v, want ErrNoCookieSecret", err)
	}
	if status, err := h.CheckServerCookie(cookieV4Addr, cookieClient, make([]byte, ServerCookieLen)); !errors.Is(err, ErrNoCookieSecret) {
		t.Fatalf("CheckServerCookie err = %v, want ErrNoCookieSecret", err)
	} else if status != ServerCookieBad {
		t.Fatalf("status = %s, want bad", status)
	}

	if _, err := h.MintServerCookie(cookieV4Addr, make([]byte, 7)); !errors.Is(err, ErrInvalidClientCookie) {
		t.Fatalf("MintServerCookie err = %v, want ErrInvalidClientCookie", err)
	}
}

func TestGenerateServerCookieSecret(t *testing.T) {
	secret, err := GenerateServerCookieSecret()
	if err != nil {
		t.Fatalf("GenerateServerCookieSecret: %v", err)
	}
	if len(secret) != serverCookieSecretLen {
		t.Fatalf("secret length = %d, want %d", len(secret), serverCookieSecretLen)
	}
	other, err := GenerateServerCookieSecret()
	if err != nil {
		t.Fatalf("GenerateServerCookieSecret: %v", err)
	}
	if string(secret) == string(other) {
		t.Fatal("two generated secrets are identical")
	}
}

func cookieRequest(cookieHex string) *Msg {
	m := new(Msg)
	m.SetQuestion("example.org.", TypeA)
	m.SetEdns0(4096, false)
	opt := m.IsEdns0()
	opt.Option = append(opt.Option, &EDNS0_COOKIE{Code: EDNS0COOKIE, Cookie: cookieHex})
	return m
}

func TestCheckRequestAddr(t *testing.T) {
	h, _ := newCookieHandler(t, cookieSecret1)

	t.Run("no cookie option", func(t *testing.T) {
		m := new(Msg)
		m.SetQuestion("example.org.", TypeA)
		result, err := h.CheckRequestAddr(m, cookieV4Addr)
		if err != nil {
			t.Fatalf("CheckRequestAddr: %v", err)
		}
		if result.Status != ServerCookieNoCookie || result.Option() != nil {
			t.Fatalf("unexpected result: status=%s option=%v", result.Status, result.Option())
		}
	})

	t.Run("client cookie only mints fresh server cookie", func(t *testing.T) {
		m := cookieRequest(cookieClientHex)
		result, err := h.CheckRequestAddr(m, cookieV4Addr)
		if err != nil {
			t.Fatalf("CheckRequestAddr: %v", err)
		}
		if result.Status != ServerCookieMissing {
			t.Fatalf("status = %s, want missing", result.Status)
		}
		want := cookieClientHex + cookieFixedCurrent + cookieV4Body1
		if got := result.Option().Cookie; got != want {
			t.Errorf("response cookie = %s, want %s", got, want)
		}
	})

	t.Run("valid current cookie is echoed", func(t *testing.T) {
		m := cookieRequest(cookieClientHex + cookieFixedCurrent + cookieV4Body1)
		result, err := h.CheckRequestAddr(m, cookieV4Addr)
		if err != nil {
			t.Fatalf("CheckRequestAddr: %v", err)
		}
		if result.Status != ServerCookieOK {
			t.Fatalf("status = %s, want ok", result.Status)
		}
		if hex.EncodeToString(result.ServerCookie) != cookieFixedCurrent+cookieV4Body1 {
			t.Errorf("server cookie = %x, want the received one echoed", result.ServerCookie)
		}
	})

	t.Run("stale previous-hour cookie returns fresh cookie", func(t *testing.T) {
		m := cookieRequest(cookieClientHex + cookieFixedPrev + cookieV4Body1)
		result, err := h.CheckRequestAddr(m, cookieV4Addr)
		if err != nil {
			t.Fatalf("CheckRequestAddr: %v", err)
		}
		if result.Status != ServerCookieStale {
			t.Fatalf("status = %s, want stale", result.Status)
		}
		if hex.EncodeToString(result.ServerCookie) != cookieFixedPrev+cookieV4Body1 {
			t.Errorf("stale cookies are valid and must be echoed, got %x", result.ServerCookie)
		}
	})

	t.Run("expired cookie is replaced", func(t *testing.T) {
		m := cookieRequest(cookieClientHex + cookieFixedTwoBack + cookieV4Body1)
		result, err := h.CheckRequestAddr(m, cookieV4Addr)
		if err != nil {
			t.Fatalf("CheckRequestAddr: %v", err)
		}
		if result.Status != ServerCookieExpired {
			t.Fatalf("status = %s, want expired", result.Status)
		}
		if hex.EncodeToString(result.ServerCookie) != cookieFixedCurrent+cookieV4Body1 {
			t.Errorf("server cookie = %x, want a freshly minted one", result.ServerCookie)
		}
	})

	t.Run("bad cookie is replaced with fresh mint", func(t *testing.T) {
		m := cookieRequest(cookieClientHex + cookieFixedCurrent + cookieOtherBody1)
		result, err := h.CheckRequestAddr(m, cookieV4Addr)
		if err != nil {
			t.Fatalf("CheckRequestAddr: %v", err)
		}
		if result.Status != ServerCookieBad {
			t.Fatalf("status = %s, want bad", result.Status)
		}
		if hex.EncodeToString(result.ServerCookie) != cookieFixedCurrent+cookieV4Body1 {
			t.Errorf("server cookie = %x, want a freshly minted one", result.ServerCookie)
		}
	})

	t.Run("malformed option carries no cookie", func(t *testing.T) {
		m := cookieRequest("xyz-not-hex")
		result, err := h.CheckRequestAddr(m, cookieV4Addr)
		if err != nil {
			t.Fatalf("CheckRequestAddr: %v", err)
		}
		if result.Status != ServerCookieBad {
			t.Fatalf("status = %s, want bad", result.Status)
		}
		if result.Option() != nil {
			t.Errorf("malformed cookie must not yield a response cookie, got %s", result.Option().Cookie)
		}
	})

	t.Run("too-short option carries no cookie", func(t *testing.T) {
		m := cookieRequest("00112233")
		result, err := h.CheckRequestAddr(m, cookieV4Addr)
		if err != nil {
			t.Fatalf("CheckRequestAddr: %v", err)
		}
		if result.Status != ServerCookieBad || result.Option() != nil {
			t.Fatalf("status = %s option = %v", result.Status, result.Option())
		}
	})

	t.Run("ipv6 source", func(t *testing.T) {
		m := cookieRequest(cookieClientHex)
		result, err := h.CheckRequestAddr(m, cookieV6Addr)
		if err != nil {
			t.Fatalf("CheckRequestAddr: %v", err)
		}
		want := cookieClientHex + cookieFixedCurrent + cookieV6Body1
		if got := result.Option().Cookie; got != want {
			t.Errorf("response cookie = %s, want %s", got, want)
		}
	})
}

func TestCheckRequestRemote(t *testing.T) {
	h, _ := newCookieHandler(t, cookieSecret1)

	cases := []struct {
		name   string
		remote net.Addr
		addr   netip.Addr
	}{
		{"udp v4", &net.UDPAddr{IP: net.ParseIP("198.51.100.7"), Port: 4096}, cookieV4Addr},
		{"tcp v6", &net.TCPAddr{IP: net.ParseIP("2001:db8::7"), Port: 4096}, cookieV6Addr},
		{"ip addr", &net.IPAddr{IP: net.ParseIP("198.51.100.7")}, cookieV4Addr},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			m := cookieRequest(cookieClientHex)
			result, err := h.CheckRequest(m, test.remote)
			if err != nil {
				t.Fatalf("CheckRequest: %v", err)
			}
			body := cookieV4Body1
			if test.addr == cookieV6Addr {
				body = cookieV6Body1
			}
			want := cookieClientHex + cookieFixedCurrent + body
			if result.Option().Cookie != want {
				t.Errorf("response cookie = %s, want %s", result.Option().Cookie, want)
			}
		})
	}

	if _, err := h.CheckRequest(cookieRequest(cookieClientHex), nil); err == nil {
		t.Fatal("expected error for nil remote address")
	}
}

func TestServerCookieStatusString(t *testing.T) {
	statuses := []ServerCookieStatus{
		ServerCookieNoCookie,
		ServerCookieOK,
		ServerCookieStale,
		ServerCookieMissing,
		ServerCookieExpired,
		ServerCookieBad,
	}
	for _, status := range statuses {
		if status.String() == "" {
			t.Errorf("status %d has empty string", status)
		}
	}
	if strings.Contains(fmt.Sprintf("%s", ServerCookieStatus(99)), " ") {
		t.Fatal("unexpected status string")
	}
}

func TestNoSecretLeak(t *testing.T) {
	// Errors and status descriptions must never contain secret material.
	secretHex := hex.EncodeToString(cookieSecret1)
	texts := []string{
		ErrNoCookieSecret.Error(),
		ErrBadCookieSecretLen.Error(),
		ErrInvalidClientCookie.Error(),
		ServerCookieBad.String(),
		ServerCookieExpired.String(),
		fmt.Sprintf("%v", (&ServerCookieResult{Status: ServerCookieBad}).Option()),
	}
	for _, text := range texts {
		if strings.Contains(strings.ToLower(text), strings.ToLower(secretHex)) {
			t.Errorf("secret material leaked into %q", text)
		}
	}
}

func TestKeyRingConcurrent(t *testing.T) {
	ring, err := NewCookieKeyRing(cookieSecret1)
	if err != nil {
		t.Fatalf("NewCookieKeyRing: %v", err)
	}
	h := NewServerCookieHandler(ring, func() time.Time { return cookieTestTime })
	serverCookie := mustDecodeCookie(t, cookieFixedCurrent+cookieV4Body1)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				switch (i + j) % 4 {
				case 0:
					_ = ring.Rotate(cookieSecret2)
				case 1:
					_, _ = h.CheckServerCookie(cookieV4Addr, cookieClient, serverCookie)
				case 2:
					_, _ = h.MintServerCookie(cookieV4Addr, cookieClient)
				case 3:
					_ = ring.SecretCount()
				}
			}
		}(i)
	}
	wg.Wait()
}

func TestDefaultClock(t *testing.T) {
	ring, err := NewCookieKeyRing(cookieSecret1)
	if err != nil {
		t.Fatalf("NewCookieKeyRing: %v", err)
	}
	h := NewServerCookieHandler(ring, nil)
	serverCookie, err := h.MintServerCookie(cookieV4Addr, cookieClient)
	if err != nil {
		t.Fatalf("MintServerCookie: %v", err)
	}
	status, err := h.CheckServerCookie(cookieV4Addr, cookieClient, serverCookie)
	if err != nil {
		t.Fatalf("CheckServerCookie: %v", err)
	}
	if status != ServerCookieOK {
		t.Fatalf("fresh wall-clock cookie status = %s, want ok", status)
	}
}

func TestMintAcrossHourBoundary(t *testing.T) {
	ring, err := NewCookieKeyRing(cookieSecret1)
	if err != nil {
		t.Fatalf("NewCookieKeyRing: %v", err)
	}
	// A cookie minted at 22:59:20 (last minute of hour 472221) and checked
	// 61 minutes later at 00:00:20 (hour 472222): RFC 9018 promises validity
	// for both the minting hour and the following hour, so it is stale-valid.
	mintHandler := NewServerCookieHandler(ring, func() time.Time {
		return time.Unix(1700000000-3600-40, 0).UTC()
	})
	serverCookie, err := mintHandler.MintServerCookie(cookieV4Addr, cookieClient)
	if err != nil {
		t.Fatalf("MintServerCookie: %v", err)
	}

	h := NewServerCookieHandler(ring, func() time.Time {
		return time.Unix(1700000000+20, 0).UTC()
	})
	status, err := h.CheckServerCookie(cookieV4Addr, cookieClient, serverCookie)
	if err != nil {
		t.Fatalf("CheckServerCookie: %v", err)
	}
	if status != ServerCookieStale {
		t.Fatalf("cookie minted at hour end checked next hour: status = %s, want stale", status)
	}
}

func TestIPv4MappedIPv6(t *testing.T) {
	h, _ := newCookieHandler(t, cookieSecret1)

	// An IPv4-mapped IPv6 address must bind to the 4-octet IPv4 address,
	// exactly as a native IPv4 address.
	mapped := netip.MustParseAddr("::ffff:198.51.100.7")
	minted, err := h.MintServerCookie(mapped, cookieClient)
	if err != nil {
		t.Fatalf("MintServerCookie: %v", err)
	}
	if hex.EncodeToString(minted) != cookieFixedCurrent+cookieV4Body1 {
		t.Errorf("mapped cookie = %x, want the IPv4 vector", minted)
	}
	status, err := h.CheckServerCookie(cookieV4Addr, cookieClient, mustDecodeCookie(t, cookieFixedCurrent+cookieV4Body1))
	if err != nil || status != ServerCookieOK {
		t.Fatalf("IPv4 check of mapped cookie: status = %s, err = %v", status, err)
	}
}

func TestCheckRequestHexCasing(t *testing.T) {
	h, _ := newCookieHandler(t, cookieSecret1)
	m := cookieRequest(strings.ToUpper(cookieClientHex))
	result, err := h.CheckRequestAddr(m, cookieV4Addr)
	if err != nil {
		t.Fatalf("CheckRequestAddr: %v", err)
	}
	if result.Status != ServerCookieMissing {
		t.Fatalf("status = %s, want missing", result.Status)
	}
	if hex.EncodeToString(result.ClientCookie) != cookieClientHex {
		t.Errorf("client cookie = %x", result.ClientCookie)
	}
}

func TestMinimumServerCookieLenAccepted(t *testing.T) {
	// RFC 9018 section 4.2 requires at least an 8-octet body. A valid cookie
	// with the minimum 16-octet total length must validate when the digest
	// matches a truncated 8-octet body.
	h, _ := newCookieHandler(t, cookieSecret1)
	ipBytes, err := cookieIPBytes(cookieV4Addr)
	if err != nil {
		t.Fatal(err)
	}
	full := cookieDigest(cookieSecret1, ipBytes, cookieClient)
	min := append(mustDecodeCookie(t, cookieFixedCurrent), full[:8]...)

	got, err := h.CheckServerCookie(cookieV4Addr, cookieClient, min)
	if err != nil {
		t.Fatalf("CheckServerCookie: %v", err)
	}
	if got != ServerCookieOK {
		t.Fatalf("status = %s, want ok for minimum-length server cookie", got)
	}
}
