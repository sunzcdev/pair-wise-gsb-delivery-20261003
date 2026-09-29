// Package report implements streaming parsing of DMARC aggregate reports
// (rua), as specified in RFC 7489 appendix C.
//
// Reports are commonly delivered as e-mail attachments, either as plain XML
// or compressed with gzip or zip. The Reader type transparently handles all
// three formats and yields records one by one, so large reports do not need
// to be held in memory.
//
// Storing reports and deduplicating them by report ID is left to the caller.
package report

import (
	"errors"
	"io"
	"net"
	"time"

	"github.com/emersion/go-msgauth/dmarc"
)

// Errors returned by this package.
var (
	// ErrCompressedTooLarge is returned when the compressed input exceeds
	// Options.MaxCompressedBytes.
	ErrCompressedTooLarge = errors.New("dmarc/report: compressed input exceeds size limit")
	// ErrDecompressedTooLarge is returned when the decompressed report
	// exceeds Options.MaxDecompressedBytes.
	ErrDecompressedTooLarge = errors.New("dmarc/report: decompressed report exceeds size limit")
	// ErrTooManyMembers is returned when a zip archive contains more
	// members than Options.MaxMembers.
	ErrTooManyMembers = errors.New("dmarc/report: archive has too many members")
	// ErrNoReport is returned when a zip archive contains no XML report.
	ErrNoReport = errors.New("dmarc/report: archive contains no XML report")
	// ErrNotAReport is returned when the XML document is not a DMARC
	// aggregate report.
	ErrNotAReport = errors.New("dmarc/report: not a DMARC aggregate report")
	// ErrXMLTooDeep is returned when the XML nesting exceeds
	// Options.MaxDepth.
	ErrXMLTooDeep = errors.New("dmarc/report: XML nesting exceeds depth limit")
)

// Default limits applied when the corresponding Options field is zero.
const (
	DefaultMaxCompressedBytes   int64 = 32 << 20
	DefaultMaxDecompressedBytes int64 = 256 << 20
	DefaultMaxMembers                 = 8
	DefaultMaxDepth                   = 64
)

// Options controls the resource limits applied while parsing a report. The
// zero value of each field selects the corresponding Default* constant.
type Options struct {
	// MaxCompressedBytes bounds the size of the input as read from the
	// underlying reader, before decompression.
	MaxCompressedBytes int64
	// MaxDecompressedBytes bounds the size of the XML document after
	// decompression. For uncompressed input this bounds the document
	// itself.
	MaxDecompressedBytes int64
	// MaxMembers bounds the number of members in a zip archive.
	MaxMembers int
	// MaxDepth bounds the nesting depth of the XML document.
	MaxDepth int
}

func (opts *Options) withDefaults() Options {
	var o Options
	if opts != nil {
		o = *opts
	}
	if o.MaxCompressedBytes <= 0 {
		o.MaxCompressedBytes = DefaultMaxCompressedBytes
	}
	if o.MaxDecompressedBytes <= 0 {
		o.MaxDecompressedBytes = DefaultMaxDecompressedBytes
	}
	if o.MaxMembers <= 0 {
		o.MaxMembers = DefaultMaxMembers
	}
	if o.MaxDepth <= 0 {
		o.MaxDepth = DefaultMaxDepth
	}
	return o
}

// DateRange is the time range covered by a report, as UTC timestamps.
type DateRange struct {
	Begin time.Time
	End   time.Time

	hasBegin, hasEnd bool
}

// ReportMetadata identifies the report generator and the report itself.
type ReportMetadata struct {
	OrgName          string
	Email            string
	ExtraContactInfo string
	ReportID         string
	DateRange        DateRange
	Errors           []string
}

// PolicyPublished is the DMARC policy published by the domain owner at the
// time the report was generated.
type PolicyPublished struct {
	Domain          string
	ADKIM           dmarc.AlignmentMode
	ASPF            dmarc.AlignmentMode
	Policy          dmarc.Policy
	SubdomainPolicy dmarc.Policy
	Percent         int
	FailureOptions  string
}

// EvalResult is the result of a DMARC evaluation: "pass" or "fail".
type EvalResult string

const (
	EvalPass EvalResult = "pass"
	EvalFail EvalResult = "fail"
)

// PolicyOverrideReason explains why the evaluated policy differs from the
// published one.
type PolicyOverrideReason struct {
	Type    string
	Comment string
}

// PolicyEvaluated is the DMARC policy applied to a record, taking local
// policy overrides into account.
type PolicyEvaluated struct {
	Disposition dmarc.Policy
	DKIM        EvalResult
	SPF         EvalResult
	Reasons     []PolicyOverrideReason
}

// Identifiers holds the envelope and header identities extracted from the
// messages summarized by a record.
type Identifiers struct {
	EnvelopeTo   string
	EnvelopeFrom string
	HeaderFrom   string
}

// DKIMAuthResult is a DKIM authentication result for a record.
type DKIMAuthResult struct {
	Domain      string
	Selector    string
	Result      string
	HumanResult string
}

// SPFAuthResult is an SPF authentication result for a record.
type SPFAuthResult struct {
	Domain string
	Scope  string
	Result string
}

// AuthResults holds the authentication results summarized by a record.
type AuthResults struct {
	DKIM []DKIMAuthResult
	SPF  []SPFAuthResult
}

// Record is a single DMARC aggregate report record: a summary of all
// messages sharing the same source, identities and authentication results.
type Record struct {
	SourceIP        net.IP
	Count           int
	PolicyEvaluated PolicyEvaluated
	Identifiers     Identifiers
	AuthResults     AuthResults
}

// Feedback is a fully parsed DMARC aggregate report.
type Feedback struct {
	Metadata        ReportMetadata
	PolicyPublished PolicyPublished
	Records         []Record
}

// Parse reads a whole DMARC aggregate report from r. It accepts plain XML,
// gzip-compressed XML and zip archives containing a single report.
//
// For large reports, prefer Reader, which yields records one by one.
func Parse(r io.Reader, opts *Options) (*Feedback, error) {
	rd, err := NewReader(r, opts)
	if err != nil {
		return nil, err
	}

	f := &Feedback{
		Metadata:        *rd.Metadata(),
		PolicyPublished: *rd.PolicyPublished(),
	}
	for {
		rec, err := rd.Next()
		if err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}
		f.Records = append(f.Records, *rec)
	}
	return f, nil
}
