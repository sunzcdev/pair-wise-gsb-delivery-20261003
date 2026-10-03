package digest

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/icholy/digest/internal/param"
)

// AuthenticationInfo is the parsed Authentication-Info response header (RFC 2617 §3.2.3).
//
// Servers use it to prove they know the shared secret (rspauth) and may hand the
// client a fresh nonce via NextNonce. The Transport only adopts NextNonce after
// the header is verified and the response still owns the current challenge
// generation.
type AuthenticationInfo struct {
	NextNonce string
	QOP       string
	RspAuth   string
	Cnonce    string
	Nc        int
}

// ErrNoAuthenticationInfo indicates that no Authentication-Info header was found.
var ErrNoAuthenticationInfo = errors.New("digest: no authentication-info found")

// FindAuthenticationInfo returns the first Authentication-Info header in the
// response headers.
func FindAuthenticationInfo(h http.Header) (*AuthenticationInfo, error) {
	var last error
	for _, header := range h.Values("Authentication-Info") {
		ai, err := ParseAuthenticationInfo(header)
		if err == nil {
			return ai, nil
		}
		last = err
	}
	if last != nil {
		return nil, last
	}
	return nil, ErrNoAuthenticationInfo
}

// ParseAuthenticationInfo parses an Authentication-Info header value.
// The header uses the same key=value syntax as the challenge headers (RFC 7615).
func ParseAuthenticationInfo(s string) (*AuthenticationInfo, error) {
	pp, err := param.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("digest: invalid authentication-info: %w", err)
	}
	var ai AuthenticationInfo
	for _, p := range pp {
		switch p.Key {
		case "nextnonce":
			ai.NextNonce = p.Value
		case "qop":
			ai.QOP = p.Value
		case "rspauth":
			ai.RspAuth = p.Value
		case "cnonce":
			ai.Cnonce = p.Value
		case "nc":
			// the header uses eight hex digits, the same as the credentials
			nc, err := strconv.ParseInt(p.Value, 16, 32)
			if err != nil {
				return nil, fmt.Errorf("digest: invalid authentication-info nc %q: %w", p.Value, err)
			}
			ai.Nc = int(nc)
		}
	}
	return &ai, nil
}
