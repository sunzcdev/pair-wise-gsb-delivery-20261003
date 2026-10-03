package dkim

import (
	"bufio"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"io"

	"golang.org/x/crypto/ed25519"
)

// Header is a raw RFC 5322 header block.
type Header = header

// ReadMessageHeader reads a raw RFC 5322 message header. The returned reader
// contains the message body, including data already buffered while reading.
func ReadMessageHeader(r io.Reader) (Header, io.Reader, error) {
	br := bufio.NewReader(r)
	h, err := readHeader(br)
	if err != nil {
		return nil, nil, err
	}
	return h, io.MultiReader(br, r), nil
}

// ParseHeaderField splits a raw header field into its name and value.
func ParseHeaderField(field string) (string, string) {
	return parseHeaderField(field)
}

// ParseSignatureParams parses a semicolon-delimited DKIM tag list.
func ParseSignatureParams(value string) (map[string]string, error) {
	return parseHeaderParams(value)
}

// FormatSignatureParams formats a semicolon-delimited signature tag list.
func FormatSignatureParams(fieldName string, params map[string]string) string {
	return formatHeaderParams(fieldName, params)
}

// NewHeaderPicker creates a picker which selects duplicate header fields from
// the bottom of the header block, as required by DKIM and ARC.
func NewHeaderPicker(fields Header) HeaderPicker {
	return HeaderPicker{newHeaderPicker(fields)}
}

// HeaderPicker selects duplicate header fields for message signatures.
type HeaderPicker struct {
	picker *headerPicker
}

// Pick selects the next most recently occurring field with the given name.
func (p HeaderPicker) Pick(name string) string {
	return p.picker.Pick(name)
}

// ValidCanonicalization reports whether c is a supported canonicalization.
func ValidCanonicalization(c Canonicalization) bool {
	_, ok := canonicalizers[c]
	return ok
}

// CanonicalizeHeader applies the requested header canonicalization.
func CanonicalizeHeader(c Canonicalization, field string) string {
	return canonicalizers[c].CanonicalizeHeader(field)
}

// CanonicalizeBody returns a body canonicalizer writing to w.
func CanonicalizeBody(c Canonicalization, w io.Writer) (io.WriteCloser, error) {
	canonicalizer, ok := canonicalizers[c]
	if !ok {
		return nil, fmt.Errorf("dkim: unsupported canonicalization %q", c)
	}
	return canonicalizer.CanonicalizeBody(w), nil
}

// AlgorithmForKey returns the DKIM/ARC algorithm identifier for a signer.
func AlgorithmForKey(public crypto.PublicKey) (string, error) {
	switch public.(type) {
	case *rsa.PublicKey:
		return "rsa-sha256", nil
	case ed25519.PublicKey:
		return "ed25519-sha256", nil
	default:
		return "", fmt.Errorf("dkim: unsupported key algorithm %T", public)
	}
}

// SignHash signs hashed using the conventions of DKIM and ARC.
func SignHash(signer crypto.Signer, hash crypto.Hash, hashed []byte) ([]byte, error) {
	algorithm, err := AlgorithmForKey(signer.Public())
	if err != nil {
		return nil, err
	}
	if algorithm == "ed25519-sha256" {
		hash = crypto.Hash(0)
	}
	return signer.Sign(rand.Reader, hashed, hash)
}

// VerifyHash validates a signature against a key published in DNS for
// selector._domainkey.domain.
func VerifyHash(domain, selector, algorithm string, hashed, signature []byte, lookupTXT func(string) ([]string, error)) error {
	keyAlgorithm, hashAlgorithm, ok := splitAlgorithm(algorithm)
	if !ok || hashAlgorithm != "sha256" {
		return permFailError("malformed algorithm name")
	}

	res, err := queryDNSTXT(domain, selector, lookupTXT)
	if err != nil {
		return err
	}
	if res.KeyAlgo != keyAlgorithm {
		return permFailError("inappropriate key algorithm")
	}
	if res.HashAlgos != nil {
		allowed := false
		for _, algo := range res.HashAlgos {
			if algo == hashAlgorithm {
				allowed = true
				break
			}
		}
		if !allowed {
			return permFailError("inappropriate hash algorithm")
		}
	}
	if res.Services != nil {
		allowed := false
		for _, service := range res.Services {
			if service == "email" {
				allowed = true
				break
			}
		}
		if !allowed {
			return permFailError("inappropriate service")
		}
	}
	var hash crypto.Hash
	switch hashAlgorithm {
	case "sha256":
		hash = crypto.SHA256
	default:
		return permFailError("unsupported hash algorithm")
	}
	return res.Verifier.Verify(hash, hashed, signature)
}

func splitAlgorithm(algorithm string) (string, string, bool) {
	switch algorithm {
	case "rsa-sha256":
		return "rsa", "sha256", true
	case "ed25519-sha256":
		return "ed25519", "sha256", true
	default:
		return "", "", false
	}
}
