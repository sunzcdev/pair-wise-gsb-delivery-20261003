package sftp

import (
	"errors"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// clientRequestServerPipePair returns a client and a RequestServer
// connected through in-memory pipes, so the tests also run on
// platforms where the unix socket based helpers are skipped.
func clientRequestServerPipePair(t *testing.T, handlers Handlers) (*Client, *RequestServer) {
	t.Helper()

	cr, sw := io.Pipe()
	sr, cw := io.Pipe()

	var options []RequestServerOption
	if *testAllocator {
		options = append(options, WithRSAllocator())
	}

	server := NewRequestServer(struct {
		io.Reader
		io.WriteCloser
	}{sr, sw}, handlers, options...)
	go server.Serve()

	client, err := NewClientPipe(cr, cw)
	require.NoError(t, err)

	return client, server
}

// fsyncTestBackend is an in-memory backend whose writes can be blocked
// and whose syncs are recorded, to test the fsync@openssh.com extension.
type fsyncTestBackend struct {
	// noSync makes Filewrite return writers that do not implement FileSyncer
	noSync bool

	mu    sync.Mutex
	files map[string]*fsyncTestFile
}

func (b *fsyncTestBackend) file(path string) *fsyncTestFile {
	b.mu.Lock()
	defer b.mu.Unlock()

	f, ok := b.files[path]
	if !ok {
		f = &fsyncTestFile{}
		b.files[path] = f
	}
	return f
}

func (b *fsyncTestBackend) getFile(path string) *fsyncTestFile {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.files[path]
}

func (b *fsyncTestBackend) Filewrite(r *Request) (io.WriterAt, error) {
	f := b.file(r.Filepath)
	if b.noSync {
		// hide the Sync method from the returned writer
		return struct{ io.WriterAt }{f}, nil
	}
	return f, nil
}

func (b *fsyncTestBackend) Fileread(r *Request) (io.ReaderAt, error) {
	return b.file(r.Filepath), nil
}

func (b *fsyncTestBackend) Filecmd(r *Request) error {
	return nil
}

func (b *fsyncTestBackend) Filelist(r *Request) (ListerAt, error) {
	return nil, os.ErrNotExist
}

type fsyncTestFile struct {
	// gate, when set, is consumed by the first WriteAt call, which
	// blocks until the channel is closed
	gate chan struct{}
	// writeStart, when set, is signaled when a WriteAt call starts
	writeStart chan struct{}

	writeErr error
	syncErr  error

	mu      sync.Mutex
	content []byte
	events  []string
	syncs   int
}

func (f *fsyncTestFile) WriteAt(p []byte, off int64) (int, error) {
	f.mu.Lock()
	gate := f.gate
	f.gate = nil
	writeStart := f.writeStart
	f.mu.Unlock()

	if writeStart != nil {
		select {
		case writeStart <- struct{}{}:
		default:
		}
	}
	if gate != nil {
		<-gate
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.writeErr != nil {
		return 0, f.writeErr
	}

	if end := int(off) + len(p); end > len(f.content) {
		content := make([]byte, end)
		copy(content, f.content)
		f.content = content
	}
	copy(f.content[off:], p)
	f.events = append(f.events, "write")

	return len(p), nil
}

func (f *fsyncTestFile) ReadAt(p []byte, off int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if off >= int64(len(f.content)) {
		return 0, io.EOF
	}
	n := copy(p, f.content[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (f *fsyncTestFile) Sync() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.syncs++
	f.events = append(f.events, "sync")

	return f.syncErr
}

func (f *fsyncTestFile) numEvents() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.events)
}

func (f *fsyncTestFile) syncCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.syncs
}

func (f *fsyncTestFile) recordedEvents() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

func (f *fsyncTestFile) writtenContent() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return string(f.content)
}

func waitForBackendEvents(t *testing.T, f *fsyncTestFile, n int) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if got := f.numEvents(); got >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d backend events, got %d", n, f.numEvents())
}

// dispatchWrite sends a write request without waiting for the response,
// the response is returned on the given channel.
func dispatchWrite(client *Client, ch chan result, handle string, offset uint64, data []byte) uint32 {
	id := client.nextID()
	client.dispatchRequest(ch, &sshFxpWritePacket{
		ID:     id,
		Handle: handle,
		Offset: offset,
		Length: uint32(len(data)),
		Data:   data,
	})
	return id
}

func requireStatusOK(t *testing.T, ch chan result, id uint32) {
	t.Helper()

	select {
	case r := <-ch:
		require.NoError(t, r.err)
		require.Equal(t, fxp(sshFxpStatus), r.typ)
		require.NoError(t, normaliseError(unmarshalStatus(id, r.data)))
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for status response")
	}
}

