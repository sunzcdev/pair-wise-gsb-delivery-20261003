package report

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-msgauth/dmarc"
)

const testReport = `<?xml version="1.0" encoding="UTF-8"?>
<feedback>
  <report_metadata>
    <org_name>Example Reporter</org_name>
    <email>postmaster@reporter.example</email>
    <extra_contact_info>https://reporter.example/dmarc</extra_contact_info>
    <report_id>12345@reporter.example</report_id>
    <date_range>
      <begin>1700000000</begin>
      <end>1700086400</end>
    </date_range>
    <error>partial data</error>
  </report_metadata>
  <policy_published>
    <domain>example.com</domain>
    <adkim>r</adkim>
    <aspf>s</aspf>
    <p>quarantine</p>
    <sp>none</sp>
    <pct>100</pct>
    <fo>0</fo>
  </policy_published>
  <record>
    <row>
      <source_ip>192.0.2.1</source_ip>
      <count>42</count>
      <policy_evaluated>
        <disposition>none</disposition>
        <dkim>pass</dkim>
        <spf>fail</spf>
        <reason>
          <type>local_policy</type>
          <comment>arc=pass</comment>
        </reason>
      </policy_evaluated>
    </row>
    <identifiers>
      <envelope_to>reporter.example</envelope_to>
      <envelope_from>example.com</envelope_from>
      <header_from>example.com</header_from>
    </identifiers>
    <auth_results>
      <dkim>
        <domain>example.com</domain>
        <selector>s1</selector>
        <result>pass</result>
        <human_result></human_result>
      </dkim>
      <spf>
        <domain>example.com</domain>
        <scope>mfrom</scope>
        <result>fail</result>
      </spf>
    </auth_results>
  </record>
  <record>
    <row>
      <source_ip>2001:db8::1</source_ip>
      <count>7</count>
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
    </auth_results>
  </record>
</feedback>
`

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

