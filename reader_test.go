package mbox

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"net/mail"
	"strings"
	"testing"
	"time"
)

const mboxWithOneMessage = `From herp.derp@example.com Thu Jan  1 00:00:01 2015
From: herp.derp@example.com (Herp Derp)
Date: Thu, 01 Jan 2015 00:00:01 +0100
Subject: Test

This is a simple test.

And, by the way, this is how a "From" line is escaped in mboxo format:

>From Herp Derp with love.

Bye.
`

const mboxWithThreeMessages = `From herp.derp@example.com Thu Jan  1 00:00:01 2015
From: herp.derp@example.com (Herp Derp)
Date: Thu, 01 Jan 2015 00:00:01 +0100
Subject: Test

This is a simple test.

And, by the way, this is how a "From" line is escaped in mboxo format:

>From Herp Derp with love.

Bye.

From derp.herp@example.com Thu Jan  1 00:00:01 2015
From: derp.herp@example.com (Derp Herp)
Date: Thu, 02 Jan 2015 00:00:01 +0100
Subject: Another test

This is another simple test.

Another line.

Bye.

From bernd.lauert@example.com Thu Jan  3 00:00:01 2015
From: bernd.lauert@example.com (Bernd Lauert)
Date: Thu, 03 Jan 2015 00:00:01 +0100
Subject: A last test

This is the last simple test.

Bye.
`

const mboxWithStartingLF = `
From herp.derp@example.com Thu Jan  1 00:00:01 2015
From: herp.derp@example.com (Herp Derp)
Date: Thu, 01 Jan 2015 00:00:01 +0100
Subject: Test

This is a simple test.

And, by the way, this is how a "From" line is escaped in mboxo format:

>From Herp Derp with love.

Bye.

From derp.herp@example.com Thu Jan  1 00:00:01 2015
From: derp.herp@example.com (Derp Herp)
Date: Thu, 02 Jan 2015 00:00:01 +0100
Subject: Another test

This is another simple test.

Another line.

Bye.

From bernd.lauert@example.com Thu Jan  3 00:00:01 2015
From: bernd.lauert@example.com (Bernd Lauert)
Date: Thu, 03 Jan 2015 00:00:01 +0100
Subject: A last test

This is the last simple test.

Bye.
`

const mboxWithThreeMessagesMalformedButValid = `From herp.derp@example.com Thu Jan  1 00:00:01 2015
From: herp.derp@example.com (Herp Derp)
Date: Thu, 01 Jan 2015 00:00:01 +0100
Subject: Test

This is a simple test.

And, by the way, this is how a "From" line is escaped in mboxo format:

>From Herp Derp with love.

Bye.
From derp.herp@example.com Thu Jan  1 00:00:01 2015
From: derp.herp@example.com (Derp Herp)
Date: Thu, 02 Jan 2015 00:00:01 +0100
Subject: Another test

This is another simple test.

Another line.

Bye.

From bernd.lauert@example.com Thu Jan  3 00:00:01 2015
From: bernd.lauert@example.com (Bernd Lauert)
Date: Thu, 03 Jan 2015 00:00:01 +0100
Subject: A last test

This is the last simple test.

Bye.
`

const mboxWithOneMessageMissingSeparator = `From: herp.derp@example.com (Herp Derp)
Date: Thu, 01 Jan 2015 00:00:01 +0100
Subject: Test

This is a simple test.

And, by the way, this is how a "From" line is escaped in mboxo format:

>From Herp Derp with love.

Bye.
`

const mboxFirstMessage = `From: herp.derp@example.com (Herp Derp)
Date: Thu, 01 Jan 2015 00:00:01 +0100
Subject: Test

This is a simple test.

And, by the way, this is how a "From" line is escaped in mboxo format:

From Herp Derp with love.

Bye.
`

func toCRLF(s string) string {
	return strings.Replace(s, "\n", "\r\n", -1)
}

func testMboxMessage(t *testing.T, mbox string, count int) {
	b := bytes.NewBufferString(mbox)
	m := NewReader(b)

	for i := 0; i < count; i++ {
		r, err := m.NextMessage()
		if err != nil {
			t.Fatalf("Unexpected error after NextMessage(): %v", err)
		}

		var text bytes.Buffer
		_, err = text.ReadFrom(r)
		if err != nil {
			t.Errorf("Unexpected error reading message body: %v", err)
		}
		want := toCRLF(mboxFirstMessage)
		if i == 0 && text.String() != want {
			t.Errorf("Expected:\n %q\ngot\n%q", want, text.String())
		}
	}

	if _, err := m.NextMessage(); err != io.EOF {
		t.Fatalf("Unexpected error after NextMessage(): %v", err)
	}
}

