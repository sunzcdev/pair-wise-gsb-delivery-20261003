package webdav

// This file implements conditional request handling as defined in RFC 7232
// (Hypertext Transfer Protocol (HTTP/1.1): Conditional Requests).
//
// The conditional headers If-Match and If-None-Match carry either a
// wildcard ("*") or a comma-separated list of entity-tags, e.g.
//
//	If-Match: "xyzzy", "r2d2xxxx"
//	If-None-Match: W/"weak-etag"
//
// A ConditionalMatch is parsed by the handler before the request body is
// read, so malformed headers are rejected without consuming a potentially
// large payload. It is then handed to the backend, which MUST evaluate it
// atomically at commit time: a handler must not try to emulate atomicity by
// statting the resource before an ordinary write.

import (
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"
)

// ErrConditionalMatchNotSupported is returned by backends that cannot
// evaluate conditional headers atomically.
//
// A backend that does not provide atomic conditional commits must reject
// requests carrying conditional headers instead of attempting a non-atomic
// read-modify-write; use NewConditionalMatchNotSupportedError to build the
// corresponding HTTP error.
var ErrConditionalMatchNotSupported = errors.New("webdav: backend doesn't support atomic conditional requests")

// NewConditionalMatchNotSupportedError creates an HTTP 501 (Not Implemented)
// error wrapping ErrConditionalMatchNotSupported.
//
// It is the canonical rejection path for backends unable to evaluate
// If-Match/If-None-Match atomically.
func NewConditionalMatchNotSupportedError() error {
	return NewHTTPError(http.StatusNotImplemented, ErrConditionalMatchNotSupported)
}

// IsConditionalMatchNotSupported reports whether err signals that the
// backend cannot atomically evaluate conditional headers.
func IsConditionalMatchNotSupported(err error) bool {
	return errors.Is(err, ErrConditionalMatchNotSupported)
}

// ETag is an HTTP entity-tag, as defined in RFC 7232 section 2.3.
//
// The opaque-tag is stored without the surrounding double quotes; Weak
// reports whether the "W/" weak indicator is present.
type ETag struct {
	// OpaqueTag is the unquoted entity-tag value.
	OpaqueTag string
	// Weak indicates the W/ weak validator.
	Weak bool
}

// ParseETag parses a single entity-tag, e.g. `"foo"` or `W/"foo"`.
//
// Surrounding whitespace is tolerated because the value may be read from a
// header field.
func ParseETag(s string) (ETag, error) {
	s = strings.TrimSpace(s)

	weak := false
	if strings.HasPrefix(s, "W/") || strings.HasPrefix(s, "w/") {
		weak = true
		s = s[2:]
	}

	if len(s) < 2 || s[0] != '"' || s[len(s)-1] != '"' {
		return ETag{}, errors.New("webdav: malformed ETag: expected quoted-string")
	}

	body := s[1 : len(s)-1]
	var b strings.Builder
	b.Grow(len(body))

	for i := 0; i < len(body); {
		c := body[i]
		if c == '"' {
			return ETag{}, errors.New("webdav: malformed ETag: unescaped quote")
		}
		if c < 0x20 || c == 0x7f {
			return ETag{}, errors.New("webdav: malformed ETag: control character")
		}
		if c == '\\' {
			i++
			if i >= len(body) {
				return ETag{}, errors.New("webdav: malformed ETag: dangling escape")
			}
			b.WriteByte(body[i])
			i++
			continue
		}
		if c >= 0x80 {
			r, size := utf8.DecodeRuneInString(body[i:])
			if r == utf8.RuneError && size == 1 {
				return ETag{}, errors.New("webdav: malformed ETag: invalid UTF-8")
			}
			b.WriteString(body[i : i+size])
			i += size
			continue
		}
		b.WriteByte(c)
		i++
	}

	return ETag{OpaqueTag: b.String(), Weak: weak}, nil
}

