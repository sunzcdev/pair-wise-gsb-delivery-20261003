package dmarc

import (
	"errors"
	"fmt"
	"hash/fnv"
	"strings"
)

// Alignment indicates which mechanism, if any, produced an identifier
// aligned with the message's From domain.
type Alignment int

const (
	AlignmentNone Alignment = iota
	AlignmentSPF
	AlignmentDKIM
)

func (a Alignment) String() string {
	switch a {
	case AlignmentSPF:
		return "spf"
	case AlignmentDKIM:
		return "dkim"
	default:
		return "none"
	}
}

// Disposition is the recommended handling for a message, as advised by the
// domain owner's DMARC policy. Evaluating a message never rejects it: the
// disposition is only a suggestion left to the caller.
type Disposition string

const (
	DispositionNone       Disposition = "none"
	DispositionQuarantine Disposition = "quarantine"
	DispositionReject     Disposition = "reject"
)

// ErrMultipleFrom is returned when a message carries more than one From
// address. RFC 7489 section 6.6.1 recommends rejecting such messages, but
// this condition is reported separately from policy-based rejections.
var ErrMultipleFrom = errors.New("dmarc: message contains multiple From addresses")

// ErrNoOrgDomain is returned when the organizational domain of the From
// domain cannot be determined, e.g. because the From domain is itself a
// public suffix.
var ErrNoOrgDomain = errors.New("dmarc: cannot determine organizational domain")

// SPFResult is the outcome of an SPF check relevant to the message.
type SPFResult struct {
	// Domain is the domain the SPF check was performed for: the MAIL FROM
	// domain, or the HELO domain if MAIL FROM is empty.
	Domain string
	// Pass reports whether the SPF check passed.
	Pass bool
}

// DKIMResult is the outcome of the verification of a single DKIM signature.
type DKIMResult struct {
	// Domain is the value of the signature's d= tag.
	Domain string
	// Valid reports whether the signature verified successfully.
	Valid bool
}

// Message carries the inputs required to evaluate DMARC for a single
// message.
type Message struct {
	// From holds the addresses found in the message's RFC 5322 From header
	// field. Domains without a local part are accepted as well.
	From []string
	// MessageID is the message's Message-ID. It feeds the deterministic
	// default pct sampler, so that the same message always receives the
	// same sampling decision.
	MessageID string
	// SPF is the message's SPF result, or nil if none is available.
	SPF *SPFResult
	// DKIM holds the verification results of the message's DKIM signatures.
	DKIM []DKIMResult
}

// Result is the outcome of a DMARC evaluation.
type Result struct {
	// Alignment indicates which mechanism produced an aligned identifier.
	Alignment Alignment
	// Disposition is the handling advised by the domain owner's policy.
	Disposition Disposition
	// Record is the DMARC record used for the evaluation, or nil if none
	// was published.
	Record *Record
}

// EvaluateOptions allows to customize the default evaluation behavior.
type EvaluateOptions struct {
	// LookupTXT returns the DNS TXT records for the given domain name. If
	// nil, net.LookupTXT is used.
	LookupTXT func(domain string) ([]string, error)
	// OrgDomain returns the organizational domain of the given domain,
	// typically using the Public Suffix List. It must return an error if
	// the organizational domain cannot be determined. If nil, a naive
	// fallback using the last two labels is used; production deployments
	// should provide a Public Suffix List based implementation.
	OrgDomain func(domain string) (string, error)
	// Percentile maps a message to a value in [0, 100) used for pct
	// sampling: the policy applies to the message if the percentile is
	// lower than the record's pct value. If nil, a deterministic FNV-1a
	// hash of the Message-ID is used, so that the same message always
	// receives the same sampling decision.
	Percentile func(messageID string) int
}

// Evaluate performs DMARC evaluation for a message and returns the
// identifier alignment and the advised disposition. It never rejects the
// message itself.
func Evaluate(msg *Message) (*Result, error) {
	return EvaluateWithOptions(msg, nil)
}

