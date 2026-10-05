package message

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
)

const rawSignedMail = "MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed;\r\n" +
	"\tboundary=\"outer-boundary\"\r\n" +
	"\r\n" +
	"--outer-boundary\r\n" +
	"Content-Type: application/octet-stream; name=\"old.bin\"\r\n" +
	"Content-Transfer-Encoding: base64\r\n" +
	"Content-Disposition: attachment;\r\n" +
	"\tfilename=\"old.bin\"\r\n" +
	"\r\n" +
	"Bw==\r\n" +
	"--outer-boundary\r\n" +
	"Content-Type: multipart/signed; protocol=\"application/pkcs7-signature\";\r\n" +
	"\tmicalg=sha-256; boundary=\"signed-boundary\"\r\n" +
	"\r\n" +
	"--signed-boundary\r\n" +
	"Content-Type: text/plain\r\n" +
	"Content-Transfer-Encoding: 7bit\r\n" +
	"\r\n" +
	"signed payload\r\n" +
	"--signed-boundary\r\n" +
	"Content-Type: application/pkcs7-signature; name=\"smime.p7s\"\r\n" +
	"Content-Transfer-Encoding: base64\r\n" +
	"Content-Disposition: attachment; filename=\"smime.p7s\"\r\n" +
	"\r\n" +
	"U0lHTkFUVVJF\r\n" +
	"--signed-boundary--\r\n" +
	"\r\n" +
	"--outer-boundary--\r\n"

func findRawPart(t *testing.T, m *RawMessage, path []int) PartRange {
	t.Helper()
	for _, part := range m.Parts() {
		if reflect.DeepEqual(part.Path, path) {
			return part
		}
	}
	t.Fatalf("MIME part %v not found", path)
	return PartRange{}
}

func findRawAttachment(t *testing.T, m *RawMessage, filename string) PartRange {
	t.Helper()
	for _, part := range m.Parts() {
		_, params, err := part.Header.ContentDisposition()
		if err != nil {
			t.Fatalf("ContentDisposition: %v", err)
		}
		if params["filename"] == filename {
			return part
		}
	}
	t.Fatalf("attachment %q not found", filename)
	return PartRange{}
}

func TestRawMessageEdit_replacementPreservesSignedSubtree(t *testing.T) {
	raw := []byte(rawSignedMail)
	m, err := ParseRawMessage(raw)
	if err != nil {
		t.Fatalf("ParseRawMessage: %v", err)
	}

	attachment := findRawAttachment(t, m, "old.bin")
	signed := findRawPart(t, m, []int{1})
	signedPayload := findRawPart(t, m, []int{1, 0})
	signature := findRawPart(t, m, []int{1, 1})

	if !signedPayload.Protected {
		t.Fatal("signed payload must be marked as protected")
	}
	if !signed.Protected || !signature.Protected || attachment.Protected {
		t.Fatal("the multipart/signed subtree should be protected, but not its sibling attachment")
	}

	originalSignedSubtree := append([]byte(nil), raw[signed.Part.Start:signed.Part.End]...)
	replacement := []byte("QQ==\r\n")
	edited, err := m.Edit([]RawEdit{{
		Range:       attachment.Body,
		Replacement: replacement,
	}})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}

	if bytes.Equal(edited, raw) {
		t.Fatal("attachment replacement did not change the message")
	}

	editedSigned := findRawPart(t, mustParseRawMessage(t, edited), []int{1})
	gotSignedSubtree := edited[editedSigned.Part.Start:editedSigned.Part.End]
	if !bytes.Equal(gotSignedSubtree, originalSignedSubtree) {
		t.Fatalf("multipart/signed subtree changed:\noriginal: %q\nedited:   %q", originalSignedSubtree, gotSignedSubtree)
	}

	if !bytes.Equal(edited[:attachment.Body.Start], raw[:attachment.Body.Start]) {
		t.Fatal("bytes before the attachment body changed")
	}
	if !bytes.Equal(edited[attachment.Body.Start+int64(len(replacement)):], raw[attachment.Body.End:]) {
		t.Fatal("bytes after the attachment body changed")
	}
	if !bytes.Contains(edited, []byte("\tboundary=\"outer-boundary\"\r\n")) {
		t.Fatal("outer folded Content-Type header was not preserved")
	}
	if !bytes.Contains(edited, []byte("--outer-boundary\r\n")) || !bytes.Contains(edited, []byte("--signed-boundary\r\n")) {
		t.Fatal("original multipart boundaries were not preserved")
	}
}

