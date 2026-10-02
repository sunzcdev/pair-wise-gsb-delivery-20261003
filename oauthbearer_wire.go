package sasl

// Shared, reviewable OAUTHBEARER client message field rules.
//
// The wire format is defined by RFC 7628 section 3.1:
//
//	client-answer        = "n," gs2-authzid "," kvsep client-answer-params
//	gs2-authzid          = "a=" saslname / ; GS2 transported authzid
//	                     ; empty string
//	client-answer-params = 1*(sep scope / auth / key "=" value)
//	kvsep                = %x01
//	auth                 = "auth=Bearer " SP b64token
//
// The GS2 header (including the saslname escaping rules) is defined by
// RFC 5801 section 2.3. These helpers are used both by the client when
// encoding the initial response and by the server when decoding it, so a
// comma or equals sign in the username cannot break the field structure.

import (
	"bytes"
	"errors"
	"strconv"
	"strings"
)

const (
	oauthBearerSep        = 0x01 // kvsep / sep
	oauthBearerBearerPref = "Bearer "
)

// saslname special characters are escaped by GS2 (RFC 5801 section 2.3):
// the equals sign MUST be escaped before the comma, as escaping itself
// introduces equals signs.
func encodeSASLname(username string) (string, error) {
	if strings.IndexByte(username, 0x00) >= 0 {
		return "", errors.New("sasl: OAUTHBEARER username contains a NUL byte")
	}
	username = strings.ReplaceAll(username, "=", "=3D")
	username = strings.ReplaceAll(username, ",", "=2C")
	return username, nil
}

// decodeSASLname reverses encodeSASLname. Only the two fixed GS2 escape
// sequences defined by RFC 5801 ("=2C" and "=3D", uppercase) are valid:
// a dangling equals sign or the escape of any other byte makes the
// authzid malformed.
func decodeSASLname(escaped []byte) (string, error) {
	var b strings.Builder
	b.Grow(len(escaped))
	for i := 0; i < len(escaped); i++ {
		c := escaped[i]
		if c == 0x00 {
			return "", errors.New("sasl: OAUTHBEARER username contains a NUL byte")
		}
		if c != '=' {
			b.WriteByte(c)
			continue
		}
		if i+2 >= len(escaped) {
			return "", errors.New("sasl: malformed GS2 escape in authzid")
		}
		switch {
		case bytes.Equal(escaped[i+1:i+3], []byte("2C")):
			b.WriteByte(',')
		case bytes.Equal(escaped[i+1:i+3], []byte("3D")):
			b.WriteByte('=')
		default:
			return "", errors.New("sasl: invalid GS2 escape in authzid")
		}
		i += 2
	}
	return b.String(), nil
}

// validBearerToken checks the b64token grammar used by the "auth"
// parameter (RFC 6750 section 1.3, referenced by RFC 7628).
func validBearerToken(token string) bool {
	if token == "" {
		return false
	}
	for i := 0; i < len(token); i++ {
		c := token[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '-', c == '.', c == '_', c == '~',
			c == '+', c == '/':
		default:
			if c != '=' {
				return false
			}
			// '=' is only legal as terminal base64 padding.
			for j := i; j < len(token); j++ {
				if token[j] != '=' {
					return false
				}
			}
			return true
		}
	}
	return true
}

func hasControlChar(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c == 0x7f {
			return true
		}
	}
	return false
}

// encodeOAuthBearerInitialResponse builds the initial response. It applies
// the same field rules the strict server checks, so the produced message is
// always well-formed.
func encodeOAuthBearerInitialResponse(opts OAuthBearerOptions) ([]byte, error) {
	saslname, err := encodeSASLname(opts.Username)
	if err != nil {
		return nil, err
	}
	if opts.Port < 0 || opts.Port > 65535 {
		return nil, errors.New("sasl: OAUTHBEARER port out of range")
	}
	if !validBearerToken(opts.Token) {
		return nil, errors.New("sasl: OAUTHBEARER token is not a valid b64token")
	}
	if hasControlChar(opts.Host) {
		return nil, errors.New("sasl: OAUTHBEARER host contains a control character")
	}

	var b []byte
	b = append(b, "n,"...)
	if saslname != "" {
		b = append(b, 'a', '=')
		b = append(b, saslname...)
	}
	b = append(b, ',')

	if opts.Host != "" {
		b = append(b, oauthBearerSep)
		b = append(b, "host="...)
		b = append(b, opts.Host...)
	}
	if opts.Port != 0 {
		b = append(b, oauthBearerSep)
		b = append(b, "port="...)
		b = strconv.AppendInt(b, int64(opts.Port), 10)
	}
	b = append(b, oauthBearerSep)
	b = append(b, "auth="+oauthBearerBearerPref...)
	b = append(b, opts.Token...)
	b = append(b, oauthBearerSep, oauthBearerSep)
	return b, nil
}

