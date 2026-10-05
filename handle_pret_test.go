package ftpserver

import (
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/secsy/goftp"
	"github.com/spf13/afero"
	"github.com/stretchr/testify/require"
)

var errPRETPrepare = errors.New("node unavailable")

// pretReservation is the reservation handed back by the fake PRET driver.
// Tests observe its whole lifecycle through the counters on pretClientDriver.
type pretReservation struct {
	driver *pretClientDriver
	plan   *TransferPlan
}

func (r *pretReservation) Commit() {
	r.driver.record("commit", r.plan)
}

func (r *pretReservation) Release() {
	r.driver.record("release", r.plan)
}

// pretClientDriver is a TestClientDriver that implements PRET and records the
// reservation lifecycle so tests can observe it directly.
type pretClientDriver struct {
	*TestClientDriver

	mu         sync.Mutex
	committed  int
	released   int
	prepared   int
	prepareErr error
	events     []string
	plans      []*TransferPlan
}

func (d *pretClientDriver) record(event string, plan *TransferPlan) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.events = append(d.events, event+":"+plan.Command+" "+plan.Path)

	switch event {
	case "commit":
		d.committed++
	case "release":
		d.released++
	}
}

func (d *pretClientDriver) snapshot() (committed, released, prepared int, events []string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	events = append([]string(nil), d.events...)

	return d.committed, d.released, d.prepared, events
}

func (d *pretClientDriver) lastPlan() *TransferPlan {
	d.mu.Lock()
	defer d.mu.Unlock()

	if len(d.plans) == 0 {
		return nil
	}

	return d.plans[len(d.plans)-1]
}

func (d *pretClientDriver) PrepareTransfer(plan *TransferPlan) (PreparedTransfer, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.prepareErr != nil {
		return nil, d.prepareErr
	}

	d.prepared++
	d.plans = append(d.plans, plan)

	return &pretReservation{driver: d, plan: plan}, nil
}

// pretServerDriver authenticates clients with the observable PRET driver.
type pretServerDriver struct {
	*TestServerDriver
	client *pretClientDriver
}

func (d *pretServerDriver) AuthUser(_ ClientContext, _, _ string) (ClientDriver, error) {
	return d.client, nil
}

func newPRETTestServer(t *testing.T, prepareErr error) (*FtpServer, *pretClientDriver) {
	t.Helper()

	serverDriver := &TestServerDriver{
		Settings: &Settings{DefaultTransferType: TransferTypeBinary},
	}
	serverDriver.Init()

	clientDriver := &pretClientDriver{
		TestClientDriver: NewTestClientDriver(serverDriver),
		prepareErr:       prepareErr,
	}
	mainDriver := &pretServerDriver{TestServerDriver: serverDriver, client: clientDriver}

	return NewTestServerWithDriver(t, mainDriver), clientDriver
}

func pretClient(t *testing.T, server *FtpServer) (*goftp.Client, goftp.RawConn) {
	t.Helper()

	conf := goftp.Config{User: authUser, Password: authPass, Timeout: 5 * time.Second}

	client, err := goftp.DialConfig(conf, server.Addr())
	require.NoError(t, err, "Couldn't connect")

	t.Cleanup(func() { _ = client.Close() })

	raw, err := client.OpenRawConn()
	require.NoError(t, err, "Couldn't open raw connection")

	t.Cleanup(func() { _ = raw.Close() })

	return client, raw
}

// doPassiveTransfer prepares a passive data connection, sends the transfer
// command, runs fn over the data connection and reads the final reply.
func doPassiveTransfer(t *testing.T, raw goftp.RawConn, command string, fn func(net.Conn)) {
	t.Helper()

	dcGetter, err := raw.PrepareDataConn()
	require.NoError(t, err)

	code, msg, err := raw.SendCommand(command)
	require.NoError(t, err)
	require.Equal(t, StatusFileStatusOK, code, msg)

	dataConn, err := dcGetter()
	require.NoError(t, err)

	fn(dataConn)
	require.NoError(t, dataConn.Close())

	code, msg, err = raw.ReadResponse()
	require.NoError(t, err)
	require.Equal(t, StatusClosingDataConn, code, msg)
}

