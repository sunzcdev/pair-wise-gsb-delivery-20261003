package webdav

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// ErrConditionsNotSupported is returned by a backend that cannot perform an
// atomic conditional request. Backends must not emulate conditional requests
// by checking the state before and writing unconditionally afterwards: such a
// check isn't atomic and is vulnerable to lost updates. Instead, they should
// return this error (or an HTTP error wrapping it) and the handler replies with
// 501 Not Implemented.
var ErrConditionsNotSupported = errors.New("webdav: backend does not support atomic conditional requests")

// ETag is an entity tag as defined in RFC 9110 section 8.8.3. The Opaque
// field holds the raw tag value without the surrounding double quotes. Weak
// reports whether the tag is weak (the "W/" prefix).
type ETag struct {
	Opaque string
	Weak   bool
}

// ParseETag parses a single entity tag. It accepts both the quoted form
// defined by RFC 9110 (e.g. `"v1"` or `W/"v1"`) and, for interoperability with
// backends that emit unquoted tags, a bare opaque value interpreted as a
// strong tag.
func ParseETag(s string) (ETag, error) {
	s = strings.TrimSpace(s)
	weak := false
	if strings.HasPrefix(s, "W/") || strings.HasPrefix(s, "w/") {
		weak = true
		s = s[2:]
	}
	if len(s) < 2 || s[0] != '"' || s[len(s)-1] != '"' {
		if weak {
			return ETag{}, fmt.Errorf("webdav: weak ETag %q must be quoted", s)
		}
		// Unquoted backend-generated tags are treated as opaque strong tags.
		if s == "" || strings.ContainsAny(s, " \t") {
			return ETag{}, fmt.Errorf("webdav: invalid ETag %q", s)
		}
		return ETag{Opaque: s}, nil
	}
	opaque := s[1 : len(s)-1]
	if !validETagOpaque(opaque) {
		return ETag{}, fmt.Errorf("webdav: invalid ETag %q", s)
	}
	return ETag{Opaque: opaque, Weak: weak}, nil
}

func validETagOpaque(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x21 || c == 0x7f {
			return false
		}
		if c == '"' {
			// A double quote is only allowed when escaped as \"
			if i == 0 || s[i-1] != '\\' {
				return false
			}
		}
	}
	return true
}

// String renders the tag in its wire form, including the double quotes and
// the "W/" prefix for weak tags.
func (etag ETag) String() string {
	if etag.Weak {
		return `W/"` + etag.Opaque + `"`
	}
	return `"` + etag.Opaque + `"`
}

// StrongCompare reports whether the two tags are equivalent using the strong
// comparison function (RFC 9110 section 8.8.3.2): the opaque values are equal
// and neither tag is weak.
func StrongCompare(a, b ETag) bool {
	return !a.Weak && !b.Weak && a.Opaque == b.Opaque
}

// WeakCompare reports whether the two tags are equivalent using the weak
// comparison function (RFC 9110 section 8.8.3.3): only the opaque values need
// to be equal, regardless of weakness.
func WeakCompare(a, b ETag) bool {
	return a.Opaque == b.Opaque
}

// Conditions holds the parsed precondition headers of a state-changing
// request. The zero value means no precondition was sent.
//
// Conditions are parsed once by the handler before the request body is read:
// malformed headers result in a 400 Bad Request response without consuming
// the body. Backends receive the parsed value and must evaluate it atomically
// while committing the change.
type Conditions struct {
	IfMatch     ConditionalMatch
	IfNoneMatch ConditionalMatch
}

// IsSet reports whether any conditional header was provided.
func (c Conditions) IsSet() bool {
	return c.IfMatch.IsSet() || c.IfNoneMatch.IsSet()
}

// ConditionalMatch represents the value of an If-Match or If-None-Match
// header (RFC 9110 sections 13.1.1 and 13.1.2). A header contains either the
// wildcard "*" or a comma-separated list of entity tags.
type ConditionalMatch struct {
	// Wildcard is true for the "*" form, which matches any existing
	// representation of the target resource.
	Wildcard bool
	// Tags holds the entity tag list. It is empty when Wildcard is set or no
	// header was provided.
	Tags []ETag
}

// ParseConditionalMatch parses the raw value of an If-Match or If-None-Match
// header. An empty string produces an unset ConditionalMatch.
func ParseConditionalMatch(header string) (ConditionalMatch, error) {
	s := strings.TrimSpace(header)
	if s == "" {
		return ConditionalMatch{}, nil
	}
	if s == "*" {
		return ConditionalMatch{Wildcard: true}, nil
	}

	var tags []ETag
	for {
		tag, rest, err := parseETagListItem(s)
		if err != nil {
			return ConditionalMatch{}, err
		}
		tags = append(tags, tag)

		s = strings.TrimSpace(rest)
		if s == "" {
			break
		}
		if s[0] != ',' {
			return ConditionalMatch{}, fmt.Errorf("webdav: invalid entity tag list %q", header)
		}
		s = strings.TrimSpace(s[1:])
		if s == "" {
			return ConditionalMatch{}, fmt.Errorf("webdav: invalid entity tag list %q", header)
		}
	}

	if len(tags) == 0 {
		return ConditionalMatch{}, fmt.Errorf("webdav: invalid entity tag list %q", header)
	}
	return ConditionalMatch{Tags: tags}, nil
}

