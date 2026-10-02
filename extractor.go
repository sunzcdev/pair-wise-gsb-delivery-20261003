package nosurf

import (
	"errors"
	"net/http"
)

// Reasons for CSRF check failures related to token extraction.
var (
	// ErrInvalidToken is returned (via Reason()) when a token extractor
	// found its source in the request, but the value was invalid
	// (e.g. not valid base64 for the built-in extractors).
	ErrInvalidToken = errors.New("The CSRF token provided in the request is malformed")
	// ErrConflictingTokens is returned (via Reason()) when more than one
	// extractor provided a token and the conflict policy is ConflictError.
	ErrConflictingTokens = errors.New("The CSRF token was provided by multiple sources")
)

// TokenExtractor extracts a CSRF token from a request.
// It allows JSON APIs and other non-form clients to supply the token
// from an application-defined location.
//
// An extractor must distinguish three outcomes:
//
//   - found == false, err == nil: the extractor's source is not present
//     in the request. The next extractor in the chain is consulted.
//   - found == true, err == nil: a token was provided.
//   - err != nil: the source was present, but its value was invalid.
//     The CSRF check fails immediately: the handler never falls back
//     to another extractor that might happen to provide a valid token.
//
// The returned token must be the base64-encoded (possibly masked) token,
// as produced by Token().
//
// Extractors should not read the request body unless necessary:
// doing so consumes it for downstream handlers.
type TokenExtractor func(r *http.Request) (token string, found bool, err error)

// ConflictPolicy determines how the handler behaves when more than one
// extractor in the chain provides a token for the same request.
type ConflictPolicy int

const (
	// ConflictFirstWins uses the token from the first extractor that
	// provides one and does not consult the remaining extractors.
	// This is the default and preserves the historical
	// "header preferred over form" behavior.
	ConflictFirstWins ConflictPolicy = iota
	// ConflictError fails the CSRF check with ErrConflictingTokens
	// if more than one extractor provides a token.
	// Note that under this policy every extractor runs,
	// so body-reading extractors (e.g. FormTokenExtractor)
	// will consume the request body.
	ConflictError
)

// HeaderTokenExtractor returns a TokenExtractor that reads the token
// from the given HTTP header. It never touches the request body,
// so it is safe to use for file uploads and other requests
// where the body must reach the downstream handler intact.
//
// A present but empty header is treated as "not provided".
// A non-empty value that is not valid base64 is reported as invalid.
func HeaderTokenExtractor(header string) TokenExtractor {
	return func(r *http.Request) (string, bool, error) {
		token := r.Header.Get(header)
		if token == "" {
			return "", false, nil
		}
		if b64decode(token) == nil {
			return "", false, ErrInvalidToken
		}
		return token, true, nil
	}
}

// FormTokenExtractor returns a TokenExtractor that reads the token
// from the given POST form field, including multipart form data.
// It parses the request body (consuming it), so requests checked
// with this extractor cannot have their body re-read downstream
// unless it is buffered by the application.
//
// A present but empty field is treated as "not provided".
// A non-empty value that is not valid base64 is reported as invalid.
func FormTokenExtractor(field string) TokenExtractor {
	return func(r *http.Request) (string, bool, error) {
		// PostFormValue calls ParseMultipartForm/ParseForm as needed
		// and covers both URL-encoded and multipart values.
		token := r.PostFormValue(field)
		if token == "" {
			return "", false, nil
		}
		if b64decode(token) == nil {
			return "", false, ErrInvalidToken
		}
		return token, true, nil
	}
}

// defaultExtractors preserves the historical behavior:
// the token is taken from the X-CSRF-Token header,
// falling back to the csrf_token form field.
func defaultExtractors() []TokenExtractor {
	return []TokenExtractor{
		HeaderTokenExtractor(HeaderName),
		FormTokenExtractor(FormFieldName),
	}
}

// extractToken runs the handler's extractor chain and returns
// the token sent with the request, if any.
func (h *CSRFHandler) extractToken(r *http.Request) (string, error) {
	var token string
	found := false

	for _, extractor := range h.extractors {
		sentToken, ok, err := extractor(r)
		if err != nil {
			// Fail closed: never fall back to another extractor
			// once one has reported invalid input.
			return "", err
		}
		if !ok {
			continue
		}
		if found {
			return "", ErrConflictingTokens
		}
		token, found = sentToken, true
		if h.conflictPolicy == ConflictFirstWins {
			break
		}
	}

	return token, nil
}