// applyOAuthBearerField stores a single key=value client-answer parameter
// into opts. strict selects RFC 7628 field validation; the non-strict path
// keeps the historical lenient behavior so existing peers keep working.
func applyOAuthBearerField(field []byte, opts *OAuthBearerOptions, strict bool) error {
	eq := bytes.IndexByte(field, '=')
	if eq < 0 {
		return errors.New("client answer parameter missing '='")
	}
	key := string(field[:eq])
	value := string(field[eq+1:])

	switch key {
	case "host":
		if strict {
			if value == "" {
				return errors.New("empty 'host' parameter")
			}
			if hasControlChar(value) {
				return errors.New("invalid 'host' parameter")
			}
		}
		opts.Host = value
	case "port":
		port, perr := strconv.ParseUint(value, 10, 16)
		if perr != nil {
			return errors.New("malformed 'port' parameter")
		}
		opts.Port = int(port)
	case "auth":
		// Token type is case-insensitive.
		if len(value) < len(oauthBearerBearerPref) ||
			!strings.EqualFold(value[:len(oauthBearerBearerPref)], oauthBearerBearerPref) {
			return errors.New("unsupported token type")
		}
		token := value[len(oauthBearerBearerPref):]
		if strict {
			if !validBearerToken(token) {
				return errors.New("invalid 'auth' parameter")
			}
		}
		opts.Token = token
	default:
		return errors.New("unknown parameter: " + key)
	}
	return nil
}

// parseOAuthBearerInitialResponse decodes an initial response. strict
// enforces the full RFC 7628 grammar and GS2 authzid escaping; the lenient
// path accepts the historical field layout and only unescapes the authzid
// when it is a well-formed GS2 string.
func parseOAuthBearerInitialResponse(response []byte, strict bool) (OAuthBearerOptions, error) {
	opts := OAuthBearerOptions{}

	if strict {
		if len(response) < 2 || response[0] != 'n' || response[1] != ',' {
			return opts, errors.New("missing 'n' GS2 channel-binding flag")
		}
		// The second comma terminates gs2-authzid. With strict escaping
		// it cannot appear inside the saslname.
		end := bytes.IndexByte(response[2:], ',')
		if end < 0 {
			return opts, errors.New("missing GS2 header terminator")
		}
		end += 2
		authzid := response[2:end]
		if len(authzid) > 0 {
			if len(authzid) < 2 || authzid[0] != 'a' || authzid[1] != '=' {
				return opts, errors.New("missing 'a=' GS2 authzid prefix")
			}
			username, err := decodeSASLname(authzid[2:])
			if err != nil {
				return opts, err
			}
			opts.Username = username
		}

		body := response[end+1:]
		// kvsep auth *(kvsep ...) kvsep: the answer ends with two
		// separators, one terminating auth and one the trailing empty
		// extension.
		if len(body) < 2 || body[len(body)-1] != oauthBearerSep ||
			body[len(body)-2] != oauthBearerSep {
			return opts, errors.New("missing trailing answer separators")
		}
		if body[0] != oauthBearerSep {
			return opts, errors.New("missing answer parameter separator")
		}

		// Splitting the answer on the separator yields an empty field
		// before the first parameter and two trailing empty fields (the
		// separator terminating the last parameter and the empty
		// extension). Any other empty field means two separators are
		// adjacent, which is malformed.
		fields := bytes.Split(body, []byte{oauthBearerSep})
		if len(fields) < 4 || len(fields[0]) != 0 ||
			len(fields[len(fields)-1]) != 0 || len(fields[len(fields)-2]) != 0 {
			return opts, errors.New("malformed client answer field boundaries")
		}
		seen := make(map[string]bool)
		for i, field := range fields[:len(fields)-2] {
			// The first field is the structural leading empty segment.
			if i == 0 {
				continue
			}
			if len(field) == 0 {
				return opts, errors.New("empty client answer parameter")
			}
			eq := bytes.IndexByte(field, '=')
			if eq <= 0 {
				return opts, errors.New("malformed client answer parameter")
			}
			key := string(field[:eq])
			if seen[key] {
				return opts, errors.New("duplicate parameter: " + key)
			}
			seen[key] = true
			if err := applyOAuthBearerField(field, &opts, true); err != nil {
				return opts, err
			}
		}
		if opts.Token == "" {
			return opts, errors.New("missing required 'auth' parameter")
		}
		return opts, nil
	}

	// Lenient, backwards-compatible parsing.
	parts := bytes.SplitN(response, []byte{','}, 3)
	if len(parts) != 3 {
		return opts, errors.New("invalid response")
	}
	if len(parts[0]) != 1 || parts[0][0] != 'n' {
		return opts, errors.New("missing 'n' in gs2-cb-flag")
	}
	authzid := parts[1]
	if len(authzid) > 0 {
		if len(authzid) < 2 || authzid[0] != 'a' || authzid[1] != '=' {
			return opts, errors.New("missing 'a=' in gs2-authzid")
		}
		// Best-effort GS2 unescaping: fall back to the raw value when
		// the peer did not escape the authzid.
		if username, err := decodeSASLname(authzid[2:]); err == nil {
			opts.Username = username
		} else {
			opts.Username = string(authzid[2:])
		}
	}

	for _, field := range bytes.Split(parts[2], []byte{oauthBearerSep}) {
		if len(field) == 0 {
			continue
		}
		if err := applyOAuthBearerField(field, &opts, false); err != nil {
			return opts, err
		}
	}
	return opts, nil
}
