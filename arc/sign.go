package arc

import (
	"bufio"
	"bytes"
	"crypto"
	"crypto/rsa"
	"encoding/base64"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/emersion/go-msgauth/authres"
	"github.com/emersion/go-msgauth/dkim"
	"golang.org/x/crypto/ed25519"
)

// ChainValidationValue is the chain-validation claim ("cv=") put on a new
// ARC-Seal.
type ChainValidationValue string

const (
	// ChainValidationNone is used by the first seal (instance 1).
	ChainValidationNone ChainValidationValue = "none"
	// ChainValidationPass claims the chain up to (but excluding) the new
	// instance validated successfully.
	ChainValidationPass ChainValidationValue = "pass"
	// ChainValidationFail claims the chain up to the new instance failed.
	ChainValidationFail ChainValidationValue = "fail"
)

// SealOptions configures Seal.
type SealOptions struct {
	// Domain and Selector identify this relay's ARC key record. Mandatory.
	Domain   string
	Selector string
	// Signer is this relay's private key. RSA and Ed25519 keys are
	// supported. Mandatory.
	Signer crypto.Signer

	// AuthenticationServID is the authserv-id of this relay, used in both
	// the AAR and as the SDID-independent service identifier. Mandatory.
	AuthenticationServID string
	// AuthResults are the authentication results recorded by this relay
	// (typically SPF, DKIM and DMARC results for the incoming message). They
	// are serialized into the AAR. May be nil ("none").
	AuthResults []authres.Result

	// Header and body canonicalization algorithms. If empty, "relaxed" is
	// used (RFC 8617 recommends relaxed canonicalization).
	HeaderCanonicalization dkim.Canonicalization
	BodyCanonicalization   dkim.Canonicalization

	// HeaderKeys restricts the header fields covered by the new AMS. If nil,
	// all message header fields are covered (except ARC-Seal and
	// ARC-Message-Signature), with From and every AAR mandatory. If set,
	// "From" MUST be included; the required AAR fields are added
	// automatically.
	HeaderKeys []string

	// CV is the chain-validation claim of the new ARC-Seal. It is usually
	// computed by verifying the incoming chain: pass for a valid chain,
	// fail for an invalid one. For instance 1 it MUST be "none"; if empty,
	// it defaults to "none" for instance 1 and "fail" for higher instances.
	CV ChainValidationValue

	// Resolver is used to verify the incoming chain when CV is empty and
	// the message already contains ARC sets. If nil, DNS is used.
	Resolver KeyResolver
	// LookupTXT is used by the default DNS resolver. Ignored when Resolver
	// is set.
	LookupTXT func(domain string) ([]string, error)
}

