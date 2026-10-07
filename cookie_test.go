package dns

import (
	"encoding/binary"
	"encoding/hex"
	"hash/fnv"
	"net"
	"strings"
	"testing"
	"time"
)

var (
	cookieTestSecret1, _ = hex.DecodeString("00112233445566778899aabbccddeeff")
	cookieTestSecret2, _ = hex.DecodeString("ffeeddccbbaa99887766554433221100")
	cookieTestClient, _  = hex.DecodeString("9694a7f9ee8f3c0d")
	cookieTestTime       = time.Unix(1700000000, 0)
)

func cookieTestHandler(secrets ...[]byte) *CookieHandler {
	return &CookieHandler{
		Secrets:  secrets,
		Lifetime: time.Hour,
		Now:      func() time.Time { return cookieTestTime },
	}
}

// TestCookieGoldenVectors pins the wire format of the server cookie
// (RFC 9018 Section 4.2) for IPv4 and IPv6 clients and across keys.
func TestCookieGoldenVectors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		secret []byte
		ip     string
		want   string
	}{
		{"ipv4", cookieTestSecret1, "192.0.2.1", "9694a7f9ee8f3c0d010000006553f10052fd292d73b4c62e"},
		{"ipv6", cookieTestSecret1, "2001:db8::1", "9694a7f9ee8f3c0d010000006553f1000c7b8d0968f537a2"},
		{"ipv4-other-key", cookieTestSecret2, "192.0.2.1", "9694a7f9ee8f3c0d010000006553f100fdf7ecfec971cace"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := cookieTestHandler(tc.secret)
			got, err := h.Cookie(cookieTestClient, net.ParseIP(tc.ip))
			if err != nil {
				t.Fatalf("Cookie: %v", err)
			}
			if hex.EncodeToString(got) != tc.want {
				t.Errorf("got %x, want %s", got, tc.want)
			}
		})
	}
}

// TestCookieServerCookieFormat independently recomputes the hash per the
// documented construction and checks the header fields.
func TestCookieServerCookieFormat(t *testing.T) {
	h := cookieTestHandler(cookieTestSecret1)
	sc, err := h.ServerCookie(cookieTestClient, net.ParseIP("192.0.2.1"))
	if err != nil {
		t.Fatalf("ServerCookie: %v", err)
	}
	if len(sc) != CookieServerLen {
		t.Fatalf("server cookie length %d, want %d", len(sc), CookieServerLen)
	}
	if sc[0] != CookieVersion {
		t.Errorf("version %d, want %d", sc[0], CookieVersion)
	}
	if sc[1] != 0 || sc[2] != 0 || sc[3] != 0 {
		t.Errorf("reserved bytes not zero: %x", sc[1:4])
	}
	if ts := binary.BigEndian.Uint32(sc[4:8]); ts != uint32(cookieTestTime.Unix()) {
		t.Errorf("timestamp %d, want %d", ts, cookieTestTime.Unix())
	}
	f := fnv.New64a()
	f.Write(cookieTestClient)
	f.Write(sc[:8])
	f.Write(net.ParseIP("192.0.2.1").To4())
	f.Write(cookieTestSecret1)
	if want := f.Sum(nil); !equalBytes(sc[8:], want) {
		t.Errorf("hash %x, want %x", sc[8:], want)
	}
}

