package ftpserver

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pretEventKind enumerates the reservation lifecycle events observable by a driver.
type pretEventKind string

const (
	pretEventPrepared pretEventKind = "prepared"
	pretEventConsumed pretEventKind = "consumed"
	pretEventCanceled pretEventKind = "canceled"
	pretEventReleased pretEventKind = "released"
)

type pretEvent struct {
	kind pretEventKind
	info PreTransferInfo
	err  error
}

type pretReservation struct {
	mu         sync.Mutex
	consumed   bool
	canceled   bool
	released   bool
	releaseErr error
	record     func(pretEvent)
	info       PreTransferInfo
}

func (r *pretReservation) Consumed() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.consumed = true
	r.record(pretEvent{kind: pretEventConsumed, info: r.info})
}

func (r *pretReservation) Cancel(info PreTransferInfo, _ TransferReservation) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.canceled = true
	r.record(pretEvent{kind: pretEventCanceled, info: info})
}

func (r *pretReservation) Release(info PreTransferInfo, _ TransferReservation, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.released = true
	r.releaseErr = err
	r.record(pretEvent{kind: pretEventReleased, info: info, err: err})
}

func (r *pretReservation) state() (consumed, canceled, released bool, releaseErr error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.consumed, r.canceled, r.released, r.releaseErr
}

// pretClientDriver wraps the test client driver and implements the PRET extension.
type pretClientDriver struct {
	*TestClientDriver
	serverDriver *pretServerDriver
}

func (d *pretClientDriver) PrepareTransfer(_ ClientContext, info PreTransferInfo) (TransferReservation, error) {
	d.serverDriver.mu.Lock()
	fail := d.serverDriver.prepareFails
	d.serverDriver.mu.Unlock()

	if fail {
		return nil, errPretPrepare
	}

	reservation := &pretReservation{info: info, record: d.serverDriver.record}
	d.serverDriver.record(pretEvent{kind: pretEventPrepared, info: info})
	d.serverDriver.mu.Lock()
	d.serverDriver.lastReservation = reservation
	d.serverDriver.reservations[info] = reservation
	d.serverDriver.mu.Unlock()

	return reservation, nil
}

// preparedReservation returns the reservation recorded for the given info, if any.
func (d *pretServerDriver) preparedReservation(info PreTransferInfo) *pretReservation {
	d.mu.Lock()
	defer d.mu.Unlock()

	for idx := len(d.events) - 1; idx >= 0; idx-- {
		if d.events[idx].kind == pretEventPrepared && d.events[idx].info == info {
			return d.reservations[info]
		}
	}

	return nil
}

var errPretPrepare = errors.New("data node unavailable")

// pretServerDriver is a test main driver enabling the PRET extension.
type pretServerDriver struct {
	*TestServerDriver

	server          *FtpServer
	mu              sync.Mutex
	events          []pretEvent
	reservations    map[PreTransferInfo]*pretReservation
	lastReservation *pretReservation
	prepareFails    bool
}

func newPretServerDriver(t *testing.T) *pretServerDriver {
	t.Helper()

	base := &TestServerDriver{
		Settings: &Settings{
			DefaultTransferType: TransferTypeBinary,
			ListenAddr:          "127.0.0.1:0",
		},
	}

	driver := &pretServerDriver{
		TestServerDriver: base,
		reservations:     make(map[PreTransferInfo]*pretReservation),
	}

	base.AuthProvider = func(_, _ string) (ClientDriver, error) {
		return &pretClientDriver{
			TestClientDriver: NewTestClientDriver(base),
			serverDriver:     driver,
		}, nil
	}

	return driver
}

func (d *pretServerDriver) record(event pretEvent) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.events = append(d.events, event)
}

func newPretTestServer(t *testing.T) *pretServerDriver {
	t.Helper()

	driver := newPretServerDriver(t)
	driver.server = NewTestServerWithTestDriver(t, driver.TestServerDriver)

	return driver
}

func (d *pretServerDriver) Addr() string {
	return d.server.Addr()
}

// dialPretControl opens and logs in a raw control connection.
func dialPretControl(t *testing.T, addr string) (net.Conn, *bufio.Reader) {
	t.Helper()

	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	require.NoError(t, err)

	reader := bufio.NewReader(conn)

	readPretResponse(t, reader) // banner
	sendPretCommand(t, conn, "USER "+authUser)
	readPretResponse(t, reader)
	sendPretCommand(t, conn, "PASS "+authPass)
	code, _ := readPretResponse(t, reader)
	require.Equal(t, StatusUserLoggedIn, code)

	t.Cleanup(func() { _ = conn.Close() })

	return conn, reader
}

