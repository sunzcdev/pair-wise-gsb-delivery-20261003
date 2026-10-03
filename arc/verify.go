package arc

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/emersion/go-msgauth/authres"
	"github.com/emersion/go-msgauth/dkim"
)

// ChainStatus describes only the cryptographic and structural chain result.
type ChainStatus string

const (
	// ChainStatusNone means the message contains no ARC set.
	ChainStatusNone ChainStatus = "none"
	// ChainStatusValid means all required sets are structurally and cryptographically valid.
	ChainStatusValid ChainStatus = "valid"
	// ChainStatusInvalid means the chain is malformed or a required signature does not verify.
	ChainStatusInvalid ChainStatus = "invalid"
)

// VerifyOptions configures ARC verification. Trust is intentionally supplied
// separately from DNS and cryptographic verification.
type VerifyOptions struct {
	LookupTXT func(domain string) ([]string, error)
	// IsDomainTrusted reports whether the receiver trusts a sealing domain.
	IsDomainTrusted func(domain string) bool
}

// InstanceVerification reports the independently verified parts of one ARC set.
type InstanceVerification struct {
	Instance int
	Domain   string
	Selector string
	Chain    ChainValue
	// AMSErr is non-nil if the AMS header signature does not verify.
	AMSErr error
	// AMSBodyErr records body mismatch. Only the newest instance's mismatch is chain-invalid.
	AMSBodyErr error
	// ASErr is non-nil if the AS header or chain metadata does not verify.
	ASErr error
	// AARErr is non-nil if AAR cannot be parsed as authentication results.
	AARErr error
	// Trusted is false when no trust callback is provided.
	Trusted bool
}

// Verification contains chain status, per-instance details and the aggregated error.
type Verification struct {
	Status      ChainStatus
	Instances   []*InstanceVerification
	Latest      *InstanceVerification
	LatestChain ChainValue
	Err         error
}

type arcField struct {
	kind     string
	index    int
	field    string
	value    string
	params   map[string]string
	parseErr error
}

// Verify validates the ARC chain in a raw RFC 5322 message. A non-nil result
// can accompany a non-nil error so callers can inspect where the chain broke.
// It never treats cryptographic validity as sufficient to accept the message.
func Verify(r io.Reader, options *VerifyOptions) (*Verification, error) {
	fields, bodyReader, err := dkim.ReadMessageHeader(r)
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(bodyReader)
	if err != nil {
		return nil, err
	}
	if options == nil {
		options = &VerifyOptions{}
	}

	collected, structureErrs := collectARCFields(fields)
	if len(collected.aars) == 0 {
		return &Verification{Status: ChainStatusNone, Err: errors.Join(structureErrs...)}, nil
	}

	last := 0
	for i := range collected.aars {
		if i > last {
			last = i
		}
	}
	if last > maxInstances {
		return nil, fmt.Errorf("arc: instance number %d exceeds maximum %d", last, maxInstances)
	}

	result := &Verification{Status: ChainStatusInvalid}
	verifications := make(map[int]*InstanceVerification)
	var verificationErrs []error

	for i := 1; i <= last; i++ {
		v := &InstanceVerification{Instance: i}
		verifications[i] = v

		aar := collected.aars[i]
		ams := collected.ams[i]
		as := collected.ass[i]
		if aar == nil || ams == nil || as == nil {
			err := fmt.Errorf("arc: instance %d is incomplete", i)
			v.AARErr = missingError(aar, err)
			v.AMSErr = missingError(ams, err)
			v.ASErr = missingError(as, err)
			structureErrs = append(structureErrs, err)
			continue
		}

		v.Domain = as.params["d"]
		v.Selector = as.params["s"]
		v.Chain = ChainValue(as.params["cv"])
		if options.IsDomainTrusted != nil {
			v.Trusted = options.IsDomainTrusted(v.Domain)
		}

		if (i == 1 && v.Chain != ChainNone) || (i > 1 && v.Chain != ChainPass && v.Chain != ChainFail) {
			err := fmt.Errorf("invalid cv value %q for instance %d", v.Chain, i)
			v.ASErr = errors.Join(v.ASErr, err)
			verificationErrs = append(verificationErrs, err)
		}
		if ams.params["d"] != v.Domain || ams.params["s"] != v.Selector {
			err := fmt.Errorf("AMS and AS domain/selector mismatch for instance %d", i)
			v.AMSErr = errors.Join(v.AMSErr, err)
			v.ASErr = errors.Join(v.ASErr, err)
			verificationErrs = append(verificationErrs, err)
		}

		v.AARErr = verifyAAR(aar)
		v.AMSErr, v.AMSBodyErr = verifyAMS(i, fields, body, ams, options)
		v.ASErr = errors.Join(v.ASErr, verifyAS(i, fields, as, options))

		if v.AARErr != nil {
			verificationErrs = append(verificationErrs, fmt.Errorf("instance %d AAR: %w", i, v.AARErr))
		}
		if v.AMSErr != nil {
			verificationErrs = append(verificationErrs, fmt.Errorf("instance %d AMS header: %w", i, v.AMSErr))
		}
		if i == last && v.AMSBodyErr != nil {
			verificationErrs = append(verificationErrs, fmt.Errorf("instance %d AMS body: %w", i, v.AMSBodyErr))
		}
		if v.ASErr != nil {
			verificationErrs = append(verificationErrs, fmt.Errorf("instance %d AS: %w", i, v.ASErr))
		}
	}

	for i := 1; i <= last; i++ {
		result.Instances = append(result.Instances, verifications[i])
	}
	result.Latest = verifications[last]
	result.LatestChain = result.Latest.Chain

	result.Err = errors.Join(append(structureErrs, verificationErrs...)...)
	if result.Err != nil {
		return result, result.Err
	}
	result.Status = ChainStatusValid
	return result, nil
}

