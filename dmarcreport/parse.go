package dmarcreport

import (
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-msgauth/dmarc"
)

// Limits bounds the resources consumed while parsing a report. The zero
// value of each field selects the default from DefaultLimits.
type Limits struct {
	// MaxCompressedBytes bounds the number of bytes read from the
	// (possibly compressed) input stream.
	MaxCompressedBytes int64
	// MaxDecompressedBytes bounds the size of the XML document after
	// decompression.
	MaxDecompressedBytes int64
	// MaxZipMembers bounds the number of members in a zip archive.
	MaxZipMembers int
	// MaxXMLDepth bounds the nesting depth of XML elements.
	MaxXMLDepth int
}

// DefaultLimits returns the limits applied by Parse when the caller does
// not override them.
func DefaultLimits() Limits {
	return Limits{
		MaxCompressedBytes:   10 << 20,
		MaxDecompressedBytes: 100 << 20,
		MaxZipMembers:        1,
		MaxXMLDepth:          32,
	}
}

func (l Limits) withDefaults() Limits {
	d := DefaultLimits()
	if l.MaxCompressedBytes <= 0 {
		l.MaxCompressedBytes = d.MaxCompressedBytes
	}
	if l.MaxDecompressedBytes <= 0 {
		l.MaxDecompressedBytes = d.MaxDecompressedBytes
	}
	if l.MaxZipMembers <= 0 {
		l.MaxZipMembers = d.MaxZipMembers
	}
	if l.MaxXMLDepth <= 0 {
		l.MaxXMLDepth = d.MaxXMLDepth
	}
	return l
}

var (
	errTooManyCompressedBytes   = errors.New("dmarcreport: compressed input exceeds size limit")
	errTooManyDecompressedBytes = errors.New("dmarcreport: decompressed report exceeds size limit")
	errTooManyZipMembers        = errors.New("dmarcreport: zip archive has too many members")
	errNoZipMember              = errors.New("dmarcreport: zip archive contains no report file")
	errXMLTooDeep               = errors.New("dmarcreport: XML document exceeds depth limit")
)

// Parse reads a single DMARC aggregate report from r. The input may be a
// plain XML document, a gzip stream or a zip archive containing a single
// report file; the format is detected from the leading bytes.
//
// handleRecord is invoked for each record as soon as it has been
// decoded, before the rest of the document is read. Records are
// delivered in document order.
//
// Parse returns the report identity, date range and published policy
// once the whole input has been consumed and validated. If the input is
// truncated or otherwise invalid, Parse returns a non-nil error even if
// some records were already delivered: a partially parsed report must
// be discarded in its entirety.
func Parse(r io.Reader, limits *Limits, handleRecord func(*Record) error) (*Report, error) {
	lim := Limits{}.withDefaults()
	if limits != nil {
		lim = limits.withDefaults()
	}
	if handleRecord == nil {
		handleRecord = func(*Record) error { return nil }
	}

	xmlStream, err := openStream(r, lim)
	if err != nil {
		return nil, err
	}

	p := &parser{
		dec:          xml.NewDecoder(xmlStream),
		maxDepth:     lim.MaxXMLDepth,
		handleRecord: handleRecord,
	}
	return p.parse()
}

// openStream detects the input format and returns a reader for the
// decompressed XML document, bounded by the decompressed size limit.
func openStream(r io.Reader, lim Limits) (io.Reader, error) {
	compressed := &limitedReader{r: r, limit: lim.MaxCompressedBytes, err: errTooManyCompressedBytes}
	br := bufio.NewReader(compressed)

	head, err := br.Peek(4)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}

	switch {
	case bytes.HasPrefix(head, []byte{0x1f, 0x8b}):
		gz, err := gzip.NewReader(br)
		if err != nil {
			return nil, fmt.Errorf("dmarcreport: cannot open gzip stream: %w", err)
		}
		return &limitedReader{r: gz, limit: lim.MaxDecompressedBytes, err: errTooManyDecompressedBytes}, nil

	case bytes.HasPrefix(head, []byte("PK\x03\x04")) || bytes.HasPrefix(head, []byte("PK\x05\x06")):
		data, err := readAll(br, lim.MaxCompressedBytes, errTooManyCompressedBytes)
		if err != nil {
			return nil, err
		}
		zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return nil, fmt.Errorf("dmarcreport: cannot open zip archive: %w", err)
		}
		if len(zr.File) > lim.MaxZipMembers {
			return nil, errTooManyZipMembers
		}
		for _, f := range zr.File {
			if f.FileInfo().IsDir() {
				continue
			}
			if f.UncompressedSize64 > uint64(lim.MaxDecompressedBytes) {
				return nil, errTooManyDecompressedBytes
			}
			rc, err := f.Open()
			if err != nil {
				return nil, fmt.Errorf("dmarcreport: cannot open zip member: %w", err)
			}
			return &limitedReader{r: rc, limit: lim.MaxDecompressedBytes, err: errTooManyDecompressedBytes}, nil
		}
		return nil, errNoZipMember

	default:
		// Assume a plain XML document.
		return &limitedReader{r: br, limit: lim.MaxDecompressedBytes, err: errTooManyDecompressedBytes}, nil
	}
}

