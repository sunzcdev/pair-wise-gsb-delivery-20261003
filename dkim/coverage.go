package dkim

import (
	"fmt"
	"sort"
	"strings"
)

// HeaderPick describes one reference from the signature's "h=" tag list to a
// concrete header instance.
//
// DKIM resolves "h=" entries by counting header instances from the bottom of
// the message: the first reference to a name selects the bottom-most instance,
// the next reference selects the one above it, and so on. Thus merely checking
// that a name appears in "h=" does not tell which of several same-named
// instances are actually signed. HeaderPick captures the actual resolution.
type HeaderPick struct {
	// Key is the header field name as referenced in "h=".
	Key string
	// Index is the index of the signed header instance in the message, or -1
	// if the reference resolved to a nonexistent instance.
	//
	// Such "empty" references are oversigning slots: they contributed nothing
	// to the signature when it was created, but cause verification to fail if a
	// header of that name is inserted afterwards (see RFC 6376 section 3.7).
	Index int
}

// Signed reports whether the reference selected an existing header instance.
func (p HeaderPick) Signed() bool { return p.Index >= 0 }

// HeaderFieldCoverage summarizes how the instances of one header field name
// are covered by a signature.
type HeaderFieldCoverage struct {
	// Key is the canonical (lower-cased) header field name.
	Key string
	// Instances are the indexes of all existing instances of this header in
	// message order. The DKIM-Signature header field is excluded.
	Instances []int
	// Signed are the indexes of the instances that were actually included in
	// the signature, in message order.
	Signed []int
	// Unsigned are the indexes of existing instances that were not referenced
	// by the signature. An attacker may freely add or alter such instances
	// without breaking the signature.
	Unsigned []int
	// Oversigned is the number of "h=" references that resolved to
	// nonexistent instances. Each one is a slot reserved to detect later
	// insertion of a same-named header (oversigning).
	Oversigned int
}

// FullySigned reports whether every existing instance is signed.
func (c HeaderFieldCoverage) FullySigned() bool {
	return len(c.Unsigned) == 0 && len(c.Instances) > 0
}

// HeaderCoverage is the per-header coverage report for one signature. It is
// computed from the header instances the signature actually references, not
// just from the names appearing in the "h=" tag.
type HeaderCoverage struct {
	// Fields holds the coverage for each header name that either exists in the
	// message or is referenced by the signature, keyed by the lower-cased
	// field name.
	Fields map[string]*HeaderFieldCoverage
	// Picks are the resolved "h=" references in "h=" order.
	Picks []HeaderPick
}

func buildHeaderCoverage(h header, headerKeys []string, sigIndex int) *HeaderCoverage {
	coverage := &HeaderCoverage{Fields: make(map[string]*HeaderFieldCoverage)}

	field := func(key string) *HeaderFieldCoverage {
		c := coverage.Fields[key]
		if c == nil {
			c = &HeaderFieldCoverage{Key: key}
			coverage.Fields[key] = c
		}
		return c
	}

	// Enumerate every existing header instance, excluding the signature field
	// itself (it is self-protected by emptying "b=").
	for i, kv := range h {
		if i == sigIndex {
			continue
		}
		k, _ := parseHeaderField(kv)
		key := strings.ToLower(k)
		if key == headerFieldNameLower {
			continue
		}
		field(key).Instances = append(field(key).Instances, i)
	}

	// Resolve the "h=" references with the same bottom-up selection used to
	// compute the signature.
	picker := newHeaderPicker(h)
	for _, key := range headerKeys {
		picker.Pick(key)
	}
	for _, p := range picker.picks {
		if p.key == headerFieldNameLower {
			continue
		}
		coverage.Picks = append(coverage.Picks, HeaderPick{Key: p.key, Index: p.at})

		c := field(p.key)
		if p.at < 0 {
			c.Oversigned++
		} else {
			c.Signed = append(c.Signed, p.at)
		}
	}

	for _, c := range coverage.Fields {
		sort.Ints(c.Signed)
		signed := make(map[int]bool, len(c.Signed))
		for _, i := range c.Signed {
			signed[i] = true
		}
		for _, i := range c.Instances {
			if !signed[i] {
				c.Unsigned = append(c.Unsigned, i)
			}
		}
	}

	return coverage
}

