package ftpserver

import (
	"bytes"
	"io"
	"testing"
	"time"

	"github.com/secsy/goftp"
	"github.com/stretchr/testify/require"
)

// makeBlockModeBlock builds a hand-crafted MODE B block: 1 descriptor byte,
// 2-byte big-endian byte count, then the payload.
func makeBlockModeBlock(descriptor byte, data []byte) []byte {
	block := []byte{descriptor, byte(len(data) >> 8), byte(len(data))}

	return append(block, data...)
}

func dialRawConn(t *testing.T, server *FtpServer) (*goftp.Client, goftp.RawConn) {
	t.Helper()

	client, err := goftp.DialConfig(goftp.Config{
		User:     authUser,
		Password: authPass,
	}, server.Addr())
	require.NoError(t, err, "Couldn't connect")

	raw, err := client.OpenRawConn()
	require.NoError(t, err, "Couldn't open raw connection")

	return client, raw
}

// TestBlockModeUpload sends hand-crafted blocks, including a header split
// across multiple TCP writes and a zero-length block, and expects the file
// to be stored only once the end-of-file marker block is received.
func TestBlockModeUpload(t *testing.T) {
	server := NewTestServer(t, false)
	client, raw := dialRawConn(t, server)

	defer func() { panicOnError(client.Close()) }()

	rc, msg, err := raw.SendCommand("MODE B")
	require.NoError(t, err)
	require.Equal(t, StatusOK, rc, msg)

	dcGetter, err := raw.PrepareDataConn()
	require.NoError(t, err)

	rc, msg, err = raw.SendCommand("STOR block.bin")
	require.NoError(t, err)
	require.Equal(t, StatusFileStatusOK, rc, msg)

	dataConn, err := dcGetter()
	require.NoError(t, err)

	part1 := []byte("hello ")
	part2 := []byte("block mode")

	// First block: send the 3-byte header byte by byte so that the server
	// receives partial headers on each network read.
	header := []byte{0, byte(len(part1) >> 8), byte(len(part1))}
	for _, headerByte := range header {
		_, err = dataConn.Write([]byte{headerByte})
		require.NoError(t, err)

		time.Sleep(20 * time.Millisecond)
	}

	_, err = dataConn.Write(part1)
	require.NoError(t, err)

	// A zero-length block carries no data and must not end the transfer
	_, err = dataConn.Write(makeBlockModeBlock(0, nil))
	require.NoError(t, err)

	// Second block: header and payload in a single write
	_, err = dataConn.Write(makeBlockModeBlock(0, part2))
	require.NoError(t, err)

	// End-of-file marker block: only now is the file complete
	_, err = dataConn.Write(makeBlockModeBlock(blockDescriptorEOF, nil))
	require.NoError(t, err)

	require.NoError(t, dataConn.Close())

	rc, msg, err = raw.ReadResponse()
	require.NoError(t, err)
	require.Equal(t, StatusClosingDataConn, rc, msg)

	// Go back to stream mode and verify the uploaded content
	rc, msg, err = raw.SendCommand("MODE S")
	require.NoError(t, err)
	require.Equal(t, StatusOK, rc, msg)

	var buf bytes.Buffer
	require.NoError(t, client.Retrieve("block.bin", &buf))
	require.Equal(t, "hello block mode", buf.String())
}

// TestBlockModeUploadIncomplete makes sure a TCP close is never mistaken for
// the end of the file: without an end-of-file marker block the transfer must
// fail and no success response must be sent.
func TestBlockModeUploadIncomplete(t *testing.T) {
	server := NewTestServer(t, false)
	client, raw := dialRawConn(t, server)

	defer func() { panicOnError(client.Close()) }()

	rc, msg, err := raw.SendCommand("MODE B")
	require.NoError(t, err)
	require.Equal(t, StatusOK, rc, msg)

	run := func(t *testing.T, name string, chunks [][]byte) {
		t.Helper()

		dcGetter, err := raw.PrepareDataConn()
		require.NoError(t, err)

		rc, msg, err := raw.SendCommand("STOR " + name)
		require.NoError(t, err)
		require.Equal(t, StatusFileStatusOK, rc, msg)

		dataConn, err := dcGetter()
		require.NoError(t, err)

		for _, chunk := range chunks {
			_, err = dataConn.Write(chunk)
			require.NoError(t, err)

			time.Sleep(10 * time.Millisecond)
		}

		// close the connection without completing the file
		require.NoError(t, dataConn.Close())

		rc, msg, err = raw.ReadResponse()
		require.NoError(t, err)
		require.Equal(t, StatusActionNotTaken, rc, msg)
	}

	t.Run("no-eof-marker", func(t *testing.T) {
		// complete data blocks but the connection closes without the
		// end-of-file marker block
		run(t, "noeof.bin", [][]byte{
			makeBlockModeBlock(0, []byte("some data")),
		})
	})

	t.Run("truncated-block", func(t *testing.T) {
		// the header announces 100 bytes but only a few are sent
		run(t, "truncated.bin", [][]byte{
			{0, 0, 100},
			[]byte("too few"),
		})
	})

	t.Run("truncated-header", func(t *testing.T) {
		// only part of a block header is sent before disconnecting
		run(t, "halfheader.bin", [][]byte{
			makeBlockModeBlock(0, []byte("first block")),
			{0, 0},
		})
	})
}