func TestMboxMessageWithOneMessage(t *testing.T) {
	testMboxMessage(t, mboxWithOneMessage, 1)
}

func TestMboxMessageWithThreeMessages(t *testing.T) {
	testMboxMessage(t, mboxWithThreeMessages, 3)
}

func TestMboxMessageWithStartingLF(t *testing.T) {
	testMboxMessage(t, mboxWithStartingLF, 3)
}

func TestMboxMessageWithThreeMessagesMalformedButValid(t *testing.T) {
	testMboxMessage(t, mboxWithThreeMessagesMalformedButValid, 3)
}

func testMboxMessageInvalid(t *testing.T, mbox string) {
	b := bytes.NewBufferString(mbox)
	m := NewReader(b)

	if _, err := m.NextMessage(); err == nil {
		t.Errorf("Missing error after Next(): %v", err)
	}
}

func TestMboxMessageWithOneMessageMissingSeparator(t *testing.T) {
	testMboxMessageInvalid(t, mboxWithOneMessageMissingSeparator)
}

func TestMboxMessageNoRead(t *testing.T) {
	b := bytes.NewBufferString(mboxWithThreeMessages)
	m := NewReader(b)

	n := 0
	for {
		_, err := m.NextMessage()
		if err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("Unexpected error after NextMessage(): %v", err)
		}
		n++
	}

	if _, err := m.NextMessage(); err != io.EOF {
		t.Fatalf("Unexpected error after NextMessage(): %v", err)
	}
	if n != 3 {
		t.Fatalf("Expected 3 mesages, got %v", n)
	}
}

// TODO: decide whether we should keep this test or not
func DisabledTestScanMessageWithBoundaries(t *testing.T) {
	sourceData := `
From derp.herp@example.com Thu Jan  1 00:00:01 2015
From: herp.derp@example.com (Herp Derp)
Date: Thu, 01 Jan 2015 00:00:01 +0100
Subject: Test
Content-Type: multipart/alternative;
        boundary=Apple-Mail-D55D9B1A-A379-4D5C-BDA9-00D35DF424A0

This is a test of boundaries.  Don't accept a new email via \nFrom until the boundary is done!'

And, by the way, this is how a "From" line is escaped in mboxo format:
From Herp Derp with love.

From Herp Derp with love.

Bye.
--Apple-Mail-D55D9B1A-A379-4D5C-BDA9-00D35DF424A0--

From derp.herp@example.com Thu Jan  1 00:00:01 2015
From: herp.derp@example.com (Herp Derp)
Date: Thu, 01 Jan 2015 00:00:01 +0100
Subject: Test

This is the second email in a test of boundaries.
`
	expected := []string{
		"This is a test of boundaries.  Don't accept a new email via \\nFrom until the boundary is done!'\n\nAnd, by the way, this is how a \"From\" line is escaped in mboxo format:\nFrom Herp Derp with love.\n\nFrom Herp Derp with love.\n\nBye.\n--Apple-Mail-D55D9B1A-A379-4D5C-BDA9-00D35DF424A0--\n",
		"This is the second email in a test of boundaries.\n",
	}
	b := bytes.NewBufferString(sourceData)
	m := NewReader(b)

	for i := range expected {
		r, err := m.NextMessage()
		if err != nil {
			t.Fatalf("Unexpected error after NextMessage(): %v", err)
		}

		msg, err := mail.ReadMessage(r)
		if err != nil {
			t.Fatalf("mail.ReadMessage() = %v", err)
		}

		var body bytes.Buffer
		_, err = body.ReadFrom(msg.Body)
		if err != nil {
			t.Errorf("%d - Unexpected error reading message body: %v", i, err)
			continue
		}
		want := toCRLF(expected[i])
		if body.String() != want {
			t.Errorf("%d - Expected:\n %q\ngot\n%q", i, want, body.String())
		}
	}

	if _, err := m.NextMessage(); err != io.EOF {
		t.Errorf("Next() succeeded")
	}
}

