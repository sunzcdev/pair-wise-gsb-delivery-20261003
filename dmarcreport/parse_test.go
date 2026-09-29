package dmarcreport

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-msgauth/dmarc"
)

const sampleReport = `<?xml version="1.0" encoding="UTF-8"?>
<feedback>
  <version>1.0</version>
  <report_metadata>
    <org_name>Example Reporter</org_name>
    <email>postmaster@reporter.example</email>
    <extra_contact_info>https://reporter.example/dmarc</extra_contact_info>
    <report_id>12345@reporter.example</report_id>
    <date_range>
      <begin>1700000000</begin>
      <end>1700086400</end>
    </date_range>
  </report_metadata>
  <policy_published>
    <domain>example.com</domain>
    <adkim>r</adkim>
    <aspf>r</aspf>
    <p>quarantine</p>
    <sp>none</sp>
    <pct>100</pct>
    <fo>0</fo>
  </policy_published>
  <record>
    <row>
      <source_ip>192.0.2.1</source_ip>
      <count>7</count>
      <policy_evaluated>
        <disposition>none</disposition>
        <dkim>pass</dkim>
        <spf>fail</spf>
        <reason>
          <type>forwarded</type>
          <comment>message was forwarded</comment>
        </reason>
      </policy_evaluated>
    </row>
    <identifiers>
      <envelope_from>example.com</envelope_from>
      <header_from>example.com</header_from>
    </identifiers>
    <auth_results>
      <dkim>
        <domain>example.com</domain>
        <selector>s1</selector>
        <result>pass</result>
      </dkim>
      <spf>
        <domain>example.com</domain>
        <result>fail</result>
      </spf>
    </auth_results>
  </record>
  <record>
    <row>
      <source_ip>2001:db8::1</source_ip>
      <count>3</count>
      <policy_evaluated>
        <disposition>quarantine</disposition>
        <dkim>fail</dkim>
        <spf>fail</spf>
      </policy_evaluated>
    </row>
    <identifiers>
      <header_from>example.com</header_from>
    </identifiers>
    <auth_results>
      <spf>
        <domain>example.com</domain>
        <result>fail</result>
      </spf>
    </auth_results>
  </record>
</feedback>`

