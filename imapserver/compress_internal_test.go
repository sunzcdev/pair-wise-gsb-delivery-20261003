package imapserver

import (
	"bufio"
	"compress/flate"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
)

// compressTestSession is a no-op Session used to test COMPRESS handling
// without a backend.
type compressTestSession struct {
	closed chan struct{}
}

func newCompressTestSession() *compressTestSession {
	return &compressTestSession{closed: make(chan struct{})}
}

func (sess *compressTestSession) Close() error {
	close(sess.closed)
	return nil
}

func (sess *compressTestSession) Login(username, password string) error { return nil }

func (sess *compressTestSession) Select(mailbox string, options *imap.SelectOptions) (*imap.SelectData, error) {
	return &imap.SelectData{}, nil
}

func (sess *compressTestSession) Create(mailbox string, options *imap.CreateOptions) error {
	return nil
}

func (sess *compressTestSession) Delete(mailbox string) error { return nil }

func (sess *compressTestSession) Rename(mailbox, newName string, options *imap.RenameOptions) error {
	return nil
}

func (sess *compressTestSession) Subscribe(mailbox string) error   { return nil }
func (sess *compressTestSession) Unsubscribe(mailbox string) error { return nil }

func (sess *compressTestSession) List(w *ListWriter, ref string, patterns []string, options *imap.ListOptions) error {
	return nil
}

func (sess *compressTestSession) Status(mailbox string, options *imap.StatusOptions) (*imap.StatusData, error) {
	return &imap.StatusData{}, nil
}

func (sess *compressTestSession) Append(mailbox string, r imap.LiteralReader, options *imap.AppendOptions) (*imap.AppendData, error) {
	return &imap.AppendData{}, nil
}

func (sess *compressTestSession) Poll(w *UpdateWriter, allowExpunge bool) error { return nil }

func (sess *compressTestSession) Idle(w *UpdateWriter, stop <-chan struct{}) error {
	<-stop
	return nil
}

func (sess *compressTestSession) Unselect() error { return nil }

func (sess *compressTestSession) Expunge(w *ExpungeWriter, uids *imap.UIDSet) error {
	return nil
}

func (sess *compressTestSession) Search(kind NumKind, criteria *imap.SearchCriteria, options *imap.SearchOptions) (*imap.SearchData, error) {
	return &imap.SearchData{}, nil
}

func (sess *compressTestSession) Fetch(w *FetchWriter, numSet imap.NumSet, options *imap.FetchOptions) error {
	return nil
}

func (sess *compressTestSession) Store(w *FetchWriter, numSet imap.NumSet, flags *imap.StoreFlags, options *imap.StoreOptions) error {
	return nil
}

func (sess *compressTestSession) Copy(numSet imap.NumSet, dest string) (*imap.CopyData, error) {
	return &imap.CopyData{}, nil
}

// setupCompressTestConn spawns a server connection over an in-memory pipe
// and returns the client end of the pipe along with the session.
func setupCompressTestConn(t *testing.T) (net.Conn, *compressTestSession) {
	t.Helper()

	sess := newCompressTestSession()
	server := New(&Options{
		NewSession: func(conn *Conn) (Session, *GreetingData, error) {
			return sess, nil, nil
		},
	})
	t.Cleanup(func() { server.Close() })

	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() { clientConn.Close() })
	go newConn(serverConn, server).serve()

	// Read the greeting
	r := bufio.NewReader(clientConn)
	if _, err := r.ReadString('\n'); err != nil {
		t.Fatalf("failed to read greeting: %v", err)
	}
	return clientConn, sess
}

// compressClientConn writes the COMPRESS command, expects the uncompressed
// OK response, and returns a flate writer for the compressed connection.
func compressClientConn(t *testing.T, conn net.Conn) *flate.Writer {
	t.Helper()

	if _, err := conn.Write([]byte("a COMPRESS DEFLATE\r\n")); err != nil {
		t.Fatalf("conn.Write() = %v", err)
	}
	r := bufio.NewReader(conn)
	line, err := r.ReadString('\n')
	if err != nil {
		t.Fatalf("failed to read COMPRESS response: %v", err)
	}
	if !strings.HasPrefix(line, "a OK") {
		t.Fatalf("COMPRESS response = %q, want OK", line)
	}

	zw, err := flate.NewWriter(conn, flate.DefaultCompression)
	if err != nil {
		t.Fatalf("flate.NewWriter() = %v", err)
	}
	return zw
}

func TestCompressDecompressedSizeLimit(t *testing.T) {
	defer func(prev int64) { maxDecompressedSize = prev }(maxDecompressedSize)
	maxDecompressedSize = 64

	conn, sess := setupCompressTestConn(t)
	zw := compressClientConn(t, conn)

	// Send a command which decompresses to much more than the limit: the
	// server must drop the connection
	if _, err := zw.Write([]byte("b NOOP " + strings.Repeat("A", 4096) + "\r\n")); err != nil {
		t.Fatalf("zw.Write() = %v", err)
	}
	if err := zw.Flush(); err != nil {
		t.Fatalf("zw.Flush() = %v", err)
	}

	// Drain responses until the server drops the connection
	zr := flate.NewReader(conn)
	for {
		if _, err := zr.Read(make([]byte, 1024)); err != nil {
			break
		}
	}

	select {
	case <-sess.closed:
		// The server closed the session
	case <-time.After(10 * time.Second):
		t.Fatal("server didn't close the connection after the decompressed size limit was exceeded")
	}
}

func TestCompressConnectionClose(t *testing.T) {
	conn, sess := setupCompressTestConn(t)
	compressClientConn(t, conn)

	// Abruptly close the connection in the middle of the DEFLATE stream:
	// the server must shut down the session cleanly
	if err := conn.Close(); err != nil {
		t.Fatalf("conn.Close() = %v", err)
	}

	select {
	case <-sess.closed:
		// The server closed the session
	case <-time.After(10 * time.Second):
		t.Fatal("server didn't close the session after the connection was closed")
	}
}
