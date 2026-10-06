// Copyright 2026 Matthew Holt
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package acmez

import (
	"errors"
	"strings"
)

// CAARecord is a parsed CAA resource record (RFC 8659).
type CAARecord struct {
	// Flag is the single CAA flag byte. Only its most significant bit,
	// the critical flag (bit 0, value 0x80), is defined.
	Flag uint8

	// Tag is the lowercase property tag, for example "issue", "issuewild"
	// or "iodef".
	Tag string

	// IssuerDomain is the CA domain named by an "issue"/"issuewild"
	// property, in lowercase ASCII without a trailing dot. It is the
	// empty string for an explicit deny-all value ("issue" ";" or
	// "issue" ";") and for non-issuer tags.
	IssuerDomain string

	// Parameters holds the optional "tag = value" parameters parsed from
	// an "issue"/"issuewild" value, such as "accounturi" and
	// "validationmethods". Parameter names are lowercased.
	Parameters map[string]string
}

// Critical reports whether the record's critical flag is set.
func (r CAARecord) Critical() bool { return r.Flag&0x80 != 0 }

// knownCAATags are the standard CAA property tags defined by RFC 8659.
// A record with the critical flag set and a tag outside this set must be
// treated as forbidding issuance (RFC 8659 §4.7).
var knownCAATags = map[string]bool{
	"issue":     true,
	"issuewild": true,
	"issuemail": true,
	"iodef":     true,
}

// parseCAARecord parses the value portion of a CAA record into a CAARecord.
//
// The value syntax is (RFC 8659 §4.2):
//
//	issuevalue-domain = [wksp] domainname [wksp]
//	issuevalue-params = *(parameter [wksp])
//	parameter = [wksp] ";" [wksp] tag [wksp] "=" [wksp] value [wksp]
//
// Quoted string parameter values follow RFC 1035 §5.1; in particular a
// semicolon inside quotes does not begin a new parameter.
func parseCAARecord(flag uint8, tag, value string) (CAARecord, error) {
	rec := CAARecord{Flag: flag, Tag: strings.ToLower(strings.TrimSpace(tag))}
	if rec.Tag == "" {
		return rec, errors.New("empty CAA tag")
	}

	segments, err := splitCAASemicolons(value)
	if err != nil {
		return rec, err
	}

	// The first segment is the (possibly empty) issuer domain; remaining
	// segments are parameters.
	rec.IssuerDomain = normalizeIssuerDomain(segments[0])
	for _, raw := range segments[1:] {
		if strings.TrimSpace(raw) == "" {
			// A trailing or empty parameter (as in the deny-all value
			// "issue" ";") carries no tag and must be ignored.
			continue
		}
		name, val, ok := parseCAAParameter(raw)
		if !ok {
			return rec, errors.New("malformed CAA parameter: " + strings.TrimSpace(raw))
		}
		if rec.Parameters == nil {
			rec.Parameters = make(map[string]string)
		}
		rec.Parameters[name] = val
	}

	return rec, nil
}

// splitCAASemicolons splits a CAA value at unquoted, unescaped semicolons.
// It preserves quoted content verbatim so the leading issuer domain and each
// parameter can be parsed separately.
func splitCAASemicolons(value string) ([]string, error) {
	var segments []string
	var current strings.Builder
	inQuotes := false

	flush := func() {
		segments = append(segments, current.String())
		current.Reset()
	}

	for i := 0; i < len(value); i++ {
		c := value[i]
		switch {
		case c == '\\' && inQuotes:
			// Inside a quoted value a backslash escapes the next byte;
			// copy the pair verbatim so a quote (or semicolon) after the
			// backslash does not alter quote tracking. The escape itself
			// is interpreted later by decodeCAAQuotedString.
			current.WriteByte(c)
			if i+1 < len(value) {
				i++
				current.WriteByte(value[i])
			}
		case c == '\\':
			// Outside quotes a backslash has no special meaning here.
			current.WriteByte(c)
		case c == '"':
			inQuotes = !inQuotes
			current.WriteByte(c)
		case c == ';' && !inQuotes:
			flush()
		default:
			current.WriteByte(c)
		}
	}
	if inQuotes {
		return nil, errors.New("unterminated quoted string in CAA value")
	}
	flush()

	return segments, nil
}

// parseCAAParameter parses a single "tag = value" segment (without the
// leading semicolon) into a lowercased parameter name and its decoded value.
func parseCAAParameter(segment string) (string, string, bool) {
	eq := strings.IndexByte(segment, '=')
	if eq < 0 {
		return "", "", false
	}
	name := strings.ToLower(strings.TrimSpace(segment[:eq]))
	if name == "" {
		return "", "", false
	}
	raw := strings.TrimSpace(segment[eq+1:])
	if raw == "" {
		return "", "", false
	}
	if raw[0] == '"' {
		decoded, err := decodeCAAQuotedString(raw)
		if err != nil {
			return "", "", false
		}
		return name, decoded, true
	}
	return name, raw, true
}

// decodeCAAQuotedString decodes an RFC 1035 §5.1 quoted string. The input
// must begin with a quote; backslash escapes are interpreted, including the
// three-digit form "\DDD". Although RFC 1035 describes DDD as decimal, the
// dominant implementations (e.g. BIND, miekg/dns, Boulder) treat it as
// octal, as do CAA records published in practice, so octal is used here.
func decodeCAAQuotedString(raw string) (string, error) {
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return "", errors.New("malformed quoted string")
	}
	body := raw[1 : len(raw)-1]

	var out strings.Builder
	for i := 0; i < len(body); i++ {
		c := body[i]
		if c != '\\' {
			out.WriteByte(c)
			continue
		}
		if i+1 >= len(body) {
			return "", errors.New("dangling escape in quoted string")
		}
		// An escaped backslash ("\\") represents a literal backslash; only
		// "\DDD" introduces a three-digit decimal escape.
		if body[i+1] == '\\' {
			out.WriteByte('\\')
			i++ // skip the escaped backslash (loop skips the leading one)
			continue
		}
		if d1 := body[i+1]; d1 >= '0' && d1 <= '7' {
			if i+3 >= len(body) {
				return "", errors.New("short octal escape in quoted string")
			}
			d2, d3 := body[i+2], body[i+3]
			if d2 < '0' || d2 > '7' || d3 < '0' || d3 > '7' {
				return "", errors.New("invalid octal escape in quoted string")
			}
			out.WriteByte((d1-'0')*64 + (d2-'0')*8 + (d3 - '0'))
			i += 3 // loop increment accounts for the leading backslash
			continue
		}
		// Any other escaped byte (e.g. "\;" or "\.") represents itself.
		out.WriteByte(body[i+1])
		i++
	}
	return out.String(), nil
}

// normalizeIssuerDomain lowercases a domain and strips a single trailing dot.
func normalizeIssuerDomain(domain string) string {
	domain = strings.ToLower(strings.TrimSpace(domain))
	domain = strings.TrimSuffix(domain, ".")
	return domain
}
