package message

import (
	"encoding/base64"
	"strings"
	"testing"
)

// buildSignedEmail builds a multipart/mixed message containing a text part, a
// multipart/signed subtree and an ordinary attachment.
//
// The text part intentionally uses a folded header to prove that unedited
// header bytes survive verbatim.
func buildSignedEmail() string {
	innerHeader :=
		"Content-Type: text/plain; charset=utf-8\r\n" +
			"Content-Transfer-Encoding: 7bit\r\n" +
			"\r\n"
	innerBody := "signed body, do not touch\r\n"
	inner := innerHeader + innerBody

	signature := base64.StdEncoding.EncodeToString([]byte("FAKESIGNATURE"))

	signed :=
		"Content-Type: multipart/signed; protocol=\"application/pgp-signature\";\r\n" +
			"\tmicalg=pgp-sha256; boundary=SIGNBOUND\r\n" +
			"\r\n" +
			"--SIGNBOUND\r\n" +
			inner +
			"\r\n" +
			"--SIGNBOUND\r\n" +
			"Content-Type: application/pgp-signature; name=signature.asc\r\n" +
			"Content-Transfer-Encoding: base64\r\n" +
			"Content-Disposition: attachment; filename=signature.asc\r\n" +
			"\r\n" +
			signature + "\r\n" +
			"--SIGNBOUND--\r\n"

	attachmentPayload := base64.StdEncoding.EncodeToString([]byte("OLD ATTACHMENT PAYLOAD"))

	return "Mime-Version: 1.0\r\n" +
		"From: alice@example.org\r\n" +
		"Subject: signed mail with attachment\r\n" +
		"Content-Type: multipart/mixed; boundary=MIXEDBOUND\r\n" +
		"\r\n" +
		"--MIXEDBOUND\r\n" +
		"Content-Type: text/plain;\r\n" +
		" charset=utf-8\r\n" +
		"Content-Transfer-Encoding: 7bit\r\n" +
		"\r\n" +
		"hello\r\n" +
		"--MIXEDBOUND\r\n" +
		signed +
		"--MIXEDBOUND\r\n" +
		"Content-Type: application/octet-stream; name=data.bin\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"Content-Disposition: attachment; filename=data.bin\r\n" +
		"\r\n" +
		attachmentPayload + "\r\n" +
		"--MIXEDBOUND--\r\n"
}

func TestParseRawMessage_structure(t *testing.T) {
	raw := buildSignedEmail()
	m, err := ParseRawMessage([]byte(raw))
	if err != nil {
		t.Fatalf("ParseRawMessage() = %v", err)
	}

	root := m.Root
	if got, want := root.MediaType, "multipart/mixed"; got != want {
		t.Fatalf("root media type = %q, want %q", got, want)
	}
	if got := len(root.Children); got != 3 {
		t.Fatalf("root has %d children, want 3", got)
	}

	signedPart := root.Children[1]
	if got, want := signedPart.MediaType, "multipart/signed"; got != want {
		t.Fatalf("child 1 media type = %q, want %q", got, want)
	}
	if got := len(signedPart.Children); got != 2 {
		t.Fatalf("signed subtree has %d children, want 2", got)
	}

	if len(m.Protections) != 1 {
		t.Fatalf("Protections = %d, want 1", len(m.Protections))
	}
	p := m.Protections[0]
	if p.MediaType != "multipart/signed" || len(p.Path) != 1 || p.Path[0] != 1 {
		t.Fatalf("unexpected protection: %+v", p)
	}

	attachment := root.Children[2]
	if got := attachment.IsProtected(); got {
		t.Errorf("attachment should not be protected, got ProtectedBy() = %+v", attachment.ProtectedBy())
	}
	innerSigned := signedPart.Children[0]
	if got := innerSigned.IsProtected(); !got {
		t.Errorf("signed inner part should be protected, got IsProtected() = false")
	}
	if got := signedPart.IsProtected(); got {
		t.Errorf("the multipart/signed container itself must not be covered by itself")
	}
}