func sendPretCommand(t *testing.T, conn net.Conn, command string) {
	t.Helper()
	_, err := fmt.Fprintf(conn, "%s\r\n", command)
	require.NoError(t, err)
}

// readPretResponse reads a possibly multiline FTP response and returns its final code.
func readPretResponse(t *testing.T, reader *bufio.Reader) (int, string) {
	t.Helper()

	var (
		code int
		body strings.Builder
	)

	line, err := reader.ReadString('\n')
	require.NoError(t, err)

	line = strings.TrimRight(line, "\r\n")
	require.GreaterOrEqual(t, len(line), 4)

	code, err = strconv.Atoi(line[:3])
	require.NoError(t, err)
	body.WriteString(line)

	if line[3] == ' ' {
		return code, body.String()
	}

	require.Equal(t, byte('-'), line[3])
	body.WriteByte('\n')

	for {
		line, err := reader.ReadString('\n')
		require.NoError(t, err)

		line = strings.TrimRight(line, "\r\n")

		require.GreaterOrEqual(t, len(line), 4)

		if currentCode, errCode := strconv.Atoi(line[:3]); errCode == nil &&
			currentCode == code && line[3] == ' ' {
			body.WriteString(line)

			break
		}

		body.WriteString(line)
		body.WriteByte('\n')
	}

	return code, body.String()
}

func dialPretDataConn(t *testing.T, conn net.Conn, reader *bufio.Reader) net.Conn {
	t.Helper()

	sendPretCommand(t, conn, "PASV")
	code, response := readPretResponse(t, reader)
	require.Equal(t, StatusEnteringPASV, code, response)

	port := getPortFromPASVResponse(t, response)

	dataConn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 5*time.Second)
	require.NoError(t, err)

	t.Cleanup(func() { _ = dataConn.Close() })

	return dataConn
}

// finishPretDataConn closes the data connection and drains the final
// 226 response so the control connection stays synchronized.
func finishPretDataConn(t *testing.T, conn net.Conn, reader *bufio.Reader, dataConn net.Conn) {
	t.Helper()

	require.NoError(t, dataConn.Close())

	code, response := readPretResponse(t, reader)
	require.Equal(t, StatusClosingDataConn, code, response)
}

func (d *pretServerDriver) snapshotEvents() []pretEvent {
	d.mu.Lock()
	defer d.mu.Unlock()

	events := make([]pretEvent, len(d.events))
	copy(events, d.events)

	return events
}

func (d *pretServerDriver) eventKinds() []pretEventKind {
	events := d.snapshotEvents()
	kinds := make([]pretEventKind, 0, len(events))

	for _, event := range events {
		kinds = append(kinds, event.kind)
	}

	return kinds
}

func waitPretEventKinds(t *testing.T, driver *pretServerDriver, expected []pretEventKind) {
	t.Helper()

	require.Eventually(t, func() bool {
		kinds := driver.eventKinds()
		if len(kinds) != len(expected) {
			return false
		}

		for idx := range expected {
			if kinds[idx] != expected[idx] {
				return false
			}
		}

		return true
	}, time.Second, 10*time.Millisecond)
}

func TestPretFeatureAdvertised(t *testing.T) {
	t.Parallel()

	server := NewTestServerWithTestDriver(t, newPretServerDriver(t).TestServerDriver)

	conn, reader := dialPretControl(t, server.Addr())

	sendPretCommand(t, conn, "FEAT")
	code, response := readPretResponse(t, reader)
	require.Equal(t, StatusSystemStatus, code)
	assert.Contains(t, response, " PRET")
}

