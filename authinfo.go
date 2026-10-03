package digest

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/icholy/digest/internal/param"
)

// AuthenticationInfo is a parsed Authentication-Info header.
// See RFC 7616 section 3.5.
type AuthenticationInfo struct {
	NextNonce string
	QOP       string
	Cnonce    string
	Nc        int
	Rspauth   string
}

// ParseAuthenticationInfo parses an Authentication-Info header value
func ParseAuthenticationInfo(s string) (*AuthenticationInfo, error) {
	pp, err := param.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("digest: invalid authentication info: %w", err)
	}
	var info AuthenticationInfo
	for _, p := range pp {
		switch p.Key {
		case "nextnonce":
			info.NextNonce = p.Value
		case "qop":
			info.QOP = p.Value
		case "cnonce":
			info.Cnonce = p.Value
		case "rspauth":
			info.Rspauth = p.Value
		case "nc":
			nc, err := strconv.ParseUint(p.Value, 16, 32)
			if err != nil {
				return nil, fmt.Errorf("digest: invalid authentication info nc: %w", err)
			}
			info.Nc = int(nc)
		}
	}
	return &info, nil
}

// String returns the formatted header value
func (info *AuthenticationInfo) String() string {
	var pp []param.Param
	if info.NextNonce != "" {
		pp = append(pp, param.Param{
			Key:   "nextnonce",
			Value: info.NextNonce,
			Quote: true,
		})
	}
	if info.QOP != "" {
		pp = append(pp, param.Param{
			Key:   "qop",
			Value: info.QOP,
		})
	}
	if info.Cnonce != "" {
		pp = append(pp, param.Param{
			Key:   "cnonce",
			Value: info.Cnonce,
			Quote: true,
		})
	}
	if info.Nc != 0 {
		pp = append(pp, param.Param{
			Key:   "nc",
			Value: fmt.Sprintf("%08x", info.Nc),
		})
	}
	if info.Rspauth != "" {
		pp = append(pp, param.Param{
			Key:   "rspauth",
			Value: info.Rspauth,
			Quote: true,
		})
	}
	return param.Format(pp...)
}

// FindAuthenticationInfo returns the parsed Authentication-Info header, or
// nil if the response does not carry one.
func FindAuthenticationInfo(h http.Header) (*AuthenticationInfo, error) {
	v := h.Get("Authentication-Info")
	if v == "" {
		return nil, nil
	}
	return ParseAuthenticationInfo(v)
}

// ResponseAuth carries the inputs needed to verify the Authentication-Info
// header of a successful response before its nextnonce is committed.
type ResponseAuth struct {
	// Request is the authorized request the response belongs to.
	Request *http.Request
	// Credentials are the credentials that were sent with the request.
	Credentials *Credentials
	// Challenge is the challenge generation the request was authorized under.
	Challenge *Challenge
	// Info is the parsed Authentication-Info header of the response.
	Info *AuthenticationInfo
	// Username and Password are the transport's credentials.
	Username string
	Password string
}

// ErrRspauth indicates that the rspauth value in an Authentication-Info
// header did not match the expected response digest.
var ErrRspauth = errors.New("digest: rspauth verification failed")

// VerifyResponseAuth is the default verifier for Authentication-Info
// updates. It accepts updates which carry no rspauth value and verifies
// rspauth for challenges with no qop or qop=auth. For qop=auth-int the
// rspauth digest covers the response body, which is not available here, so
// the update is accepted unverified; inject Transport.VerifyResponse to
// verify it yourself.
func VerifyResponseAuth(ra *ResponseAuth) error {
	if ra.Info.Rspauth == "" {
		return nil
	}
	qop := ra.Info.QOP
	if qop == "" {
		qop = ra.Credentials.QOP
	}
	if qop == "auth-int" {
		return nil
	}
	h, err := hashalg(ra.Challenge.Algorithm)
	if err != nil {
		return err
	}
	a1 := hashjoin(h, ra.Username, ra.Challenge.Realm, ra.Password)
	a2 := hashjoin(h, "", ra.Credentials.URI)
	var want string
	if qop == "" {
		want = hashjoin(h, a1, ra.Challenge.Nonce, a2)
	} else {
		want = hashjoin(h,
			a1,
			ra.Challenge.Nonce,
			fmt.Sprintf("%08x", ra.Credentials.Nc),
			ra.Credentials.Cnonce,
			qop,
			a2,
		)
	}
	if want != ra.Info.Rspauth {
		return ErrRspauth
	}
	return nil
}