// EvaluateWithOptions is like Evaluate but allows to customize the
// evaluation behavior.
func EvaluateWithOptions(msg *Message, options *EvaluateOptions) (*Result, error) {
	if options == nil {
		options = new(EvaluateOptions)
	}

	from, err := fromDomain(msg.From)
	if err != nil {
		return nil, err
	}

	fromOrg, err := orgDomain(options, from)
	if err != nil {
		return nil, err
	}

	rec, subdomain, err := lookupRecord(options, from, fromOrg)
	if err != nil {
		return nil, err
	}

	res := &Result{Record: rec, Disposition: DispositionNone}

	dkimAligned := false
	for _, dkim := range msg.DKIM {
		if !dkim.Valid {
			continue
		}
		mode := AlignmentMode(AlignmentRelaxed)
		if rec != nil {
			mode = rec.DKIMAlignment
		}
		if aligned(options, from, fromOrg, dkim.Domain, mode) {
			dkimAligned = true
			break
		}
	}

	spfAligned := false
	if msg.SPF != nil && msg.SPF.Pass {
		mode := AlignmentMode(AlignmentRelaxed)
		if rec != nil {
			mode = rec.SPFAlignment
		}
		spfAligned = aligned(options, from, fromOrg, msg.SPF.Domain, mode)
	}

	switch {
	case dkimAligned:
		res.Alignment = AlignmentDKIM
	case spfAligned:
		res.Alignment = AlignmentSPF
	}

	if rec == nil || res.Alignment != AlignmentNone {
		return res, nil
	}

	policy := rec.Policy
	if subdomain && rec.SubdomainPolicy != "" {
		policy = rec.SubdomainPolicy
	}

	pct := 100
	if rec.Percent != nil {
		pct = *rec.Percent
	}
	if pct < 100 && !sampled(options, msg.MessageID, pct) {
		policy = downgradePolicy(policy)
	}

	res.Disposition = Disposition(policy)
	return res, nil
}

func fromDomain(addrs []string) (string, error) {
	if len(addrs) == 0 {
		return "", errors.New("dmarc: message has no From address")
	}
	if len(addrs) > 1 {
		return "", ErrMultipleFrom
	}
	addr := addrs[0]
	if i := strings.LastIndex(addr, "@"); i >= 0 {
		addr = addr[i+1:]
	}
	domain := normalizeDomain(addr)
	if domain == "" {
		return "", errors.New("dmarc: empty From domain")
	}
	return domain, nil
}

func normalizeDomain(domain string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
}

func orgDomain(options *EvaluateOptions, domain string) (string, error) {
	if options.OrgDomain != nil {
		org, err := options.OrgDomain(domain)
		if err != nil {
			return "", fmt.Errorf("%w: %v", ErrNoOrgDomain, err)
		}
		org = normalizeDomain(org)
		if org == "" || !strings.HasSuffix(domain, org) {
			return "", fmt.Errorf("%w: %v", ErrNoOrgDomain, domain)
		}
		return org, nil
	}

	// Naive fallback: the organizational domain is assumed to be made of
	// the last two labels. This is incorrect for domains under multi-label
	// public suffixes such as co.uk.
	labels := strings.Split(domain, ".")
	if len(labels) < 2 {
		return "", fmt.Errorf("%w: %v", ErrNoOrgDomain, domain)
	}
	return labels[len(labels)-2] + "." + labels[len(labels)-1], nil
}

// lookupRecord fetches the DMARC record applicable to the From domain. If
// the From domain publishes no record and differs from its organizational
// domain, the organizational domain's record is used instead and subdomain
// is set to true, indicating that the record's sp policy applies.
func lookupRecord(options *EvaluateOptions, from, fromOrg string) (rec *Record, subdomain bool, err error) {
	lookupOptions := &LookupOptions{LookupTXT: options.LookupTXT}

	rec, err = LookupWithOptions(from, lookupOptions)
	if err == nil {
		return rec, false, nil
	}
	if IsTempFail(err) || !errors.Is(err, ErrNoPolicy) {
		return nil, false, err
	}
	if from == fromOrg {
		return nil, false, nil
	}

	rec, err = LookupWithOptions(fromOrg, lookupOptions)
	if err == nil {
		return rec, true, nil
	}
	if IsTempFail(err) || !errors.Is(err, ErrNoPolicy) {
		return nil, false, err
	}
	return nil, false, nil
}

// aligned reports whether the identifier domain aligns with the From domain
// under the given alignment mode.
func aligned(options *EvaluateOptions, from, fromOrg, identifier string, mode AlignmentMode) bool {
	identifier = normalizeDomain(identifier)
	if identifier == "" {
		return false
	}
	if identifier == from {
		return true
	}
	if mode == AlignmentStrict {
		return false
	}
	idOrg, err := orgDomain(options, identifier)
	return err == nil && idOrg == fromOrg
}

func sampled(options *EvaluateOptions, messageID string, pct int) bool {
	percentile := defaultPercentile
	if options.Percentile != nil {
		percentile = options.Percentile
	}
	return percentile(messageID) < pct
}

func defaultPercentile(messageID string) int {
	h := fnv.New32a()
	h.Write([]byte(messageID))
	return int(h.Sum32() % 100)
}

// downgradePolicy lowers a policy by one notch when a message is exempted
// by pct sampling, as described in RFC 7489 section 6.6.4.
func downgradePolicy(policy Policy) Policy {
	switch policy {
	case PolicyReject:
		return PolicyQuarantine
	default:
		return PolicyNone
	}
}
