package arc

import (
	"crypto"
	"crypto/rand"
	"fmt"
	"io"
	"strings"

	"github.com/emersion/go-msgauth/dkim"
)

// insertAAR inserts a new AAR field. AARs are ordered newest-first: the new
// field is placed immediately before the first existing AAR (the currently
// newest one). For the first instance it is prepended to the header.
func insertAAR(h dkim.Header, aarField string) dkim.Header {
	for i, kv := range h {
		k, _ := dkim.ParseHeaderField(kv)
		if strings.EqualFold(k, headerFieldNameAAR) {
			out := make(dkim.Header, 0, len(h)+1)
			out = append(out, h[:i]...)
			out = append(out, aarField)
			out = append(out, h[i:]...)
			return out
		}
	}
	out := make(dkim.Header, 0, len(h)+1)
	out = append(out, aarField)
	out = append(out, h...)
	return out
}

// selectAMSHeaderKeys returns the "h=" list for a new AMS. The AMS MUST cover
// From and every AAR: instance i lists arc-authentication-results i times.
// ARC-Seal and ARC-Message-Signature are never covered by the AMS.
func selectAMSHeaderKeys(h dkim.Header, instance int, userKeys []string) ([]string, error) {
	if userKeys != nil {
		keys := make([]string, 0, len(userKeys)+instance)
		hasFrom := false
		for _, k := range userKeys {
			if strings.EqualFold(k, "from") {
				hasFrom = true
			}
			keys = append(keys, k)
		}
		if !hasFrom {
			return nil, fmt.Errorf("arc: the From header field must be signed")
		}
		for j := 0; j < instance; j++ {
			keys = append(keys, headerFieldNameAAR)
		}
		return keys, nil
	}

	var keys []string
	hasFrom := false
	for _, kv := range h {
		k, _ := dkim.ParseHeaderField(kv)
		if strings.EqualFold(k, headerFieldNameAS) || strings.EqualFold(k, headerFieldNameAMS) ||
			strings.EqualFold(k, headerFieldNameAAR) {
			continue
		}
		if strings.EqualFold(k, "from") {
			hasFrom = true
		}
		keys = append(keys, k)
	}
	if !hasFrom {
		return nil, fmt.Errorf("arc: the From header field must be signed")
	}
	// The AARs follow the regular fields in the h= list, ordered newest to
	// oldest (instance i down to 1).
	for c := 0; c < instance; c++ {
		keys = append(keys, headerFieldNameAAR)
	}
	return keys, nil
}

func hashBody(body []byte, can dkim.Canonicalization) ([]byte, error) {
	hasher := crypto.SHA256.New()
	wc, ok := dkim.CanonicalizeBody(can, hasher)
	if !ok {
		return nil, fmt.Errorf("arc: unknown body canonicalization %q", can)
	}
	if _, err := wc.Write(body); err != nil {
		return nil, err
	}
	if err := wc.Close(); err != nil {
		return nil, err
	}
	return hasher.Sum(nil), nil
}

// hashAMSHeaders computes the data hash of an AMS over the given headers,
// using the tag list in params (whose "b" tag is empty).
func hashAMSHeaders(h dkim.Header, headerKeys []string, can dkim.Canonicalization, params map[string]string) ([]byte, error) {
	hasher := crypto.SHA256.New()
	picker := dkim.NewHeaderPicker(h)
	for _, key := range headerKeys {
		kv := picker.Pick(key)
		if kv == "" {
			continue
		}
		canonical, ok := dkim.CanonicalizeHeader(can, kv)
		if !ok {
			return nil, fmt.Errorf("arc: unknown header canonicalization %q", can)
		}
		if _, err := io.WriteString(hasher, canonical); err != nil {
			return nil, err
		}
	}

	self := dkim.FormatHeaderParams(headerFieldNameAMS, params)
	canonical, _ := dkim.CanonicalizeHeader(can, self)
	canonical = strings.TrimRight(canonical, "\r\n")
	if _, err := io.WriteString(hasher, canonical); err != nil {
		return nil, err
	}
	return hasher.Sum(nil), nil
}

// buildASHashInput constructs the input signed by an ARC-Seal (RFC 8617
// section 3.5): the "cv=" value, immediately followed by the canonicalized
// previous ARC-Seal and ARC-Message-Signature fields in message order, then
// the canonicalized ARC-Seal being created with its "b=" value removed.
func buildASHashInput(h dkim.Header, instance int, cv ChainValidationValue, can dkim.Canonicalization, selfField string) ([]byte, error) {
	var b strings.Builder
	b.WriteString("cv=" + string(cv))

	for _, kv := range h {
		k, v := dkim.ParseHeaderField(kv)
		if !strings.EqualFold(k, headerFieldNameAS) && !strings.EqualFold(k, headerFieldNameAMS) {
			continue
		}
		params, err := dkim.ParseHeaderParams(v)
		if err != nil {
			return nil, permFailError("malformed ARC tag list: " + err.Error())
		}
		i, err := parseInstance(params)
		if err != nil {
			return nil, permFailError("ARC field missing instance number")
		}
		if i >= instance {
			continue
		}
		canonical, ok := dkim.CanonicalizeHeader(can, kv)
		if !ok {
			return nil, fmt.Errorf("arc: unknown header canonicalization %q", can)
		}
		b.WriteString(canonical)
	}

	self := dkim.RemoveSignature(selfField)
	canonical, _ := dkim.CanonicalizeHeader(can, self)
	canonical = strings.TrimRight(canonical, "\r\n")
	b.WriteString(canonical)

	return []byte(b.String()), nil
}

// signASB signs the input of an AMS or an AS. For AMS the input is already a
// SHA-256 digest; for an AS it is the raw ASB: RSA therefore hashes it here,
// while Ed25519 signs it directly.
func signHash(signer crypto.Signer, keyAlgo string, input []byte, alreadyHashed bool) ([]byte, error) {
	hash := crypto.SHA256
	signed := input
	if keyAlgo == "ed25519" {
		hash = crypto.Hash(0)
	} else if !alreadyHashed {
		hasher := hash.New()
		hasher.Write(input)
		signed = hasher.Sum(nil)
	}
	sig, err := signer.Sign(rand.Reader, signed, hash)
	if err != nil {
		return nil, err
	}
	return sig, nil
}