// TestBlockModeDownload checks that a RETR in MODE B sends properly framed
// blocks terminated by an end-of-file marker block.
func TestBlockModeDownload(t *testing.T) {
	server := NewTestServer(t, false)
	client, raw := dialRawConn(t, server)

	defer func() { panicOnError(client.Close()) }()

	content := []byte("some content downloaded in block mode")
	require.NoError(t, client.Store("download.bin", bytes.NewReader(content)))

	rc, msg, err := raw.SendCommand("MODE B")
	require.NoError(t, err)
	require.Equal(t, StatusOK, rc, msg)

	dcGetter, err := raw.PrepareDataConn()
	require.NoError(t, err)

	rc, msg, err = raw.SendCommand("RETR download.bin")
	require.NoError(t, err)
	require.Equal(t, StatusFileStatusOK, rc, msg)

	dataConn, err := dcGetter()
	require.NoError(t, err)

	data, err := io.ReadAll(dataConn)
	require.NoError(t, err)
	require.NoError(t, dataConn.Close())

	rc, msg, err = raw.ReadResponse()
	require.NoError(t, err)
	require.Equal(t, StatusClosingDataConn, rc, msg)

	// parse the received blocks by hand
	var out bytes.Buffer

	eofSeen := false

	for len(data) >= blockHeaderSize {
		descriptor := data[0]
		count := int(data[1])<<8 | int(data[2])
		data = data[blockHeaderSize:]
		require.LessOrEqual(t, count, len(data), "block announces more data than received")

		if descriptor&blockDescriptorEOF != 0 {
			eofSeen = true
			require.Zero(t, count, "the end-of-file marker block must be empty")

			break
		}

		out.Write(data[:count])
		data = data[count:]
	}

	require.True(t, eofSeen, "the transfer must end with an end-of-file marker block")
	require.Equal(t, content, out.Bytes())
}

// TestBlockModeDoesNotLeak verifies that stream mode stays the default and
// that MODE Z keeps its deflate behavior: MODE B is only used when explicitly
// requested.
func TestBlockModeDoesNotLeak(t *testing.T) {
	settings := &Settings{
		DeflateCompressionLevel: 5,
		DefaultTransferType:     TransferTypeBinary,
	}
	server := NewTestServerWithTestDriver(t, &TestServerDriver{Debug: false, Settings: settings})
	client, raw := dialRawConn(t, server)

	defer func() { panicOnError(client.Close()) }()

	// MODE B is advertised
	rc, msg, err := raw.SendCommand("FEAT")
	require.NoError(t, err)
	require.Equal(t, StatusSystemStatus, rc)
	require.Contains(t, msg, "MODE B")

	// without any MODE command the transfer is a plain stream:
	// raw bytes sent on the data connection are stored as-is
	dcGetter, err := raw.PrepareDataConn()
	require.NoError(t, err)

	rc, msg, err = raw.SendCommand("STOR stream.bin")
	require.NoError(t, err)
	require.Equal(t, StatusFileStatusOK, rc, msg)

	dataConn, err := dcGetter()
	require.NoError(t, err)

	content := []byte("plain stream content")
	_, err = dataConn.Write(content)
	require.NoError(t, err)
	require.NoError(t, dataConn.Close())

	rc, msg, err = raw.ReadResponse()
	require.NoError(t, err)
	require.Equal(t, StatusClosingDataConn, rc, msg)

	var buf bytes.Buffer
	require.NoError(t, client.Retrieve("stream.bin", &buf))
	require.Equal(t, content, buf.Bytes())

	// MODE B then MODE S: stream mode is restored
	rc, _, err = raw.SendCommand("MODE B")
	require.NoError(t, err)
	require.Equal(t, StatusOK, rc)

	rc, _, err = raw.SendCommand("MODE S")
	require.NoError(t, err)
	require.Equal(t, StatusOK, rc)

	dcGetter, err = raw.PrepareDataConn()
	require.NoError(t, err)

	rc, msg, err = raw.SendCommand("STOR stream2.bin")
	require.NoError(t, err)
	require.Equal(t, StatusFileStatusOK, rc, msg)

	dataConn, err = dcGetter()
	require.NoError(t, err)

	_, err = dataConn.Write(content)
	require.NoError(t, err)
	require.NoError(t, dataConn.Close())

	rc, msg, err = raw.ReadResponse()
	require.NoError(t, err)
	require.Equal(t, StatusClosingDataConn, rc, msg)

	buf.Reset()
	require.NoError(t, client.Retrieve("stream2.bin", &buf))
	require.Equal(t, content, buf.Bytes())

	// MODE Z still enables deflate and not block mode
	rc, _, err = raw.SendCommand("MODE Z")
	require.NoError(t, err)
	require.Equal(t, StatusOK, rc)

	dcGetter, err = raw.PrepareDataConn()
	require.NoError(t, err)

	rc, msg, err = raw.SendCommand("STOR deflate.bin")
	require.NoError(t, err)
	require.Equal(t, StatusFileStatusOK, rc, msg)

	dataConn, err = dcGetter()
	require.NoError(t, err)

	transfer, err := newDeflateTransfer(dataConn, 5)
	require.NoError(t, err)

	_, err = transfer.Write(content)
	require.NoError(t, err)
	require.NoError(t, transfer.FinalizeTransfer())
	require.NoError(t, dataConn.Close())

	rc, msg, err = raw.ReadResponse()
	require.NoError(t, err)
	require.Equal(t, StatusClosingDataConn, rc, msg)

	// back to stream mode to verify the stored content
	rc, _, err = raw.SendCommand("MODE S")
	require.NoError(t, err)
	require.Equal(t, StatusOK, rc)

	buf.Reset()
	require.NoError(t, client.Retrieve("deflate.bin", &buf))
	require.Equal(t, content, buf.Bytes())
}
