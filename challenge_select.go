package digest

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/icholy/digest/internal/param"
)

// rawChallenge is one "scheme body" split out of a WWW-Authenticate
// header field.
type rawChallenge struct {
	scheme string
	body   string
	err    error
}

// isOWS reports whether b is optional whitespace per RFC 7230.
func isOWS(b byte) bool {
	return b == ' ' || b == '\t'
}

func trimOWS(s string) string {
	return strings.Trim(s, " \t")
}

// isSchemeChar reports whether b can appear in an auth-scheme token.
// Excluding '=', '/', '+' and ';' distinguishes scheme tokens from
// token68 during boundary lookahead.
func isSchemeChar(b byte) bool {
	switch {
	case 'a' <= b && b <= 'z':
		return true
	case 'A' <= b && b <= 'Z':
		return true
	case '0' <= b && b <= '9':
		return true
	case strings.IndexByte("!#$%&'*-.^_`|~", b) >= 0:
		return true
	default:
		return false
	}
}

// scanQuoted returns the index just past the closing quote of the
// quoted-string starting at s[i] == '"'. Escaped bytes are skipped.
// It returns -1 if the string is unterminated.
func scanQuoted(s string, i int) int {
	i++ // opening quote
	for i < len(s) {
		switch s[i] {
		case '\\':
			i += 2 // quoted-pair: skip the escaped byte
			continue
		case '"':
			return i + 1
		}
		i++
	}
	return -1
}

// healthyBoundary reports whether the comma at index i in s separates
// two challenges: it must be followed by OWS, an auth-scheme token,
// OWS, and then EOF or something other than '=' (a '=' makes the token
// a parameter key instead).
func healthyBoundary(s string, i int) bool {
	n := len(s)
	j := i + 1
	for j < n && isOWS(s[j]) {
		j++
	}
	start := j
	for j < n && isSchemeChar(s[j]) {
		j++
	}
	if start == j {
		return false
	}
	for j < n && isOWS(s[j]) {
		j++
	}
	if j == n {
		return true
	}
	return s[j] != '='
}

// balancedQuotes reports whether every quote in s is paired, honoring
// quoted-pair escapes.
func balancedQuotes(s string) bool {
	i := 0
	for i < len(s) {
		switch s[i] {
		case '\\':
			i += 2
			continue
		case '"':
			next := scanQuoted(s, i)
			if next < 0 {
				return false
			}
			i = next
			continue
		}
		i++
	}
	return true
}

// healthyCut returns the index of the earliest comma in body that
// separates two challenges without sitting inside a quoted string.
// It returns -1 when body looks like a single challenge.
func healthyCut(body string) int {
	i := 0
	for i < len(body) {
		switch body[i] {
		case '\\':
			i += 2
			continue
		case '"':
			end := scanQuoted(body, i)
			if end < 0 {
				// An unbalanced quote makes later commas ambiguous;
				// the caller validates the chunk and recovers.
				return -1
			}
			i = end
		default:
			if body[i] == ',' && healthyBoundary(body, i) {
				return i
			}
			i++
		}
	}
	return -1
}

// chunkOK reports whether a challenge body is structurally acceptable
// for its scheme. Digest bodies must parse as auth-params; other
// schemes (e.g. Basic token68) are accepted when quotes balance.
func chunkOK(scheme, body string) bool {
	body = trimOWS(body)
	if !balancedQuotes(body) {
		return false
	}
	if !strings.EqualFold(scheme, "Digest") {
		return true
	}
	if body == "" {
		return true
	}
	_, err := param.Parse(body)
	return err == nil
}

// strictBoundary reports whether the comma at i can serve as a
// challenge boundary while recovering from a malformed challenge. The
// broken text must lie before the comma (its quotes unbalanced), and
// the remainder must split into well-formed challenges. This prevents
// a comma that merely looks like a boundary — including one inside a
// later quoted value such as qop="auth,auth-int" — from being chosen.
func strictBoundary(s string, i int) bool {
	if !healthyBoundary(s, i) {
		return false
	}
	// the truncation must be before this comma
	if balancedQuotes(s[:i]) {
		return false
	}
	rest := s[i+1:]
	// the remainder must be structurally clean: balanced quotes and
	// every digest chunk parseable. A balanced remainder never enters
	// the malformed-chunk recovery branch, so this cannot recurse.
	if !balancedQuotes(rest) {
		return false
	}
	for _, rc := range splitAuthField(rest) {
		if rc.err != nil {
			return false
		}
		if !strings.EqualFold(rc.scheme, "Digest") {
			continue
		}
		if _, err := param.Parse(rc.body); err != nil {
			return false
		}
	}
	return true
}

