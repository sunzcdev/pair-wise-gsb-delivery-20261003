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
	// UsernameStar indicates the username is sent using the
	// extended username* parameter notation from RFC 7616.
	UsernameStar bool
	Realm        string
	Nonce        string
	URI          string
	Response     string
	Algorithm    string
	Cnonce       string
	Opaque       string
	QOP          string
	Nc           int
	Userhash     bool
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
	seen := make(map[string]bool, len(pp))
	for _, p := range pp {
		if seen[p.Key] {
			return nil, fmt.Errorf("digest: duplicate parameter: %q", p.Key)
		}
		seen[p.Key] = true
		switch p.Key {
		case "username":
			c.Username = p.Value
		case "username*":
			username, err := decodeUsernameStar(p.Value)
			if err != nil {
				return nil, err
			}
			c.Username = username
			c.UsernameStar = true
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
	if seen["username"] && seen["username*"] {
		return nil, errors.New("digest: conflicting username and username* parameters")
	}
	return &c, nil
}

// String formats the credentials into the header format
func (c *Credentials) String() string {
	var pp []param.Param
	// userhash takes precedence: the hashed username is sent in the
	// regular username parameter and the original name is omitted.
	switch {
	case c.Userhash:
		pp = append(pp, param.Param{
			Key:   "username",
			Value: c.Username,
			Quote: true,
		})
	case c.UsernameStar:
		pp = append(pp, param.Param{
			Key:   "username*",
			Value: encodeUsernameStar(c.Username),
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
