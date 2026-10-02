package mbox

import (
	"io"
	"io/ioutil"
	"strings"
	"testing"
	"time"
)

var testEnvelopeTime = time.Date(2015, time.January, 1, 0, 0, 1, 0, time.UTC)

func readAll(t *testing.T, r io.Reader) string {
	t.Helper()
	b, err := ioutil.ReadAll(r)
	if err != nil {
		t.Fatalf("reading message: %v", err)
	}
	return string(b)
}

func TestNextMessageWithEnvelope(t *testing.T) {
	const mbox = "From herp.derp@example.com Thu Jan  1 00:00:01 2015\r\n" +
		"Subject: Test\r\n\r\nbody\r\n"

	r := NewReader(strings.NewReader(mbox))
	rc, env, err := r.NextMessageWithEnvelope()
	if err != nil {
		t.Fatalf("NextMessageWithEnvelope() = %v", err)
	}

	if env.RawLine != "From herp.derp@example.com Thu Jan  1 00:00:01 2015" {
		t.Errorf("RawLine = %q", env.RawLine)
	}
	if env.From != "herp.derp@example.com" {
		t.Errorf("From = %q", env.From)
	}
	if !env.Date.Equal(testEnvelopeTime) {
		t.Errorf("Date = %v, want %v", env.Date, testEnvelopeTime)
	}

	if got := readAll(t, rc); got != "Subject: Test\r\n\r\nbody\r\n" {
		t.Errorf("body = %q", got)
	}

	if _, _, err := r.NextMessageWithEnvelope(); err != io.EOF {
		t.Errorf("NextMessageWithEnvelope() = %v, want io.EOF", err)
	}
}

func TestEnvelopeBelongsToCurrentMessage(t *testing.T) {
	const mbox = `From one@example.com Thu Jan  1 00:00:01 2015
Subject: One

body one

From two@example.com Fri Jan  2 00:00:01 2015
Subject: Two

body two

From three@example.com Sat Jan  3 00:00:01 2015
Subject: Three

body three
`

	r := NewReader(strings.NewReader(mbox))

	rc1, env1, err := r.NextMessageWithEnvelope()
	if err != nil {
		t.Fatal(err)
	}
	// Do not consume rc1 before asking for the next message: the previous
	// message must be skipped and the new envelope must describe message two.
	rc2, env2, err := r.NextMessageWithEnvelope()
	if err != nil {
		t.Fatal(err)
	}

	if env1.From != "one@example.com" || env2.From != "two@example.com" {
		t.Fatalf("envelopes leaked across messages: %q, %q", env1.From, env2.From)
	}
	_ = rc1

	if got := readAll(t, rc2); !strings.Contains(got, "body two") {
		t.Errorf("message two body = %q", got)
	}

	rc3, env3, err := r.NextMessageWithEnvelope()
	if err != nil {
		t.Fatal(err)
	}
	if env3.From != "three@example.com" {
		t.Errorf("From = %q, want three@example.com", env3.From)
	}
	if got := readAll(t, rc3); !strings.Contains(got, "body three") {
		t.Errorf("message three body = %q", got)
	}

	if _, _, err := r.NextMessageWithEnvelope(); err != io.EOF {
		t.Errorf("NextMessageWithEnvelope() = %v, want io.EOF", err)
	}
}

func TestMixedEntryPointsKeepPosition(t *testing.T) {
	const mbox = `From one@example.com Thu Jan  1 00:00:01 2015
Subject: One

body one

From two@example.com Fri Jan  2 00:00:01 2015
Subject: Two

body two

From three@example.com Sat Jan  3 00:00:01 2015
Subject: Three

body three
`

	r := NewReader(strings.NewReader(mbox))

	rc1, err := r.NextMessage()
	if err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, rc1); !strings.Contains(got, "body one") {
		t.Errorf("body one = %q", got)
	}

	rc2, env2, err := r.NextMessageWithEnvelope()
	if err != nil {
		t.Fatal(err)
	}
	if env2.From != "two@example.com" {
		t.Errorf("From = %q", env2.From)
	}
	if got := readAll(t, rc2); !strings.Contains(got, "body two") {
		t.Errorf("body two = %q", got)
	}

	rc3, err := r.NextMessage()
	if err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, rc3); !strings.Contains(got, "body three") {
		t.Errorf("body three = %q", got)
	}

	if _, err := r.NextMessage(); err != io.EOF {
		t.Errorf("NextMessage() = %v, want io.EOF", err)
	}
}

func TestEnvelopeEmptySender(t *testing.T) {
	const mbox = "From  Thu Jan  1 00:00:01 2015\nSubject: S\n\nbody\n"

	r := NewReader(strings.NewReader(mbox))
	_, env, err := r.NextMessageWithEnvelope()
	if err != nil {
		t.Fatal(err)
	}
	if env.From != "" {
		t.Errorf("From = %q, want empty", env.From)
	}
	if !env.Date.Equal(testEnvelopeTime) {
		t.Errorf("Date = %v, want %v", env.Date, testEnvelopeTime)
	}
}

func TestEnvelopeBareFromLine(t *testing.T) {
	const mbox = "From \nSubject: S\n\nbody\n"

	r := NewReader(strings.NewReader(mbox))
	_, env, err := r.NextMessageWithEnvelope()
	if err != nil {
		t.Fatal(err)
	}
	if env.From != "" || !env.Date.IsZero() || env.RawLine != "From " {
		t.Errorf("unexpected envelope: %+v", env)
	}
}

