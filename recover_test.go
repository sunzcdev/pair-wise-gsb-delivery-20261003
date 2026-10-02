package mbox

import (
	"bytes"
	"fmt"
	"io"
	"net/mail"
	"strings"
	"testing"
)

// mboxWithCorruption contains a fake separator line in the body of the first
// message and two consecutive corrupted regions (an invalid separator line
// followed by garbage) before the second message.
const mboxWithCorruption = `From alice@example.com Thu Jan  1 00:00:01 2015
From: alice@example.com
Subject: First

Body of the first message.

From Herp Derp with love.

Rest of the first message body.

From corrupted@example.com Thu Jan  1 00:00:99 2015
corrupted data block line one
corrupted data block line two
From bob@example.com Thu Jan  2 00:00:01 2015
From: bob@example.com
Subject: Second

Body of the second message.
`

const mboxWithGarbageStart = `this mbox file is corrupted
at its very beginning
From carol@example.com Thu Jan  3 00:00:01 2015
From: carol@example.com
Subject: Hello

Body.
`

func readMessageText(t *testing.T, msg *Message) string {
	t.Helper()
	var text bytes.Buffer
	if _, err := text.ReadFrom(msg); err != nil {
		t.Fatalf("Unexpected error reading message body: %v", err)
	}
	return text.String()
}

func TestRecoveringReaderWithCorruption(t *testing.T) {
	m := NewRecoveringReader(strings.NewReader(mboxWithCorruption))

	msg, err := m.NextMessage()
	if err != nil {
		t.Fatalf("Unexpected error after NextMessage(): %v", err)
	}
	if msg.Recovered {
		t.Errorf("Expected Recovered to be false for first message")
	}
	if len(msg.Skipped) != 0 {
		t.Errorf("Expected no skipped regions, got %v", msg.Skipped)
	}
	if msg.Offset != 0 {
		t.Errorf("Expected offset 0, got %d", msg.Offset)
	}
	want := toCRLF("From: alice@example.com\nSubject: First\n\nBody of the first message.\n")
	if text := readMessageText(t, msg); text != want {
		t.Errorf("Expected:\n %q\ngot\n%q", want, text)
	}

	msg, err = m.NextMessage()
	if err != nil {
		t.Fatalf("Unexpected error after NextMessage(): %v", err)
	}
	if !msg.Recovered {
		t.Errorf("Expected Recovered to be true for second message")
	}
	wantOffset := int64(strings.Index(mboxWithCorruption, "From bob@example.com"))
	if msg.Offset != wantOffset {
		t.Errorf("Expected offset %d, got %d", wantOffset, msg.Offset)
	}

	regions := []struct {
		text             string
		invalidSeparator bool
	}{
		{"From Herp Derp with love.\n", true},
		{"Rest of the first message body.\n", false},
		{"From corrupted@example.com Thu Jan  1 00:00:99 2015\n", true},
		{"corrupted data block line one\ncorrupted data block line two\n", false},
	}
	if len(msg.Skipped) != len(regions) {
		t.Fatalf("Expected %d skipped regions, got %v", len(regions), msg.Skipped)
	}
	for i, region := range regions {
		got := msg.Skipped[i]
		wantOffset := int64(strings.Index(mboxWithCorruption, region.text))
		if got.Offset != wantOffset || got.Length != int64(len(region.text)) {
			t.Errorf("Region %d: expected offset %d length %d, got offset %d length %d",
				i, wantOffset, len(region.text), got.Offset, got.Length)
		}
		if got.InvalidSeparator != region.invalidSeparator {
			t.Errorf("Region %d: expected InvalidSeparator %v, got %v",
				i, region.invalidSeparator, got.InvalidSeparator)
		}
		if skipped := mboxWithCorruption[got.Offset : got.Offset+got.Length]; skipped != region.text {
			t.Errorf("Region %d: expected skipped text %q, got %q", i, region.text, skipped)
		}
	}

	want = toCRLF("From: bob@example.com\nSubject: Second\n\nBody of the second message.\n")
	if text := readMessageText(t, msg); text != want {
		t.Errorf("Expected:\n %q\ngot\n%q", want, text)
	}

	if _, err := m.NextMessage(); err != io.EOF {
		t.Fatalf("Unexpected error after NextMessage(): %v", err)
	}
}

