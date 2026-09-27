package dmarc

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// ReportAuthorization is the outcome of the external report destination
// authorization check for a single report URI.
type ReportAuthorization int

const (
	// ReportAuthorized means the destination accepts DMARC reports for the
	// domain, either because it is not external or because it published a
	// valid authorization record.
	ReportAuthorized ReportAuthorization = iota
	// ReportUnauthorized means the destination did not publish a valid
	// authorization record, or the URI is not usable for reporting.
	ReportUnauthorized
	// ReportTempFail means the authorization could not be determined due to
	// a temporary failure (e.g. DNS timeout). The caller may retry later.
	ReportTempFail
)

// ReportDestination describes the authorization check result for a single
// report URI found in the "rua" or "ruf" tags of a Record.
type ReportDestination struct {
	URI     string              // the URI as it appears in the record
	Address string              // extracted report address, without any size limit
	MaxSize int64               // optional size limit in bytes, zero if none
	Status  ReportAuthorization // authorization verdict
	Reason  string              // human-readable explanation of the verdict
}

// VerifyReportDestinations checks every URI in the record's "rua" and "ruf"
// tags against the external report destination rules defined in RFC 7489
// section 7.1. domain is the domain the record was retrieved from.
//
// One ReportDestination is returned per URI, in order. Duplicate
// destinations are only looked up once.
func (r *Record) VerifyReportDestinations(domain string, options *LookupOptions) []ReportDestination {
	uris := make([]string, 0, len(r.ReportURIAggregate)+len(r.ReportURIFailure))
	uris = append(uris, r.ReportURIAggregate...)
	uris = append(uris, r.ReportURIFailure...)
	return VerifyReportURIs(domain, uris, options)
}

// VerifyReportURIs checks each report URI against the external report
// destination rules defined in RFC 7489 section 7.1. domain is the domain
// the DMARC record containing the URIs was retrieved from.
//
// One ReportDestination is returned per URI, in order. Duplicate
// destinations are only looked up once. The library only verifies
// authorization: it never sends reports nor dereferences the URIs.
func VerifyReportURIs(domain string, uris []string, options *LookupOptions) []ReportDestination {
	domain = strings.TrimSuffix(strings.ToLower(domain), ".")
	cache := make(map[string]*ReportDestination)
	results := make([]ReportDestination, len(uris))
	for i, uri := range uris {
		dest := checkReportURI(domain, uri)
		if dest.Status != reportNeedsLookup {
			results[i] = *dest
			continue
		}
		key := reportAuthDomain(dest.Address)
		if cached, ok := cache[key]; ok {
			dest.Status = cached.Status
			dest.Reason = cached.Reason
			results[i] = *dest
			continue
		}
		checkReportAuthorization(dest, options)
		cache[key] = dest
		results[i] = *dest
	}
	return results
}

// reportNeedsLookup is an internal status meaning the destination is
// external and its DNS authorization record still needs to be checked.
const reportNeedsLookup = ReportAuthorization(-1)

// checkReportURI parses a report URI and decides whether a DNS authorization
// lookup is needed.
func checkReportURI(domain, uri string) *ReportDestination {
	dest := &ReportDestination{URI: uri}

	addr, maxSize, err := parseReportURI(uri)
	if err != nil {
		dest.Status = ReportUnauthorized
		dest.Reason = err.Error()
		return dest
	}
	dest.Address = addr
	dest.MaxSize = maxSize

	at := strings.LastIndex(addr, "@")
	if at < 0 {
		dest.Status = ReportUnauthorized
		dest.Reason = "dmarc: report URI is missing a host part"
		return dest
	}
	host := strings.ToLower(addr[at+1:])

	// Destinations within the domain the record was retrieved from (or a
	// subdomain of it) are not external and need no authorization record.
	if host == domain || strings.HasSuffix(host, "."+domain) {
		dest.Status = ReportAuthorized
		dest.Reason = "destination is within the record's own domain"
		return dest
	}

	dest.Status = reportNeedsLookup
	return dest
}

// checkReportAuthorization performs the DNS authorization check for an
// external report destination, as described in RFC 7489 section 7.1.
func checkReportAuthorization(dest *ReportDestination, options *LookupOptions) {
	name := reportAuthDomain(dest.Address)

	lookupTXT := net.LookupTXT
	if options != nil && options.LookupTXT != nil {
		lookupTXT = options.LookupTXT
	}
	txts, err := lookupTXT(name)
	if err != nil {
		if dnsErr, ok := err.(*net.DNSError); ok && dnsErr.IsNotFound {
			dest.Status = ReportUnauthorized
			dest.Reason = "dmarc: no report authorization record at " + name
			return
		}
		dest.Status = ReportTempFail
		dest.Reason = "dmarc: failed to lookup report authorization record: " + err.Error()
		return
	}

	for _, txt := range txts {
		if strings.TrimSpace(txt) == "v=DMARC1" {
			dest.Status = ReportAuthorized
			dest.Reason = "authorized by " + name
			return
		}
	}

	dest.Status = ReportUnauthorized
	dest.Reason = "dmarc: no valid report authorization record at " + name
}

// reportAuthDomain returns the DNS name of the authorization record for a
// report address, i.e. "<local-part>._report._dmarc.<host>".
func reportAuthDomain(addr string) string {
	at := strings.LastIndex(addr, "@")
	local, host := addr[:at], strings.ToLower(addr[at+1:])
	return local + "._report._dmarc." + host
}

// parseReportURI extracts the report address and the optional size limit
// from a report URI, as defined in RFC 7489 sections 6.3 and 6.4. Percent
// escapes in the address are decoded.
func parseReportURI(uri string) (addr string, maxSize int64, err error) {
	if !strings.HasPrefix(uri, "mailto:") {
		return "", 0, fmt.Errorf("dmarc: unsupported report URI scheme in %q", uri)
	}
	s := strings.TrimPrefix(uri, "mailto:")
	if i := strings.IndexByte(s, '?'); i >= 0 {
		s = s[:i]
	}

	// An optional size limit follows the address: "!" 1*DIGIT [unit].
	if i := strings.LastIndexByte(s, '!'); i >= 0 {
		if size, ok := parseReportSize(s[i+1:]); ok {
			maxSize = size
			s = s[:i]
		}
	}

	addr, err = url.PathUnescape(s)
	if err != nil {
		return "", 0, fmt.Errorf("dmarc: malformed report URI %q: %v", uri, err)
	}
	return addr, maxSize, nil
}

// parseReportSize parses a report size limit, i.e. 1*DIGIT followed by an
// optional unit ("k", "m", "g" or "t"). Units are powers of ten.
func parseReportSize(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	var mult int64 = 1
	if unit := s[len(s)-1]; unit < '0' || unit > '9' {
		switch unit {
		case 'k':
			mult = 1e3
		case 'm':
			mult = 1e6
		case 'g':
			mult = 1e9
		case 't':
			mult = 1e12
		default:
			return 0, false
		}
		s = s[:len(s)-1]
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 || n > (1<<63-1)/mult {
		return 0, false
	}
	return n * mult, true
}
