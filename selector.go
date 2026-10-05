package digest

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
)

// RejectReason explains why an individual challenge was not selected.
type RejectReason string

const (
	// ReasonOtherScheme means the challenge used a non-digest scheme.
	ReasonOtherScheme RejectReason = "other scheme"
	// ReasonMalformed means the challenge text could not be parsed.
	ReasonMalformed RejectReason = "malformed"
	// ReasonUnsupportedAlgorithm means the digest algorithm is not supported.
	ReasonUnsupportedAlgorithm RejectReason = "unsupported algorithm"
	// ReasonUnsupportedQOP means none of the offered qop values are supported.
	ReasonUnsupportedQOP RejectReason = "unsupported qop"
	// ReasonNotPreferred means the algorithm was supported but not
	// listed in the caller's algorithm preferences.
	ReasonNotPreferred RejectReason = "algorithm not in preference list"
	// ReasonWeakerAlgorithm means another supported challenge used a
	// higher-preference algorithm.
	ReasonWeakerAlgorithm RejectReason = "weaker algorithm"
)

// Rejection describes a challenge that the selector considered but
// did not choose. It deliberately excludes nonce and other credential
// material, so it is safe to log.
type Rejection struct {
	Field     int
	Index     int
	Scheme    string
	Algorithm string
	Reason    RejectReason
	Detail    string
}

// Selection is the result of selecting a challenge. When Challenge is
// nil, Err explains the failure and Rejections records why every
// candidate was skipped.
type Selection struct {
	Challenge  *Challenge
	Rejections []Rejection
	Err        error
}

// SelectionError is returned when no challenge could be selected in
// preference mode. Its message and Rejections never contain nonce
// values.
type SelectionError struct {
	Rejections []Rejection
}

func (e *SelectionError) Error() string {
	var b strings.Builder
	b.WriteString("digest: no usable challenge:")
	for _, r := range e.Rejections {
		fmt.Fprintf(&b, " field %d challenge %d", r.Field, r.Index)
		if r.Scheme != "" {
			fmt.Fprintf(&b, " scheme=%s", r.Scheme)
		}
		if r.Algorithm != "" {
			fmt.Fprintf(&b, " algorithm=%s", r.Algorithm)
		}
		fmt.Fprintf(&b, " (%s", r.Reason)
		if r.Detail != "" {
			fmt.Fprintf(&b, ": %s", r.Detail)
		}
		b.WriteString(")")
	}
	return b.String()
}

// Is reports whether the target is ErrNoChallenge. Both indicate that
// the response offered no digest challenge this client can answer.
func (e *SelectionError) Is(target error) bool {
	return target == ErrNoChallenge
}

// ChallengeSelector chooses a digest challenge from the
// WWW-Authenticate header fields of a received response.
//
// The zero value reproduces the legacy behavior: the first parseable,
// supported digest challenge wins. No state is retained between
// responses, so there is no cross-request downgrade memory.
type ChallengeSelector struct {
	// Algorithms lists supported digest algorithms in descending
	// order of preference (e.g. SHA-512-256, SHA-512, SHA-256, MD5).
	// Matching is case insensitive. A challenge whose algorithm is
	// missing is interpreted as MD5, per RFC 2617. When nil, the
	// selector falls back to the first usable challenge.
	Algorithms []string
}

// defaultAlgorithm is the protocol default when algorithm is absent.
const defaultAlgorithm = "MD5"

// Select walks every challenge in every WWW-Authenticate field and
// returns a Selection describing the chosen challenge (if any) and why
// each other challenge was skipped.
func (s ChallengeSelector) Select(h http.Header) Selection {
	parsed := ParseChallenges(h)
	if len(s.Algorithms) == 0 {
		return s.selectFirst(parsed)
	}
	return s.selectPreferred(parsed)
}

// Find returns the selected challenge or an error. It is suitable for
// assignment to Transport.FindChallenge.
func (s ChallengeSelector) Find(h http.Header) (*Challenge, error) {
	sel := s.Select(h)
	return sel.Challenge, sel.Err
}

// selectFirst implements the legacy first-usable strategy while still
// iterating every challenge in every field.
func (s ChallengeSelector) selectFirst(parsed []ParsedChallenge) Selection {
	var lastErr error
	var rejections []Rejection
	for _, pc := range parsed {
		if !strings.EqualFold(pc.Scheme, "Digest") {
			continue
		}
		switch {
		case pc.Err != nil:
			lastErr = pc.Err
			rejections = append(rejections, Rejection{
				Field:  pc.Field,
				Index:  pc.Index,
				Scheme: pc.Scheme,
				Reason: ReasonMalformed,
				Detail: pc.Err.Error(),
			})
		case CanDigest(pc.Challenge):
			return Selection{Challenge: pc.Challenge, Rejections: rejections}
		default:
			rejections = append(rejections, unsupportedRejection(pc))
		}
	}
	if lastErr == nil {
		lastErr = ErrNoChallenge
	}
	return Selection{Rejections: rejections, Err: lastErr}
}