func pretWriteFile(t *testing.T, server *FtpServer, name, content string) {
	t.Helper()

	driver := server.driver.(*pretServerDriver)
	require.NoError(t, afero.WriteFile(driver.client.Fs, name, []byte(content), 0o600))
}

// TestPRETSuccess verifies that a matching transfer consumes the reservation
// and that the driver receives the announced command, path, type and offset.
func TestPRETSuccess(t *testing.T) {
	server, driver := newPRETTestServer(t, nil)
	_, raw := pretClient(t, server)

	pretWriteFile(t, server, "file.bin", "hello")

	code, msg, err := raw.SendCommand("PRET RETR /file.bin")
	require.NoError(t, err)
	require.Equal(t, StatusOK, code, msg)

	require.Equal(t, &TransferPlan{
		Command:       "RETR",
		Path:          "/file.bin",
		TransferType:  TransferTypeBinary,
		RestartOffset: 0,
	}, driver.lastPlan())

	var received strings.Builder

	doPassiveTransfer(t, raw, "RETR file.bin", func(conn net.Conn) {
		_, err := io.Copy(&received, conn)
		require.NoError(t, err)
	})

	require.Equal(t, "hello", received.String())

	committed, released, prepared, events := driver.snapshot()
	require.Equal(t, 1, prepared, events)
	require.Equal(t, 1, committed, events)
	require.Equal(t, 0, released, events)
	require.Equal(t, []string{"commit:RETR /file.bin"}, events)
}

// TestPRETSTOR verifies an upload reservation is committed and that the REST
// offset is part of the plan.
func TestPRETSTOR(t *testing.T) {
	server, driver := newPRETTestServer(t, nil)
	_, raw := pretClient(t, server)

	// Pre-create the file so resuming at offset 4 overwrites existing bytes
	// instead of extending a non-existent file with a sparse hole.
	pretWriteFile(t, server, "resume.bin", "1234old")

	code, _, err := raw.SendCommand("REST 4")
	require.NoError(t, err)
	require.Equal(t, StatusFileActionPending, code)

	code, _, err = raw.SendCommand("PRET STOR /resume.bin")
	require.NoError(t, err)
	require.Equal(t, StatusOK, code)
	require.Equal(t, int64(4), driver.lastPlan().RestartOffset)

	doPassiveTransfer(t, raw, "STOR resume.bin", func(conn net.Conn) {
		_, err := conn.Write([]byte("data"))
		require.NoError(t, err)
	})

	committed, released, prepared, events := driver.snapshot()
	require.Equal(t, 1, prepared, events)
	require.Equal(t, 1, committed, events)
	require.Equal(t, 0, released, events)

	d := server.driver.(*pretServerDriver)

	contents, err := afero.ReadFile(d.client.Fs, "resume.bin")
	require.NoError(t, err)
	require.Equal(t, "1234data", string(contents))
}

// TestPRETList verifies a directory listing commits the reservation too.
func TestPRETList(t *testing.T) {
	server, driver := newPRETTestServer(t, nil)
	_, raw := pretClient(t, server)

	pretWriteFile(t, server, "a.txt", "x")

	code, _, err := raw.SendCommand("PRET LIST /")
	require.NoError(t, err)
	require.Equal(t, StatusOK, code)
	require.Equal(t, "/", driver.lastPlan().Path)

	var received strings.Builder

	doPassiveTransfer(t, raw, "LIST /", func(conn net.Conn) {
		_, err := io.Copy(&received, conn)
		require.NoError(t, err)
	})

	require.Contains(t, received.String(), "a.txt")

	committed, released, _, events := driver.snapshot()
	require.Equal(t, 1, committed, events)
	require.Equal(t, 0, released, events)
}

