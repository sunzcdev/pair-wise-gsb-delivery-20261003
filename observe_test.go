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

type eventRecorder struct {
	mu      sync.Mutex
	events  []smtp.CommandEvent
	err     error
	doPanic bool
}

func (r *eventRecorder) ObserveCommand(ev smtp.CommandEvent) error {
	if r.doPanic {
		panic("observer exploded")
	}
	r.mu.Lock()
	r.events = append(r.events, ev)
	r.mu.Unlock()
	return r.err
}

func (r *eventRecorder) snapshot() []smtp.CommandEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]smtp.CommandEvent(nil), r.events...)
}

func waitEvents(t *testing.T, rec *eventRecorder, n int) []smtp.CommandEvent {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		evs := rec.snapshot()
		if len(evs) >= n {
			return evs
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d events, got %d: %#v", n, len(evs), evs)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func countKind(evs []smtp.CommandEvent, kind smtp.CommandKind) int {
	n := 0
	for _, ev := range evs {
		if ev.Kind == kind {
			n++
		}
	}
	return n
}

func findEvent(evs []smtp.CommandEvent, kind smtp.CommandKind) *smtp.CommandEvent {
	for i := range evs {
		if evs[i].Kind == kind {
			return &evs[i]
		}
	}
	return nil
}

func TestServerObserver(t *testing.T) {
	rec := new(eventRecorder)
	be, s, c, scanner := testServerGreeted(t, func(s *smtp.Server) {
		s.Observer = rec
	})
	defer s.Close()
	defer c.Close()

	const (
		username   = "username"
		password   = "password"
		bodyMarker = "CONFIDENTIAL-BODY-MARKER-9f8e7d"
	)
	saslResponse := base64.StdEncoding.EncodeToString(
		[]byte("\x00" + username + "\x00" + password))

	// EHLO produces a multi-line reply, it must complete as one event.
	io.WriteString(c, "EHLO localhost\r\n")
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "250 ") {
			break
		}
	}

	// AUTH PLAIN without initial response: multi-round challenge/response
	// exchange, must be reported as a single operation.
	io.WriteString(c, "AUTH PLAIN\r\n")
	scanner.Scan()
	if !strings.HasPrefix(scanner.Text(), "334") {
		t.Fatal("Invalid AUTH challenge:", scanner.Text())
	}
	io.WriteString(c, saslResponse+"\r\n")
	scanner.Scan()
	if !strings.HasPrefix(scanner.Text(), "235 ") {
		t.Fatal("Invalid AUTH response:", scanner.Text())
	}

	io.WriteString(c, "MAIL FROM:<sender@example.org>\r\n")
	scanner.Scan()
	io.WriteString(c, "RCPT TO:<rcpt@example.net>\r\n")
	scanner.Scan()
	io.WriteString(c, "DATA\r\n")
	scanner.Scan()
	body := "Subject: audit test\r\n\r\n" + bodyMarker + "\r\n"
	io.WriteString(c, body+".\r\n")
	scanner.Scan()
	if !strings.HasPrefix(scanner.Text(), "250 ") {
		t.Fatal("Invalid DATA response:", scanner.Text())
	}

	// EHLO, AUTH, MAIL, RCPT, DATA
	evs := waitEvents(t, rec, 5)

	if n := countKind(evs, smtp.CommandEHLO); n != 1 {
		t.Fatalf("expected exactly 1 EHLO event, got %d: %#v", n, evs)
	}
	if ev := findEvent(evs, smtp.CommandEHLO); ev.StatusCode != 250 {
		t.Fatal("Invalid EHLO event status:", ev.StatusCode)
	}

	if n := countKind(evs, smtp.CommandAUTH); n != 1 {
		t.Fatalf("multi-round AUTH must produce exactly 1 event, got %d: %#v", n, evs)
	}
	if ev := findEvent(evs, smtp.CommandAUTH); ev.StatusCode != 235 {
		t.Fatal("Invalid AUTH event status:", ev.StatusCode)
	}

	for _, kind := range []smtp.CommandKind{smtp.CommandMAIL, smtp.CommandRCPT, smtp.CommandDATA} {
		ev := findEvent(evs, kind)
		if ev == nil {
			t.Fatalf("missing %v event: %#v", kind, evs)
		}
		if ev.TransactionID != 1 {
			t.Fatalf("%v event has transaction ID %v, want 1", kind, ev.TransactionID)
		}
		if ev.StatusCode != 250 {
			t.Fatalf("%v event has status %v, want 250", kind, ev.StatusCode)
		}
	}

	dataEv := findEvent(evs, smtp.CommandDATA)
	if dataEv.BodyBytes != int64(len(be.messages[0].Data)) {
		t.Fatalf("DATA event body size = %v, want %v",
			dataEv.BodyBytes, len(be.messages[0].Data))
	}
	if dataEv.Duration < 0 || dataEv.Duration > time.Minute {
		t.Fatal("DATA event has implausible duration:", dataEv.Duration)
	}

	// None of the sensitive raw values may appear in any event field.
	dump := fmt.Sprintf("%#v", evs)
	for _, secret := range []string{username, password, saslResponse, bodyMarker} {
		if strings.Contains(dump, secret) {
			t.Fatalf("sensitive value %q leaked into observer events: %v", secret, dump)
		}
	}
}

