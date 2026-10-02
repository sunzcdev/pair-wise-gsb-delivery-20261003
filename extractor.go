package nosurf

import (
	"errors"
	"fmt"
	"net/http"
	"net/textproto"
)

// ErrConflictingTokens is the failure reason used when multiple token
// sources provide differing tokens
// and the conflict policy is FailOnConflict.
var ErrConflictingTokens = errors.New("The request contained multiple differing CSRF tokens")

// TokenExtractor extracts the CSRF token sent with the request.
//
// A TokenExtractor must distinguish three outcomes:
//
//   - the token was not provided: return ("", false, nil)
//   - the token was provided: return (token, true, nil)
//   - the input was present, but invalid: return ("", false, err)
//
// Returning an error aborts the whole extractor chain:
// the CSRF check fails immediately with that error as the reason
// and no other extractor is consulted.
type TokenExtractor func(r *http.Request) (token string, found bool, err error)

// TokenConflictPolicy determines how CSRFHandler behaves
// when more than one TokenExtractor provides a token for the same request.
type TokenConflictPolicy int

const (
	// FirstExtractorWins makes the handler use the token
	// provided by the first extractor in the chain that finds one.
	// This is the default and matches the historical behavior
	// of preferring the header over the form value.
	FirstExtractorWins TokenConflictPolicy = iota

	// FailOnConflict makes the CSRF check fail with ErrConflictingTokens
	// when multiple extractors provide differing tokens.
	// Identical tokens provided by multiple extractors are accepted.
	FailOnConflict
)

// HeaderTokenExtractor returns a TokenExtractor that reads the token
// from the given HTTP header. It never touches the request body,
// so it is safe to use for requests with large or streaming bodies
// (e.g. multipart uploads).
//
// The input is considered invalid if the header is present
// but empty, or has multiple values.
func HeaderTokenExtractor(header string) TokenExtractor {
	return func(r *http.Request) (string, bool, error) {
		vals, ok := r.Header[textproto.CanonicalMIMEHeaderKey(header)]
		if !ok || len(vals) == 0 {
			return "", false, nil
		}
		if len(vals) > 1 {
			return "", false, fmt.Errorf("nosurf: multiple %q header values", header)
		}
		if vals[0] == "" {
			return "", false, fmt.Errorf("nosurf: empty %q header", header)
		}
		return vals[0], true, nil
	}
}

// FormValueTokenExtractor returns a TokenExtractor that reads the token
// from the given POST form field, falling back to multipart form data.
//
// Note that it parses the request body (including multipart bodies),
// so it is unsuitable for endpoints that stream large uploads.
// To avoid consuming the body during the CSRF check,
// configure a chain consisting only of body-independent extractors,
// e.g. HeaderTokenExtractor.
func FormValueTokenExtractor(field string) TokenExtractor {
	return func(r *http.Request) (string, bool, error) {
		token := r.PostFormValue(field)

		// PostFormValue() will already have called ParseMultipartForm()
		if token == "" && r.MultipartForm != nil {
			vals := r.MultipartForm.Value[field]
			if len(vals) != 0 {
				token = vals[0]
			}
		}

		if token == "" {
			return "", false, nil
		}
		return token, true, nil
	}
}

// extractToken runs the configured TokenExtractor chain
// and returns the sent token according to the conflict policy.
func (h *CSRFHandler) extractToken(r *http.Request) (string, error) {
	var token string
	found := false

	for _, extract := range h.tokenExtractors {
		t, ok, err := extract(r)
		if err != nil {
			// Invalid input aborts the chain:
			// never fall back to a token from another source.
			return "", err
		}
		if !ok {
			continue
		}
		if !found {
			token, found = t, true
			if h.tokenConflictPolicy == FirstExtractorWins {
				return token, nil
			}
		} else if t != token {
			return "", ErrConflictingTokens
		}
	}

	return token, nil
}
