package digest

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/icholy/digest/internal/param"
)

// Credentials is a parsed version of the Authorization header
type Credentials struct {
	Username string
	// ExtendedUsername reports whether Username was sent using the RFC 5987
	// "username*" extended parameter rather than the legacy "username"
	// quoted-string. It is never set together with Userhash.
	ExtendedUsername bool
	Realm            string
	Nonce            string
	URI              string
	Response         string
	Algorithm        string
	Cnonce           string
	Opaque           string
	QOP              string
	Nc               int
	Userhash         bool
}

// ParseCredentials parses the Authorization header value into credentials
func ParseCredentials(s string) (*Credentials, error) {
	s, ok := CutPrefix(s)
	if !ok {
		return nil, errors.New("digest: invalid credentials prefix")
	}
	pp, err := param.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("digest: invalid credentials: %w", err)
	}
	var c Credentials
	var seen = map[string]bool{}
	for _, p := range pp {
		key := strings.ToLower(p.Key)
		switch key {
		case "username", "username*", "realm", "nonce", "uri", "response",
			"algorithm", "cnonce", "opaque", "qop", "nc", "userhash":
			if seen[key] {
				return nil, fmt.Errorf("digest: duplicate credentials parameter: %q", p.Key)
			}
			seen[key] = true
		}
		switch key {
		case "username":
			c.Username = p.Value
		case "username*":
			username, err := decodeExtValue(p.Value)
			if err != nil {
				return nil, fmt.Errorf("digest: invalid username*: %w", err)
			}
			c.Username = username
			c.ExtendedUsername = true
		case "realm":
			c.Realm = p.Value
		case "nonce":
			c.Nonce = p.Value
		case "uri":
			c.URI = p.Value
		case "response":
			c.Response = p.Value
		case "algorithm":
			c.Algorithm = p.Value
		case "cnonce":
			c.Cnonce = p.Value
		case "opaque":
			c.Opaque = p.Value
		case "qop":
			c.QOP = p.Value
		case "nc":
			nc, err := strconv.ParseInt(p.Value, 16, 32)
			if err != nil {
				return nil, fmt.Errorf("digest: invalid nc: %w", err)
			}
			c.Nc = int(nc)
		case "userhash":
			c.Userhash = strings.ToLower(p.Value) == "true"
		}
	}
	// "username" and "username*" carry the same value in conflicting
	// encodings, and userhash replaces the username altogether. Reject these
	// instead of silently picking one of them.
	switch {
	case seen["username"] && seen["username*"]:
		return nil, errors.New("digest: credentials contain both \"username\" and \"username*\"")
	case c.Userhash && seen["username*"]:
		return nil, errors.New("digest: credentials contain both \"userhash\" and \"username*\"")
	}
	return &c, nil
}

// String formats the credentials into the header format
func (c *Credentials) String() string {
	var pp []param.Param
	switch {
	case c.Userhash:
		// userhash takes precedence: only the hashed username is sent.
		pp = append(pp, param.Param{
			Key:   "username",
			Value: c.Username,
			Quote: true,
		})
	case c.ExtendedUsername:
		// RFC 5987 extended parameter is never quoted. The conflicting
		// legacy "username" parameter is omitted.
		pp = append(pp, param.Param{
			Key:   "username*",
			Value: encodeExtValue(c.Username),
		})
	default:
		pp = append(pp, param.Param{
			Key:   "username",
			Value: c.Username,
			Quote: true,
		})
	}
	pp = append(pp,
		param.Param{
			Key:   "realm",
			Value: c.Realm,
			Quote: true,
		},
		param.Param{
			Key:   "nonce",
			Value: c.Nonce,
			Quote: true,
		},
		param.Param{
			Key:   "uri",
			Value: c.URI,
			Quote: true,
		},
	)
	if c.Algorithm != "" {
		pp = append(pp, param.Param{
			Key:   "algorithm",
			Value: c.Algorithm,
		})
	}
	if c.QOP != "" {
		pp = append(pp, param.Param{
			Key:   "cnonce",
			Value: c.Cnonce,
			Quote: true,
		})
	}
	if c.Opaque != "" {
		pp = append(pp, param.Param{
			Key:   "opaque",
			Value: c.Opaque,
			Quote: true,
		})
	}
	if c.QOP != "" {
		pp = append(pp,
			param.Param{
				Key:   "qop",
				Value: c.QOP,
			},
			param.Param{
				Key:   "nc",
				Value: fmt.Sprintf("%08x", c.Nc),
			},
		)
	}
	if c.Userhash {
		pp = append(pp, param.Param{
			Key:   "userhash",
			Value: "true",
		})
	}
	// The RFC does not specify an order, but some implementations expect the response to be at the end.
	// See: https://github.com/icholy/digest/issues/8
	pp = append(pp, param.Param{
		Key:   "response",
		Value: c.Response,
		Quote: true,
	})
	return Prefix + param.Format(pp...)
}
