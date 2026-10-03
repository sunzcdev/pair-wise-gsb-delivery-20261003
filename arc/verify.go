package arc

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"github.com/emersion/go-msgauth/dkim"
)

// VerifyOptions customizes chain verification.
type VerifyOptions struct {
	// Resolver retrieves public keys. If nil, DNS is used.
	Resolver KeyResolver
	// LookupTXT is used by the default DNS resolver.
	LookupTXT func(domain string) ([]string, error)
	// MaxSets limits the number of ARC sets to validate. If zero, there is
	// no limit.
	MaxSets int
}

// Verification is the result of validating an ARC chain.
type Verification struct {
	// Result is the overall chain result. When no ARC sets are present it is
	// ChainNone.
	Result ChainResult
	// Sets lists one entry per instance, ordered by Instance ascending. A
	// failing chain may still contain partial results up to the point of
	// failure.
	Sets []*SealSet
	// Err is nil when Result is ChainPass. The error message indicates the
	// stage at which the chain broke (structural validation, an AMS, an AS,
	// or a "cv=" mismatch).
	Err error
	// instanceCount is the raw number of detected sets.
	instanceCount int
}

// Instances returns the number of ARC instances detected.
func (v *Verification) Instances() int {
	return v.instanceCount
}

// Trusted returns true when the chain is cryptographically valid (Result is
// ChainPass) and the newest seal's domain is in trustedDomains.
//
// This is a convenience implementing the common policy of trusting only the
// seal of the most recent (directly adjacent) relay: its AAR records the
// authentication results that relay observed before it modified the
// message. It does NOT mean the message itself is authentic: callers should
// inspect the newest set's AAR and apply their SPF/DKIM/DMARC policy.
func (v *Verification) Trusted(trustedDomains []string) bool {
	if v == nil || v.Result != ChainPass || len(v.Sets) == 0 {
		return false
	}
	newest := v.Sets[len(v.Sets)-1]
	for _, d := range trustedDomains {
		if strings.EqualFold(d, newest.Domain) {
			return true
		}
	}
	return false
}

// Verify checks the ARC chain of a message.
func Verify(r io.Reader) (*Verification, error) {
	return VerifyWithOptions(r, nil)
}

// VerifyWithOptions behaves like Verify but allows customizing key lookup and
// limits.
func VerifyWithOptions(r io.Reader, options *VerifyOptions) (*Verification, error) {
	return verifyMessage(r, options)
}

func verifyMessage(r io.Reader, options *VerifyOptions) (*Verification, error) {
	bufr := bufio.NewReader(r)
	h, err := dkim.ReadHeader(bufr)
	if err != nil {
		return nil, err
	}

	collected := collectFields(h)
	verif := &Verification{instanceCount: len(collected.aar)}

	if len(collected.aar) == 0 && len(collected.ams) == 0 && len(collected.as) == 0 {
		verif.Result = ChainNone
		return verif, nil
	}

	resolver := resolveOptions(options)

	// Structural validation: every field must carry a valid instance number,
	// exactly one AAR/AMS/AS per instance, instances must start at 1 and be
	// contiguous, and at most one AS with cv=none (instance 1).
	sets, n, err := structureSets(collected, options)
	if err != nil {
		verif.Result = ChainFail
		verif.Sets = sortedSets(sets, n)
		verif.Err = err
		return verif, nil
	}
	verif.instanceCount = n

	// Compute the body hash once (only the newest AMS needs it).
	newest := sets[n]
	body, err := io.ReadAll(bufr)
	if err != nil {
		return nil, err
	}

	// Verify every AMS. For all but the newest instance the body hash is
	// expected to be stale (a later relay may have changed the body); only
	// the header signature is meaningful for chain validation.
	amsHeaderOK := make(map[int]bool, n)
	for i := 1; i <= n; i++ {
		set := sets[i]
		amsErr, bodyErr := verifyAMS(h, body, findField(collected.ams, i), i == n, resolver)
		set.AMSErr = amsErr
		set.AMSBodyErr = bodyErr
		if amsErr == nil {
			amsHeaderOK[i] = true
		}
	}

	if amsErr := newest.AMSErr; amsErr != nil {
		verif.Result = ChainFail
		verif.Sets = sortedSets(sets, n)
		verif.Err = fmt.Errorf("newest ARC-Message-Signature (i=%d) is invalid: %w", n, amsErr)
		return verif, nil
	}
	if newest.AMSBodyErr != nil {
		verif.Result = ChainFail
		verif.Sets = sortedSets(sets, n)
		verif.Err = fmt.Errorf("newest ARC-Message-Signature (i=%d) body hash is invalid: %w", n, newest.AMSBodyErr)
		return verif, nil
	}

	// Walk the seals in instance order. RFC 8617 section 4: the cv= claim of
	// AS(i) must match the validation result of the chain up to i-1.
	if n == 1 {
		// A single set: AS(1) carries cv=none and no prior sets to seal.
		if err := verifyAS1Structure(collected, sets[1]); err != nil {
			verif.Result = ChainFail
			verif.Sets = sortedSets(sets, n)
			verif.Err = err
			return verif, nil
		}
		verif.Result = ChainPass
		verif.Sets = sortedSets(sets, n)
		return verif, nil
	}

	chainOK := amsHeaderOK[1]
	for i := 2; i <= n; i++ {
		set := sets[i]
		asErr := verifyAS(h, collected, i, set, resolver)
		set.ASErr = asErr
		if asErr != nil {
			verif.Result = ChainFail
			verif.Sets = sortedSets(sets, n)
			verif.Err = fmt.Errorf("ARC-Seal (i=%d) signature did not verify: %w", i, asErr)
			return verif, nil
		}

		want := ChainValidationFail
		if chainOK {
			want = ChainValidationPass
		}
		if ChainValidationValue(set.ASCV) != want {
			verif.Result = ChainFail
			verif.Sets = sortedSets(sets, n)
			verif.Err = fmt.Errorf("ARC-Seal (i=%d) cv mismatch: expected %q, got %q", i, want, set.ASCV)
			return verif, nil
		}

		if chainOK {
			chainOK = amsHeaderOK[i]
		}
	}

	verif.Result = chainResult(chainOK)
	verif.Sets = sortedSets(sets, n)
	if !chainOK {
		verif.Err = fmt.Errorf("an ARC-Message-Signature header did not verify")
	}
	return verif, nil
}

