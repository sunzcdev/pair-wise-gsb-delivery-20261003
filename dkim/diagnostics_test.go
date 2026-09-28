package dkim

import (
	"bytes"
	"crypto/x509"
	"encoding/base64"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
)

type eventCollector struct {
	mu     sync.Mutex
	events []DiagnosticEvent
}

func (c *eventCollector) callback(ev DiagnosticEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, ev)
}

func signCollectingEvents(t *testing.T, msg string, chunk int, options *SignOptions) []DiagnosticEvent {
	t.Helper()

	collector := new(eventCollector)
	options.Diagnostics = &Diagnostics{Callback: collector.callback}

	s, err := NewSigner(options)
	if err != nil {
		t.Fatalf("Expected no error when creating signer, got: %v", err)
	}
	data := []byte(msg)
	for len(data) > 0 {
		n := chunk
		if n > len(data) {
			n = len(data)
		}
		if _, err := s.Write(data[:n]); err != nil {
			t.Fatalf("Expected no error when writing to signer, got: %v", err)
		}
		data = data[n:]
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Expected no error when closing signer, got: %v", err)
	}

	return collector.events
}

func testSignOptions() *SignOptions {
	return &SignOptions{
		Domain:   "example.org",
		Selector: "brisbane",
		Signer:   testPrivateKey,
	}
}

const diagDupSubjectMail = "From: Joe <joe@example.org>\r\n" +
	"Subject: one\r\n" +
	"Subject: two\r\n" +
	"\r\n" +
	"Hi.\r\n"

func TestDiagnostics_duplicateSubject(t *testing.T) {
	options := testSignOptions()
	options.HeaderKeys = []string{"From", "Subject", "Subject"}

	events := signCollectingEvents(t, diagDupSubjectMail, 4096, options)

	var picks []DiagnosticEvent
	for _, ev := range events {
		if ev.Stage == DiagnosticStageHeaderPick {
			picks = append(picks, ev)
		}
	}
	if len(picks) != 3 {
		t.Fatalf("Expected 3 header pick events, got %d: %v", len(picks), picks)
	}

	fromLen := len("From: Joe <joe@example.org>\r\n")
	subjectOneLen := len("Subject: one\r\n")

	want := []struct {
		summary string
		offset  int
		length  int
	}{
		{`picked "from" occurrence 1 of 1 (newest first)`, 0, fromLen},
		{`picked "subject" occurrence 1 of 2 (newest first)`, fromLen + subjectOneLen, len("Subject: two\r\n")},
		{`picked "subject" occurrence 2 of 2 (newest first)`, fromLen, subjectOneLen},
	}
	headerBlock := diagDupSubjectMail[:strings.Index(diagDupSubjectMail, "\r\n\r\n")+2]
	for i, w := range want {
		ev := picks[i]
		if ev.Summary != w.summary {
			t.Errorf("Expected pick summary %q, got %q", w.summary, ev.Summary)
		}
		if ev.InputOffset != w.offset || ev.InputLength != w.length {
			t.Errorf("Expected pick input range [%d, %d), got [%d, %d)",
				w.offset, w.offset+w.length, ev.InputOffset, ev.InputOffset+ev.InputLength)
		}
		if ev.InputOffset+ev.InputLength > len(headerBlock) {
			t.Errorf("Pick input range [%d, %d) escapes header block of %d bytes",
				ev.InputOffset, ev.InputOffset+ev.InputLength, len(headerBlock))
		}
	}

	// The newest Subject field must be picked first.
	if got := headerBlock[picks[1].InputOffset:]; !strings.HasPrefix(got, "Subject: two\r\n") {
		t.Errorf("Expected first picked Subject to be the newest one, got %q", got)
	}
}

const diagFoldedMail = "From: Joe <joe@example.org>\r\n" +
	"Subject: Kimi \t \r\n" +
	"\t No \t\r\n" +
	"  Na Wa\r\n" +
	"\r\n" +
	"Hi.\r\n"