// candidate is a supported digest challenge competing for selection.
type candidate struct {
	chal  *Challenge
	algo  string
	field int
	index int
	rank  int
}

// selectPreferred ranks only supported digest challenges by the
// configured algorithm preferences.
func (s ChallengeSelector) selectPreferred(parsed []ParsedChallenge) Selection {
	prefs := make([]string, len(s.Algorithms))
	for i, a := range s.Algorithms {
		prefs[i] = strings.ToUpper(a)
	}
	var candidates []candidate
	var rejections []Rejection
	for _, pc := range parsed {
		r := Rejection{
			Field:  pc.Field,
			Index:  pc.Index,
			Scheme: pc.Scheme,
		}
		if !strings.EqualFold(pc.Scheme, "Digest") {
			r.Reason = ReasonOtherScheme
			rejections = append(rejections, r)
			continue
		}
		if pc.Err != nil {
			r.Reason = ReasonMalformed
			r.Detail = pc.Err.Error()
			rejections = append(rejections, r)
			continue
		}
		algo := strings.ToUpper(pc.Challenge.Algorithm)
		if algo == "" {
			algo = defaultAlgorithm
		}
		r.Algorithm = algo
		switch {
		case !algorithmSupported(algo):
			r.Reason = ReasonUnsupportedAlgorithm
			rejections = append(rejections, r)
		case !qopSupported(pc.Challenge):
			r.Reason = ReasonUnsupportedQOP
			rejections = append(rejections, r)
		default:
			rank := slices.Index(prefs, algo)
			if rank < 0 {
				r.Reason = ReasonNotPreferred
				rejections = append(rejections, r)
				continue
			}
			candidates = append(candidates, candidate{
				chal:  pc.Challenge,
				algo:  algo,
				field: pc.Field,
				index: pc.Index,
				rank:  rank,
			})
		}
	}
	if len(candidates) == 0 {
		return Selection{
			Rejections: rejections,
			Err:        &SelectionError{Rejections: rejections},
		}
	}
	best := candidates[0]
	for _, cand := range candidates[1:] {
		if lessCandidate(cand, best) {
			best = cand
		}
	}
	for _, cand := range candidates {
		if cand.chal == best.chal {
			continue
		}
		rejections = append(rejections, Rejection{
			Field:     cand.field,
			Index:     cand.index,
			Scheme:    "Digest",
			Algorithm: cand.algo,
			Reason:    ReasonWeakerAlgorithm,
		})
	}
	return Selection{
		Challenge:  best.chal,
		Rejections: rejections,
	}
}

// lessCandidate reports whether a should be chosen before b.
// Preference rank wins; exact ties are broken deterministically by
// serialized form, independent of header ordering, then by position.
func lessCandidate(a, b candidate) bool {
	if a.rank != b.rank {
		return a.rank < b.rank
	}
	sa, sb := a.chal.String(), b.chal.String()
	if sa != sb {
		return sa < sb
	}
	if a.field != b.field {
		return a.field < b.field
	}
	return a.index < b.index
}

// algorithmSupported reports whether the package can compute responses
// for the given uppercase algorithm name.
func algorithmSupported(algo string) bool {
	switch algo {
	case "MD5", "SHA-256", "SHA-512", "SHA-512-256":
		return true
	default:
		return false
	}
}

// qopSupported reports whether at least one offered qop is usable.
func qopSupported(c *Challenge) bool {
	return len(c.QOP) == 0 || c.SupportsQOP("auth") || c.SupportsQOP("auth-int")
}

func unsupportedRejection(pc ParsedChallenge) Rejection {
	r := Rejection{
		Field:  pc.Field,
		Index:  pc.Index,
		Scheme: pc.Scheme,
	}
	algo := strings.ToUpper(pc.Challenge.Algorithm)
	if algo == "" {
		algo = defaultAlgorithm
	}
	r.Algorithm = algo
	if !algorithmSupported(algo) {
		r.Reason = ReasonUnsupportedAlgorithm
	} else {
		r.Reason = ReasonUnsupportedQOP
	}
	return r
}

// FindChallenge returns the first supported challenge in the headers.
// It inspects every challenge in every WWW-Authenticate field.
func FindChallenge(h http.Header) (*Challenge, error) {
	return ChallengeSelector{}.Find(h)
}