// String formats the entity-tag for use in an HTTP header.
func (et ETag) String() string {
	var b strings.Builder
	if et.Weak {
		b.WriteString("W/")
	}
	b.WriteByte('"')
	for _, r := range et.OpaqueTag {
		if r == '"' || r == '\\' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String()
}

// ConditionalMatch represents a parsed If-Match or If-None-Match header
// value, as defined in RFC 7232 sections 3.1 and 3.2.
//
// The zero value is an unset condition. A set condition is either a
// wildcard (Wildcard is true) or a non-empty list of entity-tags.
type ConditionalMatch struct {
	// Wildcard is true for the "*" form.
	Wildcard bool
	// ETags holds the entity-tags of the list form.
	ETags []ETag
}

// ParseConditionalMatch parses an If-Match or If-None-Match header value.
//
// An empty string produces an unset condition. Otherwise the value must be
// "*" or a comma-separated list of valid entity-tags; combining the wildcard
// with entity-tags or providing an empty list is an error.
func ParseConditionalMatch(s string) (ConditionalMatch, error) {
	if strings.TrimSpace(s) == "" {
		return ConditionalMatch{}, nil
	}

	list, err := splitETagList(s)
	if err != nil {
		return ConditionalMatch{}, err
	}

	cm := ConditionalMatch{ETags: make([]ETag, 0, len(list))}
	for _, item := range list {
		if strings.TrimSpace(item) == "*" {
			if len(list) != 1 {
				return ConditionalMatch{}, errors.New("webdav: wildcard cannot be combined with entity-tags")
			}
			cm.Wildcard = true
			cm.ETags = nil
			return cm, nil
		}
		et, err := ParseETag(item)
		if err != nil {
			return ConditionalMatch{}, err
		}
		cm.ETags = append(cm.ETags, et)
	}
	return cm, nil
}

// splitETagList splits a comma-separated entity-tag list without splitting
// inside a quoted-string.
func splitETagList(s string) ([]string, error) {
	var items []string
	start := 0
	inQuotes := false
	for i := 0; i < len(s); {
		c := s[i]
		if inQuotes {
			switch c {
			case '\\':
				i++
				if i >= len(s) {
					return nil, errors.New("webdav: malformed ETag: dangling escape")
				}
			case '"':
				inQuotes = false
			}
			i++
			continue
		}
		switch c {
		case '"':
			inQuotes = true
		case ',':
			items = append(items, s[start:i])
			start = i + 1
		}
		i++
	}
	if inQuotes {
		return nil, errors.New("webdav: malformed ETag: unterminated quoted-string")
	}
	items = append(items, s[start:])

	for _, item := range items {
		if strings.TrimSpace(item) == "" {
			return nil, errors.New("webdav: malformed ETag list: empty list element")
		}
	}
	return items, nil
}

// IsSet reports whether the condition carries a value.
func (cm ConditionalMatch) IsSet() bool {
	return cm.Wildcard || len(cm.ETags) > 0
}

// NewConditionalMatch builds a list-form condition from one or more
// entity-tags. With no arguments it returns an unset condition.
func NewConditionalMatch(etags ...ETag) ConditionalMatch {
	if len(etags) == 0 {
		return ConditionalMatch{}
	}
	return ConditionalMatch{ETags: append([]ETag(nil), etags...)}
}

// WildcardConditionalMatch returns the "*" wildcard condition.
func WildcardConditionalMatch() ConditionalMatch {
	return ConditionalMatch{Wildcard: true}
}

// IsWildcard reports whether the condition is the "*" form.
func (cm ConditionalMatch) IsWildcard() bool {
	return cm.Wildcard
}

// String formats the condition back into an HTTP header value. An unset
// condition formats to the empty string.
func (cm ConditionalMatch) String() string {
	switch {
	case cm.Wildcard:
		return "*"
	case len(cm.ETags) == 0:
		return ""
	}
	parts := make([]string, len(cm.ETags))
	for i, et := range cm.ETags {
		parts[i] = et.String()
	}
	return strings.Join(parts, ", ")
}

// strongCompare reports whether two entity-tags are equal using the strong
// comparison function (RFC 7232 section 2.3.2): both validators must be
// strong and share the same opaque-tag.
func strongCompare(a, b ETag) bool {
	return !a.Weak && !b.Weak && a.OpaqueTag == b.OpaqueTag
}

// weakCompare reports whether two entity-tags are equal using the weak
// comparison function (RFC 7232 section 2.3.2): the opaque-tags must match,
// regardless of the weak indicators.
func weakCompare(a, b ETag) bool {
	return a.OpaqueTag == b.OpaqueTag
}

// StrongMatch evaluates the condition against et using the strong
// comparison function used by If-Match (RFC 7232 section 3.1).
//
// The wildcard matches whenever a current entity-tag is provided. A weak
// current entity-tag or a weak entity-tag in the list never matches under
// strong comparison.
func (cm ConditionalMatch) StrongMatch(et ETag) bool {
	if cm.Wildcard {
		return true
	}
	for _, candidate := range cm.ETags {
		if strongCompare(candidate, et) {
			return true
		}
	}
	return false
}

// WeakMatch evaluates the condition against et using the weak comparison
// function used by If-None-Match (RFC 7232 section 3.2).
//
// The wildcard matches whenever a current entity-tag is provided; the
// opaque-tags are compared regardless of either validator's weak indicator.
func (cm ConditionalMatch) WeakMatch(et ETag) bool {
	if cm.Wildcard {
		return true
	}
	for _, candidate := range cm.ETags {
		if weakCompare(candidate, et) {
			return true
		}
	}
	return false
}

// ConditionalPut bundles the conditional headers that may accompany a
// state-changing request. It is embedded in the backend write options and
// is evaluated atomically by the backend at commit time.
type ConditionalPut struct {
	IfMatch     ConditionalMatch
	IfNoneMatch ConditionalMatch
}

// ParseConditionalPutHeaders parses raw If-Match and If-None-Match header
// values. It must be called before the request body is consumed so that a
// malformed header results in a 400 response without reading the payload.
func ParseConditionalPutHeaders(ifMatch, ifNoneMatch string) (ConditionalPut, error) {
	match, err := ParseConditionalMatch(ifMatch)
	if err != nil {
		return ConditionalPut{}, err
	}
	noneMatch, err := ParseConditionalMatch(ifNoneMatch)
	if err != nil {
		return ConditionalPut{}, err
	}
	return ConditionalPut{IfMatch: match, IfNoneMatch: noneMatch}, nil
}

// Check atomically evaluates both conditions against the resource's current
// entity-tag.
//
// current is the zero ETag when the resource does not exist. If-Match is
// evaluated with the strong comparison and If-None-Match with the weak
// comparison, as mandated by RFC 7232 sections 3.1 and 3.2. On failure it
// returns a 412 Precondition Failed HTTP error.
func (cp ConditionalPut) Check(current ETag, exists bool) error {
	if cp.IfMatch.IsSet() {
		// If-Match fails when no current representation exists, and a
		// wildcard matches only when the representation does exist.
		if !exists || !cp.IfMatch.StrongMatch(current) {
			return preconditionFailed("If-Match")
		}
	}
	if cp.IfNoneMatch.IsSet() && exists && cp.IfNoneMatch.WeakMatch(current) {
		return preconditionFailed("If-None-Match")
	}
	return nil
}

func preconditionFailed(header string) error {
	return NewHTTPError(http.StatusPreconditionFailed,
		errors.New("webdav: "+header+" precondition failed"))
}
