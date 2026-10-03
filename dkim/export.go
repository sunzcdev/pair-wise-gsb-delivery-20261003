package dkim

import (
	"bufio"
	"crypto"
	"crypto/rsa"
	"io"

	"golang.org/x/crypto/ed25519"
)

// Header is a raw, unparsed message header. Each entry is a single header
// field terminated by CRLF.
type Header = header

// HeaderPicker picks header fields in the order defined by RFC 6376
// section 3.7: multiple occurrences of the same field name are selected
// bottom-up.
type HeaderPicker = headerPicker

// ReadHeader reads a raw message header. The returned reader is positioned at
// the start of the message body.
func ReadHeader(r *bufio.Reader) (Header, error) {
	return readHeader(r)
}

// NewHeaderPicker creates a new header picker for h.
func NewHeaderPicker(h Header) *HeaderPicker {
	return newHeaderPicker(h)
}

// ParseHeaderField splits a raw header field into its name and value. Both
// are trimmed.
func ParseHeaderField(s string) (string, string) {
	return parseHeaderField(s)
}

// ParseHeaderParams parses a DKIM-style tag-value list (tags separated by
// semicolons).
func ParseHeaderParams(s string) (map[string]string, error) {
	return parseHeaderParams(s)
}

// FormatHeaderParams formats a signature header field with the given name and
// tag-value list. The "b" tag, if present, is folded on its own line.
func FormatHeaderParams(headerFieldName string, params map[string]string) string {
	return formatHeaderParams(headerFieldName, params)
}

// ParseTagList parses a colon-separated tag list.
func ParseTagList(s string) []string {
	return parseTagList(s)
}

// StripWhitespace removes all Unicode whitespace from s.
func StripWhitespace(s string) string {
	return stripWhitespace(s)
}

// DecodeBase64String decodes a base64 string, ignoring whitespace.
func DecodeBase64String(s string) ([]byte, error) {
	return decodeBase64String(s)
}

// CanonicalizeHeader canonicalizes a raw header field.
func CanonicalizeHeader(can Canonicalization, kv string) (string, bool) {
	c, ok := canonicalizers[can]
	if !ok {
		return "", false
	}
	return c.CanonicalizeHeader(kv), true
}

// CanonicalizeBody returns a writer canonicalizing the message body into w.
func CanonicalizeBody(can Canonicalization, w io.Writer) (io.WriteCloser, bool) {
	c, ok := canonicalizers[can]
	if !ok {
		return nil, false
	}
	return c.CanonicalizeBody(w), true
}

// RemoveSignature removes the value of the "b" tag from a signature header
// field.
func RemoveSignature(s string) string {
	return removeSignature(s)
}

// PublicKey is a parsed public key record used to verify signatures.
type PublicKey struct {
	v *queryResult
}

// Verify reports whether sig is a valid signature of hashed.
func (k *PublicKey) Verify(hash crypto.Hash, hashed, sig []byte) error {
	return k.v.Verifier.Verify(hash, hashed, sig)
}

// KeyAlgorithm returns the key algorithm ("rsa" or "ed25519").
func (k *PublicKey) KeyAlgorithm() string {
	return k.v.KeyAlgo
}

// NewPublicKey wraps an RSA or Ed25519 public key. It is useful for
// resolvers that retrieve keys outside of DNS TXT records.
func NewPublicKey(pub crypto.PublicKey) (*PublicKey, error) {
	res := &queryResult{}
	switch pub := pub.(type) {
	case *rsa.PublicKey:
		res.Verifier = rsaVerifier{pub}
		res.KeyAlgo = "rsa"
	case ed25519.PublicKey:
		res.Verifier = ed25519Verifier{pub}
		res.KeyAlgo = "ed25519"
	default:
		return nil, permFailError("unsupported public key type")
	}
	return &PublicKey{v: res}, nil
}

// LookupKey fetches and parses the public key record for domain and selector.
// If lookupTXT is nil, net.LookupTXT is used.
func LookupKey(domain, selector string, lookupTXT func(domain string) ([]string, error)) (*PublicKey, error) {
	res, err := queryDNSTXT(domain, selector, lookupTXT)
	if err != nil {
		return nil, err
	}
	return &PublicKey{v: res}, nil
}