type collectedARCFields struct {
	aars map[int]*arcField
	ams  map[int]*arcField
	ass  map[int]*arcField
}

func collectARCFields(fields dkim.Header) (*collectedARCFields, []error) {
	collected := &collectedARCFields{
		aars: map[int]*arcField{},
		ams:  map[int]*arcField{},
		ass:  map[int]*arcField{},
	}
	var errs []error
	seen := map[string]bool{}

	for _, field := range fields {
		name, value := dkim.ParseHeaderField(field)
		if !isARCHeader(name) {
			continue
		}

		kind := strings.ToLower(name)
		flatForInstance, tagValue, rawSignature := flattenARCField(kind, value)
		params, err := dkim.ParseSignatureParams(tagValue)
		if err != nil {
			errs = append(errs, fmt.Errorf("arc: malformed %s tags: %w", kind, err))
			continue
		}

		instance, err := strconv.Atoi(strings.TrimSpace(flatForInstance))
		if err != nil || instance <= 0 {
			errs = append(errs, fmt.Errorf("arc: malformed %s instance tag", kind))
			continue
		}

		key := kind + ":" + strconv.Itoa(instance)
		if seen[key] {
			errs = append(errs, fmt.Errorf("arc: duplicate instance %d in %s", instance, kind))
			continue
		}
		seen[key] = true
		if rawSignature != "" {
			params["b"] = rawSignature
		}

		parsed := &arcField{kind: kind, index: instance, field: field, value: value, params: params}
		switch kind {
		case aarHeaderName:
			collected.aars[instance] = parsed
		case amsHeaderName:
			collected.ams[instance] = parsed
		case asHeaderName:
			collected.ass[instance] = parsed
		}
	}
	return collected, errs
}

