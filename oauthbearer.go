package sasl

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// The OAUTHBEARER mechanism name.
const OAuthBearer = "OAUTHBEARER"

// kvSep separates key-value pairs in the OAUTHBEARER client response.
const kvSep byte = 0x01

// kvSepStr is kvSep as a string, for building client responses.
const kvSepStr = "\x01"

type OAuthBearerError struct {
	Status  string `json:"status"`
	Schemes string `json:"schemes"`
	Scope   string `json:"scope"`
}

type OAuthBearerOptions struct {
	Username string
	Token    string
	Host     string
	Port     int
}

// Implements error
func (err *OAuthBearerError) Error() string {
	return fmt.Sprintf("OAUTHBEARER authentication error (%v)", err.Status)
}

// escapeAuthzid encodes a username for use as a GS2 authzid, as described
// in RFC 5801 section 5.1: "=" is escaped as "=3D" and "," as "=2C".
func escapeAuthzid(s string) string {
	return strings.NewReplacer("=", "=3D", ",", "=2C").Replace(s)
}

// unescapeAuthzid decodes a GS2-escaped authzid. It returns an error if the
// input contains an invalid escape sequence.
func unescapeAuthzid(s string) (string, error) {
	if !strings.Contains(s, "=") {
		return s, nil
	}
	var sb strings.Builder
	sb.Grow(len(s))
	i := 0
	for i < len(s) {
		c := s[i]
		if c != '=' {
			sb.WriteByte(c)
			i++
			continue
		}
		if i+2 >= len(s) {
			return "", errors.New("sasl: truncated '=' escape in gs2-authzid")
		}
		switch esc := s[i+1 : i+3]; esc {
		case "2C":
			sb.WriteByte(',')
		case "3D":
			sb.WriteByte('=')
		default:
			return "", fmt.Errorf("sasl: invalid escape '=%s' in gs2-authzid", esc)
		}
		i += 3
	}
	return sb.String(), nil
}

type oauthBearerClient struct {
	OAuthBearerOptions
}

func (a *oauthBearerClient) Start() (mech string, ir []byte, err error) {
	var authzid string
	if a.Username != "" {
		authzid = "a=" + escapeAuthzid(a.Username)
	}
	str := "n," + authzid + ","

	if a.Host != "" {
		str += kvSepStr + "host=" + a.Host
	}

	if a.Port != 0 {
		str += kvSepStr + "port=" + strconv.Itoa(a.Port)
	}
	str += kvSepStr + "auth=Bearer " + a.Token + kvSepStr + kvSepStr
	ir = []byte(str)
	return OAuthBearer, ir, nil
}

func (a *oauthBearerClient) Next(challenge []byte) ([]byte, error) {
	authBearerErr := &OAuthBearerError{}
	if err := json.Unmarshal(challenge, authBearerErr); err != nil {
		return nil, err
	} else {
		return nil, authBearerErr
	}
}

// An implementation of the OAUTHBEARER authentication mechanism, as
// described in RFC 7628.
func NewOAuthBearerClient(opt *OAuthBearerOptions) Client {
	return &oauthBearerClient{*opt}
}

type OAuthBearerAuthenticator func(opts OAuthBearerOptions) *OAuthBearerError

type oauthBearerServer struct {
	done         bool
	failErr      error
	strict       bool
	authenticate OAuthBearerAuthenticator
}

func (a *oauthBearerServer) fail(descr string) ([]byte, bool, error) {
	blob, err := json.Marshal(OAuthBearerError{
		Status:  "invalid_request",
		Schemes: "bearer",
	})
	if err != nil {
		panic(err) // wtf
	}
	a.failErr = errors.New("sasl: client error: " + descr)
	return blob, false, nil
}

