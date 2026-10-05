package digest

import (
	"fmt"
	"net/http"
	"strings"
)

// Rejection describes why a single digest challenge was not selected.
// It intentionally omits the challenge nonce.
type Rejection struct {
	Realm     string
	Algorithm string
	Reason    string
}

// SelectionError is returned when digest challenges were received, but
// none of them could be selected. It never contains nonce values.
type SelectionError struct {
	Rejected []Rejection
}

func (e *SelectionError) Error() string {
	var b strings.Builder
	b.WriteString("digest: no usable challenge")
	for _, r := range e.Rejected {
		b.WriteString("\n  - ")
		b.WriteString(r.Reason)
		var ctx []string
		if r.Algorithm != "" {
			ctx = append(ctx, "algorithm: "+r.Algorithm)
		}
		if r.Realm != "" {
			ctx = append(ctx, "realm: "+r.Realm)
		}
		if len(ctx) != 0 {
			fmt.Fprintf(&b, " (%s)", strings.Join(ctx, ", "))
		}
	}
	return b.String()
}

// Selector selects a digest challenge from the WWW-Authenticate headers
// of a 401 response. The zero value selects the first usable challenge,
// matching the behavior of FindChallenge.
type Selector struct {
	// Prefer lists hash algorithms in decreasing order of preference.
	// Only usable challenges are compared. A challenge without an
	// algorithm parameter is treated as MD5, the protocol default.
	// Challenges with unlisted algorithms are ranked last, in the order
	// they were received. If empty, the first usable challenge wins.
	Prefer []string
}

// Select returns the selected digest challenge from the headers. Every
// WWW-Authenticate field may contain multiple challenges; each is parsed
// independently, so a malformed challenge does not corrupt its neighbors.
func (s Selector) Select(h http.Header) (*Challenge, error) {
	var usable []*Challenge
	var rejected []Rejection
	for _, field := range h.Values("WWW-Authenticate") {
		for _, part := range splitChallenges(field) {
			part = strings.TrimSpace(part)
			scheme, _ := cutScheme(part)
			if !strings.EqualFold(scheme, "Digest") {
				continue
			}
			chal, err := ParseChallenge(part)
			if err != nil {
				rejected = append(rejected, Rejection{Reason: err.Error()})
				continue
			}
			if err := canDigest(chal); err != nil {
				rejected = append(rejected, Rejection{
					Realm:     chal.Realm,
					Algorithm: chal.Algorithm,
					Reason:    err.Error(),
				})
				continue
			}
			if len(s.Prefer) == 0 {
				return chal, nil
			}
			usable = append(usable, chal)
		}
	}
	if len(usable) != 0 {
		best := usable[0]
		for _, chal := range usable[1:] {
			if s.rank(chal) < s.rank(best) {
				best = chal
			}
		}
		return best, nil
	}
	if len(rejected) != 0 {
		return nil, &SelectionError{Rejected: rejected}
	}
	return nil, ErrNoChallenge
}

// rank returns the preference rank of the challenge algorithm: lower is
// more preferred, unlisted algorithms rank last.
func (s Selector) rank(c *Challenge) int {
	algorithm := c.Algorithm
	if algorithm == "" {
		algorithm = "MD5" // protocol default
	}
	for i, prefer := range s.Prefer {
		if strings.EqualFold(prefer, algorithm) {
			return i
		}
	}
	return len(s.Prefer)
}

// cutScheme splits a challenge into its authentication scheme and the
// remaining text.
func cutScheme(s string) (scheme, rest string) {
	s = strings.TrimLeft(s, " \t")
	i := strings.IndexAny(s, " \t")
	if i < 0 {
		return s, ""
	}
	return s[:i], strings.TrimLeft(s[i:], " \t")
}

// splitChallenges splits a WWW-Authenticate field into its individual
// challenges. Commas inside quoted-strings and escaped quotes are not
// treated as separators. A comma-separated token that is not followed
// by '=' starts a new challenge; anything else continues the current
// challenge's parameter list.
func splitChallenges(field string) []string {
	var segments []string
	start := 0
	var quoted, escaped bool
	for i := 0; i < len(field); i++ {
		c := field[i]
		if quoted {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				quoted = false
			}
			continue
		}
		switch c {
		case '"':
			quoted = true
		case ',':
			segments = append(segments, field[start:i])
			start = i + 1
		}
	}
	segments = append(segments, field[start:])
	var challenges []string
	for _, segment := range segments {
		if len(challenges) == 0 || isSchemeStart(segment) {
			challenges = append(challenges, segment)
		} else {
			challenges[len(challenges)-1] += "," + segment
		}
	}
	return challenges
}

// isSchemeStart reports whether a comma-separated segment begins a new
// challenge rather than continuing the previous challenge's parameters.
func isSchemeStart(segment string) bool {
	s := strings.TrimLeft(segment, " \t")
	i := strings.IndexAny(s, " \t=")
	if i < 0 {
		return true // bare scheme without parameters
	}
	return s[i] != '='
}