// TestPRETRePRET verifies that a new PRET releases the previous reservation.
func TestPRETRePRET(t *testing.T) {
	server, driver := newPRETTestServer(t, nil)
	_, raw := pretClient(t, server)

	code, _, err := raw.SendCommand("PRET RETR /first.bin")
	require.NoError(t, err)
	require.Equal(t, StatusOK, code)

	code, _, err = raw.SendCommand("PRET RETR /second.bin")
	require.NoError(t, err)
	require.Equal(t, StatusOK, code)

	committed, released, prepared, events := driver.snapshot()
	require.Equal(t, 2, prepared, events)
	require.Equal(t, 0, committed, events)
	require.Equal(t, 1, released, events)
	require.Equal(t, "release:RETR /first.bin", events[0])

	// the second reservation is still the active one
	require.Equal(t, "/second.bin", driver.lastPlan().Path)
}

// TestPRETRestartInvalidation verifies REST releases a pending reservation.
func TestPRETRestartInvalidation(t *testing.T) {
	server, driver := newPRETTestServer(t, nil)
	_, raw := pretClient(t, server)

	code, _, err := raw.SendCommand("PRET STOR /f.bin")
	require.NoError(t, err)
	require.Equal(t, StatusOK, code)

	code, _, err = raw.SendCommand("REST 10")
	require.NoError(t, err)
	require.Equal(t, StatusFileActionPending, code)

	committed, released, prepared, events := driver.snapshot()
	require.Equal(t, 1, prepared, events)
	require.Equal(t, 0, committed, events)
	require.Equal(t, 1, released, events)
	require.Equal(t, "release:STOR /f.bin", events[0])
}

// TestPRETTypeInvalidation verifies a TYPE change releases a pending
// reservation while re-asserting the same type keeps it.
func TestPRETTypeInvalidation(t *testing.T) {
	server, driver := newPRETTestServer(t, nil)
	_, raw := pretClient(t, server)

	code, _, err := raw.SendCommand("PRET RETR /f.bin")
	require.NoError(t, err)
	require.Equal(t, StatusOK, code)

	// same type: reservation stays
	code, _, err = raw.SendCommand("TYPE I")
	require.NoError(t, err)
	require.Equal(t, StatusOK, code)

	_, released, prepared, _ := driver.snapshot()
	require.Equal(t, 1, prepared)
	require.Equal(t, 0, released)

	// different type: reservation is released
	code, _, err = raw.SendCommand("TYPE A")
	require.NoError(t, err)
	require.Equal(t, StatusOK, code)

	_, released, _, events := driver.snapshot()
	require.Equal(t, 1, released, events)
	require.Equal(t, "release:RETR /f.bin", events[0])
}

// TestPRETDisconnect verifies an unused reservation is released on disconnect.
func TestPRETDisconnect(t *testing.T) {
	server, driver := newPRETTestServer(t, nil)
	client, raw := pretClient(t, server)

	code, _, err := raw.SendCommand("PRET RETR /ghost.bin")
	require.NoError(t, err)
	require.Equal(t, StatusOK, code)

	// QUIT deterministically closes the control connection; the deferred
	// end() must release the reservation the client never consumed.
	code, _, err = raw.SendCommand("QUIT")
	require.NoError(t, err)
	require.Equal(t, StatusClosingControlConn, code)

	require.NoError(t, client.Close())

	require.Eventually(t, func() bool {
		_, released, _, _ := driver.snapshot()

		return released == 1
	}, 2*time.Second, 10*time.Millisecond)

	committed, released, prepared, events := driver.snapshot()
	require.Equal(t, 1, prepared, events)
	require.Equal(t, 0, committed, events)
	require.Equal(t, 1, released, events)
	require.Equal(t, []string{"release:RETR /ghost.bin"}, events)
}

