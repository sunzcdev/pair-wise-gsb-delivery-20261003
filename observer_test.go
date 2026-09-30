package smtp_test

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-smtp"
)

type eventCollector struct {
	mu     sync.Mutex
	events []smtp.CommandEvent
}

func (ec *eventCollector) ObserveCommand(evt *smtp.CommandEvent) error {
	ec.mu.Lock()
	defer ec.mu.Unlock()
	ec.events = append(ec.events, *evt)
	return nil
}

func (ec *eventCollector) waitFor(t *testing.T, n int) []smtp.CommandEvent {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		ec.mu.Lock()
		events := append([]smtp.CommandEvent(nil), ec.events...)
		ec.mu.Unlock()
		if len(events) >= n {
			return events
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d events, got %d", n, len(events))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func eventsByCommand(events []smtp.CommandEvent, cmd string) []smtp.CommandEvent {
	var out []smtp.CommandEvent
	for _, evt := range events {
		if evt.Command == cmd {
			out = append(out, evt)
		}
	}
	return out
}

func TestSessionObserver(t *testing.T) {
	collector := new(eventCollector)
	_, s, c, scanner := testServer(t, func(s *smtp.Server) {
		s.Observer = collector
	})
	defer s.Close()
	defer c.Close()

	scanner.Scan() // greeting

	io.WriteString(c, "EHLO localhost\r\n")
	for scanner.Scan() {
		txt := scanner.Text()
		if strings.HasPrefix(txt, "250 ") {
			break
		} else if !strings.HasPrefix(txt, "250-") {
			t.Fatal("Invalid EHLO response:", txt)
		}
	}

	// Multi-round AUTH exchange: the initial response is empty, so the
	// server sends a 334 challenge first.
	saslResp := base64.StdEncoding.EncodeToString([]byte("\x00username\x00password"))
	io.WriteString(c, "AUTH PLAIN\r\n")
	scanner.Scan()
	if !strings.HasPrefix(scanner.Text(), "334 ") {
		t.Fatal("Invalid AUTH challenge response:", scanner.Text())
	}
	io.WriteString(c, saslResp+"\r\n")
	scanner.Scan()
	if !strings.HasPrefix(scanner.Text(), "235 ") {
		t.Fatal("Invalid AUTH response:", scanner.Text())
	}

	io.WriteString(c, "MAIL FROM:<sender@example.org>\r\n")
	scanner.Scan()
	io.WriteString(c, "RCPT TO:<rcpt@example.com>\r\n")
	scanner.Scan()
	io.WriteString(c, "DATA\r\n")
	scanner.Scan()
	body := "Subject: confidential\r\n\r\nsuper secret body content\r\n"
	io.WriteString(c, body)
	io.WriteString(c, ".\r\n")
	scanner.Scan()
	if !strings.HasPrefix(scanner.Text(), "250 ") {
		t.Fatal("Invalid DATA response:", scanner.Text())
	}

	io.WriteString(c, "NOOP\r\n")
	scanner.Scan()
	io.WriteString(c, "QUIT\r\n")
	scanner.Scan()

	events := collector.waitFor(t, 7)

	// EHLO produces a single event despite the multi-line reply.
	ehlo := eventsByCommand(events, "EHLO")
	if len(ehlo) != 1 || ehlo[0].Code != 250 {
		t.Fatalf("Expected a single EHLO event with code 250, got: %+v", ehlo)
	}

	// The whole AUTH exchange, including the challenge round, is a
	// single operation.
	auth := eventsByCommand(events, "AUTH")
	if len(auth) != 1 || auth[0].Code != 235 {
		t.Fatalf("Expected a single AUTH event with code 235, got: %+v", auth)
	}

	// MAIL, RCPT and DATA share the same non-zero transaction identifier.
	mail := eventsByCommand(events, "MAIL")
	rcpt := eventsByCommand(events, "RCPT")
	data := eventsByCommand(events, "DATA")
	if len(mail) != 1 || len(rcpt) != 1 || len(data) != 1 {
		t.Fatalf("Missing transaction events in: %+v", events)
	}
	txnID := mail[0].TransactionID
	if txnID == 0 {
		t.Fatal("MAIL event has zero transaction ID")
	}
	if rcpt[0].TransactionID != txnID || data[0].TransactionID != txnID {
		t.Fatalf("Transaction ID mismatch: MAIL=%v RCPT=%v DATA=%v",
			txnID, rcpt[0].TransactionID, data[0].TransactionID)
	}

	// DATA reports the cumulative body size.
	if data[0].Code != 250 {
		t.Fatal("Invalid DATA event code:", data[0].Code)
	}
	if data[0].BodyBytes != int64(len(body)) {
		t.Fatalf("Invalid DATA body size: got %v, want %v", data[0].BodyBytes, len(body))
	}

	// NOOP after the completed transaction is not part of it.
	noop := eventsByCommand(events, "NOOP")
	if len(noop) != 1 || noop[0].TransactionID != 0 {
		t.Fatalf("Expected NOOP outside of a transaction, got: %+v", noop)
	}

	// No sensitive raw value may appear in any event field.
	dump := fmt.Sprintf("%#v", events)
	for _, sensitive := range []string{"password", "username", saslResp, "super secret body content", "confidential"} {
		if strings.Contains(dump, sensitive) {
			t.Fatalf("Sensitive value %q leaked into observer events: %v", sensitive, dump)
		}
	}
}

func TestSessionObserver_LMTP(t *testing.T) {
	collector := new(eventCollector)
	_, s, c, scanner := testServerGreetedLMTP(t, func(s *smtp.Server) {
		s.LMTP = true
		s.Observer = collector
		s.ObserverMaskAddresses = true
		be := s.Backend.(*backend)
		be.implementLMTPData = true
		be.lmtpStatus = []struct {
			addr string
			err  error
		}{
			{"root@gchq.gov.uk", errors.New("nah")},
			{"root@bnd.bund.de", nil},
		}
	})
	defer s.Close()
	defer c.Close()

	sendDeliveryCmdsLMTP(t, scanner, c)

	scanner.Scan()
	if !strings.HasPrefix(scanner.Text(), "554 ") {
		t.Fatal("Invalid DATA first response:", scanner.Text())
	}
	scanner.Scan()
	if !strings.HasPrefix(scanner.Text(), "250 ") {
		t.Fatal("Invalid DATA second response:", scanner.Text())
	}

	io.WriteString(c, "QUIT\r\n")
	scanner.Scan()

	events := collector.waitFor(t, 6) // LHLO, MAIL, 2x RCPT, DATA, QUIT

	data := eventsByCommand(events, "DATA")
	if len(data) != 1 {
		t.Fatalf("Expected a single DATA event, got: %+v", data)
	}

	// Per-recipient results preserve the order in which the recipients
	// were specified, with masked addresses.
	want := []smtp.RecipientStatus{
		{Address: "***@gchq.gov.uk", Code: 554},
		{Address: "***@bnd.bund.de", Code: 250},
	}
	got := data[0].Recipients
	if len(got) != len(want) {
		t.Fatalf("Invalid recipient statuses: got %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Invalid recipient status %v: got %+v, want %+v", i, got[i], want[i])
		}
	}

	if data[0].BodyBytes != int64(len("Hey <3\r\n")) {
		t.Fatal("Invalid DATA body size:", data[0].BodyBytes)
	}

	// Masking must keep raw addresses out of every event.
	dump := fmt.Sprintf("%#v", events)
	for _, addr := range []string{"root@gchq.gov.uk", "root@bnd.bund.de", "root@nsa.gov"} {
		if strings.Contains(dump, addr) {
			t.Fatalf("Unmasked address %q leaked into observer events: %v", addr, dump)
		}
	}
}