// Seal reads a message from r, adds one ARC set (AAR, AMS and AS), and writes
// the sealed message to w.
//
// The instance number is derived from the message: the first seal uses 1,
// subsequent seals increment the highest existing instance number.
func Seal(w io.Writer, r io.Reader, options *SealOptions) error {
	if options == nil {
		return fmt.Errorf("arc: no options specified")
	}
	if options.Domain == "" {
		return fmt.Errorf("arc: no domain specified")
	}
	if options.Selector == "" {
		return fmt.Errorf("arc: no selector specified")
	}
	if options.Signer == nil {
		return fmt.Errorf("arc: no signer specified")
	}
	if options.AuthenticationServID == "" {
		return fmt.Errorf("arc: no authentication service identifier specified")
	}

	keyAlgo := ""
	switch options.Signer.Public().(type) {
	case *rsa.PublicKey:
		keyAlgo = "rsa"
	case ed25519.PublicKey:
		keyAlgo = "ed25519"
	default:
		return fmt.Errorf("arc: unsupported key algorithm %T", options.Signer.Public())
	}

	headerCan := options.HeaderCanonicalization
	if headerCan == "" {
		headerCan = dkim.CanonicalizationRelaxed
	}
	bodyCan := options.BodyCanonicalization
	if bodyCan == "" {
		bodyCan = dkim.CanonicalizationRelaxed
	}
	if _, ok := dkim.CanonicalizeHeader(headerCan, ""); !ok {
		return fmt.Errorf("arc: unknown header canonicalization %q", headerCan)
	}

	// Buffer the whole message so the new fields can be prepended.
	raw, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	br := bufio.NewReader(bytes.NewReader(raw))
	h, err := dkim.ReadHeader(br)
	if err != nil {
		return err
	}
	bodyOffset := len(raw) - br.Buffered()
	body := raw[bodyOffset:]

	collected := collectFields(h)
	if err := checkSealInput(collected); err != nil {
		return err
	}
	instance := 1
	for _, f := range append(append([]*rawField{}, collected.aar...), append(collected.ams, collected.as...)...) {
		if f.instance >= instance {
			instance = f.instance + 1
		}
	}

	cv := options.CV
	if cv == "" {
		if instance == 1 {
			cv = ChainValidationNone
		} else {
			resolver := options.Resolver
			if resolver == nil {
				resolver = defaultResolver(options.LookupTXT)
			}
			verif, _ := verifyMessage(bytes.NewReader(raw), &VerifyOptions{
				Resolver:  resolver,
				LookupTXT: options.LookupTXT,
			})
			if verif != nil && verif.Result == ChainPass {
				cv = ChainValidationPass
			} else {
				cv = ChainValidationFail
			}
		}
	}
	if instance == 1 && cv != ChainValidationNone {
		return fmt.Errorf("arc: instance 1 must use cv=%s", ChainValidationNone)
	}
	if instance > 1 && cv == ChainValidationNone {
		return fmt.Errorf("arc: instances greater than 1 cannot use cv=%s", ChainValidationNone)
	}

	// Build and insert the AAR first: the AMS covers it.
	aarValue := "i=" + strconv.Itoa(instance) + "; " + authres.Format(options.AuthenticationServID, options.AuthResults)
	aarField := headerFieldNameAAR + ": " + aarValue + "\r\n"
	h = insertAAR(h, aarField)

	// Choose the headers signed by the AMS.
	headerKeys, err := selectAMSHeaderKeys(h, instance, options.HeaderKeys)
	if err != nil {
		return err
	}

	// Body hash.
	bodyHash, err := hashBody(body, bodyCan)
	if err != nil {
		return err
	}

	amsParams := map[string]string{
		"a":  keyAlgo + "-sha256",
		"b":  "",
		"bh": base64.StdEncoding.EncodeToString(bodyHash),
		"c":  string(headerCan) + "/" + string(bodyCan),
		"d":  options.Domain,
		"h":  strings.Join(headerKeys, ":"),
		"i":  strconv.Itoa(instance),
		"s":  options.Selector,
	}

	headerHash, err := hashAMSHeaders(h, headerKeys, headerCan, amsParams)
	if err != nil {
		return err
	}
	sig, err := signHash(options.Signer, keyAlgo, headerHash, true)
	if err != nil {
		return err
	}
	amsParams["b"] = base64.StdEncoding.EncodeToString(sig)
	amsField := dkim.FormatHeaderParams(headerFieldNameAMS, amsParams)

	// Build the ARC-Seal.
	asParams := map[string]string{
		"a":  keyAlgo + "-sha256",
		"cv": string(cv),
		"d":  options.Domain,
		"i":  strconv.Itoa(instance),
		"s":  options.Selector,
	}

	var asField string
	if instance == 1 {
		// RFC 8617 section 3.5: the first ARC-Seal (cv=none) MAY omit "b=".
		// There is no "b=" tag, so no trailing CRLF is emitted; add it here.
		asField = dkim.FormatHeaderParams(headerFieldNameAS, asParams) + "\r\n"
	} else {
		asParams["a"] = keyAlgo + "-sha256"
		asParams["b"] = ""
		asField = dkim.FormatHeaderParams(headerFieldNameAS, asParams)

		sealHashInput, err := buildASHashInput(h, instance, cv, headerCan, asField)
		if err != nil {
			return err
		}

		sealSig, err := signHash(options.Signer, keyAlgo, sealHashInput, false)
		if err != nil {
			return err
		}
		asParams["b"] = base64.StdEncoding.EncodeToString(sealSig)
		asField = dkim.FormatHeaderParams(headerFieldNameAS, asParams)
	}

	// RFC 8617 section 5.1: AS(i) and AMS(i) are prepended at the top of
	// the message; AAR(i) was inserted before the existing AARs.
	out := bytes.NewBuffer(nil)
	out.WriteString(asField)
	out.WriteString(amsField)
	for _, kv := range h {
		out.WriteString(kv)
	}
	out.WriteString("\r\n")
	if _, err := w.Write(out.Bytes()); err != nil {
		return err
	}
	_, err = w.Write(body)
	return err
}