func TestScanMessageWithTextBoundary(t *testing.T) {
	sourceData := `
From derp.herp@example.com Thu Jan  1 00:00:01 2015
From: herp.derp@example.com (Herp Derp)
Date: Thu, 01 Jan 2015 00:00:01 +0100
Subject: Test
Content-Type: text/html; charset="utf-8";
 boundary="monkey_d3df4dc8-da5e-47dd-be15-f19c5ed55194"

This is a test of boundaries.  Don't accept a new email via \nFrom until the boundary is done!'

And, by the way, this is how a "From" line is escaped in mboxo format:

Bye.

From derp.herp@example.com Thu Jan  1 00:00:01 2015
From: herp.derp@example.com (Herp Derp)
Date: Thu, 01 Jan 2015 00:00:01 +0100
Subject: Test

This is the second email in a test of boundaries.
`
	expected := []string{
		"This is a test of boundaries.  Don't accept a new email via \\nFrom until the boundary is done!'\n\nAnd, by the way, this is how a \"From\" line is escaped in mboxo format:\n\nBye.\n",
		"This is the second email in a test of boundaries.\n",
	}
	b := bytes.NewBufferString(sourceData)
	m := NewReader(b)

	for i := range expected {
		r, err := m.NextMessage()
		if err != nil {
			t.Fatalf("Unexpected error after NextMessage(): %v", err)
		}

		msg, err := mail.ReadMessage(r)
		if err != nil {
			t.Fatalf("mail.ReadMessage() = %v", err)
		}

		var body bytes.Buffer
		_, err = body.ReadFrom(msg.Body)
		if err != nil {
			t.Errorf("%d - Unexpected error reading message body: %v", i, err)
			continue
		}
		want := toCRLF(expected[i])
		if body.String() != want {
			t.Errorf("%d - Expected:\n %q\ngot\n%q", i, want, body.String())
		}
	}

	if _, err := m.NextMessage(); err != io.EOF {
		t.Errorf("Next() succeeded")
	}
}

func TestScanMessageWithBoundarySemicolon(t *testing.T) {
	mbox := `From notifications@github.com Tue Jun  7 05:46:46 2016
From: Sender <notifications@github.com>
To: foo/bar <bar@noreply.github.com>
Message-ID: <foo/bar/1/0@github.com>
Subject: Re: [foo/bar] [question] Baz? (#1)
Content-Type: multipart/alternative; boundary="--==_mimepart_5755da228145a_38da3facdf97329c42987b";
MIME-Version: 1.0


----==_mimepart_5755da228145a_38da3facdf97329c42987b
Content-Type: text/plain; charset="UTF-8"
Content-Transfer-Encoding: 8bit

Blah blah

----==_mimepart_5755da228145a_38da3facdf97329c42987b
Content-Type: text/html; charset="UTF-8"
Content-Transfer-Encoding: 8bit

<meta http-equiv="Content-Type" content="text/html; charset=utf-8">
<p>Blah blah</p>

----==_mimepart_5755da228145a_38da3facdf97329c42987b--

From notifications@github.com Tue Jun  7 05:52:15 2016
From: Author <notifications@github.com>
To: frob/blab <blab@noreply.github.com>
Message-ID: <frob/blab/1/0@github.com>
Subject: Re: [frob/blab] [question] Bling? (#1)
Content-Type: multipart/alternative; boundary="--==_mimepart_5755db739a819_79783f996b0172c04025ee";
MIME-Version: 1.0


----==_mimepart_5755db739a819_79783f996b0172c04025ee
Content-Type: text/plain; charset="UTF-8"
Content-Transfer-Encoding: 8bit

Blah blah

----==_mimepart_5755db739a819_79783f996b0172c04025ee
Content-Type: text/html; charset="UTF-8"
Content-Transfer-Encoding: 8bit

<meta http-equiv="Content-Type" content="text/html; charset=utf-8">
<p>Blah blah</p>

----==_mimepart_5755db739a819_79783f996b0172c04025ee--

`
	expected := 2

	b := bytes.NewBufferString(mbox)
	m := NewReader(b)

	n := 0
	for {
		_, err := m.NextMessage()
		if err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("m.NextMessaage() = %v", err)
		}
		n += 1
	}

	if n != expected {
		t.Errorf("Expected: %d; got: %d", expected, n)
	}
}

