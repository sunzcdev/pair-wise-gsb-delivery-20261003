package sftp

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// blockableFile is a controllable file backend used to prove that the
// fsync barrier waits for earlier writes on the same handle without
// blocking operations on other handles.
//
// WriteAt blocks on writeGate (if non-nil) before touching the backend;
// Sync blocks on syncGate (if non-nil). The recorded channels let a test
// observe exactly how far each request got.
type blockableFile struct {
	name string

	mu      sync.Mutex
	content []byte

	writeErr  error
	syncErr   error
	noSync    bool // when true, the value does not implement syncer
	writeGate chan struct{}
	syncGate  chan struct{}

	writeStarted chan struct{}
	syncStarted  chan struct{}
	readStarted  chan struct{}

	syncCalled chan struct{}

	writeOnce sync.Once
	syncOnce  sync.Once
}

func (f *blockableFile) Stat() (os.FileInfo, error) {
	return fakeFileInfo{name: f.name}, nil
}

func (f *blockableFile) ReadAt(b []byte, off int64) (int, error) {
	if f.readStarted != nil {
		select {
		case <-f.readStarted:
		default:
			close(f.readStarted)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	if off >= int64(len(f.content)) {
		return 0, io.EOF
	}

	n := copy(b, f.content[off:])
	if n < len(b) {
		return n, io.EOF
	}
	return n, nil
}

func (f *blockableFile) WriteAt(b []byte, off int64) (int, error) {
	if f.writeStarted != nil {
		f.writeOnce.Do(func() { close(f.writeStarted) })
	}
	if f.writeGate != nil {
		<-f.writeGate
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.writeErr != nil {
		return 0, f.writeErr
	}

	grow := int64(len(b)) + off - int64(len(f.content))
	if grow > 0 {
		f.content = append(f.content, make([]byte, grow)...)
	}
	return copy(f.content[off:], b), nil
}

func (f *blockableFile) Readdir(int) ([]os.FileInfo, error) { return nil, nil }
func (f *blockableFile) Name() string                       { return f.name }
func (f *blockableFile) Truncate(int64) error               { return nil }
func (f *blockableFile) Chmod(fs.FileMode) error            { return nil }
func (f *blockableFile) Chown(int, int) error               { return nil }
func (f *blockableFile) Close() error                       { return nil }

func (f *blockableFile) Sync() error {
	if f.syncStarted != nil {
		f.syncOnce.Do(func() { close(f.syncStarted) })
	}
	if f.syncCalled != nil {
		select {
		case <-f.syncCalled:
		default:
			close(f.syncCalled)
		}
	}
	if f.syncGate != nil {
		<-f.syncGate
	}
	return f.syncErr
}

type fakeFileInfo struct{ name string }

func (fi fakeFileInfo) Name() string       { return fi.name }
func (fi fakeFileInfo) Size() int64        { return 0 }
func (fi fakeFileInfo) Mode() fs.FileMode  { return 0o644 }
func (fi fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (fi fakeFileInfo) IsDir() bool        { return false }
func (fi fakeFileInfo) Sys() any           { return nil }

// sendRaw marshals a packet via its MarshalBinary and writes the complete
// frame directly, bypassing the Client's request serialization.
func sendRaw(t *testing.T, w io.Writer, p interface{ MarshalBinary() ([]byte, error) }) {
	t.Helper()
	data, err := p.MarshalBinary()
	require.NoError(t, err)

	buf := make([]byte, 4)
	binary.BigEndian.PutUint32(buf, uint32(len(data)-4))
	buf = append(buf, data[4:]...)

	_, err = w.Write(buf)
	require.NoError(t, err)
}

// recvRaw reads one SFTP packet frame: a type byte followed by the body.
func recvRaw(t *testing.T, r io.Reader) (fxp, []byte) {
	t.Helper()

	var header [4]byte
	_, err := io.ReadFull(r, header[:])
	require.NoError(t, err)

	length := binary.BigEndian.Uint32(header[:])
	body := make([]byte, length)
	_, err = io.ReadFull(r, body)
	require.NoError(t, err)

	return fxp(body[0]), body[1:]
}

func statusCodeFromRaw(t *testing.T, r io.Reader) uint32 {
	t.Helper()
	typ, data := recvRaw(t, r)
	require.Equal(t, fxp(sshFxpStatus), typ)
	_, data = unmarshalUint32(data)
	code, _, _ := unmarshalUint32Safe(data)
	return code
}

// rawServerPairWithFiles starts a Server on a loopback connection backed by
// the supplied files (keyed by client-visible path) and returns the client
// side of the connection after the SFTP version handshake.
func rawServerPairWithFiles(t *testing.T, files map[string]*blockableFile) (net.Conn, *Server) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { listener.Close() })

	type accepted struct {
		conn net.Conn
		err  error
	}
	acceptedCh := make(chan accepted, 1)
	go func() {
		c, err := listener.Accept()
		acceptedCh <- accepted{c, err}
	}()

	conn, err := net.Dial("tcp", listener.Addr().String())
	require.NoError(t, err)

	acc := <-acceptedCh
	require.NoError(t, acc.err)

	server, err := NewServer(struct {
		io.Reader
		io.WriteCloser
	}{acc.conn, acc.conn})
	require.NoError(t, err)

	server.openFileFn = func(name string, flag int, perm fs.FileMode) (file, error) {
		for clientPath, f := range files {
			if server.toLocalPath(clientPath) == name {
				return f, nil
			}
		}
		return nil, os.ErrNotExist
	}

	go server.Serve()

	// SSH_FXP_INIT
	sendRaw(t, conn, &sshFxInitPacket{Version: sftpProtocolVersion})
	// consume SSH_FXP_VERSION
	versionType, _ := recvRaw(t, conn)
	require.Equal(t, fxp(sshFxpVersion), versionType)

	return conn, server
}

func rawOpen(t *testing.T, conn net.Conn, pathName string) string {
	t.Helper()

	id := uint32(time.Now().UnixNano())
	sendRaw(t, conn, &sshFxpOpenPacket{
		ID:     id,
		Path:   pathName,
		Pflags: sshFxfRead | sshFxfWrite | sshFxfCreat,
		Attrs:  emptyFileStat,
	})

	typ, data := recvRaw(t, conn)
	require.Equal(t, fxp(sshFxpHandle), typ)
	gotID, data := unmarshalUint32(data)
	require.Equal(t, id, gotID)
	handle, _, err := unmarshalStringSafe(data)
	require.NoError(t, err)
	return handle
}

// recvOne tries to read one packet frame without failing the test; it
// returns 0 on a read timeout/deadline.
func recvOne(r io.Reader) (fxp, []byte) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, nil
	}
	length := binary.BigEndian.Uint32(header[:])
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return 0, nil
	}
	return fxp(body[0]), body[1:]
}