func TestRawMessage_ReplaceAttachment_preservesSignedSubtree(t *testing.T) {
	raw := []byte(buildSignedEmail())
	m, err := ParseRawMessage(raw)
	if err != nil {
		t.Fatalf("ParseRawMessage() = %v", err)
	}

	signedRange := m.Protections[0].Range
	signedBytesBefore := append([]byte(nil), raw[signedRange.Start:signedRange.End]...)

	// Fold markers outside the edited region must survive.
	folded := "Content-Type: text/plain;\r\n charset=utf-8\r\n"
	if !bytesContains(raw, []byte(folded)) {
		t.Fatal("test message must contain a folded header")
	}

	var newHeader Header
	newHeader.SetContentType("application/octet-stream", map[string]string{"name": "data.bin"})
	newHeader.Set("Content-Transfer-Encoding", "base64")
	newHeader.SetContentDisposition("attachment", map[string]string{"filename": "data.bin"})

	newEncoded := base64.StdEncoding.EncodeToString([]byte("BRAND NEW PAYLOAD"))
	newPart, err := EncodePart(newHeader, strings.NewReader("BRAND NEW PAYLOAD"))
	if err != nil {
		t.Fatalf("EncodePart() = %v", err)
	}
	if !bytesContains(newPart, []byte(newEncoded)) {
		t.Fatalf("encoded part %q does not contain base64 payload %q", newPart, newEncoded)
	}

	out, report, err := m.ReplacePart([]int{2}, newPart)
	if err != nil {
		t.Fatalf("ReplacePart() = %v", err)
	}
	if len(report.InvalidatedProtections) != 0 {
		t.Fatalf("unexpected invalidated protections: %+v", report.InvalidatedProtections)
	}

	// The signed subtree bytes must be identical.
	signedAfter, err := ParseRawMessage(out)
	if err != nil {
		t.Fatalf("ParseRawMessage(out) = %v", err)
	}
	signedRangeAfter := signedAfter.Protections[0].Range
	signedBytesAfter := out[signedRangeAfter.Start:signedRangeAfter.End]
	if string(signedBytesBefore) != string(signedBytesAfter) {
		t.Fatalf("multipart/signed subtree bytes changed:\nbefore:\n%q\nafter:\n%q",
			signedBytesBefore, signedBytesAfter)
	}

	// Folded header outside the edit region survives byte-exactly.
	if !bytesContains(out, []byte(folded)) {
		t.Errorf("folded header was not preserved: %q", folded)
	}

	// Root multipart boundary is preserved and still used.
	for _, marker := range []string{"--MIXEDBOUND\r\n", "--MIXEDBOUND--\r\n", "--SIGNBOUND\r\n", "--SIGNBOUND--\r\n"} {
		if !bytesContains(out, []byte(marker)) {
			t.Errorf("boundary marker %q missing after edit", marker)
		}
	}

	// The new attachment decodes to the new payload and keeps base64 CTE.
	newAttachment := signedAfter.Root.Children[2]
	if got := newAttachment.Header.Get("Content-Transfer-Encoding"); got != "base64" {
		t.Errorf("attachment CTE = %q, want base64", got)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(newAttachment.RawBody())))
	if err != nil {
		t.Fatalf("cannot decode new attachment body: %v", err)
	}
	if string(decoded) != "BRAND NEW PAYLOAD" {
		t.Fatalf("new attachment payload = %q", string(decoded))
	}
	if bytesContains(out, []byte("OLD ATTACHMENT PAYLOAD")) {
		t.Errorf("old payload still present")
	}
}

func TestRawMessage_ReplaceAttachment_byteSpliceExactness(t *testing.T) {
	raw := []byte(buildSignedEmail())
	m, err := ParseRawMessage(raw)
	if err != nil {
		t.Fatalf("ParseRawMessage() = %v", err)
	}

	attachment := m.Root.Children[2]

	var newHeader Header
	newHeader.SetContentType("application/octet-stream", map[string]string{"name": "data.bin"})
	newHeader.Set("Content-Transfer-Encoding", "base64")
	newHeader.SetContentDisposition("attachment", map[string]string{"filename": "data.bin"})
	newPart, err := EncodePart(newHeader, strings.NewReader("BRAND NEW PAYLOAD"))
	if err != nil {
		t.Fatalf("EncodePart() = %v", err)
	}

	out, _, err := m.ReplacePart([]int{2}, newPart)
	if err != nil {
		t.Fatalf("ReplacePart() = %v", err)
	}

	// The new part must be exactly EncodePart's output, with no extra blank
	// line before the closing boundary.
	wantTail := string(newPart) + "--MIXEDBOUND--\r\n"
	if !strings.HasSuffix(string(out), wantTail) {
		t.Fatalf("message tail mismatch:\nwant suffix:\n%q\ngot:\n%q",
			wantTail, string(out[len(out)-len(wantTail):]))
	}

	// Everything before and after the replaced range is byte-identical.
	r := attachment.WholeRange
	prefixSame := string(out[:r.Start]) == string(raw[:r.Start])
	suffixSame := string(out[r.Start+len(newPart):]) == string(raw[r.End:])
	if !prefixSame || !suffixSame {
		t.Fatalf("bytes outside the replaced attachment changed (prefix=%v suffix=%v)", prefixSame, suffixSame)
	}
}