func flattenARCField(kind, value string) (instance string, tagValue string, rawSignature string) {
	lines := strings.Split(value, crlf)
	var kept []string
	var signatureParts []string
	skippingB := false
	for _, line := range lines {
		if skippingB {
			if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
				signatureParts = append(signatureParts, strings.TrimSpace(line))
				continue
			}
			skippingB = false
		}
		if idx := indexSignatureTag(line); idx >= 0 && kind != aarHeaderName {
			prefix, suffix, foundSemicolon := strings.Cut(line, ";")
			kept = append(kept, line[:idx+2])
			signatureParts = append(signatureParts, strings.TrimSpace(prefix[idx+2:]))
			if foundSemicolon {
				kept = append(kept, ";"+suffix)
			} else {
				skippingB = true
			}
			continue
		}
		kept = append(kept, line)
	}
	tagValue = strings.NewReplacer("\r\n ", " ", "\r\n\t", " ", "\n ", " ", "\n\t", " ").Replace(strings.Join(kept, crlf))
	if kind == aarHeaderName {
		first, _, _ := strings.Cut(tagValue, ";")
		tagValue = first
	}
	for _, tag := range strings.Split(tagValue, ";") {
		name, raw, ok := strings.Cut(tag, "=")
		if ok && strings.EqualFold(strings.TrimSpace(name), "i") {
			return strings.TrimSpace(raw), tagValue, strings.Join(signatureParts, "")
		}
	}
	return "", tagValue, strings.Join(signatureParts, "")
}

func missingError(field *arcField, err error) error {
	if field == nil {
		return err
	}
	return nil
}

func verifyAAR(aar *arcField) error {
	if _, ok := aar.params["i"]; !ok {
		return errors.New("missing i tag")
	}
	authValue := strings.TrimSpace(removeInstanceTag(aar.value))
	if authValue == "" {
		return errors.New("missing authentication results")
	}
	if _, _, err := authres.Parse(authValue); err != nil {
		return err
	}
	return nil
}

func removeInstanceTag(value string) string {
	parts := strings.Split(value, ";")
	var kept []string
	for _, part := range parts {
		key, _, ok := strings.Cut(part, "=")
		if ok && strings.EqualFold(strings.TrimSpace(key), "i") {
			continue
		}
		kept = append(kept, part)
	}
	return strings.Join(kept, ";")
}

func verifyAMS(instance int, fields dkim.Header, body []byte, ams *arcField, options *VerifyOptions) (headerErr, bodyErr error) {
	params := ams.params
	if err := requireTags(params, "i", "v", "a", "b", "bh", "d", "h", "s"); err != nil {
		return err, nil
	}
	if params["v"] != "1" {
		return errors.New("incompatible version"), nil
	}

	headerCan, bodyCan, err := parseCanonicalizations(params)
	if err != nil {
		return err, nil
	}

	keys := strings.Split(params["h"], ":")
	if countHeaderKeys(keys, "from") < 1 {
		return errors.New("From field not signed"), nil
	}
	if countHeaderKeys(keys, aarHeaderName) < instance {
		return fmt.Errorf("AAR set is not covered for instance %d", instance), nil
	}
	if instance > 1 && countHeaderKeys(keys, amsHeaderName) < instance-1 {
		return fmt.Errorf("previous AMS sets are not covered for instance %d", instance), nil
	}
	if instance > 1 && countHeaderKeys(keys, asHeaderName) < instance-1 {
		return fmt.Errorf("previous AS sets are not covered for instance %d", instance), nil
	}

	expectedBodyHash, err := decodeBase64Tag(params["bh"])
	if err != nil {
		return fmt.Errorf("malformed body hash: %w", err), nil
	}
	actualBodyHash, err := hashBody(bodyCan, body)
	if err != nil {
		return nil, err
	}
	if subtle.ConstantTimeCompare(actualBodyHash, expectedBodyHash) != 1 {
		bodyErr = errors.New("body hash did not verify")
	}

	signature, err := decodeBase64Tag(params["b"])
	if err != nil {
		return fmt.Errorf("malformed signature: %w", err), bodyErr
	}
	filtered := filterFieldsForAMS(fields, instance)
	hashed, err := hashMessageSignature(headerCan, filtered, params["h"], ams.field)
	if err != nil {
		return err, bodyErr
	}
	if err := validateQueryMethod(params["q"]); err != nil {
		return err, bodyErr
	}
	if err := dkim.VerifyHash(params["d"], params["s"], params["a"], hashed, signature, options.LookupTXT); err != nil {
		return fmt.Errorf("signature did not verify: %w", err), bodyErr
	}
	return nil, bodyErr
}