func TestRecoveringReaderWithGarbageStart(t *testing.T) {
	// The strict Reader stops at the format error.
	if _, err := NewReader(strings.NewReader(mboxWithGarbageStart)).NextMessage(); err != ErrInvalidFormat {
		t.Fatalf("Expected ErrInvalidFormat from strict Reader, got %v", err)
	}

	m := NewRecoveringReader(strings.NewReader(mboxWithGarbageStart))

	msg, err := m.NextMessage()
	if err != nil {
		t.Fatalf("Unexpected error after NextMessage(): %v", err)
	}
	if !msg.Recovered {
		t.Errorf("Expected Recovered to be true")
	}
	wantOffset := int64(strings.Index(mboxWithGarbageStart, "From carol@example.com"))
	if msg.Offset != wantOffset {
		t.Errorf("Expected offset %d, got %d", wantOffset, msg.Offset)
	}

	garbage := "this mbox file is corrupted\nat its very beginning\n"
	if len(msg.Skipped) != 1 {
		t.Fatalf("Expected 1 skipped region, got %v", msg.Skipped)
	}
	got := msg.Skipped[0]
	if got.Offset != 0 || got.Length != int64(len(garbage)) || got.InvalidSeparator {
		t.Errorf("Expected skipped region {0 %d false}, got %+v", len(garbage), got)
	}

	want := toCRLF("From: carol@example.com\nSubject: Hello\n\nBody.\n")
	if text := readMessageText(t, msg); text != want {
		t.Errorf("Expected:\n %q\ngot\n%q", want, text)
	}

	if _, err := m.NextMessage(); err != io.EOF {
		t.Fatalf("Unexpected error after NextMessage(): %v", err)
	}
}

func TestRecoveringReaderScanLimit(t *testing.T) {
	m := NewRecoveringReader(strings.NewReader(mboxWithGarbageStart))
	m.MaxScanBytes = 4

	if _, err := m.NextMessage(); err != ErrRecoveryLimit {
		t.Fatalf("Expected ErrRecoveryLimit, got %v", err)
	}

	// The scan can be resumed with a larger budget.
	m.MaxScanBytes = 0
	msg, err := m.NextMessage()
	if err != nil {
		t.Fatalf("Unexpected error after NextMessage(): %v", err)
	}
	if !msg.Recovered {
		t.Errorf("Expected Recovered to be true")
	}
}

func TestRecoveringReaderClean(t *testing.T) {
	m := NewRecoveringReader(strings.NewReader(mboxWithThreeMessages))

	var offsets []int64
	if strings.HasPrefix(mboxWithThreeMessages, "From ") {
		offsets = append(offsets, 0)
	}
	for i := 0; i < len(mboxWithThreeMessages); {
		j := strings.Index(mboxWithThreeMessages[i:], "\nFrom ")
		if j < 0 {
			break
		}
		offsets = append(offsets, int64(i+j+1))
		i += j + 1
	}

	for i := 0; i < 3; i++ {
		msg, err := m.NextMessage()
		if err != nil {
			t.Fatalf("Unexpected error after NextMessage(): %v", err)
		}
		if msg.Recovered {
			t.Errorf("Expected Recovered to be false for message %d", i)
		}
		if msg.Offset != offsets[i] {
			t.Errorf("Message %d: expected offset %d, got %d", i, offsets[i], msg.Offset)
		}
		readMessageText(t, msg)
	}

	if _, err := m.NextMessage(); err != io.EOF {
		t.Fatalf("Unexpected error after NextMessage(): %v", err)
	}
}

func ExampleRecoveringReader() {
	mr := NewRecoveringReader(strings.NewReader(mboxWithCorruption))
	for {
		msg, err := mr.NextMessage()
		if err == io.EOF {
			break
		} else if err != nil {
			fmt.Println("Oops, something went wrong!", err)
			return
		}

		m, err := mail.ReadMessage(msg)
		if err != nil {
			fmt.Println("Oops, something went wrong!", err)
			return
		}

		fmt.Printf("Message %q at offset %d", m.Header.Get("Subject"), msg.Offset)
		if msg.Recovered {
			fmt.Printf(" (recovered, %d bytes skipped in %d regions)", skippedBytes(msg.Skipped), len(msg.Skipped))
		}
		fmt.Println()
	}

	// Output:
	// Message "First" at offset 0
	// Message "Second" at offset 288 (recovered, 170 bytes skipped in 4 regions)
}

func skippedBytes(regions []SkippedRegion) int64 {
	var n int64
	for _, r := range regions {
		n += r.Length
	}
	return n
}
