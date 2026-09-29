package report

import (
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

// parser is a token-level XML decoder which enforces a nesting depth limit.
type parser struct {
	dec      *xml.Decoder
	depth    int
	maxDepth int
}

// next returns the next XML token, tracking the nesting depth.
func (p *parser) next() (xml.Token, error) {
	tok, err := p.dec.Token()
	if err == io.EOF && p.depth > 0 {
		// The document ended before all elements were closed: the
		// report is truncated.
		return nil, io.ErrUnexpectedEOF
	}
	if err != nil {
		return nil, err
	}
	switch tok.(type) {
	case xml.StartElement:
		p.depth++
		if p.depth > p.maxDepth {
			return nil, ErrXMLTooDeep
		}
	case xml.EndElement:
		p.depth--
	}
	return tok, nil
}

// skip consumes the remainder of the current element, whose start element
// has already been consumed.
func (p *parser) skip() error {
	depth := 1
	for depth > 0 {
		tok, err := p.next()
		if err != nil {
			return err
		}
		switch tok.(type) {
		case xml.StartElement:
			depth++
		case xml.EndElement:
			depth--
		}
	}
	return nil
}

// text consumes and returns the character data of the current element,
// whose start element has already been consumed. Nested elements are
// skipped.
func (p *parser) text() (string, error) {
	var sb strings.Builder
	for {
		tok, err := p.next()
		if err != nil {
			return "", err
		}
		switch tok := tok.(type) {
		case xml.CharData:
			sb.Write(tok)
		case xml.StartElement:
			if err := p.skip(); err != nil {
				return "", err
			}
		case xml.EndElement:
			return strings.TrimSpace(sb.String()), nil
		}
	}
}

func (p *parser) parseMetadata(meta *ReportMetadata) error {
	return p.children(func(name string) error {
		switch name {
		case "org_name":
			return p.scanText(&meta.OrgName)
		case "email":
			return p.scanText(&meta.Email)
		case "extra_contact_info":
			return p.scanText(&meta.ExtraContactInfo)
		case "report_id":
			return p.scanText(&meta.ReportID)
		case "date_range":
			return p.parseDateRange(&meta.DateRange)
		case "error":
			var s string
			if err := p.scanText(&s); err != nil {
				return err
			}
			meta.Errors = append(meta.Errors, s)
			return nil
		default:
			return p.skip()
		}
	})
}

func (p *parser) parseDateRange(dr *DateRange) error {
	var begin, end int64
	err := p.children(func(name string) error {
		switch name {
		case "begin":
			dr.hasBegin = true
			return p.scanInt64(&begin)
		case "end":
			dr.hasEnd = true
			return p.scanInt64(&end)
		default:
			return p.skip()
		}
	})
	if err != nil {
		return err
	}
	dr.Begin = time.Unix(begin, 0).UTC()
	dr.End = time.Unix(end, 0).UTC()
	return nil
}

func (p *parser) parsePolicyPublished(pp *PolicyPublished) error {
	return p.children(func(name string) error {
		switch name {
		case "domain":
			return p.scanText(&pp.Domain)
		case "adkim":
			return p.scanText((*string)(&pp.ADKIM))
		case "aspf":
			return p.scanText((*string)(&pp.ASPF))
		case "p":
			return p.scanText((*string)(&pp.Policy))
		case "sp":
			return p.scanText((*string)(&pp.SubdomainPolicy))
		case "pct":
			return p.scanInt(&pp.Percent)
		case "fo":
			return p.scanText(&pp.FailureOptions)
		default:
			return p.skip()
		}
	})
}

func (p *parser) parseRecord(rec *Record) error {
	return p.children(func(name string) error {
		switch name {
		case "row":
			return p.parseRow(rec)
		case "identifiers":
			return p.parseIdentifiers(&rec.Identifiers)
		case "auth_results":
			return p.parseAuthResults(&rec.AuthResults)
		default:
			return p.skip()
		}
	})
}

func (p *parser) parseRow(rec *Record) error {
	return p.children(func(name string) error {
		switch name {
		case "source_ip":
			var s string
			if err := p.scanText(&s); err != nil {
				return err
			}
			ip := net.ParseIP(s)
			if ip == nil {
				return fmt.Errorf("dmarc/report: invalid source IP %q", s)
			}
			rec.SourceIP = ip
			return nil
		case "count":
			return p.scanInt(&rec.Count)
		case "policy_evaluated":
			return p.parsePolicyEvaluated(&rec.PolicyEvaluated)
		default:
			return p.skip()
		}
	})
}

func (p *parser) parsePolicyEvaluated(pe *PolicyEvaluated) error {
	return p.children(func(name string) error {
		switch name {
		case "disposition":
			return p.scanText((*string)(&pe.Disposition))
		case "dkim":
			return p.scanText((*string)(&pe.DKIM))
		case "spf":
			return p.scanText((*string)(&pe.SPF))
		case "reason":
			var reason PolicyOverrideReason
			err := p.children(func(name string) error {
				switch name {
				case "type":
					return p.scanText(&reason.Type)
				case "comment":
					return p.scanText(&reason.Comment)
				default:
					return p.skip()
				}
			})
			if err != nil {
				return err
			}
			pe.Reasons = append(pe.Reasons, reason)
			return nil
		default:
			return p.skip()
		}
	})
}

func (p *parser) parseIdentifiers(id *Identifiers) error {
	return p.children(func(name string) error {
		switch name {
		case "envelope_to":
			return p.scanText(&id.EnvelopeTo)
		case "envelope_from":
			return p.scanText(&id.EnvelopeFrom)
		case "header_from":
			return p.scanText(&id.HeaderFrom)
		default:
			return p.skip()
		}
	})
}

func (p *parser) parseAuthResults(ar *AuthResults) error {
	return p.children(func(name string) error {
		switch name {
		case "dkim":
			var res DKIMAuthResult
			err := p.children(func(name string) error {
				switch name {
				case "domain":
					return p.scanText(&res.Domain)
				case "selector":
					return p.scanText(&res.Selector)
				case "result":
					return p.scanText(&res.Result)
				case "human_result":
					return p.scanText(&res.HumanResult)
				default:
					return p.skip()
				}
			})
			if err != nil {
				return err
			}
			ar.DKIM = append(ar.DKIM, res)
			return nil
		case "spf":
			var res SPFAuthResult
			err := p.children(func(name string) error {
				switch name {
				case "domain":
					return p.scanText(&res.Domain)
				case "scope":
					return p.scanText(&res.Scope)
				case "result":
					return p.scanText(&res.Result)
				default:
					return p.skip()
				}
			})
			if err != nil {
				return err
			}
			ar.SPF = append(ar.SPF, res)
			return nil
		default:
			return p.skip()
		}
	})
}

// children invokes fn for each direct child of the current element, whose
// start element has already been consumed.
func (p *parser) children(fn func(name string) error) error {
	for {
		tok, err := p.next()
		if err != nil {
			return err
		}
		switch tok := tok.(type) {
		case xml.StartElement:
			if err := fn(tok.Name.Local); err != nil {
				return err
			}
		case xml.EndElement:
			return nil
		}
	}
}

func (p *parser) scanText(s *string) error {
	text, err := p.text()
	if err != nil {
		return err
	}
	*s = text
	return nil
}

func (p *parser) scanInt(v *int) error {
	text, err := p.text()
	if err != nil {
		return err
	}
	n, err := strconv.Atoi(text)
	if err != nil {
		return fmt.Errorf("dmarc/report: invalid integer %q", text)
	}
	*v = n
	return nil
}

func (p *parser) scanInt64(v *int64) error {
	text, err := p.text()
	if err != nil {
		return err
	}
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return fmt.Errorf("dmarc/report: invalid integer %q", text)
	}
	*v = n
	return nil
}