func TestEnvelopeUnknownDateLenient(t *testing.T) {
	const mbox = "From bob@example.com sometime last week\nSubject: S\n\nbody\n"

	r := NewReader(strings.NewReader(mbox))
	rc, env, err := r.NextMessageWithEnvelope()
	if err != nil {
		t.Fatalf("lenient mode returned error: %v", err)
	}
	if env.From != "bob@example.com" || !env.Date.IsZero() {
		t.Errorf("unexpected envelope: %+v", env)
	}
	if got := readAll(t, rc); !strings.Contains(got, "body") {
		t.Errorf("body = %q", got)
	}
}

func TestEnvelopeUnknownDateStrict(t *testing.T) {
	const mbox = "\nFrom bob@example.com sometime last week\nSubject: S\n\nbody\n"

	r := NewReader(strings.NewReader(mbox))
	r.Strict = true

	_, _, err := r.NextMessageWithEnvelope()
	ee, ok := err.(*EnvelopeError)
	if !ok {
		t.Fatalf("error = %v, want *EnvelopeError", err)
	}
	if ee.Err != ErrUnknownDate {
		t.Errorf("underlying error = %v, want ErrUnknownDate", ee.Err)
	}
	if ee.Line != 2 {
		t.Errorf("Line = %d, want 2", ee.Line)
	}
	if ee.RawLine != "From bob@example.com sometime last week" {
		t.Errorf("RawLine = %q", ee.RawLine)
	}
}

func TestStrictEnvelopeErrorIsRecoverable(t *testing.T) {
	const mbox = `From bad@example.com not a date
Subject: Bad

body bad

From good@example.com Sat Jan  3 00:00:01 2015
Subject: Good

body good
`

	r := NewReader(strings.NewReader(mbox))
	r.Strict = true

	_, _, err := r.NextMessageWithEnvelope()
	if ee, ok := err.(*EnvelopeError); !ok || ee.Err != ErrUnknownDate {
		t.Fatalf("first NextMessageWithEnvelope() = %v, want ErrUnknownDate", err)
	}

	rc, env, err := r.NextMessageWithEnvelope()
	if err != nil {
		t.Fatalf("second NextMessageWithEnvelope() = %v", err)
	}
	if env.From != "good@example.com" {
		t.Errorf("From = %q, want good@example.com", env.From)
	}
	if got := readAll(t, rc); !strings.Contains(got, "body good") {
		t.Errorf("body = %q", got)
	}

	if _, _, err := r.NextMessageWithEnvelope(); err != io.EOF {
		t.Errorf("NextMessageWithEnvelope() = %v, want io.EOF", err)
	}
}

func TestEnvelopeTrailingTextLenient(t *testing.T) {
	const mbox = "From bob@example.com Thu Jan  1 00:00:01 2015 junk\nSubject: S\n\nbody\n"

	r := NewReader(strings.NewReader(mbox))
	_, env, err := r.NextMessageWithEnvelope()
	if err != nil {
		t.Fatalf("lenient mode returned error: %v", err)
	}
	if !env.Date.Equal(testEnvelopeTime) {
		t.Errorf("Date = %v, want %v", env.Date, testEnvelopeTime)
	}
}

func TestEnvelopeTrailingTextStrict(t *testing.T) {
	const mbox = "From bob@example.com Thu Jan  1 00:00:01 2015 junk\nSubject: S\n\nbody\n"

	r := NewReader(strings.NewReader(mbox))
	r.Strict = true

	_, err := r.NextMessage()
	ee, ok := err.(*EnvelopeError)
	if !ok {
		t.Fatalf("error = %v, want *EnvelopeError", err)
	}
	if ee.Err != ErrTrailingText {
		t.Errorf("underlying error = %v, want ErrTrailingText", ee.Err)
	}
	if ee.Line != 1 {
		t.Errorf("Line = %d, want 1", ee.Line)
	}
}

func TestEnvelopeLongSeparatorLine(t *testing.T) {
	// bufio.Reader.ReadLine delivers long lines in fragments; the raw
	// separator must be reassembled in full.
	tail := strings.Repeat("x", 50000)
	raw := "From bob@example.com Thu Jan  1 00:00:01 2015 " + tail
	mbox := raw + "\nSubject: S\n\nbody\n"

	r := NewReader(strings.NewReader(mbox))
	_, env, err := r.NextMessageWithEnvelope()
	if err != nil {
		t.Fatal(err)
	}
	if env.RawLine != raw {
		t.Errorf("RawLine length = %d, want %d", len(env.RawLine), len(raw))
	}
	if env.From != "bob@example.com" {
		t.Errorf("From = %q", env.From)
	}
	if !env.Date.Equal(testEnvelopeTime) {
		t.Errorf("Date = %v, want %v", env.Date, testEnvelopeTime)
	}
}

func TestEnvelopeSeparatorWithoutFinalNewline(t *testing.T) {
	// The separator line itself is the last line of the stream.
	mbox := "From bob@example.com Thu Jan  1 00:00:01 2015"

	r := NewReader(strings.NewReader(mbox))
	_, env, err := r.NextMessageWithEnvelope()
	if err != nil {
		t.Fatal(err)
	}
	if env.RawLine != mbox {
		t.Errorf("RawLine = %q", env.RawLine)
	}

	if _, _, err := r.NextMessageWithEnvelope(); err != io.EOF {
		t.Errorf("NextMessageWithEnvelope() = %v, want io.EOF", err)
	}
}

func TestStrictAcceptsValidMessages(t *testing.T) {
	r := NewReader(strings.NewReader(mboxWithThreeMessages))
	r.Strict = true

	n := 0
	for {
		_, _, err := r.NextMessageWithEnvelope()
		if err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("NextMessageWithEnvelope() = %v", err)
		}
		n++
	}
	if n != 3 {
		t.Errorf("read %d messages, want 3", n)
	}
}
