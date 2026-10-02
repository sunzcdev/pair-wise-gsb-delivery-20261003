package mbox

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// mboxWithCorruption contains a fake separator line inside a message body
// and two consecutive corrupted regions between valid messages.
const mboxWithCorruption = `From alice@example.com Thu Jan  1 00:00:01 2015
From: alice@example.com
Subject: One

First message.

From bogus1
garbage line

From bogus2@example.com
more garbage

From bob@example.com Thu Jan  2 00:00:01 2015
From: bob@example.com
Subject: Two

Body with a fake separator:

From carol@example.com Thu Jan  3 00:00:01 2015
This is still body text.
More body text.

From dave@example.com Thu Jan  4 00:00:01 2015
From: dave@example.com
Subject: Three

Third message.
`

func readMessageText(t *testing.T, r io.Reader) string {
	t.Helper()
	var text bytes.Buffer
	if _, err := text.ReadFrom(r); err != nil {
		t.Fatalf("Unexpected error reading message body: %v", err)
	}
	return text.String()
}

func TestRecoveringReader(t *testing.T) {
	b := bytes.NewBufferString(mboxWithCorruption)
	m := NewRecoveringReader(b, 0)

	// First message: clean, no recovery.
	r, info, err := m.NextMessage()
	if err != nil {
		t.Fatalf("Unexpected error after NextMessage(): %v", err)
	}
	if want, got := "From: alice@example.com\r\nSubject: One\r\n\r\nFirst message.\r\n", readMessageText(t, r); got != want {
		t.Errorf("Expected:\n%q\ngot\n%q", want, got)
	}
	if info.Recovered {
		t.Errorf("Expected Recovered to be false, got %v", info.Recovered)
	}
	if info.Offset != 0 {
		t.Errorf("Expected offset 0, got %v", info.Offset)
	}
	if len(info.Skipped) != 0 {
		t.Errorf("Expected no skipped regions, got %v", info.Skipped)
	}

	// Second message: recovered after two consecutive corrupted regions.
	r, info, err = m.NextMessage()
	if err != nil {
		t.Fatalf("Unexpected error after NextMessage(): %v", err)
	}
	if want, got := "From: bob@example.com\r\nSubject: Two\r\n\r\nBody with a fake separator:\r\n", readMessageText(t, r); got != want {
		t.Errorf("Expected:\n%q\ngot\n%q", want, got)
	}
	if !info.Recovered {
		t.Errorf("Expected Recovered to be true, got %v", info.Recovered)
	}
	if want := int64(strings.Index(mboxWithCorruption, "From bob@example.com")); info.Offset != want {
		t.Errorf("Expected offset %v, got %v", want, info.Offset)
	}
	if len(info.Skipped) != 2 {
		t.Fatalf("Expected 2 skipped regions, got %v", info.Skipped)
	}
	for i, want := range []string{
		"From bogus1\ngarbage line\n",
		"From bogus2@example.com\nmore garbage\n",
	} {
		got := mboxWithCorruption[info.Skipped[i].Offset : info.Skipped[i].Offset+info.Skipped[i].Length]
		if got != want {
			t.Errorf("Skipped region %d: expected %q, got %q", i, want, got)
		}
		if wantOffset := int64(strings.Index(mboxWithCorruption, want)); info.Skipped[i].Offset != wantOffset {
			t.Errorf("Skipped region %d: expected offset %v, got %v", i, wantOffset, info.Skipped[i].Offset)
		}
	}

	// Third message: recovered after the fake separator and the rest of
	// the second message body were skipped.
	r, info, err = m.NextMessage()
	if err != nil {
		t.Fatalf("Unexpected error after NextMessage(): %v", err)
	}
	if want, got := "From: dave@example.com\r\nSubject: Three\r\n\r\nThird message.\r\n", readMessageText(t, r); got != want {
		t.Errorf("Expected:\n%q\ngot\n%q", want, got)
	}
	if !info.Recovered {
		t.Errorf("Expected Recovered to be true, got %v", info.Recovered)
	}
	if want := int64(strings.Index(mboxWithCorruption, "From dave@example.com")); info.Offset != want {
		t.Errorf("Expected offset %v, got %v", want, info.Offset)
	}
	if len(info.Skipped) != 1 {
		t.Fatalf("Expected 1 skipped region, got %v", info.Skipped)
	}
	want := "From carol@example.com Thu Jan  3 00:00:01 2015\nThis is still body text.\nMore body text.\n"
	got := mboxWithCorruption[info.Skipped[0].Offset : info.Skipped[0].Offset+info.Skipped[0].Length]
	if got != want {
		t.Errorf("Skipped region: expected %q, got %q", want, got)
	}

	if _, _, err := m.NextMessage(); err != io.EOF {
		t.Fatalf("Unexpected error after NextMessage(): %v", err)
	}
}

func TestRecoveringReaderScanLimit(t *testing.T) {
	b := bytes.NewBufferString(mboxWithCorruption)
	m := NewRecoveringReader(b, 10)

	if _, _, err := m.NextMessage(); err != nil {
		t.Fatalf("Unexpected error after NextMessage(): %v", err)
	}
	if _, _, err := m.NextMessage(); err != ErrRecoveryLimit {
		t.Fatalf("Expected ErrRecoveryLimit, got %v", err)
	}
}

func TestRecoveringReaderCleanMbox(t *testing.T) {
	b := bytes.NewBufferString(mboxWithThreeMessages)
	m := NewRecoveringReader(b, 0)

	n := 0
	for {
		r, info, err := m.NextMessage()
		if err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("Unexpected error after NextMessage(): %v", err)
		}
		if info.Recovered {
			t.Errorf("Expected Recovered to be false, got %v", info.Recovered)
		}
		readMessageText(t, r)
		n++
	}
	if n != 3 {
		t.Fatalf("Expected 3 messages, got %v", n)
	}
}

// TestStrictReaderStopsAtCorruption ensures the default strict Reader still
// stops at the first format error.
func TestStrictReaderStopsAtCorruption(t *testing.T) {
	corrupted := "garbage\n\n" + mboxWithOneMessage

	m := NewReader(bytes.NewBufferString(corrupted))
	if _, err := m.NextMessage(); err != ErrInvalidFormat {
		t.Fatalf("Expected ErrInvalidFormat, got %v", err)
	}

	rm := NewRecoveringReader(bytes.NewBufferString(corrupted), 0)
	r, info, err := rm.NextMessage()
	if err != nil {
		t.Fatalf("Unexpected error after NextMessage(): %v", err)
	}
	if want, got := toCRLF(mboxFirstMessage), readMessageText(t, r); got != want {
		t.Errorf("Expected:\n%q\ngot\n%q", want, got)
	}
	if !info.Recovered {
		t.Errorf("Expected Recovered to be true, got %v", info.Recovered)
	}
	if len(info.Skipped) != 1 {
		t.Fatalf("Expected 1 skipped region, got %v", info.Skipped)
	}
	if want := "garbage\n"; corrupted[info.Skipped[0].Offset:info.Skipped[0].Offset+info.Skipped[0].Length] != want {
		t.Errorf("Expected skipped region %q, got %q", want, corrupted[info.Skipped[0].Offset:info.Skipped[0].Offset+info.Skipped[0].Length])
	}
}