func chainResult(ok bool) ChainResult {
	if ok {
		return ChainPass
	}
	return ChainFail
}

func resolveOptions(options *VerifyOptions) KeyResolver {
	if options != nil && options.Resolver != nil {
		return options.Resolver
	}
	var lookupTXT func(domain string) ([]string, error)
	if options != nil {
		lookupTXT = options.LookupTXT
	}
	return defaultResolver(lookupTXT)
}

func sortedSets(sets map[int]*SealSet, n int) []*SealSet {
	out := make([]*SealSet, 0, n)
	for i := 1; i <= n; i++ {
		if sets[i] != nil {
			out = append(out, sets[i])
		}
	}
	return out
}

// structureSets performs the structural validation (RFC 8617 section 4,
// steps 1 and 2) and returns the sets keyed by instance number along with the
// highest instance number.
func structureSets(c collectedFields, options *VerifyOptions) (map[int]*SealSet, int, error) {
	sets := make(map[int]*SealSet)
	maxI := 0

	check := func(fields []*rawField, kind string) error {
		seen := make(map[int]bool)
		for _, f := range fields {
			if f.instance == 0 {
				return fmt.Errorf("%w: %s field with missing or invalid instance number", ErrMalformedChain, kindName(kind))
			}
			if seen[f.instance] {
				return fmt.Errorf("%w: duplicate %s with instance number %d", ErrMalformedChain, kindName(kind), f.instance)
			}
			seen[f.instance] = true
			if f.instance > maxI {
				maxI = f.instance
			}
		}
		return nil
	}
	if err := check(c.aar, "aar"); err != nil {
		return sets, maxI, err
	}
	if err := check(c.ams, "ams"); err != nil {
		return sets, maxI, err
	}
	if err := check(c.as, "as"); err != nil {
		return sets, maxI, err
	}

	if options != nil && options.MaxSets > 0 && maxI > options.MaxSets {
		return sets, maxI, fmt.Errorf("%w: too many ARC sets (%d > %d)", ErrMalformedChain, maxI, options.MaxSets)
	}

	for i := 1; i <= maxI; i++ {
		set := &SealSet{Instance: i}
		if f := findField(c.aar, i); f != nil {
			set.AAR = f.raw
			set.AARAuthServ = aarAuthServ(f.raw)
		}
		if f := findField(c.ams, i); f != nil {
			set.AMS = f.raw
			if d := strings.TrimSpace(f.params["d"]); d != "" {
				set.AMSDomain = d
			}
			if s := strings.TrimSpace(f.params["s"]); s != "" {
				set.AMSSelector = s
			}
		}
		if f := findField(c.as, i); f != nil {
			set.AS = f.raw
			if d := strings.TrimSpace(f.params["d"]); d != "" {
				set.Domain = d
			}
			if s := strings.TrimSpace(f.params["s"]); s != "" {
				set.Selector = s
			}
			set.ASCV = strings.TrimSpace(f.params["cv"])
		}
		sets[i] = set

		missing := []string{}
		if set.AAR == "" {
			missing = append(missing, headerFieldNameAAR)
		}
		if set.AMS == "" {
			missing = append(missing, headerFieldNameAMS)
		}
		if set.AS == "" {
			missing = append(missing, headerFieldNameAS)
		}
		if len(missing) > 0 {
			return sets, maxI, fmt.Errorf("%w: instance %d is missing %s", ErrMalformedChain, i, strings.Join(missing, ", "))
		}
	}

	if maxI < 1 {
		return sets, 0, fmt.Errorf("%w: no valid ARC sets", ErrMalformedChain)
	}

	// Instance 1 must be cv=none; higher instances must not claim cv=none.
	cv1 := sets[1].ASCV
	if cv1 != "none" {
		return sets, maxI, fmt.Errorf("%w: ARC-Seal (i=1) must have cv=none, got %q", ErrMalformedChain, cv1)
	}
	for i := 2; i <= maxI; i++ {
		if sets[i].ASCV == "" {
			return sets, maxI, fmt.Errorf("%w: ARC-Seal (i=%d) is missing cv", ErrMalformedChain, i)
		}
		if sets[i].ASCV == "none" {
			return sets, maxI, fmt.Errorf("%w: ARC-Seal (i=%d) cannot have cv=none", ErrMalformedChain, i)
		}
		if sets[i].ASCV != "pass" && sets[i].ASCV != "fail" {
			return sets, maxI, fmt.Errorf("%w: ARC-Seal (i=%d) has invalid cv %q", ErrMalformedChain, i, sets[i].ASCV)
		}
	}

	return sets, maxI, nil
}

func kindName(kind string) string {
	switch kind {
	case "aar":
		return headerFieldNameAAR
	case "ams":
		return headerFieldNameAMS
	case "as":
		return headerFieldNameAS
	}
	return kind
}

func findField(fields []*rawField, instance int) *rawField {
	for _, f := range fields {
		if f.instance == instance {
			return f
		}
	}
	return nil
}
