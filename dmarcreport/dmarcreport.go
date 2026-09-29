// Package dmarcreport parses DMARC aggregate reports, as defined in
// RFC 7489 appendix C.
//
// Reports are streamed: records are delivered to the caller one by one
// while the document is being decoded, so arbitrarily large reports do
// not need to be held in memory. Plain XML, gzip-compressed XML and zip
// archives containing a single report are accepted.
//
// Whether parsed reports are stored, and deduplication by report ID, is
// left to the caller.
package dmarcreport

import (
	"net/netip"
	"time"

	"github.com/emersion/go-msgauth/dmarc"
)

// Report contains the identity and policy sections of a DMARC aggregate
// report. Individual records are not stored here; they are streamed to
// the caller via Parse.
type Report struct {
	Metadata        Metadata
	PolicyPublished PolicyPublished
}

// Metadata identifies the report and its generator.
type Metadata struct {
	OrgName          string
	Email            string
	ExtraContactInfo string
	ReportID         string
	DateRange        DateRange
	Errors           []string
}

// DateRange is the time range covered by a report, in UTC.
type DateRange struct {
	Begin time.Time
	End   time.Time
}

// PolicyPublished is the DMARC policy the reporting site applied.
type PolicyPublished struct {
	Domain          string
	ADKIM           dmarc.AlignmentMode
	ASPF            dmarc.AlignmentMode
	Policy          dmarc.Policy
	SubdomainPolicy dmarc.Policy
	Percent         int
	FailureOptions  string
}

// Record is a single row of an aggregate report: one group of messages
// sharing a source, a policy evaluation result and authentication
// results.
type Record struct {
	SourceIP        netip.Addr
	Count           int
	PolicyEvaluated PolicyEvaluated
	Identifiers     Identifiers
	AuthResults     AuthResults
}

// EvalResult is the result of a DMARC evaluation step.
type EvalResult string

const (
	EvalResultPass EvalResult = "pass"
	EvalResultFail EvalResult = "fail"
)

// PolicyEvaluated is the DMARC policy evaluation summary for a record.
type PolicyEvaluated struct {
	Disposition dmarc.Policy
	DKIM        EvalResult
	SPF         EvalResult
	Reasons     []PolicyOverrideReason
}

// PolicyOverrideReason explains why the published policy was not
// applied to a record.
type PolicyOverrideReason struct {
	Type    string
	Comment string
}

// Identifiers holds the envelope and header identities of a record.
type Identifiers struct {
	EnvelopeTo   string
	EnvelopeFrom string
	HeaderFrom   string
}

// AuthResults holds the per-record DKIM and SPF authentication results.
type AuthResults struct {
	DKIM []DKIMAuthResult
	SPF  []SPFAuthResult
}

// DKIMAuthResult is a single DKIM authentication result.
type DKIMAuthResult struct {
	Domain      string
	Selector    string
	Result      string
	HumanResult string
}

// SPFAuthResult is a single SPF authentication result.
type SPFAuthResult struct {
	Domain string
	Scope  string
	Result string
}