func (a *oauthBearerServer) Next(response []byte) (challenge []byte, done bool, err error) {
	// Per RFC, we cannot just send an error, we need to return JSON-structured
	// value as a challenge and then after getting dummy response from the
	// client stop the exchange.
	if a.failErr != nil {
		// Server libraries (go-smtp, go-imap) will not call Next on
		// protocol-specific SASL cancel response ('*'). However, GS2 (and
		// indirectly OAUTHBEARER) defines a protocol-independent way to do so
		// using 0x01.
		if len(response) != 1 || response[0] != kvSep {
			return nil, true, errors.New("sasl: invalid response")
		}
		return nil, true, a.failErr
	}

	if a.done {
		err = ErrUnexpectedClientResponse
		return
	}

	// Generate empty challenge.
	if response == nil {
		return []byte{}, false, nil
	}

	a.done = true

	var opts OAuthBearerOptions
	if a.strict {
		var parseErr error
		opts, parseErr = parseOAuthBearerResponse(response)
		if parseErr != nil {
			return a.fail(parseErr.Error())
		}
	} else {
		var failChallenge []byte
		opts, failChallenge = a.parseLenient(response)
		if failChallenge != nil {
			return failChallenge, false, nil
		}
	}

	authzErr := a.authenticate(opts)
	if authzErr != nil {
		blob, err := json.Marshal(authzErr)
		if err != nil {
			panic(err) // wtf
		}
		a.failErr = authzErr
		return blob, false, nil
	}

	return nil, true, nil
}

// parseLenient parses the client response without enforcing the exact
// format specified by the RFC. On malformed input it returns a non-nil
// failure challenge to be sent to the client.
func (a *oauthBearerServer) parseLenient(response []byte) (OAuthBearerOptions, []byte) {
	fail := func(descr string) (OAuthBearerOptions, []byte) {
		blob, _, _ := a.fail(descr)
		return OAuthBearerOptions{}, blob
	}

	// Cut n,a=username,\x01host=...\x01auth=...
	// into
	//   n
	//   a=username
	//   \x01host=...\x01auth=...\x01\x01
	parts := bytes.SplitN(response, []byte{','}, 3)
	if len(parts) != 3 {
		return fail("Invalid response")
	}
	flag := parts[0]
	authzid := parts[1]
	if !bytes.Equal(flag, []byte{'n'}) {
		return fail("Invalid response, missing 'n' in gs2-cb-flag")
	}
	opts := OAuthBearerOptions{}
	if len(authzid) > 0 {
		if !bytes.HasPrefix(authzid, []byte("a=")) {
			return fail("Invalid response, missing 'a=' in gs2-authzid")
		}
		// Decode GS2 escaping on a best-effort basis: if the authzid
		// contains invalid escape sequences, keep it as-is for
		// backwards compatibility.
		raw := string(bytes.TrimPrefix(authzid, []byte("a=")))
		if u, err := unescapeAuthzid(raw); err == nil {
			opts.Username = u
		} else {
			opts.Username = raw
		}
	}

	// Cut \x01host=...\x01auth=...\x01\x01
	// into
	//   *empty*
	//   host=...
	//   auth=...
	//   *empty*
	//
	// Note that this code does not do a lot of checks to make sure the input
	// follows the exact format specified by RFC.
	params := bytes.Split(parts[2], []byte{kvSep})
	for _, p := range params {
		// Skip empty fields (one at start and end).
		if len(p) == 0 {
			continue
		}

		pParts := bytes.SplitN(p, []byte{'='}, 2)
		if len(pParts) != 2 {
			return fail("Invalid response, missing '='")
		}

		switch string(pParts[0]) {
		case "host":
			opts.Host = string(pParts[1])
		case "port":
			port, err := strconv.ParseUint(string(pParts[1]), 10, 16)
			if err != nil {
				return fail("Invalid response, malformed 'port' value")
			}
			opts.Port = int(port)
		case "auth":
			const prefix = "bearer "
			strValue := string(pParts[1])
			// Token type is case-insensitive.
			if !strings.HasPrefix(strings.ToLower(strValue), prefix) {
				return fail("Unsupported token type")
			}
			opts.Token = strValue[len(prefix):]
		default:
			return fail("Invalid response, unknown parameter: " + string(pParts[0]))
		}
	}

	return opts, nil
}