func TestReadingMessageWithLongLine(t *testing.T) {
	// We are testing longer line than what `bufio.MaxScanTokenSize` defines.
	// RFC 5322 section 2.1.1 says line MUST be less than 998 bytes long, but
	// some providers generates messages with such long lines.
	want := strings.Repeat("This is very long line.", 5000) // Over 100k bytes.
	mbox := fmt.Sprintf(`From herp.derp@example.com Thu Jan  1 00:00:01 2015
From: herp.derp@example.com (Herp Derp)
Date: Thu, 01 Jan 2015 00:00:01 +0100
Subject: Test

%s`, want)

	b := bytes.NewBufferString(mbox)
	m := NewReader(b)

	r, err := m.NextMessage()
	if err != nil {
		t.Fatalf("m.NextMessaage() = %v", err)
	}

	msg, err := mail.ReadMessage(r)
	if err != nil {
		t.Fatalf("mail.ReadMessage() = %v", err)
	}

	var body bytes.Buffer
	_, err = body.ReadFrom(msg.Body)
	if err != nil {
		t.Errorf("body.ReadFrom() = %v", err)
	}

	if body.String() != want+"\r\n" {
		t.Errorf("Expected:\n %q\ngot\n%q", want, body.String())
	}

	_, err = m.NextMessage()
	if err != io.EOF {
		t.Fatalf("m.NextMessage() = %v", err)
	}
}

func ExampleReader() {
	r := strings.NewReader(`From herp.derp@example.com Thu Jan  1 00:00:01 2015
From: herp.derp@example.com (Herp Derp)
Date: Thu, 01 Jan 2015 00:00:01 +0100
Subject: Test

This is a simple test.

CU.

From derp.herp@example.com Thu Jan  1 00:00:01 2015
From: derp.herp@example.com (Derp Herp)
Date: Thu, 02 Jan 2015 00:00:01 +0100
Subject: Another test

This is another simple test.

Bye.
`)

	mr := NewReader(r)
	for {
		r, err := mr.NextMessage()
		if err == io.EOF {
			break
		} else if err != nil {
			fmt.Print("Oops, something went wrong!", err)
			return
		}

		msg, err := mail.ReadMessage(r)
		if err != nil {
			fmt.Print("Oops, something went wrong!", err)
			return
		}

		fmt.Printf("Message from %v\n", msg.Header.Get("From"))
	}

	// Output:
	// Message from herp.derp@example.com (Herp Derp)
	// Message from derp.herp@example.com (Derp Herp)
}

func TestNextMessageWithEnvelope(t *testing.T) {
	b := bytes.NewBufferString(mboxWithThreeMessages)
	m := NewReader(b)

	wantFrom := []string{
		"herp.derp@example.com",
		"derp.herp@example.com",
		"bernd.lauert@example.com",
	}
	wantDate := []time.Time{
		time.Date(2015, 1, 1, 0, 0, 1, 0, time.UTC),
		time.Date(2015, 1, 1, 0, 0, 1, 0, time.UTC),
		time.Date(2015, 1, 3, 0, 0, 1, 0, time.UTC),
	}
	wantLine := []string{
		"From herp.derp@example.com Thu Jan  1 00:00:01 2015",
		"From derp.herp@example.com Thu Jan  1 00:00:01 2015",
		"From bernd.lauert@example.com Thu Jan  3 00:00:01 2015",
	}

	for i := 0; i < 3; i++ {
		env, r, err := m.NextMessageWithEnvelope()
		if err != nil {
			t.Fatalf("Unexpected error after NextMessageWithEnvelope(): %v", err)
		}
		if env.From != wantFrom[i] {
			t.Errorf("Message %d: expected From %q, got %q", i, wantFrom[i], env.From)
		}
		if !env.Date.Equal(wantDate[i]) {
			t.Errorf("Message %d: expected Date %v, got %v", i, wantDate[i], env.Date)
		}
		if env.Line != wantLine[i] {
			t.Errorf("Message %d: expected Line %q, got %q", i, wantLine[i], env.Line)
		}
		if r == nil {
			t.Errorf("Message %d: expected non-nil message reader", i)
		}
	}

	if _, _, err := m.NextMessageWithEnvelope(); err != io.EOF {
		t.Fatalf("Unexpected error after NextMessageWithEnvelope(): %v", err)
	}
}

func TestNextMessageWithEnvelopeBody(t *testing.T) {
	b := bytes.NewBufferString(mboxWithOneMessage)
	m := NewReader(b)

	env, r, err := m.NextMessageWithEnvelope()
	if err != nil {
		t.Fatalf("Unexpected error after NextMessageWithEnvelope(): %v", err)
	}
	if env.From != "herp.derp@example.com" {
		t.Errorf("Expected From %q, got %q", "herp.derp@example.com", env.From)
	}

	var text bytes.Buffer
	if _, err := text.ReadFrom(r); err != nil {
		t.Fatalf("Unexpected error reading message body: %v", err)
	}
	if want := toCRLF(mboxFirstMessage); text.String() != want {
		t.Errorf("Expected:\n %q\ngot\n%q", want, text.String())
	}
}