func TestRawMessageEdit_protectedRangeRequiresConfirmation(t *testing.T) {
	raw := []byte(rawSignedMail)
	m, err := ParseRawMessage(raw)
	if err != nil {
		t.Fatalf("ParseRawMessage: %v", err)
	}

	signedPayload := findRawPart(t, m, []int{1, 0})
	_, err = m.Edit([]RawEdit{{
		Range:       signedPayload.Body,
		Replacement: []byte("changed payload\r\n"),
	}})

	var signatureErr *SignatureInvalidationError
	if !errors.As(err, &signatureErr) {
		t.Fatalf("Edit error = %v, want SignatureInvalidationError", err)
	}
	if signatureErr.Range != signedPayload.Body {
		t.Fatalf("SignatureInvalidationError range = %v, want %v", signatureErr.Range, signedPayload.Body)
	}

	edited, err := m.Edit([]RawEdit{{
		Range:                      signedPayload.Body,
		Replacement:                []byte("changed payload\r\n"),
		AllowSignatureInvalidation: true,
	}})
	if err != nil {
		t.Fatalf("Edit with explicit signature invalidation: %v", err)
	}
	if bytes.Equal(edited, raw) {
		t.Fatal("explicit protected edit did not change the message")
	}
}

func TestRawMessageEdit_signatureControlRequiresConfirmation(t *testing.T) {
	raw := []byte(rawSignedMail)
	m, err := ParseRawMessage(raw)
	if err != nil {
		t.Fatalf("ParseRawMessage: %v", err)
	}

	signature := findRawPart(t, m, []int{1, 1})
	_, err = m.Edit([]RawEdit{{
		Range:       signature.Body,
		Replacement: []byte("Q0hBTkdFRw==\r\n"),
	}})

	var signatureErr *SignatureInvalidationError
	if !errors.As(err, &signatureErr) {
		t.Fatalf("Edit error = %v, want SignatureInvalidationError", err)
	}
}

func TestParseRawMessage_multipartWithoutBoundary(t *testing.T) {
	raw := "Content-Type: multipart/mixed\r\n\r\n--boundary\r\n"
	_, err := ParseRawMessage([]byte(raw))
	if err == nil {
		t.Fatal("ParseRawMessage succeeded without a locatable boundary parameter")
	}
}

func TestParseRawMessage_multipartWithoutDelimiterCRLF(t *testing.T) {
	raw := []byte(strings.Replace(rawSignedMail, "--signed-boundary--\r\n\r\n--outer-boundary--", "--signed-boundary--\r\n--outer-boundary--", 1))
	_, err := ParseRawMessage(raw)
	if err == nil {
		t.Fatal("ParseRawMessage succeeded for a nested boundary without the required CRLF")
	}
}

func TestRawMessageEdit_rejectsStructurallyUnalignedRange(t *testing.T) {
	m, err := ParseRawMessage([]byte(rawSignedMail))
	if err != nil {
		t.Fatalf("ParseRawMessage: %v", err)
	}

	attachment := findRawPart(t, m, []int{0})
	_, err = m.Edit([]RawEdit{{
		Range:       ByteRange{Start: attachment.Body.Start + 1, End: attachment.Part.End + 1},
		Replacement: []byte("x"),
	}})
	if err == nil {
		t.Fatal("Edit succeeded for a range crossing a MIME structural boundary")
	}
}

func mustParseRawMessage(t *testing.T, raw []byte) *RawMessage {
	t.Helper()
	m, err := ParseRawMessage(raw)
	if err != nil {
		t.Fatalf("ParseRawMessage: %v", err)
	}
	return m
}