func verifyAS(instance int, fields dkim.Header, as *arcField, options *VerifyOptions) error {
	params := as.params
	if err := requireTags(params, "i", "v", "a", "b", "cv", "d", "h", "s"); err != nil {
		return err
	}
	if params["v"] != "1" {
		return errors.New("incompatible version")
	}

	headerCan, _, err := parseCanonicalizations(params)
	if err != nil {
		return err
	}

	keys := strings.Split(params["h"], ":")
	if countHeaderKeys(keys, aarHeaderName) < instance {
		return fmt.Errorf("AAR set is not covered for instance %d", instance)
	}
	if countHeaderKeys(keys, amsHeaderName) < instance {
		return fmt.Errorf("AMS set is not covered for instance %d", instance)
	}
	if instance > 1 && countHeaderKeys(keys, asHeaderName) < instance-1 {
		return fmt.Errorf("previous AS sets are not covered for instance %d", instance)
	}

	signature, err := decodeBase64Tag(params["b"])
	if err != nil {
		return fmt.Errorf("malformed signature: %w", err)
	}
	filtered := filterFieldsForAS(fields, instance)
	hashed, err := hashSeal(headerCan, filtered, params["h"], as.field)
	if err != nil {
		return err
	}
	if err := validateQueryMethod(params["q"]); err != nil {
		return err
	}
	return dkim.VerifyHash(params["d"], params["s"], params["a"], hashed, signature, options.LookupTXT)
}

func validateQueryMethod(value string) error {
	if value == "" || value == string(dkim.QueryMethodDNSTXT) {
		return nil
	}
	return fmt.Errorf("unsupported public key query method %q", value)
}

func filterFieldsForAMS(fields dkim.Header, instance int) dkim.Header {
	var filtered dkim.Header
	for _, field := range fields {
		name, value := dkim.ParseHeaderField(field)
		if !isARCHeader(name) {
			filtered = append(filtered, field)
			continue
		}
		fieldInstance, err := parseInstance(value)
		if err != nil {
			continue
		}
		if strings.EqualFold(name, asHeaderName) && fieldInstance >= instance {
			continue
		}
		if !strings.EqualFold(name, asHeaderName) && fieldInstance > instance {
			continue
		}
		filtered = append(filtered, field)
	}
	return filtered
}

func filterFieldsForAS(fields dkim.Header, instance int) dkim.Header {
	var filtered dkim.Header
	for _, field := range fields {
		name, value := dkim.ParseHeaderField(field)
		if !strings.EqualFold(name, asHeaderName) {
			filtered = append(filtered, field)
			continue
		}
		fieldInstance, err := parseInstance(value)
		if err != nil || fieldInstance < instance {
			filtered = append(filtered, field)
		}
	}
	return filtered
}

func parseCanonicalizations(params map[string]string) (dkim.Canonicalization, dkim.Canonicalization, error) {
	value, ok := params["c"]
	if !ok {
		return defaultCanonicalization, defaultCanonicalization, nil
	}
	parts := strings.Split(strings.TrimSpace(value), "/")
	if len(parts) == 1 {
		parts = append(parts, string(defaultCanonicalization))
	}
	if len(parts) != 2 {
		return "", "", fmt.Errorf("malformed canonicalization %q", value)
	}
	headerCan := dkim.Canonicalization(strings.TrimSpace(parts[0]))
	bodyCan := dkim.Canonicalization(strings.TrimSpace(parts[1]))
	if !dkim.ValidCanonicalization(headerCan) || !dkim.ValidCanonicalization(bodyCan) {
		return "", "", fmt.Errorf("unsupported canonicalization %q", value)
	}
	return headerCan, bodyCan, nil
}

func requireTags(params map[string]string, tags ...string) error {
	for _, tag := range tags {
		if _, ok := params[tag]; !ok {
			return fmt.Errorf("missing required tag %q", tag)
		}
	}
	return nil
}