func TestNextMessageWithEnvelopeLenientBadDate(t *testing.T) {
	b := bytes.NewBufferString(`From herp.derp@example.com this is not a date
From: herp.derp@example.com (Herp Derp)

Body.
`)
	m := NewReader(b)

	env, r, err := m.NextMessageWithEnvelope()
	if err != nil {
		t.Fatalf("Unexpected error after NextMessageWithEnvelope(): %v", err)
	}
	if env.From != "herp.derp@example.com" {
		t.Errorf("Expected From %q, got %q", "herp.derp@example.com", env.From)
	}
	if !env.Date.IsZero() {
		t.Errorf("Expected zero Date, got %v", env.Date)
	}
	if env.Line != "From herp.derp@example.com this is not a date" {
		t.Errorf("Unexpected Line: %q", env.Line)
	}

	body, err := ioutil.ReadAll(r)
	if err != nil {
		t.Fatalf("Unexpected error reading message body: %v", err)
	}
	if !strings.Contains(string(body), "Body.") {
		t.Errorf("Expected message body to be readable, got %q", body)
	}
}

func TestNextMessageWithEnvelopeStrictBadDate(t *testing.T) {
	mbox := `From herp.derp@example.com Thu Jan  1 00:00:01 2015
From: herp.derp@example.com (Herp Derp)

First.

From derp.herp@example.com not a date
From: derp.herp@example.com (Derp Herp)

Second.
`
	b := bytes.NewBufferString(mbox)
	m := NewReaderWithOptions(b, ReaderOptions{Strict: true})

	env, _, err := m.NextMessageWithEnvelope()
	if err != nil {
		t.Fatalf("Unexpected error after NextMessageWithEnvelope(): %v", err)
	}
	if env.From != "herp.derp@example.com" {
		t.Errorf("Expected From %q, got %q", "herp.derp@example.com", env.From)
	}

	_, _, err = m.NextMessageWithEnvelope()
	if err == nil {
		t.Fatal("Expected error after NextMessageWithEnvelope()")
	}
	perr, ok := err.(*ParseError)
	if !ok {
		t.Fatalf("Expected *ParseError, got %T (%v)", err, err)
	}
	if perr.Message != 2 {
		t.Errorf("Expected Message 2, got %d", perr.Message)
	}
	if perr.Line != "From derp.herp@example.com not a date" {
		t.Errorf("Unexpected Line: %q", perr.Line)
	}

	// The error is sticky.
	if _, _, err := m.NextMessageWithEnvelope(); err != perr {
		t.Fatalf("Expected sticky error %v, got %v", perr, err)
	}
	if _, err := m.NextMessage(); err != perr {
		t.Fatalf("Expected sticky error %v, got %v", perr, err)
	}
}

func TestNextMessageWithEnvelopeEmptySender(t *testing.T) {
	mbox := "From  Thu Jan  1 00:00:01 2015\n" +
		"From: herp.derp@example.com (Herp Derp)\n" +
		"\n" +
		"Body.\n"
	b := bytes.NewBufferString(mbox)
	m := NewReader(b)

	env, _, err := m.NextMessageWithEnvelope()
	if err != nil {
		t.Fatalf("Unexpected error after NextMessageWithEnvelope(): %v", err)
	}
	if env.From != "" {
		t.Errorf("Expected empty From, got %q", env.From)
	}
	if want := time.Date(2015, 1, 1, 0, 0, 1, 0, time.UTC); !env.Date.Equal(want) {
		t.Errorf("Expected Date %v, got %v", want, env.Date)
	}
}

func TestNextMessageWithEnvelopeTrailingText(t *testing.T) {
	mbox := "From herp.derp@example.com Thu Jan  1 00:00:01 2015 some trailing text\n" +
		"From: herp.derp@example.com (Herp Derp)\n" +
		"\n" +
		"Body.\n"
	b := bytes.NewBufferString(mbox)
	m := NewReader(b)

	env, _, err := m.NextMessageWithEnvelope()
	if err != nil {
		t.Fatalf("Unexpected error after NextMessageWithEnvelope(): %v", err)
	}
	if env.From != "herp.derp@example.com" {
		t.Errorf("Expected From %q, got %q", "herp.derp@example.com", env.From)
	}
	if want := time.Date(2015, 1, 1, 0, 0, 1, 0, time.UTC); !env.Date.Equal(want) {
		t.Errorf("Expected Date %v, got %v", want, env.Date)
	}
	if !strings.HasSuffix(env.Line, " some trailing text") {
		t.Errorf("Expected Line to preserve trailing text, got %q", env.Line)
	}
}