// TestPRETExternalClose verifies that closing the client context externally
// (without an orderly QUIT) also releases the pending reservation.
func TestPRETExternalClose(t *testing.T) {
	server, driver := newPRETTestServer(t, nil)
	_, raw := pretClient(t, server)

	code, _, err := raw.SendCommand("PRET RETR /ghost.bin")
	require.NoError(t, err)
	require.Equal(t, StatusOK, code)

	serverDriver := server.driver.(*pretServerDriver)
	require.Len(t, serverDriver.Clients, 1)
	require.NoError(t, serverDriver.DisconnectClient())

	require.Eventually(t, func() bool {
		_, released, _, _ := driver.snapshot()

		return released == 1
	}, 2*time.Second, 10*time.Millisecond)

	committed, released, prepared, events := driver.snapshot()
	require.Equal(t, 1, prepared, events)
	require.Equal(t, 0, committed, events)
	require.Equal(t, 1, released, events)
}

// TestPRETPrepareFailure verifies a PrepareTransfer error replies 550 and
// leaves no pending reservation.
func TestPRETPrepareFailure(t *testing.T) {
	server, driver := newPRETTestServer(t, errPRETPrepare)
	_, raw := pretClient(t, server)

	code, msg, err := raw.SendCommand("PRET RETR /file.bin")
	require.NoError(t, err)
	require.Equal(t, StatusActionNotTaken, code, msg)

	committed, released, prepared, _ := driver.snapshot()
	require.Equal(t, 0, prepared)
	require.Equal(t, 0, committed)
	require.Equal(t, 0, released)

	// a normal (non-announced) transfer still works
	pretWriteFile(t, server, "file.bin", "ok")

	var received strings.Builder

	doPassiveTransfer(t, raw, "RETR file.bin", func(conn net.Conn) {
		_, err := io.Copy(&received, conn)
		require.NoError(t, err)
	})

	require.Equal(t, "ok", received.String())

	committed, released, _, _ = driver.snapshot()
	require.Equal(t, 0, committed)
	require.Equal(t, 0, released)

	// after a successful re-PRET the flow works end to end
	driver.prepareErr = nil

	code, _, err = raw.SendCommand("PRET RETR /file.bin")
	require.NoError(t, err)
	require.Equal(t, StatusOK, code)

	received.Reset()

	doPassiveTransfer(t, raw, "RETR file.bin", func(conn net.Conn) {
		_, err := io.Copy(&received, conn)
		require.NoError(t, err)
	})

	require.Equal(t, "ok", received.String())

	committed, released, prepared, events := driver.snapshot()
	require.Equal(t, 1, prepared, events)
	require.Equal(t, 1, committed, events)
	require.Equal(t, 0, released, events)
}

// TestPRETMismatch verifies a different command/path releases the reservation
// and is rejected with 503 without performing the transfer.
func TestPRETMismatch(t *testing.T) {
	server, driver := newPRETTestServer(t, nil)
	_, raw := pretClient(t, server)

	pretWriteFile(t, server, "file.bin", "data")

	code, _, err := raw.SendCommand("PRET RETR /file.bin")
	require.NoError(t, err)
	require.Equal(t, StatusOK, code)

	// announced RETR, execute STOR: mismatch
	dcGetter, err := raw.PrepareDataConn()
	require.NoError(t, err)

	code, msg, err := raw.SendCommand("STOR file.bin")
	require.NoError(t, err)
	require.Equal(t, StatusBadCommandSequence, code, msg)

	committed, released, _, events := driver.snapshot()
	require.Equal(t, 0, committed, events)
	require.Equal(t, 1, released, events)

	// the passive listener opened for the rejected command is still around;
	// drain it so the next command starts clean
	dataConn, err := dcGetter()
	require.NoError(t, err)
	require.NoError(t, dataConn.Close())

	// different path mismatches too
	code, _, err = raw.SendCommand("PRET RETR /file.bin")
	require.NoError(t, err)
	require.Equal(t, StatusOK, code)

	pretWriteFile(t, server, "other.bin", "nope")

	code, msg, err = raw.SendCommand("RETR other.bin")
	require.NoError(t, err)
	require.Equal(t, StatusBadCommandSequence, code, msg)

	committed, released, _, events = driver.snapshot()
	require.Equal(t, 0, committed, events)
	require.Equal(t, 2, released, events)
}

