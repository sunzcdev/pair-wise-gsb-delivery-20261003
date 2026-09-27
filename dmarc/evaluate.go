package dmarc

import (
	"errors"
	"fmt"
	"hash/fnv"
	"net/mail"
	"strings"
)

// SPFResult is the outcome of SPF authentication (RFC 7208) for a message.
type SPFResult struct {
	// Pass is true if SPF evaluation returned "pass".
	Pass bool
	// Domain is the domain the SPF check was performed on: the envelope-from
	// domain, or the HELO/EHLO domain if the envelope-from is empty.
	Domain string
}

// DKIMResult is the outcome of the verification of a single DKIM signature.
type DKIMResult struct {
	// Pass is true if the signature verified successfully.
	Pass bool
	// Domain is the signing domain, i.e. the "d=" tag of the signature.
	Domain string
}

// PolicyProvider supplies the domain rules needed to evaluate DMARC for a
// message.
type PolicyProvider interface {
	// LookupRecord returns the DMARC record published for domain. It returns
	// ErrNoPolicy if the domain doesn't publish a record, and a temporary
	// failure error (see IsTempFail) if the lookup cannot be completed.
	LookupRecord(domain string) (*Record, error)
	// OrganizationalDomain returns the organizational domain of domain, as
	// defined by the public suffix list (RFC 7489 section 3.2). It returns an
	// error if the organizational domain cannot be determined, e.g. because
	// domain itself is a public suffix.
	OrganizationalDomain(domain string) (string, error)
}

// Sampler decides whether a message is selected when a record's "pct" is
// less than 100. It is called with the From domain and the record's pct
// value, and returns true if the policy applies to this message.
type Sampler func(from string, pct int) bool

// HashSampler is a Sampler which selects messages deterministically based on
// a hash of the From domain. Evaluating the same message twice always yields
// the same selection.
func HashSampler(from string, pct int) bool {
	h := fnv.New32a()
	h.Write([]byte(strings.ToLower(from)))
	return int(h.Sum32()%100) < pct
}

var (
	// ErrNoFrom is returned by Evaluate when the message has no From
	// identity.
	ErrNoFrom = errors.New("dmarc: message has no From identity")
	// ErrMultipleFrom is returned by Evaluate when the message has more than
	// one From identity. RFC 7489 section 6.6.1 requires such messages to be
	// treated as a permanent error, not as a policy failure.
	ErrMultipleFrom = errors.New("dmarc: message has multiple From identities")
	// ErrNoOrganizationalDomain is returned by Evaluate when the
	// organizational domain of the From domain cannot be determined.
	ErrNoOrganizationalDomain = errors.New("dmarc: cannot determine organizational domain")
)

// Evaluation is the outcome of DMARC evaluation for a message, as described
// in RFC 7489 section 6.6.
type Evaluation struct {
	// From is the domain of the message's From identity.
	From string
	// OrganizationalDomain is the organizational domain of From.
	OrganizationalDomain string
	// Record is the DMARC record applied to the message. It is nil if no
	// record was found.
	Record *Record
	// DKIMAligned is true if at least one passing DKIM signature is aligned
	// with the From domain.
	DKIMAligned bool
	// SPFAligned is true if a passing SPF result is aligned with the From
	// domain.
	SPFAligned bool
	// Disposition is the policy the domain owner requests to be applied to
	// the message: PolicyNone, PolicyQuarantine or PolicyReject. It is only
	// a suggestion: applying it (or not) is left to the caller.
	Disposition Policy
	// SampledOut is true if the record's "pct" excluded this message from
	// the published policy.
	SampledOut bool
}

// Pass returns true if the message passes DMARC, i.e. if either DKIM or SPF
// is aligned with the From domain.
func (eval *Evaluation) Pass() bool {
	return eval.DKIMAligned || eval.SPFAligned
}