func gzipData(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func zipData(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for name, data := range files {
		f, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func collect(t *testing.T, r *strings.Reader, limits *Limits) (*Report, []*Record, error) {
	t.Helper()
	var records []*Record
	report, err := Parse(r, limits, func(rec *Record) error {
		records = append(records, rec)
		return nil
	})
	return report, records, err
}

func checkSample(t *testing.T, report *Report, records []*Record) {
	t.Helper()

	m := report.Metadata
	if m.OrgName != "Example Reporter" {
		t.Errorf("OrgName = %q", m.OrgName)
	}
	if m.Email != "postmaster@reporter.example" {
		t.Errorf("Email = %q", m.Email)
	}
	if m.ReportID != "12345@reporter.example" {
		t.Errorf("ReportID = %q", m.ReportID)
	}
	if want := time.Unix(1700000000, 0).UTC(); !m.DateRange.Begin.Equal(want) {
		t.Errorf("DateRange.Begin = %v, want %v", m.DateRange.Begin, want)
	}
	if want := time.Unix(1700086400, 0).UTC(); !m.DateRange.End.Equal(want) {
		t.Errorf("DateRange.End = %v, want %v", m.DateRange.End, want)
	}

	pp := report.PolicyPublished
	if pp.Domain != "example.com" {
		t.Errorf("PolicyPublished.Domain = %q", pp.Domain)
	}
	if pp.Policy != dmarc.PolicyQuarantine {
		t.Errorf("PolicyPublished.Policy = %q", pp.Policy)
	}
	if pp.SubdomainPolicy != dmarc.PolicyNone {
		t.Errorf("PolicyPublished.SubdomainPolicy = %q", pp.SubdomainPolicy)
	}
	if pp.ADKIM != dmarc.AlignmentRelaxed || pp.ASPF != dmarc.AlignmentRelaxed {
		t.Errorf("alignment = %q/%q", pp.ADKIM, pp.ASPF)
	}
	if pp.Percent != 100 {
		t.Errorf("PolicyPublished.Percent = %d", pp.Percent)
	}

	if len(records) != 2 {
		t.Fatalf("got %d records, want 2", len(records))
	}
	r0 := records[0]
	if r0.SourceIP.String() != "192.0.2.1" {
		t.Errorf("record 0 SourceIP = %v", r0.SourceIP)
	}
	if r0.Count != 7 {
		t.Errorf("record 0 Count = %d", r0.Count)
	}
	if r0.PolicyEvaluated.Disposition != dmarc.PolicyNone {
		t.Errorf("record 0 Disposition = %q", r0.PolicyEvaluated.Disposition)
	}
	if r0.PolicyEvaluated.DKIM != EvalResultPass || r0.PolicyEvaluated.SPF != EvalResultFail {
		t.Errorf("record 0 eval = %q/%q", r0.PolicyEvaluated.DKIM, r0.PolicyEvaluated.SPF)
	}
	if len(r0.PolicyEvaluated.Reasons) != 1 || r0.PolicyEvaluated.Reasons[0].Type != "forwarded" {
		t.Errorf("record 0 Reasons = %+v", r0.PolicyEvaluated.Reasons)
	}
	if r0.Identifiers.HeaderFrom != "example.com" {
		t.Errorf("record 0 HeaderFrom = %q", r0.Identifiers.HeaderFrom)
	}
	if len(r0.AuthResults.DKIM) != 1 || r0.AuthResults.DKIM[0].Selector != "s1" {
		t.Errorf("record 0 DKIM auth results = %+v", r0.AuthResults.DKIM)
	}
	if len(r0.AuthResults.SPF) != 1 || r0.AuthResults.SPF[0].Result != "fail" {
		t.Errorf("record 0 SPF auth results = %+v", r0.AuthResults.SPF)
	}
	if records[1].SourceIP.String() != "2001:db8::1" || records[1].Count != 3 {
		t.Errorf("record 1 = %+v", records[1])
	}
}

func TestParseXML(t *testing.T) {
	report, records, err := collect(t, strings.NewReader(sampleReport), nil)
	if err != nil {
		t.Fatal(err)
	}
	checkSample(t, report, records)
}

func TestParseGzip(t *testing.T) {
	report, records, err := collect(t, strings.NewReader(string(gzipData(t, []byte(sampleReport)))), nil)
	if err != nil {
		t.Fatal(err)
	}
	checkSample(t, report, records)
}

func TestParseZip(t *testing.T) {
	data := zipData(t, map[string][]byte{"report.xml": []byte(sampleReport)})
	report, records, err := collect(t, strings.NewReader(string(data)), nil)
	if err != nil {
		t.Fatal(err)
	}
	checkSample(t, report, records)
}

func TestParseZipTooManyMembers(t *testing.T) {
	data := zipData(t, map[string][]byte{
		"a.xml": []byte(sampleReport),
		"b.xml": []byte(sampleReport),
	})
	if _, _, err := collect(t, strings.NewReader(string(data)), nil); err == nil {
		t.Fatal("expected error for multi-member zip")
	}
}

func TestParseTruncatedXML(t *testing.T) {
	// Cut the input right after the first complete record: the caller
	// receives one record, but Parse must still fail.
	cut := strings.Index(sampleReport, "</record>") + len("</record>")
	var records []*Record
	_, err := Parse(strings.NewReader(sampleReport[:cut]), nil, func(rec *Record) error {
		records = append(records, rec)
		return nil
	})
	if err == nil {
		t.Fatal("expected error for truncated report")
	}
	if len(records) != 1 {
		t.Fatalf("got %d records before failure, want 1", len(records))
	}
}

func TestParseTruncatedGzip(t *testing.T) {
	data := gzipData(t, []byte(sampleReport))
	data = data[:len(data)/2]
	if _, _, err := collect(t, strings.NewReader(string(data)), nil); err == nil {
		t.Fatal("expected error for truncated gzip stream")
	}
}

func TestParseTruncatedZip(t *testing.T) {
	data := zipData(t, map[string][]byte{"report.xml": []byte(sampleReport)})
	data = data[:len(data)/2]
	if _, _, err := collect(t, strings.NewReader(string(data)), nil); err == nil {
		t.Fatal("expected error for truncated zip archive")
	}
}

func TestParseMissingIdentity(t *testing.T) {
	for _, field := range []string{"org_name", "email", "report_id"} {
		doc := strings.Replace(sampleReport, "<"+field+">", "<x_"+field+">", 1)
		doc = strings.Replace(doc, "</"+field+">", "</x_"+field+">", 1)
		if _, _, err := collect(t, strings.NewReader(doc), nil); err == nil {
			t.Errorf("expected error for missing %s", field)
		}
	}
}

func TestParseBadDateRange(t *testing.T) {
	doc := strings.Replace(sampleReport, "<begin>1700000000</begin>", "<begin>1700086400</begin>", 1)
	doc = strings.Replace(doc, "<end>1700086400</end>", "<end>1700000000</end>", 1)
	if _, _, err := collect(t, strings.NewReader(doc), nil); err == nil {
		t.Fatal("expected error for reversed date range")
	}
}

func TestParseUnknownExtensionElements(t *testing.T) {
	doc := strings.Replace(sampleReport, "</policy_published>",
		`</policy_published>
  <vendor_extension xmlns="urn:example">
    <nested><deeper>ignored</deeper></nested>
  </vendor_extension>`, 1)
	doc = strings.Replace(doc, "</identifiers>",
		`<extra_ids><report_id>not-the-real-one</report_id></extra_ids></identifiers>`, 1)
	report, records, err := collect(t, strings.NewReader(doc), nil)
	if err != nil {
		t.Fatal(err)
	}
	checkSample(t, report, records)
}

func TestParseLimits(t *testing.T) {
	gz := gzipData(t, []byte(sampleReport))

	t.Run("compressed bytes", func(t *testing.T) {
		limits := &Limits{MaxCompressedBytes: int64(len(gz) - 1)}
		if _, _, err := collect(t, strings.NewReader(string(gz)), limits); err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("decompressed bytes", func(t *testing.T) {
		limits := &Limits{MaxDecompressedBytes: int64(len(sampleReport) - 1)}
		if _, _, err := collect(t, strings.NewReader(sampleReport), limits); err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("decompressed bytes gzip", func(t *testing.T) {
		limits := &Limits{MaxDecompressedBytes: int64(len(sampleReport) - 1)}
		if _, _, err := collect(t, strings.NewReader(string(gz)), limits); err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("zip member size", func(t *testing.T) {
		data := zipData(t, map[string][]byte{"report.xml": []byte(sampleReport)})
		limits := &Limits{MaxDecompressedBytes: int64(len(sampleReport) - 1)}
		if _, _, err := collect(t, strings.NewReader(string(data)), limits); err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("xml depth", func(t *testing.T) {
		limits := &Limits{MaxXMLDepth: 3}
		if _, _, err := collect(t, strings.NewReader(sampleReport), limits); err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("exact fit", func(t *testing.T) {
		limits := &Limits{MaxDecompressedBytes: int64(len(sampleReport))}
		if _, _, err := collect(t, strings.NewReader(sampleReport), limits); err != nil {
			t.Fatalf("exact-size report should pass: %v", err)
		}
	})
}

func TestParseEmpty(t *testing.T) {
	if _, _, err := collect(t, strings.NewReader(""), nil); err == nil {
		t.Fatal("expected error for empty input")
	}
}