func TestPretSuccessfulTransfer(t *testing.T) {
	t.Parallel()

	driver := newPretTestServer(t)
	conn, reader := dialPretControl(t, driver.Addr())

	sendPretCommand(t, conn, "PRET STOR /upload.bin")
	code, response := readPretResponse(t, reader)
	require.Equal(t, StatusFileActionPending, code, response)

	reservation := driver.lastReservation
	require.NotNil(t, reservation)

	dataConn := dialPretDataConn(t, conn, reader)

	sendPretCommand(t, conn, "STOR /upload.bin")
	code, response = readPretResponse(t, reader)
	require.Equal(t, StatusFileStatusOK, code, response)

	_, err := dataConn.Write([]byte("hello pret"))
	require.NoError(t, err)
	require.NoError(t, dataConn.Close())

	code, response = readPretResponse(t, reader)
	require.Equal(t, StatusClosingDataConn, code, response)

	waitPretEventKinds(t, driver, []pretEventKind{
		pretEventPrepared,
		pretEventConsumed,
		pretEventReleased,
	})

	consumed, canceled, released, releaseErr := reservation.state()
	assert.True(t, consumed)
	assert.False(t, canceled)
	assert.True(t, released)
	assert.NoError(t, releaseErr)

	// a second transfer without a new PRET is rejected
	sendPretCommand(t, conn, "STOR /upload.bin")
	code, response = readPretResponse(t, reader)
	require.Equal(t, StatusBadCommandSequence, code, response)
}

func TestPretRetrConsumesAndReleases(t *testing.T) {
	t.Parallel()

	driver := newPretTestServer(t)
	conn, reader := dialPretControl(t, driver.Addr())

	// upload a file first
	sendPretCommand(t, conn, "PRET STOR /file.bin")
	readPretResponse(t, reader)
	dataConn := dialPretDataConn(t, conn, reader)
	sendPretCommand(t, conn, "STOR /file.bin")
	readPretResponse(t, reader)
	_, err := dataConn.Write([]byte("payload"))
	require.NoError(t, err)
	require.NoError(t, dataConn.Close())
	readPretResponse(t, reader)

	// download it through another PRET
	sendPretCommand(t, conn, "PRET RETR /file.bin")
	readPretResponse(t, reader)

	retrInfo := PreTransferInfo{Command: "RETR", Path: "/file.bin", TransferType: TransferTypeBinary}
	retrReservation := driver.preparedReservation(retrInfo)
	require.NotNil(t, retrReservation)

	dataConn = dialPretDataConn(t, conn, reader)
	sendPretCommand(t, conn, "RETR /file.bin")
	readPretResponse(t, reader)

	contents, err := io.ReadAll(dataConn)
	require.NoError(t, err)
	require.Equal(t, "payload", string(contents))
	require.NoError(t, dataConn.Close())
	readPretResponse(t, reader)

	waitPretEventKinds(t, driver, []pretEventKind{
		pretEventPrepared, pretEventConsumed, pretEventReleased,
		pretEventPrepared, pretEventConsumed, pretEventReleased,
	})

	consumed, canceled, released, releaseErr := retrReservation.state()
	assert.True(t, consumed)
	assert.False(t, canceled)
	assert.True(t, released)
	assert.NoError(t, releaseErr)
}

func TestPretRePretCancelsPrevious(t *testing.T) {
	t.Parallel()

	driver := newPretTestServer(t)
	conn, reader := dialPretControl(t, driver.Addr())

	sendPretCommand(t, conn, "PRET STOR /first.bin")
	code, response := readPretResponse(t, reader)
	require.Equal(t, StatusFileActionPending, code, response)
	firstInfo := PreTransferInfo{Command: "STOR", Path: "/first.bin", TransferType: TransferTypeBinary}
	first := driver.preparedReservation(firstInfo)
	require.NotNil(t, first)

	// the driver decides the second file must land on another node
	sendPretCommand(t, conn, "PRET STOR /second.bin")
	code, response = readPretResponse(t, reader)
	require.Equal(t, StatusFileActionPending, code, response)
	secondInfo := PreTransferInfo{Command: "STOR", Path: "/second.bin", TransferType: TransferTypeBinary}
	second := driver.preparedReservation(secondInfo)
	require.NotNil(t, second)

	_, canceled, released, _ := first.state()
	assert.True(t, canceled)
	assert.False(t, released)

	dataConn := dialPretDataConn(t, conn, reader)
	sendPretCommand(t, conn, "STOR /second.bin")
	readPretResponse(t, reader)
	finishPretDataConn(t, conn, reader, dataConn)

	_, canceled, released, _ = second.state()
	assert.False(t, canceled)
	require.Eventually(t, func() bool {
		_, _, isReleased, _ := second.state()

		return isReleased
	}, time.Second, 5*time.Millisecond)

	kinds := driver.eventKinds()
	require.Equal(t, []pretEventKind{
		pretEventPrepared, pretEventCanceled,
		pretEventPrepared, pretEventConsumed, pretEventReleased,
	}, kinds)
}