// clientServerPairWithFiles starts a classic Server whose open file handles
// are backed by the supplied files, keyed by the client-visible path.
func clientServerPairWithFiles(t *testing.T, files map[string]*blockableFile) (*Client, *Server) {
	t.Helper()

	// Use a real loopback connection so the transport has kernel buffering:
	// a blocked server-side backend must not stop the server from receiving
	// further client packets (which io.Pipe/net.Pipe would do).
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { listener.Close() })

	serverConnCh := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			serverConnCh <- conn
		}
	}()

	clientConn, err := net.Dial("tcp", listener.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { clientConn.Close() })
	serverConn := <-serverConnCh

	server, err := NewServer(struct {
		io.Reader
		io.WriteCloser
	}{serverConn, serverConn})
	require.NoError(t, err)

	server.openFileFn = func(name string, flag int, perm fs.FileMode) (file, error) {
		for clientPath, f := range files {
			if server.toLocalPath(clientPath) == name {
				if f.noSync {
					return &blockableFileNoSync{f}, nil
				}
				return f, nil
			}
		}
		return nil, os.ErrNotExist
	}

	go server.Serve()

	client, err := NewClientPipe(clientConn, clientConn)
	require.NoError(t, err)

	return client, server
}

// blockableFileNoSync is a file backend that deliberately does not implement
// syncer, simulating storage that cannot flush to stable storage.
type blockableFileNoSync struct{ f *blockableFile }

