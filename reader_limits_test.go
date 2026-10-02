package mbox

import (
	"io"
	"io/ioutil"
	"strings"
	"testing"
)

func limitError(t *testing.T, err error) *LimitError {
	t.Helper()
	le, ok := err.(*LimitError)
	if !ok {
		t.Fatalf("Expected *LimitError, got %T (%v)", err, err)
	}
	return le
}

// limitTestMbox builds an mbox with three messages, the second one having the
// provided body. It returns the mbox source and the expected unescaped
// contents of the first and third messages.
func limitTestMbox(msg2Body string) (mbox, want1, want3 string) {
	sep1 := "From a@example.com Thu Jan  1 00:00:01 2015\n"
	msg1 := "From: a@example.com\nSubject: one\n\nfirst body\n"
	sep2 := "From b@example.com Thu Jan  1 00:00:02 2015\n"
	msg2 := "From: b@example.com\nSubject: two\n\n" + msg2Body
	sep3 := "From c@example.com Thu Jan  1 00:00:03 2015\n"
	msg3 := "From: c@example.com\nSubject: three\n\nthird body\n"
	return sep1 + msg1 + "\n" + sep2 + msg2 + "\n" + sep3 + msg3,
		toCRLF(msg1), toCRLF(msg3)
}

func readString(t *testing.T, r io.Reader) (string, error) {
	t.Helper()
	b, err := ioutil.ReadAll(r)
	return string(b), err
}

func TestReaderLimitRawBytes(t *testing.T) {
	sep := "From a@example.com Thu Jan  1 00:00:01 2015\n"
	mbox := sep + "From: a@example.com\n\n" + strings.Repeat("0123456789\n", 2000)

	r := NewReader(strings.NewReader(mbox))
	r.Limits = Limits{MaxRawBytes: 4096}

	mr, err := r.NextMessage()
	if err != nil {
		t.Fatalf("NextMessage() = %v", err)
	}
	_, err = ioutil.ReadAll(mr)
	le := limitError(t, err)
	if le.Budget != BudgetRawBytes {
		t.Errorf("Budget = %v, want %v", le.Budget, BudgetRawBytes)
	}
	if le.MessageIndex != 0 {
		t.Errorf("MessageIndex = %d, want 0", le.MessageIndex)
	}
	if le.Limit != 4096 {
		t.Errorf("Limit = %d, want 4096", le.Limit)
	}
	msgStart := int64(len(sep))
	if le.RawOffset <= msgStart+4096 || le.RawOffset > msgStart+4096+8192 {
		t.Errorf("RawOffset = %d, want within (%d, %d]", le.RawOffset, msgStart+4096, msgStart+4096+8192)
	}
}

func TestReaderLimitOutputBytes(t *testing.T) {
	mbox := "From a@example.com Thu Jan  1 00:00:01 2015\n" +
		"From: a@example.com\n\n" + strings.Repeat("0123456789\n", 200)

	r := NewReader(strings.NewReader(mbox))
	r.Limits = Limits{MaxOutputBytes: 1000}

	mr, err := r.NextMessage()
	if err != nil {
		t.Fatalf("NextMessage() = %v", err)
	}
	b, err := ioutil.ReadAll(mr)
	le := limitError(t, err)
	if le.Budget != BudgetOutputBytes {
		t.Errorf("Budget = %v, want %v", le.Budget, BudgetOutputBytes)
	}
	if int64(len(b)) != 1000 {
		t.Errorf("ReadAll returned %d bytes, want exactly 1000", len(b))
	}
}

func TestReaderLimitLineBytes(t *testing.T) {
	mbox := "From a@example.com Thu Jan  1 00:00:01 2015\n" +
		"From: a@example.com\n\n" + strings.Repeat("x", 10000) + "\n"

	r := NewReader(strings.NewReader(mbox))
	r.Limits = Limits{MaxLineBytes: 1000}

	mr, err := r.NextMessage()
	if err != nil {
		t.Fatalf("NextMessage() = %v", err)
	}
	_, err = ioutil.ReadAll(mr)
	le := limitError(t, err)
	if le.Budget != BudgetLineBytes {
		t.Errorf("Budget = %v, want %v", le.Budget, BudgetLineBytes)
	}
	if le.MessageIndex != 0 {
		t.Errorf("MessageIndex = %d, want 0", le.MessageIndex)
	}
}