func equalBytes(a, b []byte) bool {
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

func TestCookieValidate(t *testing.T) {
	h := cookieTestHandler(cookieTestSecret1)
	ip := net.ParseIP("192.0.2.1")
	valid, err := h.Cookie(cookieTestClient, ip)
	if err != nil {
		t.Fatalf("Cookie: %v", err)
	}

	tampered := append([]byte(nil), valid...)
	tampered[len(tampered)-1] ^= 0xff

	badVersion := append([]byte(nil), valid...)
	badVersion[CookieClientLen] = 2

	badReserved := append([]byte(nil), valid...)
	badReserved[CookieClientLen+1] = 1

	for _, tc := range []struct {
		name   string
		cookie []byte
		ip     net.IP
		want   CookieStatus
	}{
		{"absent", nil, ip, CookieAbsent},
		{"empty", []byte{}, ip, CookieAbsent},
		{"short-client-cookie", cookieTestClient[:7], ip, CookieInvalid},
		{"client-only", cookieTestClient, ip, CookieClientOnly},
		{"valid", valid, ip, CookieValid},
		{"server-cookie-too-short", valid[:CookieClientLen+7], ip, CookieInvalid},
		{"server-cookie-odd-length", valid[:CookieClientLen+15], ip, CookieInvalid},
		{"server-cookie-too-long", append(append([]byte(nil), valid...), 0x00), ip, CookieInvalid},
		{"way-too-long", make([]byte, CookieClientLen+33), ip, CookieInvalid},
		{"tampered-hash", tampered, ip, CookieInvalid},
		{"bad-version", badVersion, ip, CookieInvalid},
		{"bad-reserved", badReserved, ip, CookieInvalid},
		{"other-ipv4", valid, net.ParseIP("192.0.2.2"), CookieInvalid},
		{"ipv4-as-ipv6", valid, net.ParseIP("192.0.2.1").To16(), CookieValid},
		{"no-ip", valid, nil, CookieInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := h.Validate(tc.cookie, tc.ip); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCookieValidateIPv6(t *testing.T) {
	h := cookieTestHandler(cookieTestSecret1)
	ip := net.ParseIP("2001:db8::1")
	c, err := h.Cookie(cookieTestClient, ip)
	if err != nil {
		t.Fatalf("Cookie: %v", err)
	}
	if got := h.Validate(c, ip); got != CookieValid {
		t.Errorf("got %v, want %v", got, CookieValid)
	}
	if got := h.Validate(c, net.ParseIP("2001:db8::2")); got != CookieInvalid {
		t.Errorf("other address: got %v, want %v", got, CookieInvalid)
	}
}

func TestCookieExpiry(t *testing.T) {
	now := cookieTestTime
	h := &CookieHandler{
		Secrets:  [][]byte{cookieTestSecret1},
		Lifetime: time.Hour,
		Now:      func() time.Time { return now },
	}
	ip := net.ParseIP("192.0.2.1")
	c, err := h.Cookie(cookieTestClient, ip)
	if err != nil {
		t.Fatalf("Cookie: %v", err)
	}

	now = cookieTestTime.Add(30 * time.Minute)
	if got := h.Validate(c, ip); got != CookieValid {
		t.Errorf("within lifetime: got %v, want %v", got, CookieValid)
	}

	now = cookieTestTime.Add(time.Hour + time.Second)
	if got := h.Validate(c, ip); got != CookieExpired {
		t.Errorf("past lifetime: got %v, want %v", got, CookieExpired)
	}

	now = cookieTestTime.Add(-time.Minute)
	if got := h.Validate(c, ip); got != CookieInvalid {
		t.Errorf("future timestamp: got %v, want %v", got, CookieInvalid)
	}
}

func TestCookieKeyRotation(t *testing.T) {
	ip := net.ParseIP("192.0.2.1")

	old := cookieTestHandler(cookieTestSecret1)
	oldCookie, err := old.Cookie(cookieTestClient, ip)
	if err != nil {
		t.Fatalf("Cookie: %v", err)
	}

	// Rotate: new key signs, both keys verify.
	rotated := cookieTestHandler(cookieTestSecret2, cookieTestSecret1)
	if got := rotated.Validate(oldCookie, ip); got != CookieValid {
		t.Errorf("old cookie after rotation: got %v, want %v", got, CookieValid)
	}
	newCookie, err := rotated.Cookie(cookieTestClient, ip)
	if err != nil {
		t.Fatalf("Cookie: %v", err)
	}
	if got := rotated.Validate(newCookie, ip); got != CookieValid {
		t.Errorf("new cookie after rotation: got %v, want %v", got, CookieValid)
	}

	// The new cookie must have been signed with the new key only.
	if got := old.Validate(newCookie, ip); got != CookieInvalid {
		t.Errorf("new cookie with old key ring: got %v, want %v", got, CookieInvalid)
	}

	// Retire the old key: old cookies stop verifying.
	retired := cookieTestHandler(cookieTestSecret2)
	if got := retired.Validate(oldCookie, ip); got != CookieInvalid {
		t.Errorf("old cookie after retiring old key: got %v, want %v", got, CookieInvalid)
	}
	if got := retired.Validate(newCookie, ip); got != CookieValid {
		t.Errorf("new cookie after retiring old key: got %v, want %v", got, CookieValid)
	}
}

func TestCookieGenerateErrors(t *testing.T) {
	h := cookieTestHandler(cookieTestSecret1)
	if _, err := h.Cookie([]byte{1, 2, 3}, net.ParseIP("192.0.2.1")); err != ErrCookieClient {
		t.Errorf("short client cookie: got %v, want %v", err, ErrCookieClient)
	}
	if _, err := h.Cookie(cookieTestClient, nil); err != ErrCookieAddr {
		t.Errorf("nil address: got %v, want %v", err, ErrCookieAddr)
	}
	noKeys := cookieTestHandler()
	if _, err := noKeys.Cookie(cookieTestClient, net.ParseIP("192.0.2.1")); err != ErrCookieSecret {
		t.Errorf("no secrets: got %v, want %v", err, ErrCookieSecret)
	}
}

// TestCookieNoSecretLeak ensures errors and status strings never carry key
// material.
func TestCookieNoSecretLeak(t *testing.T) {
	secretHex := hex.EncodeToString(cookieTestSecret1)
	for _, err := range []error{ErrCookieSecret, ErrCookieClient, ErrCookieAddr} {
		if strings.Contains(err.Error(), secretHex) || strings.Contains(err.Error(), string(cookieTestSecret1)) {
			t.Errorf("error %q leaks secret material", err)
		}
	}
	for _, s := range []CookieStatus{CookieAbsent, CookieClientOnly, CookieValid, CookieExpired, CookieInvalid} {
		if strings.Contains(s.String(), secretHex) {
			t.Errorf("status %q leaks secret material", s)
		}
	}
}