func (w *blockableFileNoSync) Stat() (os.FileInfo, error) { return w.f.Stat() }
func (w *blockableFileNoSync) ReadAt(b []byte, off int64) (int, error) {
	return w.f.ReadAt(b, off)
}
func (w *blockableFileNoSync) WriteAt(b []byte, off int64) (int, error) {
	return w.f.WriteAt(b, off)
}
func (w *blockableFileNoSync) Readdir(n int) ([]os.FileInfo, error) { return w.f.Readdir(n) }
func (w *blockableFileNoSync) Name() string                         { return w.f.Name() }
func (w *blockableFileNoSync) Truncate(size int64) error            { return w.f.Truncate(size) }
func (w *blockableFileNoSync) Chmod(m fs.FileMode) error            { return w.f.Chmod(m) }
func (w *blockableFileNoSync) Chown(uid, gid int) error             { return w.f.Chown(uid, gid) }
func (w *blockableFileNoSync) Close() error                         { return w.f.Close() }

// TestServerFsyncBarrier proves that an fsync request picked up by a worker
// before an earlier write finishes cannot succeed until that write
// completes, and that operations on a second handle keep running.
func TestServerFsyncBarrier(t *testing.T) {
	const (
		pathA = "/barrier-a"
		pathB = "/barrier-b"
	)

	writeStarted := make(chan struct{})
	syncStarted := make(chan struct{})
	readStarted := make(chan struct{})
	writeGate := make(chan struct{})
	syncGate := make(chan struct{})

	fileA := &blockableFile{
		name:         pathA,
		writeStarted: writeStarted,
		syncStarted:  syncStarted,
		writeGate:    writeGate,
		syncGate:     syncGate,
	}
	fileB := &blockableFile{
		name:        pathB,
		readStarted: readStarted,
		content:     []byte("world"),
	}

	// Drive the server with raw SFTP frames so the Client library's
	// request serialization (which would never interleave write/fsync on
	// one File) cannot mask a broken server-side barrier.
	conn, server := rawServerPairWithFiles(t, map[string]*blockableFile{
		pathA: fileA,
		pathB: fileB,
	})
	defer server.Close()
	defer conn.Close()

	handleA, handleB := rawOpen(t, conn, pathA), rawOpen(t, conn, pathB)

	var id uint32
	nextID := func() uint32 { id++; return id }

	// 1. Dispatch a write that blocks in the backend.
	writeID := nextID()
	sendRaw(t, conn, &sshFxpWritePacket{
		ID:     writeID,
		Handle: handleA,
		Offset: 0,
		Length: uint32(len("hello")),
		Data:   []byte("hello"),
	})
	<-writeStarted

	// 2. Dispatch fsync on the same handle while the write is still
	// running. The fsync worker must wait even though it picked up the
	// request.
	fsyncID := nextID()
	sendRaw(t, conn, &sshFxpFsyncPacket{ID: fsyncID, Handle: handleA})

	select {
	case <-syncStarted:
		t.Fatal("fsync backend called before the earlier write completed")
	case <-time.After(20 * time.Millisecond):
	}

	// 3. A read on a different handle must reach and run on the backend
	// immediately: the barrier must not stall operations on other handles.
	readID := nextID()
	sendRaw(t, conn, &sshFxpReadPacket{
		ID: readID, Handle: handleB, Offset: 0, Len: 5,
	})

	select {
	case <-readStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("read on a different handle did not reach the backend")
	}

	// The controller emits replies in request order, and the blocked write
	// is earliest, so no reply is delivered yet.
	// 4. The fsync reply must not have overtaken the write: nothing is
	// readable while the write is still blocked.
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(20*time.Millisecond)))
	if typ, _ := recvOne(conn); typ != 0 {
		t.Fatalf("unexpected early reply type %d while write outstanding", typ)
	}
	var zeroDeadline time.Time
	require.NoError(t, conn.SetReadDeadline(zeroDeadline))

	// 5. Finish the write. Its OK reply is delivered first, releasing the
	// barrier so the blocked fsync worker may finally call Sync.
	close(writeGate)
	assert.Equal(t, uint32(sshFxOk), statusCodeFromRaw(t, conn))

	select {
	case <-syncStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("fsync did not start after the write completed")
	}

	// 6. While the backend Sync is blocked, neither the fsync reply nor any
	// later reply is delivered (the controller keeps global reply order).
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(20*time.Millisecond)))
	if typ, _ := recvOne(conn); typ != 0 {
		t.Fatalf("fsync reported before backend sync completed: type %d", typ)
	}
	require.NoError(t, conn.SetReadDeadline(zeroDeadline))

	close(syncGate)
	assert.Equal(t, uint32(sshFxOk), statusCodeFromRaw(t, conn))

	// The other handle's read was executed long ago (readStarted) and its
	// queued reply is now delivered.
	readType, readData := recvRaw(t, conn)
	require.Equal(t, fxp(sshFxpData), readType)
	readIDGot, readData := unmarshalUint32(readData)
	require.Equal(t, readID, readIDGot)
	readLen, readData, _ := unmarshalUint32Safe(readData)
	assert.Equal(t, uint32(5), readLen)
	assert.Equal(t, []byte("world"), readData[:5])

	fileA.mu.Lock()
	assert.Equal(t, []byte("hello"), fileA.content)
	fileA.mu.Unlock()
}