func TestPretMismatchInvalidatesReservation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		pretCmd  string
		mutate   func(*testing.T, net.Conn, *bufio.Reader)
		transfer string
	}{
		{
			name:     "command-changed",
			pretCmd:  "PRET STOR /file.bin",
			transfer: "RETR /file.bin",
		},
		{
			name:     "path-changed",
			pretCmd:  "PRET STOR /announced.bin",
			transfer: "STOR /actual.bin",
		},
		{
			name:    "restart-changed",
			pretCmd: "PRET RETR /file.bin",
			mutate: func(t *testing.T, conn net.Conn, reader *bufio.Reader) {
				sendPretCommand(t, conn, "REST 3")
				readPretResponse(t, reader)
			},
			transfer: "RETR /file.bin",
		},
		{
			name:    "type-changed",
			pretCmd: "PRET RETR /file.bin",
			mutate: func(t *testing.T, conn net.Conn, reader *bufio.Reader) {
				sendPretCommand(t, conn, "TYPE A")
				readPretResponse(t, reader)
			},
			transfer: "RETR /file.bin",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			driver := newPretTestServer(t)
			conn, reader := dialPretControl(t, driver.Addr())

			sendPretCommand(t, conn, test.pretCmd)
			code, response := readPretResponse(t, reader)
			require.Equal(t, StatusFileActionPending, code, response)

			reservation := driver.lastReservation

			if test.mutate != nil {
				test.mutate(t, conn, reader)
			}

			dialPretDataConn(t, conn, reader)
			sendPretCommand(t, conn, test.transfer)
			code, response = readPretResponse(t, reader)
			require.Equal(t, StatusBadCommandSequence, code, response)

			consumed, canceled, released, _ := reservation.state()
			assert.False(t, consumed)
			assert.True(t, canceled)
			assert.False(t, released)

			// the old reservation is gone: the exact same command now fails as
			// "no PRET" rather than "mismatch", proving the session marker was
			// cleaned up and not kept forever
			sendPretCommand(t, conn, strings.Fields(test.pretCmd)[1]+" "+strings.Fields(test.pretCmd)[2])
			code, response = readPretResponse(t, reader)
			require.Equal(t, StatusBadCommandSequence, code, response)
			assert.Contains(t, response, "requires a matching PRET")
		})
	}
}

func TestPretDisconnectCancelsPendingReservation(t *testing.T) {
	t.Parallel()

	driver := newPretTestServer(t)
	conn, reader := dialPretControl(t, driver.Addr())

	sendPretCommand(t, conn, "PRET STOR /left-hanging.bin")
	code, response := readPretResponse(t, reader)
	require.Equal(t, StatusFileActionPending, code, response)

	reservation := driver.lastReservation

	require.NoError(t, conn.Close())

	require.Eventually(t, func() bool {
		_, canceled, _, _ := reservation.state()

		return canceled
	}, time.Second, 10*time.Millisecond)

	consumed, canceled, released, _ := reservation.state()
	assert.False(t, consumed)
	assert.True(t, canceled)
	assert.False(t, released)

	kinds := driver.eventKinds()
	assert.Equal(t, []pretEventKind{pretEventPrepared, pretEventCanceled}, kinds)
}

func TestPretPrepareFailure(t *testing.T) {
	t.Parallel()

	driver := newPretTestServer(t)
	conn, reader := dialPretControl(t, driver.Addr())

	driver.mu.Lock()
	driver.prepareFails = true
	driver.mu.Unlock()

	sendPretCommand(t, conn, "PRET STOR /no-node.bin")
	code, response := readPretResponse(t, reader)
	require.Equal(t, StatusActionNotTaken, code, response)
	assert.Contains(t, response, errPretPrepare.Error())

	assert.Empty(t, driver.eventKinds())

	// transfer is rejected: no reservation was created
	dialPretDataConn(t, conn, reader)
	sendPretCommand(t, conn, "STOR /no-node.bin")
	code, response = readPretResponse(t, reader)
	require.Equal(t, StatusBadCommandSequence, code, response)

	// once the node is back, a new PRET works
	driver.mu.Lock()
	driver.prepareFails = false
	driver.mu.Unlock()

	sendPretCommand(t, conn, "PRET STOR /no-node.bin")
	code, response = readPretResponse(t, reader)
	require.Equal(t, StatusFileActionPending, code, response)

	dataConn := dialPretDataConn(t, conn, reader)
	sendPretCommand(t, conn, "STOR /no-node.bin")
	code, response = readPretResponse(t, reader)
	require.Equal(t, StatusFileStatusOK, code, response)
	require.NoError(t, dataConn.Close())
	code, response = readPretResponse(t, reader)
	require.Equal(t, StatusClosingDataConn, code, response)

	waitPretEventKinds(t, driver, []pretEventKind{
		pretEventPrepared, pretEventConsumed, pretEventReleased,
	})
}