// limitedReader fails with err once more than limit bytes have been read.
type limitedReader struct {
	r     io.Reader
	limit int64
	n     int64
	err   error
}

func (l *limitedReader) Read(p []byte) (int, error) {
	remaining := l.limit - l.n
	if remaining <= 0 {
		// The limit was reached exactly; only fail if the underlying
		// stream actually has more data.
		var b [1]byte
		n, err := l.r.Read(b[:])
		if n > 0 {
			return 0, l.err
		}
		if err == nil {
			err = io.EOF
		}
		return 0, err
	}
	if int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := l.r.Read(p)
	l.n += int64(n)
	return n, err
}

func readAll(r io.Reader, limit int64, limitErr error) ([]byte, error) {
	var buf bytes.Buffer
	_, err := io.Copy(&buf, &limitedReader{r: r, limit: limit, err: limitErr})
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// parser is a streaming XML state machine for the DMARC aggregate
// report schema.
type parser struct {
	dec          *xml.Decoder
	maxDepth     int
	handleRecord func(*Record) error

	report Report
	path   []string

	record   *Record
	dkim     *DKIMAuthResult
	spf      *SPFAuthResult
	reason   *PolicyOverrideReason
	dateSeen struct{ begin, end bool }
}

func (p *parser) parse() (*Report, error) {
	for {
		tok, err := p.dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("dmarcreport: malformed XML: %w", err)
		}
		switch tok := tok.(type) {
		case xml.StartElement:
			if err := p.start(tok); err != nil {
				return nil, err
			}
		case xml.CharData:
			if err := p.text(string(tok)); err != nil {
				return nil, err
			}
		case xml.EndElement:
			if err := p.end(tok); err != nil {
				return nil, err
			}
		}
	}
	if len(p.path) != 0 {
		return nil, fmt.Errorf("dmarcreport: malformed XML: unclosed element <%s>", p.path[len(p.path)-1])
	}
	if err := p.validate(); err != nil {
		return nil, err
	}
	return &p.report, nil
}

func (p *parser) start(se xml.StartElement) error {
	p.path = append(p.path, se.Name.Local)
	if len(p.path) > p.maxDepth {
		return errXMLTooDeep
	}
	switch p.location() {
	case "feedback/record":
		p.record = &Record{}
	case "feedback/record/auth_results/dkim":
		p.record.AuthResults.DKIM = append(p.record.AuthResults.DKIM, DKIMAuthResult{})
		p.dkim = &p.record.AuthResults.DKIM[len(p.record.AuthResults.DKIM)-1]
	case "feedback/record/auth_results/spf":
		p.record.AuthResults.SPF = append(p.record.AuthResults.SPF, SPFAuthResult{})
		p.spf = &p.record.AuthResults.SPF[len(p.record.AuthResults.SPF)-1]
	case "feedback/record/row/policy_evaluated/reason":
		p.record.PolicyEvaluated.Reasons = append(p.record.PolicyEvaluated.Reasons, PolicyOverrideReason{})
		p.reason = &p.record.PolicyEvaluated.Reasons[len(p.record.PolicyEvaluated.Reasons)-1]
	}
	return nil
}

func (p *parser) end(ee xml.EndElement) error {
	if len(p.path) == 0 || p.path[len(p.path)-1] != ee.Name.Local {
		return fmt.Errorf("dmarcreport: malformed XML: unexpected </%s>", ee.Name.Local)
	}
	loc := p.location()
	p.path = p.path[:len(p.path)-1]
	switch loc {
	case "feedback/record/auth_results/dkim":
		p.dkim = nil
	case "feedback/record/auth_results/spf":
		p.spf = nil
	case "feedback/record/row/policy_evaluated/reason":
		p.reason = nil
	case "feedback/record":
		rec := p.record
		p.record = nil
		if err := validateRecord(rec); err != nil {
			return err
		}
		if err := p.handleRecord(rec); err != nil {
			return err
		}
	}
	return nil
}

func (p *parser) location() string {
	return strings.Join(p.path, "/")
}