// TestServerFsyncUnsupported checks the report when the backend cannot
// synchronize at all.
func TestServerFsyncUnsupported(t *testing.T) {
	const pathName = "/unsupported"

	backend := &blockableFile{name: pathName, noSync: true}

	client, server := clientServerPairWithFiles(t, map[string]*blockableFile{pathName: backend})
	defer client.Close()
	defer server.Close()

	f, err := client.OpenFile(pathName, os.O_RDWR|os.O_CREATE)
	require.NoError(t, err)
	defer f.Close()

	err = f.Sync()
	var statusErr *StatusError
	require.ErrorAs(t, err, &statusErr)
	assert.Equal(t, uint32(sshFxOPUnsupported), statusErr.Code)
}

var (
	errFsyncTestWrite = errors.New("fsync-test: backend write failed")
	errFsyncTestSync  = errors.New("fsync-test: backend sync failed")
)

// TestServerFsyncWriteFailure checks that a failed earlier write is
// reported as a generic failure distinct from a sync failure.
func TestServerFsyncWriteFailure(t *testing.T) {
	const pathName = "/write-fail"

	backend := &blockableFile{name: pathName, writeErr: errFsyncTestWrite}

	client, server := clientServerPairWithFiles(t, map[string]*blockableFile{pathName: backend})
	defer client.Close()
	defer server.Close()

	f, err := client.OpenFile(pathName, os.O_RDWR|os.O_CREATE)
	require.NoError(t, err)
	defer f.Close()

	_, werr := f.WriteAt([]byte("x"), 0)
	require.Error(t, werr)
	assert.Contains(t, werr.Error(), errFsyncTestWrite.Error())

	// With the failed write drained, fsync itself succeeds here: the write
	// barrier releases once a write finishes regardless of its result.
	require.NoError(t, f.Sync())
}

// TestServerFsyncSyncFailure checks the report when the backend Sync call
// itself fails.
func TestServerFsyncSyncFailure(t *testing.T) {
	const pathName = "/sync-fail"

	backend := &blockableFile{name: pathName, syncErr: errFsyncTestSync}

	client, server := clientServerPairWithFiles(t, map[string]*blockableFile{pathName: backend})
	defer client.Close()
	defer server.Close()

	f, err := client.OpenFile(pathName, os.O_RDWR|os.O_CREATE)
	require.NoError(t, err)
	defer f.Close()

	err = f.Sync()
	var statusErr *StatusError
	require.ErrorAs(t, err, &statusErr)
	assert.Equal(t, uint32(sshFxFailure), statusErr.Code)
	assert.Contains(t, statusErr.Error(), errFsyncTestSync.Error())
	assert.NotContains(t, statusErr.Error(), errFsyncTestWrite.Error())
}

// TestServerFsyncBadHandle checks the report for fsync against a handle that
// is not open.
func TestServerFsyncBadHandle(t *testing.T) {
	client, server := clientServerPair(t)
	defer client.Close()
	defer server.Close()

	id := client.nextID()
	typ, data, err := client.clientConn.sendPacket(context.Background(), nil, &sshFxpFsyncPacket{
		ID:     id,
		Handle: "no-such-handle",
	})
	require.NoError(t, err)
	require.Equal(t, fxp(sshFxpStatus), typ)

	err = normaliseError(unmarshalStatus(id, data))
	var statusErr *StatusError
	require.ErrorAs(t, err, &statusErr)
	assert.Equal(t, uint32(sshFxFailure), statusErr.Code)
	assert.Contains(t, statusErr.Error(), "bad file descriptor")
}