func TestSessionObserver_failureIgnored(t *testing.T) {
	be, s, c, scanner := testServer(t, func(s *smtp.Server) {
		s.ErrorLog = log.New(ioutil.Discard, "", 0)
		s.Observer = smtp.SessionObserverFunc(func(evt *smtp.CommandEvent) error {
			if evt.Command == "DATA" {
				panic("observer is on fire")
			}
			return errors.New("observer failure")
		})
	})
	defer s.Close()
	defer c.Close()

	scanner.Scan() // greeting

	io.WriteString(c, "EHLO localhost\r\n")
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "250 ") {
			break
		}
	}
	io.WriteString(c, "MAIL FROM:<sender@example.org>\r\n")
	scanner.Scan()
	io.WriteString(c, "RCPT TO:<rcpt@example.com>\r\n")
	scanner.Scan()
	io.WriteString(c, "DATA\r\n")
	scanner.Scan()
	io.WriteString(c, "Important message\r\n")
	io.WriteString(c, ".\r\n")
	scanner.Scan()

	// The panicking observer must not turn the completed delivery
	// into a failure.
	if !strings.HasPrefix(scanner.Text(), "250 ") {
		t.Fatal("Delivery failed due to observer panic:", scanner.Text())
	}

	io.WriteString(c, "NOOP\r\n")
	scanner.Scan()
	if !strings.HasPrefix(scanner.Text(), "250 ") {
		t.Fatal("Connection broken after observer panic:", scanner.Text())
	}

	if len(be.anonmsgs) != 1 {
		t.Fatal("Invalid number of sent messages:", len(be.anonmsgs))
	}
}