func TestReaderAutoDiscardRespectsLimits(t *testing.T) {
	mbox, want1, want3 := limitTestMbox(strings.Repeat("y", 100000) + "\n")

	r := NewReader(strings.NewReader(mbox))
	r.Limits = Limits{MaxRawBytes: 2048}

	mr, err := r.NextMessage()
	if err != nil {
		t.Fatalf("NextMessage() = %v", err)
	}
	got1, err := readString(t, mr)
	if err != nil {
		t.Fatalf("ReadAll(msg1) = %v", err)
	}
	if got1 != want1 {
		t.Errorf("msg1 = %q, want %q", got1, want1)
	}

	// Only read a prefix of the second message, then move on. The automatic
	// discard must hit the raw budget.
	mr, err = r.NextMessage()
	if err != nil {
		t.Fatalf("NextMessage() = %v", err)
	}
	if _, err := io.ReadFull(mr, make([]byte, 10)); err != nil {
		t.Fatalf("ReadFull(msg2) = %v", err)
	}
	_, err = r.NextMessage()
	le := limitError(t, err)
	if le.Budget != BudgetRawBytes {
		t.Errorf("Budget = %v, want %v", le.Budget, BudgetRawBytes)
	}
	if le.MessageIndex != 1 {
		t.Errorf("MessageIndex = %d, want 1", le.MessageIndex)
	}

	// The next call resynchronizes on the real boundary of the third
	// message.
	mr, err = r.NextMessage()
	if err != nil {
		t.Fatalf("NextMessage() after LimitError = %v", err)
	}
	got3, err := readString(t, mr)
	if err != nil {
		t.Fatalf("ReadAll(msg3) = %v", err)
	}
	if got3 != want3 {
		t.Errorf("msg3 = %q, want %q", got3, want3)
	}

	if _, err := r.NextMessage(); err != io.EOF {
		t.Fatalf("NextMessage() = %v, want io.EOF", err)
	}
}

func TestReaderSkipMessage(t *testing.T) {
	mbox, want1, want3 := limitTestMbox("second body\n")

	r := NewReader(strings.NewReader(mbox))

	mr, err := r.NextMessage()
	if err != nil {
		t.Fatalf("NextMessage() = %v", err)
	}
	if _, err := io.ReadFull(mr, make([]byte, 5)); err != nil {
		t.Fatalf("ReadFull(msg1) = %v", err)
	}
	if err := r.SkipMessage(); err != nil {
		t.Fatalf("SkipMessage() = %v", err)
	}

	// Skipping with no active message is a no-op.
	if err := r.SkipMessage(); err != nil {
		t.Fatalf("SkipMessage() = %v", err)
	}

	mr, err = r.NextMessage()
	if err != nil {
		t.Fatalf("NextMessage() = %v", err)
	}
	got2, err := readString(t, mr)
	if err != nil {
		t.Fatalf("ReadAll(msg2) = %v", err)
	}
	if !strings.Contains(got2, "second body") {
		t.Errorf("msg2 = %q, want it to contain %q", got2, "second body")
	}

	// The message was fully read, skipping is a no-op.
	if err := r.SkipMessage(); err != nil {
		t.Fatalf("SkipMessage() = %v", err)
	}

	mr, err = r.NextMessage()
	if err != nil {
		t.Fatalf("NextMessage() = %v", err)
	}
	got3, err := readString(t, mr)
	if err != nil {
		t.Fatalf("ReadAll(msg3) = %v", err)
	}
	if got3 != want3 {
		t.Errorf("msg3 = %q, want %q", got3, want3)
	}
	_ = want1

	if _, err := r.NextMessage(); err != io.EOF {
		t.Fatalf("NextMessage() = %v, want io.EOF", err)
	}
}

func TestReaderSkipMessageScanLimit(t *testing.T) {
	mbox, _, want3 := limitTestMbox(strings.Repeat("a line of body text\n", 10000))

	r := NewReader(strings.NewReader(mbox))
	r.Limits = Limits{MaxSkipScanBytes: 1024}

	// Skip the small first message to reach the huge second one.
	if _, err := r.NextMessage(); err != nil {
		t.Fatalf("NextMessage() = %v", err)
	}
	if err := r.SkipMessage(); err != nil {
		t.Fatalf("SkipMessage() = %v", err)
	}
	if _, err := r.NextMessage(); err != nil {
		t.Fatalf("NextMessage() = %v", err)
	}

	// The second message is huge: a single skip must exceed the scan budget.
	err := r.SkipMessage()
	le := limitError(t, err)
	if le.Budget != BudgetSkipScanBytes {
		t.Errorf("Budget = %v, want %v", le.Budget, BudgetSkipScanBytes)
	}
	if le.MessageIndex != 1 {
		t.Errorf("MessageIndex = %d, want 1", le.MessageIndex)
	}

	// Resume skipping in bounded steps until the boundary is found.
	for i := 0; i < 1000 && err != nil; i++ {
		err = r.SkipMessage()
		if err != nil {
			limitError(t, err)
		}
	}
	if err != nil {
		t.Fatalf("SkipMessage() did not complete: %v", err)
	}

	// The third message follows the boundary found by the skip.
	mr, err := r.NextMessage()
	if err != nil {
		t.Fatalf("NextMessage() = %v", err)
	}
	got3, err := readString(t, mr)
	if err != nil {
		t.Fatalf("ReadAll(msg3) = %v", err)
	}
	if got3 != want3 {
		t.Errorf("msg3 = %q, want %q", got3, want3)
	}
}