// TestPRETNotImplemented verifies PRET is rejected when the driver does not
// implement the extension, and it isn't advertised in FEAT.
func TestPRETNotImplemented(t *testing.T) {
	server := NewTestServer(t, false)
	_, raw := pretClient(t, server)

	code, msg, err := raw.SendCommand("FEAT")
	require.NoError(t, err)
	require.Equal(t, StatusSystemStatus, code)
	require.NotContains(t, msg, "PRET")

	code, msg, err = raw.SendCommand("PRET RETR /file.bin")
	require.NoError(t, err)
	require.Equal(t, StatusCommandNotImplemented, code, msg)
}

// TestPRETAdvertised verifies FEAT announces PRET when supported.
func TestPRETAdvertised(t *testing.T) {
	server, _ := newPRETTestServer(t, nil)
	_, raw := pretClient(t, server)

	code, msg, err := raw.SendCommand("FEAT")
	require.NoError(t, err)
	require.Equal(t, StatusSystemStatus, code)
	require.Contains(t, msg, "PRET")
}

// TestPRETSyntax verifies bad PRET parameters are rejected with 501.
func TestPRETSyntax(t *testing.T) {
	server, _ := newPRETTestServer(t, nil)
	_, raw := pretClient(t, server)

	for _, command := range []string{
		"PRET NOPE /f",
		"PRET",
		"PRET RETR",
	} {
		code, _, err := raw.SendCommand(command)
		require.NoError(t, err)
		require.Equal(t, StatusSyntaxErrorParameters, code, command)
	}
}

// TestPRETTransferWithoutDataConn verifies that when the announced transfer
// cannot open a data connection (no PASV/PORT) the reservation is released
// again instead of leaking.
func TestPRETTransferWithoutDataConn(t *testing.T) {
	server, driver := newPRETTestServer(t, nil)
	_, raw := pretClient(t, server)

	pretWriteFile(t, server, "file.bin", "data")

	code, _, err := raw.SendCommand("PRET RETR /file.bin")
	require.NoError(t, err)
	require.Equal(t, StatusOK, code)

	// No passive/active connection was prepared: opening the transfer fails.
	code, msg, err := raw.SendCommand("RETR file.bin")
	require.NoError(t, err)
	require.Contains(t, []int{StatusActionNotTaken, StatusCannotOpenDataConnection}, code, msg)

	require.Eventually(t, func() bool {
		_, released, _, _ := driver.snapshot()

		return released == 1
	}, 2*time.Second, 10*time.Millisecond)

	committed, released, prepared, events := driver.snapshot()
	require.Equal(t, 1, prepared, events)
	require.Equal(t, 0, committed, events)
	require.Equal(t, 1, released, events)

	// nothing leaks: a subsequent successful re-PRET works
	code, _, err = raw.SendCommand("PRET RETR /file.bin")
	require.NoError(t, err)
	require.Equal(t, StatusOK, code)

	var received strings.Builder

	doPassiveTransfer(t, raw, "RETR file.bin", func(conn net.Conn) {
		_, err := io.Copy(&received, conn)
		require.NoError(t, err)
	})

	require.Equal(t, "data", received.String())

	committed, released, prepared, events = driver.snapshot()
	require.Equal(t, 2, prepared, events)
	require.Equal(t, 1, committed, events)
	require.Equal(t, 1, released, events)
}
