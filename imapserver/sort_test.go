package imapserver

import (
	"bufio"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/internal/imapwire"
)

func newSortTestConn(input string) (*Conn, *imapwire.Decoder) {
	dec := imapwire.NewDecoder(bufio.NewReader(strings.NewReader(input)), imapwire.ConnSideServer)
	return &Conn{state: imap.ConnStateSelected}, dec
}

func TestReadSortCriteria(t *testing.T) {
	conn, dec := newSortTestConn("(REVERSE DATE SUBJECT) UTF-8 ALL\r\n")
	err := conn.handleSort(dec, NumKindSeq)
	// No backend session is registered, so handleSort reaches the
	// "SORT is not supported" error *after* parsing successfully.
	if err == nil {
		t.Fatal("handleSort succeeded without a session")
	}
	if got := err.Error(); !strings.Contains(got, "not supported") {
		t.Fatalf("handleSort() = %v, want a not-supported error", err)
	}
}

func TestReadSortCriteriaUnknownKey(t *testing.T) {
	conn, dec := newSortTestConn("(DISPLAYFROM) UTF-8 ALL\r\n")
	err := conn.handleSort(dec, NumKindSeq)
	if err == nil {
		t.Fatal("handleSort succeeded with an unsupported sort key")
	}
	if got := err.Error(); !strings.Contains(got, "unsupported SORT sort key") {
		t.Fatalf("handleSort() = %v, want an unsupported sort key error", err)
	}
}

func TestReadSortCriteriaBadCharset(t *testing.T) {
	conn, dec := newSortTestConn("(DATE) ISO-8859-1 ALL\r\n")
	err := conn.handleSort(dec, NumKindSeq)
	imapErr, ok := err.(*imap.Error)
	if !ok {
		t.Fatalf("handleSort() = %T (%v), want *imap.Error", err, err)
	}
	if imapErr.Code != imap.ResponseCodeBadCharset {
		t.Fatalf("error code = %q, want %q", imapErr.Code, imap.ResponseCodeBadCharset)
	}
}