// An fsync request must not complete before all the writes received
// before it on the same handle have completed, even if those writes
// complete out of order, and it must not block reads and writes on
// other handles.
func TestRequestServerFsyncWaitsForWrites(t *testing.T) {
	gate := make(chan struct{})
	writeStart := make(chan struct{}, 1)

	backend := &fsyncTestBackend{files: map[string]*fsyncTestFile{
		"/blocked": {gate: gate, writeStart: writeStart},
		"/other":   {},
	}}

	client, server := clientRequestServerPipePair(t, Handlers{backend, backend, backend, backend})
	defer client.Close()
	defer server.Close()

	// open both handles upfront: open requests are served by the same
	// sequential worker that will process the fsync request
	bf, err := client.Create("/blocked")
	require.NoError(t, err)
	defer bf.Close()
	of, err := client.Create("/other")
	require.NoError(t, err)
	defer of.Close()

	blocked := backend.getFile("/blocked")
	other := backend.getFile("/other")

	// the first write blocks inside the backend
	write1Ch := make(chan result, 1)
	write1ID := dispatchWrite(client, write1Ch, bf.handle, 0, []byte("blocked"))
	select {
	case <-writeStart:
	case <-time.After(10 * time.Second):
		t.Fatal("first write did not reach the backend")
	}

	// a second write on the same handle completes out of order,
	// before the blocked first one
	write2Ch := make(chan result, 1)
	write2ID := dispatchWrite(client, write2Ch, bf.handle, 7, []byte("second"))
	waitForBackendEvents(t, blocked, 1)

	// the fsync request is received after both writes
	syncErr := make(chan error, 1)
	go func() {
		syncErr <- bf.Sync()
	}()

	// a write on another handle is received after the fsync request and
	// must not be blocked by the pending barrier; note that its response
	// is only sent after the fsync one, as responses are sent in
	// request order, so check the backend received the write instead
	otherCh := make(chan result, 1)
	otherID := dispatchWrite(client, otherCh, of.handle, 0, []byte("other"))
	waitForBackendEvents(t, other, 1)

	// the fsync request has been received by the server at this point,
	// but it must not complete while the first write is still blocked
	select {
	case err := <-syncErr:
		t.Fatalf("fsync returned before the blocked write completed: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	// release the blocked write, everything can now complete, in order
	close(gate)
	requireStatusOK(t, write1Ch, write1ID)
	requireStatusOK(t, write2Ch, write2ID)
	require.NoError(t, <-syncErr)
	requireStatusOK(t, otherCh, otherID)

	// the backend synced only after both writes completed
	assert.Equal(t, []string{"write", "write", "sync"}, blocked.recordedEvents())
	assert.Equal(t, "blockedsecond", blocked.writtenContent())
	assert.Equal(t, "other", other.writtenContent())
}

// If the backend does not implement FileSyncer, fsync requests must
// fail with an sshFxOPUnsupported status error.
func TestRequestServerFsyncUnsupported(t *testing.T) {
	backend := &fsyncTestBackend{
		noSync: true,
		files:  make(map[string]*fsyncTestFile),
	}

	client, server := clientRequestServerPipePair(t, Handlers{backend, backend, backend, backend})
	defer client.Close()
	defer server.Close()

	f, err := client.Create("/file")
	require.NoError(t, err)
	defer f.Close()

	_, err = f.Write([]byte("hello"))
	require.NoError(t, err)

	err = f.Sync()
	require.Error(t, err)

	var statusErr *StatusError
	require.ErrorAs(t, err, &statusErr)
	assert.Equal(t, ErrSSHFxOpUnsupported, statusErr.FxCode())
}

// A failing write, a failing sync and an unsupported sync must be
// distinguishable from each other.
func TestRequestServerFsyncErrorDistinction(t *testing.T) {
	errWrite := errors.New("backend write failure")
	errSync := errors.New("backend sync failure")

	backend := &fsyncTestBackend{files: map[string]*fsyncTestFile{
		"/write-fails": {writeErr: errWrite},
		"/sync-fails":  {syncErr: errSync},
		"/ok":          {},
	}}

	client, server := clientRequestServerPipePair(t, Handlers{backend, backend, backend, backend})
	defer client.Close()
	defer server.Close()

	// a write failure is reported on the write itself, a later
	// fsync on the same handle still succeeds
	wf, err := client.Create("/write-fails")
	require.NoError(t, err)
	defer wf.Close()

	_, err = wf.Write([]byte("x"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), errWrite.Error())

	require.NoError(t, wf.Sync())

	// a sync failure is reported on the fsync request, writes succeed
	sf, err := client.Create("/sync-fails")
	require.NoError(t, err)
	defer sf.Close()

	_, err = sf.Write([]byte("x"))
	require.NoError(t, err)

	err = sf.Sync()
	require.Error(t, err)
	assert.Contains(t, err.Error(), errSync.Error())

	// successful writes and sync
	okf, err := client.Create("/ok")
	require.NoError(t, err)
	defer okf.Close()

	_, err = okf.Write([]byte("x"))
	require.NoError(t, err)
	require.NoError(t, okf.Sync())

	assert.Equal(t, 1, backend.getFile("/ok").syncCount())
}