// TestPacketManagerBarrierSnapshot checks the barrier semantics directly:
// fsync waits only for earlier writes to the same handle.
func TestPacketManagerBarrierSnapshot(t *testing.T) {
	mgr := newPktMgr(nil)

	writeA := mgr.registerIO(2, "hA", true)
	writeB := mgr.registerIO(3, "hB", true)
	readA := mgr.registerIO(4, "hA", false)

	fsyncA := mgr.registerIO(5, "hA", true) // dispatched after fsync

	proceeded := make(chan struct{})
	go func() {
		mgr.waitForPriorIO(6, "hA", true) // fsync on hA, orderID 6
		close(proceeded)
	}()

	select {
	case <-proceeded:
		t.Fatal("barrier passed while an earlier write was in flight")
	case <-time.After(20 * time.Millisecond):
	}

	// Completing a write on another handle must not help.
	writeB()
	select {
	case <-proceeded:
		t.Fatal("barrier passed when only an other-handle write completed")
	case <-time.After(20 * time.Millisecond):
	}

	// Completing the later (post-fsync) write must not help either.
	fsyncA()
	select {
	case <-proceeded:
		t.Fatal("barrier passed when only a later write completed")
	case <-time.After(20 * time.Millisecond):
	}

	// Reads are ignored by the write-only barrier.
	readA()
	select {
	case <-proceeded:
		t.Fatal("write-only barrier was released by a read completion")
	case <-time.After(20 * time.Millisecond):
	}

	writeA()
	select {
	case <-proceeded:
	case <-time.After(2 * time.Second):
		t.Fatal("barrier did not pass after the earlier write completed")
	}
}

// TestPacketManagerCloseBarrier checks that close waits on both prior reads
// and writes, scoped to the same handle.
func TestPacketManagerCloseBarrier(t *testing.T) {
	mgr := newPktMgr(nil)

	writeA := mgr.registerIO(2, "hA", true)
	readA := mgr.registerIO(3, "hA", false)
	writeB := mgr.registerIO(4, "hB", true)

	closed := make(chan struct{})
	go func() {
		mgr.waitForPriorIO(5, "hA", false)
		close(closed)
	}()

	writeB() // other handle: irrelevant
	select {
	case <-closed:
		t.Fatal("close barrier ignored an earlier write on the same handle")
	case <-time.After(20 * time.Millisecond):
	}

	writeA()
	select {
	case <-closed:
		t.Fatal("close barrier ignored an earlier read on the same handle")
	case <-time.After(20 * time.Millisecond):
	}

	readA()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("close barrier did not pass after prior IO completed")
	}
}

// requestFsyncFile is a WriterAtReaderAt returned by a request-server
// handler, implementing Sync so the fsync extension reaches it.
type requestFsyncFile struct {
	content []byte

	mu sync.Mutex

	writeGate    chan struct{}
	syncGate     chan struct{}
	writeStarted chan struct{}
	syncStarted  chan struct{}

	writeErr error
	syncErr  error
}

func (f *requestFsyncFile) ReadAt(b []byte, off int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if off >= int64(len(f.content)) {
		return 0, io.EOF
	}
	return copy(b, f.content[off:]), nil
}