func TestDiagnostics_foldedWhitespace(t *testing.T) {
	options := testSignOptions()
	options.HeaderCanonicalization = CanonicalizationRelaxed
	options.HeaderKeys = []string{"From", "Subject"}

	events := signCollectingEvents(t, diagFoldedMail, 4096, options)

	rawSubject := "Subject: Kimi \t \r\n\t No \t\r\n  Na Wa\r\n"
	canonicalSubject := "subject:Kimi No Na Wa\r\n"

	var found bool
	for _, ev := range events {
		if ev.Stage != DiagnosticStageHeader || !strings.Contains(ev.Summary, "subject") {
			continue
		}
		found = true
		if ev.InputLength != len(rawSubject) {
			t.Errorf("Expected header event input length %d, got %d", len(rawSubject), ev.InputLength)
		}
		if ev.OutputBytes != len(canonicalSubject) {
			t.Errorf("Expected header event output %d bytes, got %d", len(canonicalSubject), ev.OutputBytes)
		}
	}
	if !found {
		t.Fatal("Expected a header diagnostic event for the folded Subject field")
	}
}

func finalBodyEvent(t *testing.T, events []DiagnosticEvent) DiagnosticEvent {
	t.Helper()
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Stage == DiagnosticStageBody {
			return events[i]
		}
	}
	t.Fatal("Expected at least one body diagnostic event")
	return DiagnosticEvent{}
}

func TestDiagnostics_trailingBlankLines(t *testing.T) {
	const mail = "From: Joe <joe@example.org>\r\n" +
		"\r\n" +
		"Hello\r\n\r\n\r\n"

	t.Run("simple", func(t *testing.T) {
		events := signCollectingEvents(t, mail, 4096, testSignOptions())
		ev := finalBodyEvent(t, events)
		if want := "simple body: discarded 3 trailing line break(s), wrote final CRLF"; ev.Summary != want {
			t.Errorf("Expected final body summary %q, got %q", want, ev.Summary)
		}
		if want := len("Hello\r\n"); ev.OutputBytes != want {
			t.Errorf("Expected final body output %d bytes, got %d", want, ev.OutputBytes)
		}
		if want := len("Hello\r\n\r\n\r\n"); ev.InputLength != want {
			t.Errorf("Expected final body input %d bytes, got %d", want, ev.InputLength)
		}
	})

	t.Run("relaxed", func(t *testing.T) {
		options := testSignOptions()
		options.BodyCanonicalization = CanonicalizationRelaxed
		events := signCollectingEvents(t, mail, 4096, options)
		ev := finalBodyEvent(t, events)
		if want := "relaxed body: discarded 3 trailing line break(s)"; ev.Summary != want {
			t.Errorf("Expected final body summary %q, got %q", want, ev.Summary)
		}
		if want := len("Hello\r\n"); ev.OutputBytes != want {
			t.Errorf("Expected final body output %d bytes, got %d", want, ev.OutputBytes)
		}
	})

	t.Run("relaxed empty", func(t *testing.T) {
		options := testSignOptions()
		options.BodyCanonicalization = CanonicalizationRelaxed
		events := signCollectingEvents(t, "From: Joe <joe@example.org>\r\n\r\n\r\n\r\n", 4096, options)
		ev := finalBodyEvent(t, events)
		if want := "relaxed body: empty body, no output"; ev.Summary != want {
			t.Errorf("Expected final body summary %q, got %q", want, ev.Summary)
		}
		if ev.OutputBytes != 0 {
			t.Errorf("Expected final body output 0 bytes, got %d", ev.OutputBytes)
		}
	})
}

// filterChunkEvents drops per-chunk body events, which legitimately depend
// on the read chunk sizes. The remaining events (header picks, header
// canonicalization, final body summary) must not depend on chunking.
func filterChunkEvents(events []DiagnosticEvent) []DiagnosticEvent {
	var out []DiagnosticEvent
	for _, ev := range events {
		if ev.Stage == DiagnosticStageBody && ev.Summary == "canonicalized body chunk" {
			continue
		}
		out = append(out, ev)
	}
	return out
}

func TestDiagnostics_chunkSizeInvariance(t *testing.T) {
	signOptions := func() *SignOptions {
		options := testSignOptions()
		options.HeaderKeys = []string{"From", "Subject", "Subject"}
		return options
	}

	reference := filterChunkEvents(signCollectingEvents(t, diagDupSubjectMail, 4096, signOptions()))
	for _, chunk := range []int{1, 3, 7, 64} {
		events := filterChunkEvents(signCollectingEvents(t, diagDupSubjectMail, chunk, signOptions()))
		if !reflect.DeepEqual(reference, events) {
			t.Errorf("Expected chunk size %d to produce events %v, got %v", chunk, reference, events)
		}
	}
}