func TestPretInvalidSyntax(t *testing.T) {
	t.Parallel()

	driver := newPretTestServer(t)
	conn, reader := dialPretControl(t, driver.Addr())

	sendPretCommand(t, conn, "PRET DELE /whatever")
	code, response := readPretResponse(t, reader)
	require.Equal(t, StatusSyntaxErrorParameters, code, response)

	sendPretCommand(t, conn, "PRET STOR")
	code, response = readPretResponse(t, reader)
	require.Equal(t, StatusSyntaxErrorParameters, code, response)

	sendPretCommand(t, conn, "PRET")
	code, response = readPretResponse(t, reader)
	require.Equal(t, StatusSyntaxErrorParameters, code, response)

	assert.Empty(t, driver.eventKinds())
}

func TestPretDisabledWithoutExtension(t *testing.T) {
	t.Parallel()

	server := NewTestServer(t, false)
	conn, reader := dialPretControl(t, server.Addr())

	sendPretCommand(t, conn, "FEAT")
	_, response := readPretResponse(t, reader)
	assert.NotContains(t, response, " PRET")

	// a driver without the extension is not bothered, transfers stay plain
	sendPretCommand(t, conn, "PRET STOR /file.bin")
	code, response := readPretResponse(t, reader)
	require.Equal(t, StatusCommandNotImplemented, code, response)

	dataConn := dialPretDataConn(t, conn, reader)
	sendPretCommand(t, conn, "STOR /file.bin")
	code, response = readPretResponse(t, reader)
	require.Equal(t, StatusFileStatusOK, code, response)
	require.NoError(t, dataConn.Close())
	code, _ = readPretResponse(t, reader)
	require.Equal(t, StatusClosingDataConn, code)
}

func TestPrestListFlow(t *testing.T) {
	t.Parallel()

	driver := newPretTestServer(t)
	conn, reader := dialPretControl(t, driver.Addr())

	sendPretCommand(t, conn, "PRET LIST")
	code, response := readPretResponse(t, reader)
	require.Equal(t, StatusFileActionPending, code, response)

	dataConn := dialPretDataConn(t, conn, reader)
	sendPretCommand(t, conn, "LIST")
	code, response = readPretResponse(t, reader)
	require.Equal(t, StatusFileStatusOK, code, response)

	_, err := io.Copy(io.Discard, dataConn)
	require.NoError(t, err)
	require.NoError(t, dataConn.Close())
	readPretResponse(t, reader)

	waitPretEventKinds(t, driver, []pretEventKind{
		pretEventPrepared, pretEventConsumed, pretEventReleased,
	})

	info := driver.snapshotEvents()[0].info
	assert.Equal(t, "LIST", info.Command)
	assert.Equal(t, "/", info.Path)
}

func TestPretFailedTransferReleasesWithError(t *testing.T) {
	t.Parallel()

	driver := newPretTestServer(t)
	conn, reader := dialPretControl(t, driver.Addr())

	sendPretCommand(t, conn, "PRET RETR /missing.bin")
	code, response := readPretResponse(t, reader)
	require.Equal(t, StatusFileActionPending, code, response)

	// the file handle lookup fails before any data connection is used
	sendPretCommand(t, conn, "RETR /missing.bin")
	code, response = readPretResponse(t, reader)
	require.Equal(t, StatusActionNotTaken, code, response)

	waitPretEventKinds(t, driver, []pretEventKind{
		pretEventPrepared,
		pretEventConsumed,
		pretEventReleased,
	})

	reservation := driver.lastReservation
	consumed, canceled, released, releaseErr := reservation.state()
	assert.True(t, consumed)
	assert.False(t, canceled)
	assert.True(t, released)
	assert.Error(t, releaseErr)

	// failure does not keep the reservation on the session
	sendPretCommand(t, conn, "RETR /missing.bin")
	code, response = readPretResponse(t, reader)
	require.Equal(t, StatusBadCommandSequence, code, response)
}