func TestRawMessage_EditInsideSignedSubtree_requiresAcknowledgement(t *testing.T) {
	raw := []byte(buildSignedEmail())
	m, err := ParseRawMessage(raw)
	if err != nil {
		t.Fatalf("ParseRawMessage() = %v", err)
	}

	inner, err := m.Entity([]int{1, 0})
	if err != nil {
		t.Fatalf("Entity() = %v", err)
	}

	// Replace the signed inner body (still a locatable leaf-body edit).
	ed := Edit{
		Range:       inner.BodyRange,
		Replacement: []byte("tampered body\r\n"),
	}

	if _, _, err := m.Edit([]Edit{ed}); err == nil {
		t.Fatal("editing the protected subtree without acknowledgement must fail")
	} else if !IsProtectedSubtree(err) {
		t.Fatalf("expected ProtectedSubtreeError, got %v", err)
	}

	out, report, err := m.Edit([]Edit{ed}, WithInvalidateSignatures())
	if err != nil {
		t.Fatalf("Edit(WithInvalidateSignatures) = %v", err)
	}
	if len(report.InvalidatedProtections) != 1 {
		t.Fatalf("InvalidatedProtections = %d, want 1", len(report.InvalidatedProtections))
	}
	ip := report.InvalidatedProtections[0]
	if ip.MediaType != "multipart/signed" || ip.Path[0] != 1 {
		t.Fatalf("unexpected invalidated protection: %+v", ip)
	}
	if !bytesContains(out, []byte("tampered body")) {
		t.Errorf("edit was not applied")
	}

	// A signature part next to the signed content is inside the subtree too.
	sig, err := m.Entity([]int{1, 1})
	if err != nil {
		t.Fatalf("Entity(signature) = %v", err)
	}
	_, _, err = m.Edit([]Edit{{Range: sig.BodyRange, Replacement: []byte("AAAA\r\n")}})
	if !IsProtectedSubtree(err) {
		t.Fatalf("editing signature part: expected ProtectedSubtreeError, got %v", err)
	}
}

func TestRawMessage_unlocatableRanges(t *testing.T) {
	raw := []byte(buildSignedEmail())
	m, err := ParseRawMessage(raw)
	if err != nil {
		t.Fatalf("ParseRawMessage() = %v", err)
	}

	root := m.Root
	textPart := root.Children[0]
	signedPart := root.Children[1]

	tests := []struct {
		name  string
		start int
		end   int
	}{
		{
			name:  "range spans the boundary between text and signed subtree",
			start: textPart.WholeRange.End,
			end:   signedPart.WholeRange.Start + 5,
		},
		{
			name:  "range inside a multipart body but not a whole child",
			start: root.BodyRange.Start + 1,
			end:   signedPart.Children[0].WholeRange.Start,
		},
		{
			name:  "out of bounds",
			start: len(raw),
			end:   len(raw) + 1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := m.Edit([]Edit{{
				Range:       ByteRange{Start: tc.start, End: tc.end},
				Replacement: []byte("x"),
			}})
			if err == nil {
				t.Fatal("expected LocateError, got nil")
			}
			if !IsLocateError(err) {
				t.Fatalf("expected LocateError, got %v", err)
			}
		})
	}
}

func TestRawMessage_rejectsResultThatBreaksBoundary(t *testing.T) {
	raw := []byte(buildSignedEmail())
	m, err := ParseRawMessage(raw)
	if err != nil {
		t.Fatalf("ParseRawMessage() = %v", err)
	}

	textPart := m.Root.Children[0]
	// Replace the text part header with bytes that are not a valid header
	// block, so the re-parse must reject the resulting message.
	bad := []byte("not a valid header line without colon\r\n\r\nbody")
	_, _, err = m.Edit([]Edit{{Range: textPart.WholeRange, Replacement: bad}})
	if !IsLocateError(err) {
		t.Fatalf("expected LocateError for corrupted framing, got %v", err)
	}
}

func TestParseRawMessage_loneLFRejected(t *testing.T) {
	raw := strings.ReplaceAll(buildSignedEmail(), "\r\n", "\n")
	_, err := ParseRawMessage([]byte(raw))
	if !IsLocateError(err) {
		t.Fatalf("expected LocateError for bare-LF message, got %v", err)
	}
}

func TestParseRawMessage_missingClosingBoundaryRejected(t *testing.T) {
	raw := buildSignedEmail()
	// Drop the closing delimiter of the root multipart.
	truncated := strings.TrimSuffix(raw, "--MIXEDBOUND--\r\n")
	_, err := ParseRawMessage([]byte(truncated))
	if !IsLocateError(err) {
		t.Fatalf("expected LocateError for missing closing boundary, got %v", err)
	}
}

func TestRawMessage_overlappingEditsRejected(t *testing.T) {
	raw := []byte(buildSignedEmail())
	m, err := ParseRawMessage(raw)
	if err != nil {
		t.Fatalf("ParseRawMessage() = %v", err)
	}

	textPart := m.Root.Children[0]
	r := textPart.WholeRange
	edits := []Edit{
		{Range: ByteRange{Start: r.Start, End: r.Start + 10}, Replacement: []byte("a")},
		{Range: ByteRange{Start: r.Start + 5, End: r.Start + 15}, Replacement: []byte("b")},
	}
	_, _, err = m.Edit(edits)
	if !IsLocateError(err) {
		t.Fatalf("expected LocateError for overlapping edits, got %v", err)
	}
}

func bytesContains(haystack, needle []byte) bool {
	return strings.Contains(string(haystack), string(needle))
}