const headerFieldNameLower = "dkim-signature"

// HeaderProblem describes why a required header field does not satisfy an
// application policy.
type HeaderProblem int

const (
	// HeaderMissing means the header field is not present in the message.
	HeaderMissing HeaderProblem = iota
	// HeaderNotSigned means the header is present but none of its instances
	// are signed.
	HeaderNotSigned
	// HeaderPartiallySigned means some but not all of the header's instances
	// are signed.
	HeaderPartiallySigned
	// HeaderNotOversigned means the header is fully signed but the policy
	// requires oversigning slots and none are present.
	HeaderNotOversigned
)

func (p HeaderProblem) String() string {
	switch p {
	case HeaderMissing:
		return "header field is missing"
	case HeaderNotSigned:
		return "header field is not signed"
	case HeaderPartiallySigned:
		return "not all instances of the header field are signed"
	case HeaderNotOversigned:
		return "header field is not protected by oversigning"
	default:
		return "header field does not satisfy policy"
	}
}

// HeaderRequirement is a policy rule for one header field.
type HeaderRequirement struct {
	// RequireOversign requires at least one "h=" reference to a nonexistent
	// instance of this header, so that later appending/prepending a same-named
	// header breaks the signature.
	RequireOversign bool
}

// Policy is an application-level signature policy. It is independent of the
// cryptographic DKIM verification: a signature may verify yet fail the policy
// (for instance when an unsigned duplicate of a required header is present).
type Policy struct {
	// Headers maps a lower-cased header field name to its requirement. Every
	// listed header must be present and have all of its instances signed.
	Headers map[string]HeaderRequirement
}

// HeaderPolicyResult is the policy outcome for a single required header.
type HeaderPolicyResult struct {
	*HeaderFieldCoverage
	// Problem explains why the header does not satisfy the policy; it is nil
	// when the header is compliant.
	Problem *HeaderProblem
}

// PolicyError reports that a cryptographically valid signature does not
// satisfy the application policy.
type PolicyError struct {
	// Headers maps each required (lower-cased) header name to its outcome.
	Headers map[string]*HeaderPolicyResult
}

func (err *PolicyError) Error() string {
	var b strings.Builder
	b.WriteString("dkim: signature does not satisfy header policy:")
	for key, res := range err.Headers {
		if res.Problem == nil {
			continue
		}
		fmt.Fprintf(&b, " %s: %s;", key, res.Problem)
	}
	return strings.TrimSuffix(b.String(), ";")
}

// PolicyResult is the outcome of evaluating a Policy against a signature.
type PolicyResult struct {
	// Headers maps each required (lower-cased) header name to its outcome.
	Headers map[string]*HeaderPolicyResult
	// Err is a *PolicyError when at least one required header does not satisfy
	// the policy, and nil otherwise.
	Err error
}

// Evaluate applies the policy to the coverage report. It does not perform any
// cryptographic verification, so it can be inspected independently of
// [Verification.Err].
func (c *HeaderCoverage) Evaluate(policy *Policy) *PolicyResult {
	result := &PolicyResult{Headers: make(map[string]*HeaderPolicyResult)}

	for name, requirement := range policy.Headers {
		key := strings.ToLower(name)
		fc := c.Fields[key]
		res := &HeaderPolicyResult{HeaderFieldCoverage: fc}

		switch {
		case fc == nil || len(fc.Instances) == 0:
			problem := HeaderMissing
			res.Problem = &problem
		case len(fc.Signed) == 0:
			problem := HeaderNotSigned
			res.Problem = &problem
		case len(fc.Unsigned) > 0:
			problem := HeaderPartiallySigned
			res.Problem = &problem
		case requirement.RequireOversign && fc.Oversigned == 0:
			problem := HeaderNotOversigned
			res.Problem = &problem
		}

		result.Headers[key] = res
	}

	for _, res := range result.Headers {
		if res.Problem != nil {
			result.Err = &PolicyError{Headers: result.Headers}
			break
		}
	}

	return result
}
