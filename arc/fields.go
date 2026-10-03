package arc

import (
	"crypto"
	"fmt"
	"strconv"
	"strings"

	"github.com/emersion/go-msgauth/authres"
	"github.com/emersion/go-msgauth/dkim"
)

// SealSet groups the three header fields of one ARC instance.
type SealSet struct {
	// Instance is the instance number (the "i=" tag), starting at 1.
	Instance int

	// AAR is the raw ARC-Authentication-Results header field.
	AAR string
	// AMS is the raw ARC-Message-Signature header field.
	AMS string
	// AS is the raw ARC-Seal header field. It may be empty for instance 1
	// (which MAY omit the "b=" tag), and is always present for higher
	// instances.
	AS string

	// Domain and Selector identify the key used by the ARC-Seal. They are
	// empty when AS is missing or malformed.
	Domain   string
	Selector string
	// ASCV is the chain-validation claim ("cv=") of the ARC-Seal.
	ASCV string
	// ASErr is nil if the ARC-Seal cryptographically verifies.
	ASErr error

	// AMSErr is nil if the ARC-Message-Signature header signature verifies
	// against the header fields present in the current message. For all but
	// the newest instance, only the header signature is checked: the body
	// hash is allowed to differ because a later relay may have modified the
	// body.
	AMSErr error
	// AMSBodyErr is non-nil when the AMS body hash does not verify.
	AMSBodyErr error
	// AMSDomain and AMSSelector identify the key used by the AMS.
	AMSDomain   string
	AMSSelector string

	// AARAuthServ is the authserv-id of the AAR.
	AARAuthServ string
}

type rawField struct {
	index  int
	kind   string // "aar", "ams" or "as"
	raw    string
	params map[string]string
	// instance is the parsed "i=" tag, or 0 when missing/malformed.
	instance int
}

func parseInstance(params map[string]string) (int, error) {
	s, ok := params["i"]
	if !ok {
		return 0, permFailError("missing instance number")
	}
	i, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || i < 1 {
		return 0, permFailError(fmt.Sprintf("invalid instance number %q", s))
	}
	return i, nil
}

// collectedFields holds the raw ARC header fields in message order.
type collectedFields struct {
	aar []*rawField
	ams []*rawField
	as  []*rawField
}

// collectFields scans the message header for ARC fields in message order.
func collectFields(h dkim.Header) collectedFields {
	var out collectedFields

	for idx, kv := range h {
		k, v := dkim.ParseHeaderField(kv)
		var kind string
		switch {
		case strings.EqualFold(k, headerFieldNameAAR):
			kind = "aar"
		case strings.EqualFold(k, headerFieldNameAMS):
			kind = "ams"
		case strings.EqualFold(k, headerFieldNameAS):
			kind = "as"
		default:
			continue
		}

		// AAR uses its own tag syntax ("i=" is followed by an RFC 7601
		// Authentication-Results value); AMS and AS use DKIM tag lists.
		var params map[string]string
		if kind == "aar" {
			params = parseAARTags(v)
		} else {
			params, _ = dkim.ParseHeaderParams(v)
		}

		f := &rawField{index: idx, kind: kind, raw: kv, params: params}
		if i, err := parseInstance(params); err == nil {
			f.instance = i
		}

		switch kind {
		case "aar":
			out.aar = append(out.aar, f)
		case "ams":
			out.ams = append(out.ams, f)
		case "as":
			out.as = append(out.as, f)
		}
	}

	return out
}

// parseAARTags extracts leading "i=<n>;" tags from an AAR value.
func parseAARTags(v string) map[string]string {
	params := make(map[string]string)
	// AAR values may be folded; normalize CRLF+WSP to a space first.
	v = strings.NewReplacer("\r\n ", " ", "\r\n\t", " ").Replace(v)
	// The i= tag is the first tag of the field value, terminated by ";".
	s := v
	if idx := strings.IndexByte(s, ';'); idx >= 0 {
		s = s[:idx]
	}
	tag, val, ok := strings.Cut(s, "=")
	if ok && strings.EqualFold(strings.TrimSpace(tag), "i") {
		params["i"] = strings.TrimSpace(val)
	}
	return params
}

// aarAuthServ extracts the authserv-id from a raw AAR field.
func aarAuthServ(raw string) string {
	_, v := dkim.ParseHeaderField(raw)
	// Strip the leading "i=<n>;" tag(s).
	if idx := strings.IndexByte(v, ';'); idx >= 0 {
		v = v[idx+1:]
	}
	id, _, _ := authres.Parse(v)
	return id
}

func checkHashAlgo(a string) (keyAlgo string, hash crypto.Hash, err error) {
	keyAlgo, hashAlgo, ok := strings.Cut(dkim.StripWhitespace(a), "-")
	if !ok {
		return "", 0, permFailError("malformed algorithm name")
	}
	switch keyAlgo {
	case "rsa", "ed25519":
	default:
		return "", 0, permFailError("unsupported key algorithm: " + keyAlgo)
	}
	if hashAlgo != "sha256" {
		return "", 0, permFailError("unsupported hash algorithm: " + hashAlgo)
	}
	return keyAlgo, crypto.SHA256, nil
}
