package arc

import (
	"crypto"
	"crypto/subtle"
	"fmt"
	"io"
	"strings"

	"github.com/emersion/go-msgauth/dkim"
)

// verifyAMS validates one ARC-Message-Signature against the current message.
//
// It performs the same checks as a DKIM signature except that the signature
// has no "v=" tag and a mandatory "i=" tag. When checkBody is false (old
// instances), only the header signature is enforced: the body hash is
// returned separately in bodyErr and does not fail chain validation.
func verifyAMS(h dkim.Header, body []byte, f *rawField, checkBody bool, resolver KeyResolver) (headerErr, bodyErr error) {
	if f == nil {
		return permFailError("missing ARC-Message-Signature"), nil
	}
	params := f.params

	for _, tag := range []string{"a", "b", "bh", "d", "h", "s", "i", "c"} {
		if _, ok := params[tag]; !ok {
			return permFailError("AMS missing required tag " + tag), nil
		}
	}
	if _, ok := params["v"]; ok {
		return permFailError("AMS must not contain a v= tag"), nil
	}
	if i, err := parseInstance(params); err != nil || i != f.instance {
		return permFailError("AMS instance number is inconsistent"), nil
	}

	keyAlgo, hash, err := checkHashAlgo(params["a"])
	if err != nil {
		return err, nil
	}

	domain := dkim.StripWhitespace(params["d"])
	selector := dkim.StripWhitespace(params["s"])
	key, err := fetchKey(resolver, domain, selector)
	if err != nil {
		return asPermFail(err), nil
	}
	if key.KeyAlgorithm() != keyAlgo {
		return permFailError("AMS key algorithm mismatch"), nil
	}

	headerCan, bodyCan, err := parseCanonicalization(params["c"])
	if err != nil {
		return err, nil
	}

	bodyHashed, err := dkim.DecodeBase64String(params["bh"])
	if err != nil {
		return permFailError("malformed AMS body hash: " + err.Error()), nil
	}
	sig, err := dkim.DecodeBase64String(params["b"])
	if err != nil {
		return permFailError("malformed AMS signature: " + err.Error()), nil
	}

	// RFC 8617 section 4.1.2: old AMS body hashes are ignored. Still compute
	// it for diagnostics when checkBody is false and the fields are sane.
	actualBodyHash, err := canonicalBodyHash(body, bodyCan)
	if err != nil {
		if checkBody {
			return err, nil
		}
	} else if subtle.ConstantTimeCompare(actualBodyHash, bodyHashed) != 1 {
		if checkBody {
			return failError("AMS body hash did not verify"), nil
		}
		bodyErr = failError("AMS body hash did not verify (stale body, expected for an old instance)")
	}

	headerKeys := dkim.ParseTagList(params["h"])
	if !containsFold(headerKeys, "from") {
		return permFailError("AMS must sign the From field"), nil
	}
	if !containsFold(headerKeys, headerFieldNameAAR) {
		return permFailError("AMS must sign ARC-Authentication-Results"), nil
	}

	dataHash, err := canonicalDataHash(h, headerKeys, headerCan, headerFieldNameAMS, f.raw)
	if err != nil {
		return err, nil
	}

	if err := key.Verify(hash, dataHash, sig); err != nil {
		return failError("AMS signature did not verify: " + err.Error()), bodyErr
	}
	return nil, bodyErr
}

// verifyAS cryptographically validates one ARC-Seal (i >= 2).
func verifyAS(h dkim.Header, c collectedFields, instance int, set *SealSet, resolver KeyResolver) error {
	f := findField(c.as, instance)
	if f == nil {
		return permFailError("missing ARC-Seal")
	}
	params := f.params

	for _, tag := range []string{"a", "b", "cv", "d", "i", "s"} {
		if _, ok := params[tag]; !ok {
			return permFailError("AS missing required tag " + tag)
		}
	}
	if _, ok := params["v"]; ok {
		return permFailError("AS must not contain a v= tag")
	}
	if i, err := parseInstance(params); err != nil || i != instance {
		return permFailError("AS instance number is inconsistent")
	}
	if dkim.StripWhitespace(params["b"]) == "" {
		return permFailError("AS signature is empty")
	}

	keyAlgo, hash, err := checkHashAlgo(params["a"])
	if err != nil {
		return err
	}

	// ARC-Seal has no canonicalization tag: the seal always uses the
	// canonicalization algorithms specified by the AMS of the same instance.
	amsField := findField(c.ams, instance)
	if amsField == nil {
		return permFailError(fmt.Sprintf("AMS for instance %d missing", instance))
	}
	headerCan, _, err := parseCanonicalization(amsField.params["c"])
	if err != nil {
		return err
	}

	domain := dkim.StripWhitespace(params["d"])
	selector := dkim.StripWhitespace(params["s"])
	key, err := fetchKey(resolver, domain, selector)
	if err != nil {
		return asPermFail(err)
	}
	if key.KeyAlgorithm() != keyAlgo {
		return permFailError("AS key algorithm mismatch")
	}

	sig, err := dkim.DecodeBase64String(params["b"])
	if err != nil {
		return permFailError("malformed AS signature: " + err.Error())
	}

	input, err := buildASHashInput(h, instance, ChainValidationValue(set.ASCV), headerCan, f.raw)
	if err != nil {
		return err
	}

	signed := input
	if keyAlgo == "rsa" {
		// RSA signs the SHA-256 hash of the ASB; Ed25519 signs the ASB
		// directly.
		hasher := hash.New()
		hasher.Write(input)
		signed = hasher.Sum(nil)
	}
	if err := key.Verify(asVerifyHash(hash, keyAlgo), signed, sig); err != nil {
		return failError("AS signature did not verify: " + err.Error())
	}
	return nil
}