func (f *requestFsyncFile) WriteAt(b []byte, off int64) (int, error) {
	if f.writeStarted != nil {
		select {
		case <-f.writeStarted:
		default:
			close(f.writeStarted)
		}
	}
	if f.writeGate != nil {
		<-f.writeGate
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	grow := int64(len(b)) + off - int64(len(f.content))
	if grow > 0 {
		f.content = append(f.content, make([]byte, grow)...)
	}
	return copy(f.content[off:], b), nil
}

func (f *requestFsyncFile) Sync() error {
	if f.syncStarted != nil {
		select {
		case <-f.syncStarted:
		default:
			close(f.syncStarted)
		}
	}
	if f.syncGate != nil {
		<-f.syncGate
	}
	return f.syncErr
}

type fsyncRequestHandler struct {
	files map[string]*requestFsyncFile
	mu    sync.Mutex
}

func (h *fsyncRequestHandler) Filewrite(r *Request) (io.WriterAt, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if f, ok := h.files[r.Filepath]; ok {
		return f, nil
	}
	return nil, os.ErrNotExist
}

func (h *fsyncRequestHandler) OpenFile(r *Request) (WriterAtReaderAt, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if f, ok := h.files[r.Filepath]; ok {
		return f, nil
	}
	return nil, os.ErrNotExist
}

func (h *fsyncRequestHandler) Fileread(r *Request) (io.ReaderAt, error) {
	return h.OpenFile(r)
}

func (h *fsyncRequestHandler) Filecmd(r *Request) error { return nil }
func (h *fsyncRequestHandler) Filelist(r *Request) (ListerAt, error) {
	return listerat(nil), nil
}

// TestRequestServerFsyncBarrier exercises the same write/fsync barrier on
// the request handler based server, using the real client. File.WriteAt
// holds only an RLock, so write and Sync can be dispatched concurrently;
// the server-side barrier must still order them.
func TestRequestServerFsyncBarrier(t *testing.T) {
	const pathName = "/rs-fsync"

	writeStarted := make(chan struct{})
	syncStarted := make(chan struct{})
	writeGate := make(chan struct{})
	syncGate := make(chan struct{})

	backend := &requestFsyncFile{
		writeStarted: writeStarted,
		syncStarted:  syncStarted,
		writeGate:    writeGate,
		syncGate:     syncGate,
	}
	handler := &fsyncRequestHandler{files: map[string]*requestFsyncFile{pathName: backend}}

	p := clientRequestServerPairWithHandlers(t, Handlers{
		FileGet: handler, FilePut: handler, FileCmd: handler, FileList: handler,
	})
	defer p.Close()

	f, err := p.cli.OpenFile(pathName, os.O_RDWR|os.O_CREATE)
	require.NoError(t, err)
	defer f.Close()

	writeDone := make(chan error, 1)
	go func() {
		_, err := f.WriteAt([]byte("payload"), 0)
		writeDone <- err
	}()

	<-writeStarted

	fsyncDone := make(chan error, 1)
	go func() { fsyncDone <- f.Sync() }()

	select {
	case <-syncStarted:
		t.Fatal("fsync backend called before the earlier write completed")
	case <-time.After(20 * time.Millisecond):
	}

	close(writeGate)
	require.NoError(t, <-writeDone)

	select {
	case <-syncStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("fsync did not start after the write completed")
	}

	select {
	case err := <-fsyncDone:
		t.Fatalf("fsync reported before backend sync completed: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	close(syncGate)
	require.NoError(t, <-fsyncDone)

	backend.mu.Lock()
	assert.Equal(t, []byte("payload"), backend.content)
	backend.mu.Unlock()
}

// TestRequestServerFsyncSyncFailure covers the request-server path when
// the backend Sync fails.
func TestRequestServerFsyncSyncFailure(t *testing.T) {
	const pathName = "/rs-fsync-fail"

	backend := &requestFsyncFile{syncErr: errFsyncTestSync}
	handler := &fsyncRequestHandler{files: map[string]*requestFsyncFile{pathName: backend}}

	p := clientRequestServerPairWithHandlers(t, Handlers{
		FileGet: handler, FilePut: handler, FileCmd: handler, FileList: handler,
	})
	defer p.Close()

	f, err := p.cli.OpenFile(pathName, os.O_RDWR|os.O_CREATE)
	require.NoError(t, err)
	defer f.Close()

	err = f.Sync()
	var statusErr *StatusError
	require.ErrorAs(t, err, &statusErr)
	assert.Equal(t, uint32(sshFxFailure), statusErr.Code)
	assert.Contains(t, statusErr.Error(), errFsyncTestSync.Error())
}

// TestRequestServerFsyncUnsupported covers the request-server path when a
// backend does not implement a Sync method.
func TestRequestServerFsyncUnsupported(t *testing.T) {
	const pathName = "/rs-fsync-nosync"

	handler := &testHandler{filecontents: filecontents, output: &fakefile{}}

	p := clientRequestServerPairWithHandlers(t, Handlers{
		FileGet: handler, FilePut: handler, FileCmd: handler, FileList: handler,
	})
	defer p.Close()

	f, err := p.cli.OpenFile(pathName, os.O_RDWR|os.O_CREATE)
	require.NoError(t, err)
	defer f.Close()

	err = f.Sync()
	var statusErr *StatusError
	require.ErrorAs(t, err, &statusErr)
	assert.Equal(t, uint32(sshFxOPUnsupported), statusErr.Code)
}
