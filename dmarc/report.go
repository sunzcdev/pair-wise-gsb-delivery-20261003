package dmarc

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
)

// ReportAuthorization is the result of the external report destination
// authorization check (RFC 7489 section 7.1) for a single report URI found
// in a DMARC record's "rua" or "ruf" tag.
type ReportAuthorization struct {
	URI        string // URI as it appeared in the DMARC record
	Address    string // decoded destination address (empty if the URI is invalid)
	Authorized bool   // the destination is authorized to receive reports
	Temporary  bool   // the check failed temporarily and may be retried later
	Reason     string // human-readable explanation of the conclusion
}

var reportURISizeLimit = regexp.MustCompile(`!\d+[kKmMgGtT]?$`)

// parseReportURI extracts the destination address from a DMARC report URI.
// It strips the optional size limit suffix ("!10m") and decodes percent
// escapes (e.g. "mailto:dmarc%40example.com").
func parseReportURI(uri string) (string, error) {
	const scheme = "mailto:"
	if len(uri) <= len(scheme) || !strings.EqualFold(uri[:len(scheme)], scheme) {
		return "", errors.New("dmarc: unsupported report URI scheme (only mailto: is supported)")
	}

	addr := reportURISizeLimit.ReplaceAllString(uri[len(scheme):], "")

	addr, err := url.PathUnescape(addr)
	if err != nil {
		return "", fmt.Errorf("dmarc: invalid URI escaping in report URI: %v", err)
	}

	at := strings.LastIndex(addr, "@")
	if at <= 0 || at == len(addr)-1 {
		return "", errors.New("dmarc: report URI does not contain a valid mail address")
	}
	return addr, nil
}

func normalizeDomain(domain string) string {
	return strings.TrimSuffix(strings.ToLower(domain), ".")
}

// VerifyReportDestinations checks whether each of the given report URIs
// (typically Record.ReportURIAggregate or Record.ReportURIFailure) is
// authorized to receive DMARC reports for recordDomain, the domain the DMARC
// record was retrieved from. It implements the external destination
// verification described in RFC 7489 section 7.1: for each destination
// outside recordDomain, a TXT record starting with "v=DMARC1" must exist at
// "<recordDomain>._report._dmarc.<destination host>".
//
// One ReportAuthorization is returned per input URI, in order. Duplicate
// destinations are only queried once and share their conclusion. A result
// with Temporary set indicates a transient DNS failure; the caller may retry
// the check for that destination later.
//
// The check only performs DNS queries: it never sends mail nor accesses the
// resources the URIs point to.
func VerifyReportDestinations(recordDomain string, uris []string, options *LookupOptions) []ReportAuthorization {
	recordDomain = normalizeDomain(recordDomain)

	results := make([]ReportAuthorization, len(uris))
	checked := make(map[string]*ReportAuthorization) // by lowercased address
	for i, uri := range uris {
		res := &results[i]
		res.URI = uri

		addr, err := parseReportURI(uri)
		if err != nil {
			res.Reason = err.Error()
			continue
		}
		res.Address = addr

		key := strings.ToLower(addr)
		if prev, ok := checked[key]; ok {
			res.Authorized = prev.Authorized
			res.Temporary = prev.Temporary
			res.Reason = prev.Reason
			continue
		}
		checked[key] = res

		checkReportDestination(recordDomain, res, options)
	}
	return results
}

func checkReportDestination(recordDomain string, res *ReportAuthorization, options *LookupOptions) {
	host := normalizeDomain(res.Address[strings.LastIndex(res.Address, "@")+1:])

	if host == recordDomain {
		res.Authorized = true
		res.Reason = "destination is in the same domain as the DMARC record"
		return
	}

	qname := recordDomain + "._report._dmarc." + host

	var txts []string
	var err error
	if options != nil && options.LookupTXT != nil {
		txts, err = options.LookupTXT(qname)
	} else {
		txts, err = net.LookupTXT(qname)
	}
	if err != nil {
		if netErr, ok := err.(net.Error); ok && netErr.Temporary() {
			res.Temporary = true
			res.Reason = "temporary DNS failure querying " + qname + ": " + err.Error()
		} else if dnsErr, ok := err.(*net.DNSError); ok && dnsErr.IsNotFound {
			res.Reason = "no authorization record found at " + qname
		} else {
			res.Reason = "failed to query " + qname + ": " + err.Error()
		}
		return
	}

	for _, txt := range txts {
		if strings.HasPrefix(txt, "v=DMARC1") {
			res.Authorized = true
			res.Reason = "authorized by " + qname
			return
		}
	}
	res.Reason = "no DMARC authorization record found at " + qname
}