func asVerifyHash(hash crypto.Hash, keyAlgo string) crypto.Hash {
	if keyAlgo == "ed25519" {
		return crypto.Hash(0)
	}
	return hash
}

// verifyAS1Structure checks the cv=none seal of instance 1. It MAY omit the
// "b=" tag; if present, it is not validated (there is nothing to seal).
func verifyAS1Structure(c collectedFields, set *SealSet) error {
	f := findField(c.as, 1)
	if f == nil {
		return fmt.Errorf("%w: instance 1 is missing %s", ErrMalformedChain, headerFieldNameAS)
	}
	cv := strings.TrimSpace(f.params["cv"])
	if cv != "none" {
		return fmt.Errorf("%w: ARC-Seal (i=1) must have cv=none, got %q", ErrMalformedChain, cv)
	}
	return nil
}

func parseCanonicalization(s string) (dkim.Canonicalization, dkim.Canonicalization, error) {
	headerCan := dkim.CanonicalizationSimple
	bodyCan := dkim.CanonicalizationSimple
	s = dkim.StripWhitespace(s)
	if s == "" {
		return headerCan, bodyCan, nil
	}
	parts := strings.SplitN(s, "/", 2)
	if parts[0] != "" {
		headerCan = dkim.Canonicalization(parts[0])
	}
	if len(parts) > 1 && parts[1] != "" {
		bodyCan = dkim.Canonicalization(parts[1])
	}
	if _, ok := dkim.CanonicalizeHeader(headerCan, ""); !ok {
		return "", "", permFailError("unsupported header canonicalization algorithm")
	}
	if _, ok := dkim.CanonicalizeBody(bodyCan, io.Discard); !ok {
		return "", "", permFailError("unsupported body canonicalization algorithm")
	}
	return headerCan, bodyCan, nil
}

func canonicalBodyHash(body []byte, can dkim.Canonicalization) ([]byte, error) {
	hasher := crypto.SHA256.New()
	wc, ok := dkim.CanonicalizeBody(can, hasher)
	if !ok {
		return nil, permFailError("unsupported body canonicalization algorithm")
	}
	if _, err := wc.Write(body); err != nil {
		return nil, err
	}
	if err := wc.Close(); err != nil {
		return nil, err
	}
	return hasher.Sum(nil), nil
}

// canonicalDataHash computes the hash covered by an AMS: the canonicalized
// headers named in headerKeys followed by the canonicalized signature field
// itself with its "b=" value emptied.
func canonicalDataHash(h dkim.Header, headerKeys []string, can dkim.Canonicalization, fieldName, sigField string) ([]byte, error) {
	hasher := crypto.SHA256.New()
	picker := dkim.NewHeaderPicker(h)
	for _, key := range headerKeys {
		kv := picker.Pick(key)
		if kv == "" {
			continue
		}
		canonical, ok := dkim.CanonicalizeHeader(can, kv)
		if !ok {
			return nil, permFailError("unsupported header canonicalization algorithm")
		}
		if _, err := hasher.Write([]byte(canonical)); err != nil {
			return nil, err
		}
	}

	self := dkim.RemoveSignature(sigField)
	canonical, _ := dkim.CanonicalizeHeader(can, self)
	canonical = strings.TrimRight(canonical, "\r\n")
	if _, err := hasher.Write([]byte(canonical)); err != nil {
		return nil, err
	}
	return hasher.Sum(nil), nil
}

func containsFold(list []string, want string) bool {
	for _, s := range list {
		if strings.EqualFold(s, want) {
			return true
		}
	}
	return false
}