func TestNextMessageWithEnvelopeLongSeparator(t *testing.T) {
	longFrom := strings.Repeat("x", 10000) + "@example.com"
	mbox := "From herp.derp@example.com Thu Jan  1 00:00:01 2015\n" +
		"From: herp.derp@example.com (Herp Derp)\n" +
		"\n" +
		"First.\n" +
		"\n" +
		"From " + longFrom + " Thu Jan  2 00:00:01 2015\n" +
		"From: derp.herp@example.com (Derp Herp)\n" +
		"\n" +
		"Second.\n"
	b := bytes.NewBufferString(mbox)
	m := NewReader(b)

	env, _, err := m.NextMessageWithEnvelope()
	if err != nil {
		t.Fatalf("Unexpected error after NextMessageWithEnvelope(): %v", err)
	}
	if env.From != "herp.derp@example.com" {
		t.Errorf("Expected From %q, got %q", "herp.derp@example.com", env.From)
	}

	env, _, err = m.NextMessageWithEnvelope()
	if err != nil {
		t.Fatalf("Unexpected error after NextMessageWithEnvelope(): %v", err)
	}
	if env.From != longFrom {
		t.Errorf("Expected From of length %d, got %d", len(longFrom), len(env.From))
	}
	if want := time.Date(2015, 1, 2, 0, 0, 1, 0, time.UTC); !env.Date.Equal(want) {
		t.Errorf("Expected Date %v, got %v", want, env.Date)
	}
}

func TestNextMessageWithEnvelopeSeparatorAtEOFWithoutNewline(t *testing.T) {
	b := bytes.NewBufferString("From herp.derp@example.com Thu Jan  1 00:00:01 2015")
	m := NewReader(b)

	env, r, err := m.NextMessageWithEnvelope()
	if err != nil {
		t.Fatalf("Unexpected error after NextMessageWithEnvelope(): %v", err)
	}
	if env.From != "herp.derp@example.com" {
		t.Errorf("Expected From %q, got %q", "herp.derp@example.com", env.From)
	}

	body, err := ioutil.ReadAll(r)
	if err != nil {
		t.Fatalf("Unexpected error reading message body: %v", err)
	}
	if len(body) != 0 {
		t.Errorf("Expected empty body, got %q", body)
	}

	if _, _, err := m.NextMessageWithEnvelope(); err != io.EOF {
		t.Fatalf("Unexpected error after NextMessageWithEnvelope(): %v", err)
	}
}

func TestMixedEntryPoints(t *testing.T) {
	b := bytes.NewBufferString(mboxWithThreeMessages)
	m := NewReader(b)

	// First message via NextMessage, read to completion.
	r, err := m.NextMessage()
	if err != nil {
		t.Fatalf("Unexpected error after NextMessage(): %v", err)
	}
	if _, err := io.Copy(ioutil.Discard, r); err != nil {
		t.Fatalf("Unexpected error reading message body: %v", err)
	}

	// Second message via NextMessageWithEnvelope, only partially read.
	env, r, err := m.NextMessageWithEnvelope()
	if err != nil {
		t.Fatalf("Unexpected error after NextMessageWithEnvelope(): %v", err)
	}
	if env.From != "derp.herp@example.com" {
		t.Errorf("Expected From %q, got %q", "derp.herp@example.com", env.From)
	}
	p := make([]byte, 10)
	if _, err := r.Read(p); err != nil {
		t.Fatalf("Unexpected error reading message body: %v", err)
	}

	// Third message via NextMessage again. The envelope of the previous
	// call must belong to the second message even though it was not read
	// to completion.
	env, r, err = m.NextMessageWithEnvelope()
	if err != nil {
		t.Fatalf("Unexpected error after NextMessageWithEnvelope(): %v", err)
	}
	if env.From != "bernd.lauert@example.com" {
		t.Errorf("Expected From %q, got %q", "bernd.lauert@example.com", env.From)
	}
	if _, err := io.Copy(ioutil.Discard, r); err != nil {
		t.Fatalf("Unexpected error reading message body: %v", err)
	}

	if _, err := m.NextMessage(); err != io.EOF {
		t.Fatalf("Unexpected error after NextMessage(): %v", err)
	}
}