func TestDiagnostics_eventBudget(t *testing.T) {
	for _, max := range []int{1, 2, 5} {
		collector := new(eventCollector)
		options := testSignOptions()
		options.Diagnostics = &Diagnostics{Callback: collector.callback, MaxEvents: max}

		r := strings.NewReader(diagDupSubjectMail)
		var b bytes.Buffer
		if err := Sign(&b, r, options); err != nil {
			t.Fatalf("Expected no error while signing mail, got: %v", err)
		}

		if len(collector.events) != max {
			t.Errorf("Expected exactly %d events with MaxEvents=%d, got %d", max, max, len(collector.events))
		}
	}
}

// limitedReader returns at most n bytes per Read, to simulate small read
// chunks on the verification side.
type limitedReader struct {
	r io.Reader
	n int
}

func (lr *limitedReader) Read(b []byte) (int, error) {
	if len(b) > lr.n {
		b = b[:lr.n]
	}
	return lr.r.Read(b)
}

func testLookupTXT(domain string) ([]string, error) {
	pubDER, err := x509.MarshalPKIXPublicKey(&testPrivateKey.PublicKey)
	if err != nil {
		return nil, err
	}
	return []string{"v=DKIM1; k=rsa; p=" + base64.StdEncoding.EncodeToString(pubDER)}, nil
}

func verifyCollectingEvents(t *testing.T, msg string, chunk int) ([]DiagnosticEvent, *Verification) {
	t.Helper()

	collector := new(eventCollector)
	options := &VerifyOptions{
		LookupTXT:   testLookupTXT,
		Diagnostics: &Diagnostics{Callback: collector.callback},
	}

	verifications, err := VerifyWithOptions(&limitedReader{r: strings.NewReader(msg), n: chunk}, options)
	if err != nil {
		t.Fatalf("Expected no error while verifying signature, got: %v", err)
	}
	if len(verifications) != 1 {
		t.Fatalf("Expected exactly one verification, got %v", len(verifications))
	}
	return collector.events, verifications[0]
}

func TestDiagnostics_verify(t *testing.T) {
	var signed bytes.Buffer
	options := testSignOptions()
	options.HeaderKeys = []string{"From", "Subject", "Subject"}
	if err := Sign(&signed, strings.NewReader(diagDupSubjectMail), options); err != nil {
		t.Fatalf("Expected no error while signing mail, got: %v", err)
	}

	events, v := verifyCollectingEvents(t, signed.String(), 4096)
	if v.Err != nil {
		t.Fatalf("Expected no verification error, got: %v", v.Err)
	}

	stages := map[DiagnosticStage]bool{}
	for _, ev := range events {
		stages[ev.Stage] = true
	}
	for _, stage := range []DiagnosticStage{DiagnosticStageHeaderPick, DiagnosticStageHeader, DiagnosticStageBody} {
		if !stages[stage] {
			t.Errorf("Expected at least one %q diagnostic event", stage)
		}
	}

	// The duplicate Subject pick must be reported on the verify side too.
	var subjectPicks int
	for _, ev := range events {
		if ev.Stage == DiagnosticStageHeaderPick && strings.Contains(ev.Summary, `"subject"`) {
			subjectPicks++
		}
	}
	if subjectPicks != 2 {
		t.Errorf("Expected 2 subject pick events, got %d", subjectPicks)
	}

	// Events must not leak message content.
	for _, ev := range events {
		if strings.Contains(ev.Summary, "Hi.") || strings.Contains(ev.Summary, "one") || strings.Contains(ev.Summary, "two") {
			t.Errorf("Expected no message content in diagnostic event, got %q", ev.Summary)
		}
	}

	// Different read chunk sizes must yield the same final positions and
	// summaries.
	reference := filterChunkEvents(events)
	for _, chunk := range []int{1, 5, 17} {
		chunkEvents, v := verifyCollectingEvents(t, signed.String(), chunk)
		if v.Err != nil {
			t.Fatalf("Expected no verification error with chunk size %d, got: %v", chunk, v.Err)
		}
		if !reflect.DeepEqual(reference, filterChunkEvents(chunkEvents)) {
			t.Errorf("Expected chunk size %d to produce events %v, got %v", chunk, reference, filterChunkEvents(chunkEvents))
		}
	}
}