// errUnterminatedQuote marks a challenge whose text contains a
// truncated quoted string. The message intentionally contains no
// parameter values.
var errUnterminatedQuote = errors.New("digest: malformed challenge: unterminated quoted string")

// splitAuthField splits one WWW-Authenticate field value into its
// individual challenges. It understands quoted-string commas, escaped
// quotes and scheme boundaries. A syntactically broken challenge does
// not consume later valid challenges: the broken text is emitted with
// an err set and parsing resumes at the recovered boundary.
func splitAuthField(field string) []rawChallenge {
	var out []rawChallenge
	s := field
	for s != "" {
		s = trimOWS(s)
		if s == "" {
			break
		}
		// read the scheme token
		i := 0
		for i < len(s) && isSchemeChar(s[i]) {
			i++
		}
		scheme := s[:i]
		s = s[i:]
		// skip the OWS between scheme and body
		j := 0
		for j < len(s) && isOWS(s[j]) {
			j++
		}
		body := s[j:]
		// tentative first boundary
		cut := healthyCut(body)
		chunk, rest := body, ""
		if cut >= 0 {
			chunk, rest = body[:cut], body[cut+1:]
		}
		// A valid chunk ends the search. An invalid one means a
		// truncated quoted string may have swallowed a later scheme
		// boundary; find the first comma whose remainder is sound.
		if !chunkOK(scheme, chunk) {
			recovered := -1
			for p := 0; p < len(body); p++ {
				if body[p] == ',' && strictBoundary(body, p) {
					recovered = p
					break
				}
			}
			if recovered >= 0 {
				out = append(out, rawChallenge{
					scheme: scheme,
					body:   trimOWS(body[:recovered]),
					err:    errUnterminatedQuote,
				})
				s = body[recovered+1:]
				continue
			}
			out = append(out, rawChallenge{
				scheme: scheme,
				body:   trimOWS(chunk),
				err:    errUnterminatedQuote,
			})
			s = rest
			continue
		}
		out = append(out, rawChallenge{
			scheme: scheme,
			body:   trimOWS(chunk),
		})
		s = rest
	}
	return out
}

// challengeFromParams builds a Challenge from parsed auth-params.
func challengeFromParams(pp []param.Param) *Challenge {
	var c Challenge
	for _, p := range pp {
		switch strings.ToLower(p.Key) {
		case "realm":
			c.Realm = p.Value
		case "domain":
			c.Domain = strings.Fields(p.Value)
		case "nonce":
			c.Nonce = p.Value
		case "algorithm":
			c.Algorithm = p.Value
		case "stale":
			c.Stale = strings.EqualFold(p.Value, "true")
		case "opaque":
			c.Opaque = p.Value
		case "qop":
			for _, q := range strings.Split(p.Value, ",") {
				if q = strings.TrimSpace(q); q != "" {
					c.QOP = append(c.QOP, q)
				}
			}
		case "charset":
			c.Charset = p.Value
		case "userhash":
			c.Userhash = strings.EqualFold(p.Value, "true")
		}
	}
	return &c
}

// ParsedChallenge is one challenge extracted from WWW-Authenticate
// header fields. Challenge is nil for non-digest schemes and for
// malformed digest challenges; in those cases Err explains why.
type ParsedChallenge struct {
	// Scheme is the auth-scheme token (e.g. "Basic", "Digest").
	Scheme string
	// Field is the index of the WWW-Authenticate header field it
	// came from.
	Field int
	// Index is the position of the challenge within that field.
	Index int
	// Challenge is the parsed digest challenge. It is nil when
	// Scheme is not "digest" or when Err is set.
	Challenge *Challenge
	// Err is set when the challenge text is malformed.
	Err error
}

// ParseChallenges parses all challenges from all WWW-Authenticate
// header fields, preserving each challenge's own parameters. Multiple
// challenges may be combined in a single field value.
func ParseChallenges(h http.Header) []ParsedChallenge {
	var out []ParsedChallenge
	for fi, field := range h.Values("WWW-Authenticate") {
		for ci, raw := range splitAuthField(field) {
			pc := ParsedChallenge{
				Scheme: raw.scheme,
				Field:  fi,
				Index:  ci,
			}
			if !strings.EqualFold(raw.scheme, "Digest") {
				out = append(out, pc)
				continue
			}
			if raw.err != nil {
				pc.Err = raw.err
				out = append(out, pc)
				continue
			}
			pp, err := param.Parse(raw.body)
			if err != nil {
				pc.Err = fmt.Errorf("digest: invalid challenge: %w", err)
				out = append(out, pc)
				continue
			}
			pc.Challenge = challengeFromParams(pp)
			out = append(out, pc)
		}
	}
	return out
}