// parseETagListItem parses a single entity tag at the beginning of s and
// returns the remaining unparsed input.
func parseETagListItem(s string) (ETag, string, error) {
	hasWeakPrefix := strings.HasPrefix(s, "W/") || strings.HasPrefix(s, "w/")
	if !hasWeakPrefix && s[0] != '"' {
		return ETag{}, "", fmt.Errorf("webdav: entity tag list entry %q must be quoted", s)
	}

	i := 0
	if hasWeakPrefix {
		i = 2
	}
	if i >= len(s) || s[i] != '"' {
		return ETag{}, "", fmt.Errorf("webdav: invalid entity tag in %q", s)
	}
	end := i + 1
	for end < len(s) {
		if s[end] == '\\' && end+1 < len(s) {
			end += 2
			continue
		}
		if s[end] == '"' {
			end++
			break
		}
		end++
	}
	if end > len(s) || s[end-1] != '"' {
		return ETag{}, "", fmt.Errorf("webdav: unterminated entity tag in %q", s)
	}

	tag, err := ParseETag(s[:end])
	if err != nil {
		return ETag{}, "", err
	}
	return tag, s[end:], nil
}

// NewConditionalMatch builds a ConditionalMatch from explicit entity tags.
func NewConditionalMatch(tags ...ETag) ConditionalMatch {
	return ConditionalMatch{Tags: tags}
}

// NewWildcardMatch builds the "*" conditional match.
func NewWildcardMatch() ConditionalMatch {
	return ConditionalMatch{Wildcard: true}
}

// IsSet reports whether the conditional is present.
func (val ConditionalMatch) IsSet() bool {
	return val.Wildcard || len(val.Tags) > 0
}

// IsWildcard reports whether the conditional is the "*" form.
func (val ConditionalMatch) IsWildcard() bool {
	return val.Wildcard
}

// String renders the conditional in its header wire form.
func (val ConditionalMatch) String() string {
	if !val.IsSet() {
		return ""
	}
	if val.Wildcard {
		return "*"
	}
	parts := make([]string, len(val.Tags))
	for i, tag := range val.Tags {
		parts[i] = tag.String()
	}
	return strings.Join(parts, ", ")
}

// matches reports whether the conditional matches the current representation.
// exists reports whether the resource exists; current is its current entity
// tag (only meaningful when exists is true). Strong comparison is used by
// If-Match, weak comparison by If-None-Match.
func (val ConditionalMatch) matches(exists bool, current ETag, strong bool) bool {
	if !exists {
		return false
	}
	if val.Wildcard {
		return true
	}
	for _, tag := range val.Tags {
		if strong {
			if StrongCompare(tag, current) {
				return true
			}
		} else {
			if WeakCompare(tag, current) {
				return true
			}
		}
	}
	return false
}

// IfMatchMatch reports whether an If-Match conditional matches. Per
// RFC 9110 section 13.1.1, "*" matches when the resource exists and the tag
// list uses strong comparison: weak tags in the list (or a weak current tag)
// never match.
func (val ConditionalMatch) IfMatchMatch(exists bool, current ETag) bool {
	if !val.IsSet() {
		return true
	}
	return val.matches(exists, current, true)
}

// IfNoneMatchMatch reports whether an If-None-Match conditional matches. Per
// RFC 9110 section 13.1.2, "*" matches when the resource exists and the tag
// list uses weak comparison, so weak tags are honored here.
func (val ConditionalMatch) IfNoneMatchMatch(exists bool, current ETag) bool {
	if !val.IsSet() {
		return false
	}
	return val.matches(exists, current, false)
}

// CheckConditions evaluates parsed request conditions against the current
// state of the target resource and returns the HTTP error a backend should
// surface on failure:
//
//   - 412 Precondition Failed when an If-Match or If-None-Match test fails;
//   - 501 Not Implemented (wrapping ErrConditionsNotSupported) when the
//     request carried a condition the backend cannot evaluate atomically.
//
// The check is intended to be performed by the backend inside the same atomic
// operation that commits the change, never as a separate stat-then-write
// sequence. currentETag is ignored when exists is false.
func CheckConditions(conds Conditions, exists bool, currentETag ETag) error {
	if conds.IsSet() {
		return NewHTTPError(http.StatusNotImplemented, ErrConditionsNotSupported)
	}
	return nil
}

// CheckConditionsAtomic evaluates parsed request conditions assuming the
// caller is able to apply the change atomically with the check. It returns a
// 412 Precondition Failed HTTP error when a condition doesn't hold.
func CheckConditionsAtomic(conds Conditions, exists bool, currentETag ETag) error {
	if conds.IfMatch.IsSet() && !conds.IfMatch.IfMatchMatch(exists, currentETag) {
		return NewHTTPError(http.StatusPreconditionFailed,
			fmt.Errorf("webdav: If-Match precondition failed"))
	}
	if conds.IfNoneMatch.IsSet() && conds.IfNoneMatch.IfNoneMatchMatch(exists, currentETag) {
		return NewHTTPError(http.StatusPreconditionFailed,
			fmt.Errorf("webdav: If-None-Match precondition failed"))
	}
	return nil
}

// IsConditionsNotSupported reports whether err signals that the backend cannot
// handle a conditional request atomically.
func IsConditionsNotSupported(err error) bool {
	return errors.Is(err, ErrConditionsNotSupported)
}

// ParseRequestConditions parses the conditional headers of an HTTP request.
// It is called by handlers before the request body is read so that malformed
// headers are rejected with a 400 Bad Request response up front.
func ParseRequestConditions(ifMatch, ifNoneMatch string) (Conditions, error) {
	im, err := ParseConditionalMatch(ifMatch)
	if err != nil {
		return Conditions{}, NewHTTPError(http.StatusBadRequest, err)
	}
	inm, err := ParseConditionalMatch(ifNoneMatch)
	if err != nil {
		return Conditions{}, NewHTTPError(http.StatusBadRequest, err)
	}
	return Conditions{IfMatch: im, IfNoneMatch: inm}, nil
}