func (p *parser) text(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	m := &p.report.Metadata
	pp := &p.report.PolicyPublished

	switch p.location() {
	case "feedback/report_metadata/org_name":
		m.OrgName += s
	case "feedback/report_metadata/email":
		m.Email += s
	case "feedback/report_metadata/extra_contact_info":
		m.ExtraContactInfo += s
	case "feedback/report_metadata/report_id":
		m.ReportID += s
	case "feedback/report_metadata/error":
		m.Errors = append(m.Errors, s)
	case "feedback/report_metadata/date_range/begin":
		t, err := parseUnixTime(s)
		if err != nil {
			return err
		}
		m.DateRange.Begin = t
		p.dateSeen.begin = true
	case "feedback/report_metadata/date_range/end":
		t, err := parseUnixTime(s)
		if err != nil {
			return err
		}
		m.DateRange.End = t
		p.dateSeen.end = true
	case "feedback/policy_published/domain":
		pp.Domain += s
	case "feedback/policy_published/adkim":
		pp.ADKIM = dmarc.AlignmentMode(s)
	case "feedback/policy_published/aspf":
		pp.ASPF = dmarc.AlignmentMode(s)
	case "feedback/policy_published/p":
		pp.Policy = dmarc.Policy(s)
	case "feedback/policy_published/sp":
		pp.SubdomainPolicy = dmarc.Policy(s)
	case "feedback/policy_published/pct":
		n, err := strconv.Atoi(s)
		if err != nil {
			return fmt.Errorf("dmarcreport: invalid policy_published/pct %q", s)
		}
		pp.Percent = n
	case "feedback/policy_published/fo":
		pp.FailureOptions = s
	default:
		if p.record != nil {
			return p.recordText(s)
		}
		// Unknown element: extension content is ignored.
	}
	return nil
}

func (p *parser) recordText(s string) error {
	rec := p.record
	pe := &rec.PolicyEvaluated
	switch strings.TrimPrefix(p.location(), "feedback/record/") {
	case "row/source_ip":
		addr, err := netip.ParseAddr(s)
		if err != nil {
			return fmt.Errorf("dmarcreport: invalid record source_ip %q", s)
		}
		rec.SourceIP = addr
	case "row/count":
		n, err := strconv.Atoi(s)
		if err != nil {
			return fmt.Errorf("dmarcreport: invalid record count %q", s)
		}
		rec.Count = n
	case "row/policy_evaluated/disposition":
		pe.Disposition = dmarc.Policy(s)
	case "row/policy_evaluated/dkim":
		pe.DKIM = EvalResult(s)
	case "row/policy_evaluated/spf":
		pe.SPF = EvalResult(s)
	case "row/policy_evaluated/reason/type":
		p.reason.Type += s
	case "row/policy_evaluated/reason/comment":
		p.reason.Comment += s
	case "identifiers/envelope_to":
		rec.Identifiers.EnvelopeTo += s
	case "identifiers/envelope_from":
		rec.Identifiers.EnvelopeFrom += s
	case "identifiers/header_from":
		rec.Identifiers.HeaderFrom += s
	case "auth_results/dkim/domain":
		p.dkim.Domain += s
	case "auth_results/dkim/selector":
		p.dkim.Selector += s
	case "auth_results/dkim/result":
		p.dkim.Result += s
	case "auth_results/dkim/human_result":
		p.dkim.HumanResult += s
	case "auth_results/spf/domain":
		p.spf.Domain += s
	case "auth_results/spf/scope":
		p.spf.Scope += s
	case "auth_results/spf/result":
		p.spf.Result += s
	}
	return nil
}

// validate checks the identity fields required to make the report
// usable: without them the caller cannot deduplicate or attribute the
// report.
func (p *parser) validate() error {
	m := &p.report.Metadata
	switch {
	case m.OrgName == "":
		return errors.New("dmarcreport: missing report_metadata/org_name")
	case m.Email == "":
		return errors.New("dmarcreport: missing report_metadata/email")
	case m.ReportID == "":
		return errors.New("dmarcreport: missing report_metadata/report_id")
	case !p.dateSeen.begin || !p.dateSeen.end:
		return errors.New("dmarcreport: missing report_metadata/date_range")
	case m.DateRange.End.Before(m.DateRange.Begin):
		return errors.New("dmarcreport: report date range ends before it begins")
	}
	pp := &p.report.PolicyPublished
	switch {
	case pp.Domain == "":
		return errors.New("dmarcreport: missing policy_published/domain")
	case pp.Policy == "":
		return errors.New("dmarcreport: missing policy_published/p")
	}
	return nil
}

func validateRecord(rec *Record) error {
	if !rec.SourceIP.IsValid() {
		return errors.New("dmarcreport: record is missing row/source_ip")
	}
	return nil
}

func parseUnixTime(s string) (time.Time, error) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("dmarcreport: invalid timestamp %q", s)
	}
	return time.Unix(n, 0).UTC(), nil
}