// parseOAuthBearerResponse strictly validates and parses an OAUTHBEARER
// client response, as described in RFC 7628 section 3.1:
//
//	client-resp = gs2-header kvsep *kvpair kvsep kvsep
//	gs2-header  = gs2-cb-flag "," [ gs2-authzid ] ","
//	kvpair      = key "=" value
func parseOAuthBearerResponse(response []byte) (OAuthBearerOptions, error) {
	var opts OAuthBearerOptions

	if !bytes.HasPrefix(response, []byte("n,")) {
		return opts, errors.New("Invalid response, missing 'n' in gs2-cb-flag")
	}
	rest := response[len("n,"):]

	// The authzid cannot contain a raw ',', so the next ',' always
	// terminates the GS2 header.
	idx := bytes.IndexByte(rest, ',')
	if idx < 0 {
		return opts, errors.New("Invalid response, malformed gs2-header")
	}
	authzid := rest[:idx]
	rest = rest[idx+1:]
	if len(authzid) > 0 {
		if !bytes.HasPrefix(authzid, []byte("a=")) {
			return opts, errors.New("Invalid response, missing 'a=' in gs2-authzid")
		}
		u, err := unescapeAuthzid(string(authzid[len("a="):]))
		if err != nil {
			return opts, errors.New("Invalid response, " + err.Error())
		}
		opts.Username = u
	}

	// The key-value pair section must start with kvsep and be terminated
	// by exactly two kvsep bytes.
	if len(rest) == 0 || rest[0] != kvSep {
		return opts, errors.New("Invalid response, missing key-value pair separator")
	}
	if len(rest) < 2 || rest[len(rest)-1] != kvSep || rest[len(rest)-2] != kvSep {
		return opts, errors.New("Invalid response, missing trailing separators")
	}
	var body []byte
	if len(rest) >= 3 {
		body = rest[1 : len(rest)-2]
	}

	seen := map[string]bool{}
	if len(body) > 0 {
		for _, field := range bytes.Split(body, []byte{kvSep}) {
			if len(field) == 0 {
				return opts, errors.New("Invalid response, empty key-value pair")
			}
			kv := bytes.SplitN(field, []byte{'='}, 2)
			if len(kv) != 2 || len(kv[0]) == 0 {
				return opts, errors.New("Invalid response, malformed key-value pair")
			}
			key := string(kv[0])
			if seen[key] {
				return opts, errors.New("Invalid response, duplicate '" + key + "' parameter")
			}
			seen[key] = true

			switch key {
			case "host":
				opts.Host = string(kv[1])
			case "port":
				port, err := strconv.ParseUint(string(kv[1]), 10, 16)
				if err != nil {
					return opts, errors.New("Invalid response, malformed 'port' value")
				}
				opts.Port = int(port)
			case "auth":
				const prefix = "bearer "
				strValue := string(kv[1])
				// Token type is case-insensitive.
				if !strings.HasPrefix(strings.ToLower(strValue), prefix) {
					return opts, errors.New("Unsupported token type")
				}
				opts.Token = strValue[len(prefix):]
				if opts.Token == "" {
					return opts, errors.New("Invalid response, empty 'auth' token")
				}
			default:
				return opts, errors.New("Invalid response, unknown parameter: " + key)
			}
		}
	}

	if !seen["auth"] {
		return opts, errors.New("Invalid response, missing 'auth' parameter")
	}

	return opts, nil
}

// NewOAuthBearerServer creates a lenient OAUTHBEARER server, kept for
// backwards compatibility. New code should use NewOAuthBearerServerStrict.
func NewOAuthBearerServer(auth OAuthBearerAuthenticator) Server {
	return &oauthBearerServer{authenticate: auth}
}

// NewOAuthBearerServerStrict creates an OAUTHBEARER server that strictly
// validates the client response against RFC 7628: GS2 authzid escaping,
// key-value pair boundaries, the trailing separators and the mandatory
// 'auth' parameter are all checked, and duplicate host/port/auth
// parameters are rejected.
func NewOAuthBearerServerStrict(auth OAuthBearerAuthenticator) Server {
	return &oauthBearerServer{authenticate: auth, strict: true}
}