func TestReaderLimitErrorThenEOF(t *testing.T) {
	mbox := "From a@example.com Thu Jan  1 00:00:01 2015\n" +
		"From: a@example.com\n\n" + strings.Repeat("q", 100000) + "\n"

	r := NewReader(strings.NewReader(mbox))
	r.Limits = Limits{MaxRawBytes: 1024}

	mr, err := r.NextMessage()
	if err != nil {
		t.Fatalf("NextMessage() = %v", err)
	}
	if _, err := ioutil.ReadAll(mr); err != nil {
		limitError(t, err)
	} else {
		t.Fatal("ReadAll() succeeded, want LimitError")
	}

	// Draining the failed message reports the same budget violation.
	_, err = r.NextMessage()
	le := limitError(t, err)
	if le.Budget != BudgetRawBytes {
		t.Errorf("Budget = %v, want %v", le.Budget, BudgetRawBytes)
	}

	// No further boundary exists: the stream is at its end.
	if _, err := r.NextMessage(); err != io.EOF {
		t.Fatalf("NextMessage() = %v, want io.EOF", err)
	}
}

// shortReader returns at most max bytes per Read and counts the total number
// of bytes consumed from the underlying reader.
type shortReader struct {
	r   io.Reader
	max int
	n   int64
}

func (sr *shortReader) Read(p []byte) (int, error) {
	if len(p) > sr.max {
		p = p[:sr.max]
	}
	n, err := sr.r.Read(p)
	sr.n += int64(n)
	return n, err
}

func TestReaderLimitShortReadsRaw(t *testing.T) {
	body := strings.Repeat("z", 4<<20) + "\n" // 4 MiB single line
	mbox := "From a@example.com Thu Jan  1 00:00:01 2015\n" +
		"From: a@example.com\n\n" + body

	sr := &shortReader{r: strings.NewReader(mbox), max: 7}
	r := NewReader(sr)
	r.Limits = Limits{MaxRawBytes: 16 << 10}

	mr, err := r.NextMessage()
	if err != nil {
		t.Fatalf("NextMessage() = %v", err)
	}
	_, err = ioutil.ReadAll(mr)
	le := limitError(t, err)
	if le.Budget != BudgetRawBytes {
		t.Errorf("Budget = %v, want %v", le.Budget, BudgetRawBytes)
	}
	if sr.n > 128<<10 {
		t.Errorf("Reader consumed %d bytes from the source for a 16 KiB budget; the whole message was buffered", sr.n)
	}
}

func TestReaderLimitShortReadsOutput(t *testing.T) {
	body := strings.Repeat("z", 4<<20) + "\n" // 4 MiB single line
	mbox := "From a@example.com Thu Jan  1 00:00:01 2015\n" +
		"From: a@example.com\n\n" + body

	sr := &shortReader{r: strings.NewReader(mbox), max: 7}
	r := NewReader(sr)
	r.Limits = Limits{MaxOutputBytes: 8 << 10}

	mr, err := r.NextMessage()
	if err != nil {
		t.Fatalf("NextMessage() = %v", err)
	}
	b, err := ioutil.ReadAll(mr)
	le := limitError(t, err)
	if le.Budget != BudgetOutputBytes {
		t.Errorf("Budget = %v, want %v", le.Budget, BudgetOutputBytes)
	}
	if int64(len(b)) != 8<<10 {
		t.Errorf("ReadAll returned %d bytes, want exactly %d", len(b), 8<<10)
	}
	if sr.n > 128<<10 {
		t.Errorf("Reader consumed %d bytes from the source for an 8 KiB budget; the whole message was buffered", sr.n)
	}
}
