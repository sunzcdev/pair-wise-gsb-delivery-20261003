package ftpserver

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

type fragmentedReadWriter struct {
	readChunks  [][]byte
	writeBuffer bytes.Buffer
}

type failingBlockReadWriter struct {
	err error
}

func (f *failingBlockReadWriter) Read(_ []byte) (int, error) {
	return 0, io.EOF
}

func (f *failingBlockReadWriter) Write(_ []byte) (int, error) {
	return 0, f.err
}

func (f *fragmentedReadWriter) Read(p []byte) (int, error) {
	if len(f.readChunks) == 0 {
		return 0, io.EOF
	}

	chunk := f.readChunks[0]
	n := copy(p, chunk)
	f.readChunks[0] = chunk[n:]

	if len(f.readChunks[0]) == 0 {
		f.readChunks = f.readChunks[1:]
	}

	return n, nil
}

func (f *fragmentedReadWriter) Write(p []byte) (int, error) {
	return f.writeBuffer.Write(p)
}

func TestBlockTransferRead(t *testing.T) {
	t.Parallel()

	t.Run("fragmented header and zero length block", func(t *testing.T) {
		t.Parallel()

		conn := &fragmentedReadWriter{readChunks: [][]byte{
			{0x00}, {0x00}, {0x03},
			{'a'},
			{'b', 'c'},
			{0x00, 0x00, 0x00},
			{0x40, 0x00, 0x00},
			{0x80, 0x00, 0x00},
		}}
		transfer := newBlockTransfer(conn, false)

		data, err := io.ReadAll(transfer)
		require.NoError(t, err)
		require.Equal(t, []byte("abc"), data)
	})

	t.Run("block larger than read buffer and EOF on data block", func(t *testing.T) {
		t.Parallel()

		conn := bytes.NewBuffer([]byte{0x80, 0x00, 0x03, 'a', 'b', 'c'})
		transfer := newBlockTransfer(conn, false)

		buf := make([]byte, 1)
		n, err := transfer.Read(buf)
		require.NoError(t, err)
		require.Equal(t, 1, n)
		require.Equal(t, byte('a'), buf[0])

		data, err := io.ReadAll(transfer)
		require.NoError(t, err)
		require.Equal(t, []byte("bc"), data)
	})

	t.Run("fragmented final data block", func(t *testing.T) {
		t.Parallel()

		conn := &fragmentedReadWriter{readChunks: [][]byte{
			{0x80}, {0x00}, {0x02},
			{'o'}, {'k'},
		}}
		transfer := newBlockTransfer(conn, false)

		data, err := io.ReadAll(transfer)
		require.NoError(t, err)
		require.Equal(t, []byte("ok"), data)
	})

	t.Run("restart marker", func(t *testing.T) {
		t.Parallel()

		conn := bytes.NewBuffer([]byte{
			0x20, 0x00, 0x02, '1', '2',
			0x00, 0x00, 0x01, 'x',
			0x80, 0x00, 0x00,
		})
		transfer := newBlockTransfer(conn, false)

		data, err := io.ReadAll(transfer)
		require.NoError(t, err)
		require.Equal(t, []byte("x"), data)
	})

	t.Run("TCP EOF before marker", func(t *testing.T) {
		t.Parallel()

		conn := bytes.NewBuffer([]byte{0x00, 0x00, 0x01, 'x'})
		transfer := newBlockTransfer(conn, false)

		_, err := io.ReadAll(transfer)
		require.ErrorIs(t, err, errBlockTransferIncomplete)
	})

	t.Run("half header before TCP EOF", func(t *testing.T) {
		t.Parallel()

		conn := &fragmentedReadWriter{readChunks: [][]byte{{0x00, 0x00}}}
		transfer := newBlockTransfer(conn, false)

		_, err := io.ReadAll(transfer)
		require.ErrorIs(t, err, errBlockTransferIncomplete)
	})

	t.Run("payload before TCP EOF", func(t *testing.T) {
		t.Parallel()

		conn := &fragmentedReadWriter{readChunks: [][]byte{{0x00, 0x00, 0x02, 'x'}}}
		transfer := newBlockTransfer(conn, false)

		_, err := io.ReadAll(transfer)
		require.ErrorIs(t, err, errBlockTransferIncomplete)
	})

	t.Run("invalid descriptor", func(t *testing.T) {
		t.Parallel()

		conn := bytes.NewBuffer([]byte{0x01, 0x00, 0x00})
		transfer := newBlockTransfer(conn, false)

		_, err := io.ReadAll(transfer)
		require.ErrorIs(t, err, errBlockTransferInvalid)
	})

	t.Run("invalid combined restart descriptor", func(t *testing.T) {
		t.Parallel()

		conn := bytes.NewBuffer([]byte{0x60, 0x00, 0x00})
		transfer := newBlockTransfer(conn, false)

		_, err := io.ReadAll(transfer)
		require.ErrorIs(t, err, errBlockTransferInvalid)
	})
}

func TestBlockTransferWrite(t *testing.T) {
	t.Parallel()

	conn := &bytes.Buffer{}
	transfer := newBlockTransfer(conn, true)

	data := make([]byte, blockMaxDataLen+1)
	data[0] = 'a'
	data[blockMaxDataLen] = 'b'

	n, err := transfer.Write(data)
	require.NoError(t, err)
	require.Equal(t, len(data), n)
	require.NoError(t, transfer.FinalizeTransfer())

	expected := []byte{0x00, 0xff, 0xff}
	expected = append(expected, make([]byte, blockMaxDataLen)...)
	expected = append(expected, 0x00, 0x00, 0x01, 'b', 0x80, 0x00, 0x00)
	expected[3] = 'a'
	require.Equal(t, expected, conn.Bytes())
}

func TestBlockTransferFinalizeUpload(t *testing.T) {
	t.Parallel()

	conn := &bytes.Buffer{}
	transfer := newBlockTransfer(conn, false)

	require.NoError(t, transfer.FinalizeTransfer())
	require.Empty(t, conn.Bytes())
}

func TestBlockTransferFinalizeDownloadError(t *testing.T) {
	t.Parallel()

	expectedErr := errors.New("write failed")
	transfer := newBlockTransfer(&failingBlockReadWriter{err: expectedErr}, true)

	err := transfer.FinalizeTransferWithError(nil)
	require.ErrorIs(t, err, expectedErr)
}

func TestBlockTransferErrorCode(t *testing.T) {
	t.Parallel()

	require.Equal(t, StatusTransferAborted, getErrorCode(errBlockTransferIncomplete, StatusActionNotTaken))
	require.True(t, errors.Is(errBlockTransferIncomplete, errBlockTransferIncomplete))
}