func TestServerObserverLMTP(t *testing.T) {
	rec := new(eventRecorder)
	_, s, c, scanner := testServerGreetedLMTP(t, func(s *smtp.Server) {
		s.LMTP = true
		s.Observer = rec
		s.ObserverAddressMask = smtp.MaskAddress
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

	// LHLO, MAIL, RCPT, RCPT, DATA
	evs := waitEvents(t, rec, 5)

	dataEv := findEvent(evs, smtp.CommandDATA)
	if dataEv == nil {
		t.Fatalf("missing DATA event: %#v", evs)
	}
	if len(dataEv.Recipients) != 2 {
		t.Fatalf("expected 2 recipient results, got %#v", dataEv.Recipients)
	}

	// Per-recipient results must be in RCPT TO order, with masked addresses.
	want := []smtp.RecipientResult{
		{Address: "***@gchq.gov.uk", StatusCode: 554},
		{Address: "***@bnd.bund.de", StatusCode: 250},
	}
	for i, w := range want {
		if dataEv.Recipients[i] != w {
			t.Fatalf("recipient %d = %#v, want %#v", i, dataEv.Recipients[i], w)
		}
	}

	dump := fmt.Sprintf("%#v", evs)
	if strings.Contains(dump, "root@") {
		t.Fatal("unmasked recipient address leaked into observer events:", dump)
	}
}

func TestServerObserverFailure(t *testing.T) {
	rec := &eventRecorder{err: errors.New("audit backend unavailable")}
	be, s, c, scanner := testServerGreeted(t, func(s *smtp.Server) {
		s.Observer = rec
		s.ErrorLog = log.New(ioutil.Discard, "", 0)
	})
	defer s.Close()
	defer c.Close()

	deliver := func() {
		t.Helper()
		io.WriteString(c, "EHLO localhost\r\n")
		for scanner.Scan() {
			if strings.HasPrefix(scanner.Text(), "250 ") {
				break
			}
		}
		io.WriteString(c, "MAIL FROM:<sender@example.org>\r\n")
		scanner.Scan()
		io.WriteString(c, "RCPT TO:<rcpt@example.net>\r\n")
		scanner.Scan()
		io.WriteString(c, "DATA\r\n")
		scanner.Scan()
		io.WriteString(c, "payload\r\n.\r\n")
		scanner.Scan()
		if !strings.HasPrefix(scanner.Text(), "250 ") {
			t.Fatal("observer failure turned delivery into failure:", scanner.Text())
		}
	}

	// An observer returning errors must not fail completed deliveries.
	deliver()

	// A panicking observer must not fail completed deliveries either.
	rec.mu.Lock()
	rec.err = nil
	rec.doPanic = true
	rec.mu.Unlock()
	deliver()

	if len(be.anonmsgs) != 2 {
		t.Fatal("invalid number of delivered messages:", len(be.anonmsgs))
	}

	// The connection must still be usable.
	io.WriteString(c, "NOOP\r\n")
	scanner.Scan()
	if !strings.HasPrefix(scanner.Text(), "250 ") {
		t.Fatal("connection broken after observer panic:", scanner.Text())
	}
}