func zipData(t *testing.T, name string, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	f, err := w.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func checkFeedback(t *testing.T, f *Feedback) {
	t.Helper()

	if f.Metadata.OrgName != "Example Reporter" {
		t.Errorf("OrgName = %q", f.Metadata.OrgName)
	}
	if f.Metadata.ReportID != "12345@reporter.example" {
		t.Errorf("ReportID = %q", f.Metadata.ReportID)
	}
	if want := time.Unix(1700000000, 0).UTC(); !f.Metadata.DateRange.Begin.Equal(want) {
		t.Errorf("DateRange.Begin = %v, want %v", f.Metadata.DateRange.Begin, want)
	}
	if want := time.Unix(1700086400, 0).UTC(); !f.Metadata.DateRange.End.Equal(want) {
		t.Errorf("DateRange.End = %v, want %v", f.Metadata.DateRange.End, want)
	}
	if len(f.Metadata.Errors) != 1 || f.Metadata.Errors[0] != "partial data" {
		t.Errorf("Errors = %v", f.Metadata.Errors)
	}

	if f.PolicyPublished.Domain != "example.com" {
		t.Errorf("PolicyPublished.Domain = %q", f.PolicyPublished.Domain)
	}
	if f.PolicyPublished.Policy != dmarc.PolicyQuarantine {
		t.Errorf("PolicyPublished.Policy = %q", f.PolicyPublished.Policy)
	}
	if f.PolicyPublished.ASPF != dmarc.AlignmentStrict {
		t.Errorf("PolicyPublished.ASPF = %q", f.PolicyPublished.ASPF)
	}
	if f.PolicyPublished.Percent != 100 {
		t.Errorf("PolicyPublished.Percent = %d", f.PolicyPublished.Percent)
	}

	if len(f.Records) != 2 {
		t.Fatalf("len(Records) = %d, want 2", len(f.Records))
	}
	rec := f.Records[0]
	if rec.SourceIP.String() != "192.0.2.1" {
		t.Errorf("SourceIP = %v", rec.SourceIP)
	}
	if rec.Count != 42 {
		t.Errorf("Count = %d", rec.Count)
	}
	if rec.PolicyEvaluated.Disposition != dmarc.PolicyNone {
		t.Errorf("Disposition = %q", rec.PolicyEvaluated.Disposition)
	}
	if rec.PolicyEvaluated.DKIM != EvalPass || rec.PolicyEvaluated.SPF != EvalFail {
		t.Errorf("evaluated DKIM/SPF = %q/%q", rec.PolicyEvaluated.DKIM, rec.PolicyEvaluated.SPF)
	}
	if len(rec.PolicyEvaluated.Reasons) != 1 || rec.PolicyEvaluated.Reasons[0].Type != "local_policy" {
		t.Errorf("Reasons = %+v", rec.PolicyEvaluated.Reasons)
	}
	if rec.Identifiers.HeaderFrom != "example.com" {
		t.Errorf("HeaderFrom = %q", rec.Identifiers.HeaderFrom)
	}
	if len(rec.AuthResults.DKIM) != 1 || rec.AuthResults.DKIM[0].Selector != "s1" {
		t.Errorf("AuthResults.DKIM = %+v", rec.AuthResults.DKIM)
	}
	if len(rec.AuthResults.SPF) != 1 || rec.AuthResults.SPF[0].Scope != "mfrom" {
		t.Errorf("AuthResults.SPF = %+v", rec.AuthResults.SPF)
	}
	if f.Records[1].SourceIP.String() != "2001:db8::1" {
		t.Errorf("second SourceIP = %v", f.Records[1].SourceIP)
	}
}

func TestParseXML(t *testing.T) {
	f, err := Parse(strings.NewReader(testReport), nil)
	if err != nil {
		t.Fatal(err)
	}
	checkFeedback(t, f)
}

func TestParseGzip(t *testing.T) {
	f, err := Parse(bytes.NewReader(gzipData(t, []byte(testReport))), nil)
	if err != nil {
		t.Fatal(err)
	}
	checkFeedback(t, f)
}

func TestParseZip(t *testing.T) {
	f, err := Parse(bytes.NewReader(zipData(t, "reporter.example!example.com!1700000000!1700086400.xml", []byte(testReport))), nil)
	if err != nil {
		t.Fatal(err)
	}
	checkFeedback(t, f)
}

func TestReaderStreamsRecords(t *testing.T) {
	rd, err := NewReader(strings.NewReader(testReport), nil)
	if err != nil {
		t.Fatal(err)
	}
	if rd.Metadata().ReportID != "12345@reporter.example" {
		t.Errorf("ReportID = %q", rd.Metadata().ReportID)
	}
	if rd.PolicyPublished().Domain != "example.com" {
		t.Errorf("Domain = %q", rd.PolicyPublished().Domain)
	}

	var n int
	for {
		rec, err := rd.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if rec.Count <= 0 {
			t.Errorf("record %d: Count = %d", n, rec.Count)
		}
		n++
	}
	if n != 2 {
		t.Errorf("streamed %d records, want 2", n)
	}
}

func TestTruncatedReportFails(t *testing.T) {
	// Cut the report in the middle of the second record: the first
	// record can still be delivered, but the final state must be a
	// failure.
	cut := strings.Index(testReport, "<count>7</count>")
	if cut < 0 {
		t.Fatal("test report changed")
	}
	truncated := testReport[:cut]

	rd, err := NewReader(strings.NewReader(truncated), nil)
	if err != nil {
		t.Fatal(err)
	}

	rec, err := rd.Next()
	if err != nil {
		t.Fatalf("first Next() = %v", err)
	}
	if rec.Count != 42 {
		t.Errorf("Count = %d", rec.Count)
	}

	if _, err := rd.Next(); err == nil || err == io.EOF {
		t.Fatalf("second Next() = %v, want non-EOF error", err)
	}
	// The failure is terminal: subsequent calls keep failing.
	if _, err := rd.Next(); err == nil || err == io.EOF {
		t.Fatalf("third Next() = %v, want non-EOF error", err)
	}

	if _, err := Parse(strings.NewReader(truncated), nil); err == nil {
		t.Fatal("Parse of truncated report succeeded")
	}
}

func TestTruncatedGzipFails(t *testing.T) {
	data := gzipData(t, []byte(testReport))
	if _, err := Parse(bytes.NewReader(data[:len(data)/2]), nil); err == nil {
		t.Fatal("Parse of truncated gzip report succeeded")
	}
}

func TestUnknownExtensionElementsIgnored(t *testing.T) {
	report := strings.Replace(testReport, "</policy_published>",
		`<extension><deeply><nested><thing attr="1">x</thing></nested></deeply></extension>
  </policy_published>`, 1)
	report = strings.Replace(report, "</feedback>",
		`<vendor_extra><foo>bar</foo></vendor_extra>
</feedback>`, 1)

	f, err := Parse(strings.NewReader(report), nil)
	if err != nil {
		t.Fatal(err)
	}
	checkFeedback(t, f)
}

func TestMissingIdentityFields(t *testing.T) {
	for _, field := range []string{
		"<org_name>Example Reporter</org_name>",
		"<report_id>12345@reporter.example</report_id>",
		"<begin>1700000000</begin>",
		"<domain>example.com</domain>",
	} {
		report := strings.Replace(testReport, field, "", 1)
		if _, err := Parse(strings.NewReader(report), nil); err == nil {
			t.Errorf("Parse without %s succeeded", field)
		}
	}
}

func TestNotAReport(t *testing.T) {
	if _, err := Parse(strings.NewReader("<html></html>"), nil); !errors.Is(err, ErrNotAReport) {
		t.Fatalf("Parse = %v, want ErrNotAReport", err)
	}
}

func TestCompressedSizeLimit(t *testing.T) {
	data := gzipData(t, []byte(testReport))
	_, err := Parse(bytes.NewReader(data), &Options{MaxCompressedBytes: 16})
	if !errors.Is(err, ErrCompressedTooLarge) {
		t.Fatalf("Parse = %v, want ErrCompressedTooLarge", err)
	}
	// Exactly at the limit: must succeed.
	xmlData := []byte(strings.TrimSpace(testReport))
	if _, err := Parse(bytes.NewReader(xmlData), &Options{MaxCompressedBytes: int64(len(xmlData))}); err != nil {
		t.Fatalf("Parse at exact limit = %v", err)
	}
}

func TestDecompressedSizeLimit(t *testing.T) {
	data := gzipData(t, []byte(testReport))
	_, err := Parse(bytes.NewReader(data), &Options{MaxDecompressedBytes: int64(len(testReport) / 2)})
	if !errors.Is(err, ErrDecompressedTooLarge) {
		t.Fatalf("Parse = %v, want ErrDecompressedTooLarge", err)
	}

	_, err = Parse(strings.NewReader(testReport), &Options{MaxDecompressedBytes: int64(len(testReport) / 2)})
	if !errors.Is(err, ErrDecompressedTooLarge) {
		t.Fatalf("Parse plain XML = %v, want ErrDecompressedTooLarge", err)
	}
}

func TestZipMemberLimit(t *testing.T) {
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, name := range []string{"a.xml", "b.xml", "c.xml"} {
		f, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte(testReport)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	_, err := Parse(bytes.NewReader(buf.Bytes()), &Options{MaxMembers: 2})
	if !errors.Is(err, ErrTooManyMembers) {
		t.Fatalf("Parse = %v, want ErrTooManyMembers", err)
	}
}

func TestZipWithoutReport(t *testing.T) {
	data := zipData(t, "readme.txt", []byte("hello"))
	if _, err := Parse(bytes.NewReader(data), nil); !errors.Is(err, ErrNoReport) {
		t.Fatalf("Parse = %v, want ErrNoReport", err)
	}
}

func TestXMLDepthLimit(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("<feedback>")
	for i := 0; i < 100; i++ {
		sb.WriteString("<a>")
	}
	report := sb.String()

	_, err := Parse(strings.NewReader(report), &Options{MaxDepth: 10})
	if !errors.Is(err, ErrXMLTooDeep) {
		t.Fatalf("Parse = %v, want ErrXMLTooDeep", err)
	}
}

func TestInvalidSourceIP(t *testing.T) {
	report := strings.Replace(testReport, "192.0.2.1", "not-an-ip", 1)
	if _, err := Parse(strings.NewReader(report), nil); err == nil {
		t.Fatal("Parse with invalid source IP succeeded")
	}
}