// Evaluate performs DMARC evaluation of a message, as described in RFC 7489
// section 6.6.1.
//
// from contains the message's From identities, spf and dkim the results of
// SPF and DKIM authentication already performed by the caller. The domain
// rules (DMARC records and organizational domains) are obtained from
// provider. If sampler is nil, HashSampler is used for records with a "pct"
// below 100.
//
// Evaluate only computes the requested disposition; enforcing it (rejecting
// or quarantining the message) is left to the caller.
//
// Malformed messages (no or multiple From identities, undeterminable
// organizational domain) are reported as permanent errors and never as a
// policy failure. Temporary lookup failures are reported as temporary errors
// (see IsTempFail).
func Evaluate(from []string, spf []SPFResult, dkim []DKIMResult, provider PolicyProvider, sampler Sampler) (*Evaluation, error) {
	if len(from) == 0 {
		return nil, ErrNoFrom
	}
	if len(from) > 1 {
		return nil, ErrMultipleFrom
	}
	if sampler == nil {
		sampler = HashSampler
	}

	fromDomain, err := identityDomain(from[0])
	if err != nil {
		return nil, err
	}

	orgDomain, err := provider.OrganizationalDomain(fromDomain)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoOrganizationalDomain, err)
	}
	if orgDomain == "" {
		return nil, ErrNoOrganizationalDomain
	}

	eval := &Evaluation{
		From:                 fromDomain,
		OrganizationalDomain: orgDomain,
		Disposition:          PolicyNone,
	}

	record, err := lookupRecord(provider, fromDomain, orgDomain)
	if err != nil {
		return nil, err
	}
	if record == nil {
		// No DMARC record published: nothing to evaluate against.
		return eval, nil
	}
	eval.Record = record

	for _, res := range dkim {
		if !res.Pass {
			continue
		}
		d := normalizeDomain(res.Domain)
		if d == "" {
			continue
		}
		if identifiersAlign(fromDomain, d, orgDomain, record.DKIMAlignment, provider) {
			eval.DKIMAligned = true
			break
		}
	}

	for _, res := range spf {
		if !res.Pass {
			continue
		}
		d := normalizeDomain(res.Domain)
		if d == "" {
			continue
		}
		if identifiersAlign(fromDomain, d, orgDomain, record.SPFAlignment, provider) {
			eval.SPFAligned = true
			break
		}
	}

	if eval.Pass() {
		return eval, nil
	}

	policy := record.Policy
	if !strings.EqualFold(fromDomain, orgDomain) && record.SubdomainPolicy != "" {
		policy = record.SubdomainPolicy
	}
	if policy == PolicyNone {
		return eval, nil
	}

	if record.Percent != nil && *record.Percent < 100 && !sampler(fromDomain, *record.Percent) {
		eval.SampledOut = true
		return eval, nil
	}

	eval.Disposition = policy
	return eval, nil
}

// lookupRecord fetches the DMARC record for domain, falling back to its
// organizational domain, as described in RFC 7489 section 6.6.3. It returns
// a nil record (and a nil error) if no record is published.
func lookupRecord(provider PolicyProvider, domain, orgDomain string) (*Record, error) {
	record, err := provider.LookupRecord(domain)
	if err == ErrNoPolicy && !strings.EqualFold(domain, orgDomain) {
		record, err = provider.LookupRecord(orgDomain)
	}
	if err == ErrNoPolicy {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return record, nil
}

// identifiersAlign reports whether idDomain is aligned with fromDomain, per
// RFC 7489 section 3.1. fromOrg is the organizational domain of fromDomain.
func identifiersAlign(fromDomain, idDomain, fromOrg string, mode AlignmentMode, provider PolicyProvider) bool {
	if strings.EqualFold(fromDomain, idDomain) {
		return true
	}
	if mode == AlignmentStrict {
		return false
	}
	idOrg, err := provider.OrganizationalDomain(idDomain)
	if err != nil || idOrg == "" {
		return false
	}
	return strings.EqualFold(fromOrg, idOrg)
}

// identityDomain extracts the domain part of a From identity, which may be a
// mailbox ("Alice <alice@example.com>"), an address or a bare domain.
func identityDomain(identity string) (string, error) {
	if addr, err := mail.ParseAddress(identity); err == nil {
		identity = addr.Address
	}
	if i := strings.LastIndexByte(identity, '@'); i >= 0 {
		identity = identity[i+1:]
	}
	identity = normalizeDomain(identity)
	if identity == "" {
		return "", errors.New("dmarc: From identity has no domain")
	}
	return identity, nil
}

func normalizeDomain(domain string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
}
