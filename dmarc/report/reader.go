package report

import (
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Reader streams records from a DMARC aggregate report. The report metadata
// and the published policy are available right after NewReader returns;
// records are then produced one at a time by Next.
//
// If the input is truncated or otherwise malformed, Next returns a non-EOF
// error and the Reader stays in a failed state: records already delivered
// must be discarded by the caller.
type Reader struct {
	p       *parser
	meta    ReportMetadata
	policy  PolicyPublished
	pending bool // a record start element has been consumed but not parsed
	done    bool
	err     error
}

// NewReader reads a DMARC aggregate report from r. It accepts plain XML,
// gzip-compressed XML and zip archives containing a single report, detected
// from the input magic bytes. The report metadata and published policy are
// parsed and validated before NewReader returns.
func NewReader(r io.Reader, opts *Options) (*Reader, error) {
	o := opts.withDefaults()

	src, err := openXML(r, &o)
	if err != nil {
		return nil, err
	}

	rd := &Reader{p: &parser{dec: xml.NewDecoder(src), maxDepth: o.MaxDepth}}
	if err := rd.readHeader(); err != nil {
		return nil, err
	}
	return rd, nil
}

// Metadata returns the report metadata. It is safe to call as soon as
// NewReader has returned.
func (rd *Reader) Metadata() *ReportMetadata {
	return &rd.meta
}

// PolicyPublished returns the DMARC policy published by the domain owner.
// It is safe to call as soon as NewReader has returned.
func (rd *Reader) PolicyPublished() *PolicyPublished {
	return &rd.policy
}

// Next returns the next record in the report. It returns io.EOF once all
// records have been consumed. Any other error is terminal: the report is
// incomplete or malformed and records already returned must be discarded.
func (rd *Reader) Next() (*Record, error) {
	if rd.err != nil {
		return nil, rd.err
	}
	if rd.done {
		return nil, io.EOF
	}

	rec, err := rd.next()
	if err == io.EOF {
		rd.done = true
	} else if err != nil {
		rd.err = err
	}
	return rec, err
}

// readHeader consumes the XML prolog, the root element, the report metadata
// and the published policy, stopping just before the first record (or at
// the end of the report).
func (rd *Reader) readHeader() error {
	var seenRoot bool
	for {
		tok, err := rd.p.next()
		if err != nil {
			return err
		}

		switch tok := tok.(type) {
		case xml.StartElement:
			if !seenRoot {
				seenRoot = true
				if tok.Name.Local != "feedback" {
					return fmt.Errorf("%w: unexpected root element %q", ErrNotAReport, tok.Name.Local)
				}
				continue
			}
			switch tok.Name.Local {
			case "report_metadata":
				if err := rd.p.parseMetadata(&rd.meta); err != nil {
					return err
				}
			case "policy_published":
				if err := rd.p.parsePolicyPublished(&rd.policy); err != nil {
					return err
				}
			case "record":
				rd.pending = true
				return rd.validate()
			default:
				// Unknown extension element: ignore it.
				if err := rd.p.skip(); err != nil {
					return err
				}
			}
		case xml.EndElement:
			// End of feedback without any record.
			return rd.validate()
		}
	}
}

// validate checks the identity fields required to attribute the report.
func (rd *Reader) validate() error {
	if rd.meta.OrgName == "" {
		return errors.New("dmarc/report: missing organization name")
	}
	if rd.meta.ReportID == "" {
		return errors.New("dmarc/report: missing report ID")
	}
	if !rd.meta.DateRange.hasBegin || !rd.meta.DateRange.hasEnd {
		return errors.New("dmarc/report: missing report date range")
	}
	if rd.meta.DateRange.End.Before(rd.meta.DateRange.Begin) {
		return errors.New("dmarc/report: report date range end precedes begin")
	}
	if rd.policy.Domain == "" {
		return errors.New("dmarc/report: missing published policy domain")
	}
	return nil
}

func (rd *Reader) next() (*Record, error) {
	if rd.pending {
		rd.pending = false
		var rec Record
		if err := rd.p.parseRecord(&rec); err != nil {
			return nil, err
		}
		return &rec, nil
	}

	for {
		tok, err := rd.p.next()
		if err != nil {
			return nil, err
		}

		switch tok := tok.(type) {
		case xml.StartElement:
			if tok.Name.Local == "record" {
				var rec Record
				if err := rd.p.parseRecord(&rec); err != nil {
					return nil, err
				}
				return &rec, nil
			}
			// Unknown extension element: ignore it.
			if err := rd.p.skip(); err != nil {
				return nil, err
			}
		case xml.EndElement:
			// End of the feedback element.
			return nil, io.EOF
		}
	}
}

// limitedReader returns errLimit once more than n bytes have been read from
// the underlying reader.
type limitedReader struct {
	r        io.Reader
	n        int64
	errLimit error
}

func (lr *limitedReader) Read(p []byte) (int, error) {
	for lr.n <= 0 {
		// The limit was reached exactly: only fail if there is
		// actually more data.
		var b [1]byte
		n, err := lr.r.Read(b[:])
		if n > 0 {
			return 0, lr.errLimit
		}
		if err != nil {
			return 0, err
		}
	}
	if int64(len(p)) > lr.n {
		p = p[:lr.n]
	}
	n, err := lr.r.Read(p)
	lr.n -= int64(n)
	return n, err
}

// openXML detects the container format of r and returns a reader for the
// XML document inside it, with the configured size limits applied.
func openXML(r io.Reader, o *Options) (io.Reader, error) {
	// The compressed byte limit applies to everything read from the
	// underlying reader, including format sniffing, so it sits below
	// the buffering layer.
	compressed := &limitedReader{r: r, n: o.MaxCompressedBytes, errLimit: ErrCompressedTooLarge}
	br := bufio.NewReader(compressed)
	head, _ := br.Peek(4)

	decompress := func(r io.Reader) io.Reader {
		return &limitedReader{r: r, n: o.MaxDecompressedBytes, errLimit: ErrDecompressedTooLarge}
	}

	switch {
	case len(head) >= 2 && head[0] == 0x1f && head[1] == 0x8b:
		gz, err := gzip.NewReader(br)
		if err != nil {
			return nil, fmt.Errorf("dmarc/report: invalid gzip stream: %w", err)
		}
		return decompress(gz), nil

	case len(head) >= 4 && (string(head[:4]) == "PK\x03\x04" || string(head[:4]) == "PK\x05\x06"):
		// The zip central directory sits at the end of the archive,
		// so the whole (bounded) archive has to be buffered.
		data, err := io.ReadAll(br)
		if err != nil {
			return nil, err
		}
		zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return nil, fmt.Errorf("dmarc/report: invalid zip archive: %w", err)
		}
		if len(zr.File) > o.MaxMembers {
			return nil, ErrTooManyMembers
		}
		for _, f := range zr.File {
			if !strings.HasSuffix(strings.ToLower(f.Name), ".xml") {
				continue
			}
			m, err := f.Open()
			if err != nil {
				return nil, fmt.Errorf("dmarc/report: cannot open zip member: %w", err)
			}
			return decompress(m), nil
		}
		return nil, ErrNoReport

	default:
		return decompress(br), nil
	}
}
