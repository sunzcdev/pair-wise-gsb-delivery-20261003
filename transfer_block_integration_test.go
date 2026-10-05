package ftpserver

import (
	"encoding/binary"
	"io"
	"net"
	"os"
	"path"
	"testing"

	"github.com/secsy/goftp"
	"github.com/stretchr/testify/require"
)

type blockFrame struct {
	descriptor byte
	data       []byte
	writeSplit int
}

func blockFrames(data []byte) []blockFrame {
	return []blockFrame{
		{data: data[:2], writeSplit: 1},
		{data: data[2:3]},
		{descriptor: blockDescriptorEndOfRecord},
		{descriptor: blockDescriptorEndOfFile},
	}
}

func encodeBlockFrame(frame blockFrame) []byte {
	header := make([]byte, blockHeaderSize)
	header[0] = frame.descriptor
	binary.BigEndian.PutUint16(header[1:], uint16(len(frame.data)))

	return append(header, frame.data...)
}

func writeBlockFrames(t *testing.T, conn net.Conn, frames []blockFrame) {
	t.Helper()

	for _, frame := range frames {
		payload := encodeBlockFrame(frame)
		split := frame.writeSplit
		if split <= 0 || split >= len(payload) {
			_, err := conn.Write(payload)
			require.NoError(t, err)

			continue
		}

		_, err := conn.Write(payload[:split])
		require.NoError(t, err)

		_, err = conn.Write(payload[split:])
		require.NoError(t, err)
	}
}

func openBlockRawClient(t *testing.T, server *FtpServer) (*goftp.Client, goftp.RawConn) {
	t.Helper()

	client, err := goftp.DialConfig(goftp.Config{User: authUser, Password: authPass}, server.Addr())
	require.NoError(t, err)

	t.Cleanup(func() { _ = client.Close() })

	raw, err := client.OpenRawConn()
	require.NoError(t, err)

	returnCode, response, err := raw.SendCommand("TYPE I")
	require.NoError(t, err)
	require.Equal(t, StatusOK, returnCode, response)

	returnCode, response, err = raw.SendCommand("MODE B")
	require.NoError(t, err)
	require.Equal(t, StatusOK, returnCode, response)

	return client, raw
}

func TestBlockModeUpload(t *testing.T) {
	driver := &TestServerDriver{Debug: false}
	server := NewTestServerWithTestDriver(t, driver)
	_, raw := openBlockRawClient(t, server)

	contents := []byte("abc")
	dcGetter, err := raw.PrepareDataConn()
	require.NoError(t, err)

	returnCode, response, err := raw.SendCommand("STOR block-upload.bin")
	require.NoError(t, err)
	require.Equal(t, StatusFileStatusOK, returnCode, response)

	dataConn, err := dcGetter()
	require.NoError(t, err)
	writeBlockFrames(t, dataConn, blockFrames(contents))
	require.NoError(t, dataConn.Close())

	returnCode, response, err = raw.ReadResponse()
	require.NoError(t, err)
	require.Equal(t, StatusClosingDataConn, returnCode, response)

	readContents, err := os.ReadFile(path.Join(driver.serverDir, "block-upload.bin"))
	require.NoError(t, err)
	require.Equal(t, contents, readContents)
}

func TestBlockModeUploadIncomplete(t *testing.T) {
	driver := &TestServerDriver{Debug: false}
	server := NewTestServerWithTestDriver(t, driver)
	client, _ := openBlockRawClient(t, server)

	tests := []struct {
		name   string
		chunks [][]byte
	}{
		{
			name:   "half header",
			chunks: [][]byte{{0x00}},
		},
		{
			name:   "early data close",
			chunks: [][]byte{{0x00, 0x00, 0x03, 'a', 'b', 'c'}},
		},
		{
			name:   "truncated payload",
			chunks: [][]byte{{0x00, 0x00, 0x03, 'a', 'b'}},
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			raw, err := client.OpenRawConn()
			require.NoError(t, err)
			defer func() { require.NoError(t, raw.Close()) }()

			returnCode, response, err := raw.SendCommand("MODE B")
			require.NoError(t, err)
			require.Equal(t, StatusOK, returnCode, response)

			dcGetter, err := raw.PrepareDataConn()
			require.NoError(t, err)

			returnCode, response, err = raw.SendCommand("STOR incomplete.bin")
			require.NoError(t, err)
			require.Equal(t, StatusFileStatusOK, returnCode, response)

			dataConn, err := dcGetter()
			require.NoError(t, err)

			for _, chunk := range testCase.chunks {
				_, err = dataConn.Write(chunk)
				require.NoError(t, err)
			}

			require.NoError(t, dataConn.Close())

			returnCode, response, err = raw.ReadResponse()
			require.NoError(t, err)
			require.Equal(t, StatusTransferAborted, returnCode, response)
		})
	}
}

func TestBlockModeDownload(t *testing.T) {
	driver := &TestServerDriver{Debug: false}
	server := NewTestServerWithTestDriver(t, driver)
	contents := []byte("abc")
	require.NoError(t, os.WriteFile(path.Join(driver.serverDir, "block-download.bin"), contents, 0o600))

	_, raw := openBlockRawClient(t, server)

	dcGetter, err := raw.PrepareDataConn()
	require.NoError(t, err)

	returnCode, response, err := raw.SendCommand("RETR block-download.bin")
	require.NoError(t, err)
	require.Equal(t, StatusFileStatusOK, returnCode, response)

	dataConn, err := dcGetter()
	require.NoError(t, err)

	received, err := io.ReadAll(dataConn)
	require.NoError(t, err)
	require.NoError(t, dataConn.Close())

	frames := []blockFrame{
		{data: contents},
		{descriptor: blockDescriptorEndOfFile},
	}
	expected := make([]byte, 0, len(received))
	for _, frame := range frames {
		expected = append(expected, encodeBlockFrame(frame)...)
	}
	require.Equal(t, expected, received)

	returnCode, response, err = raw.ReadResponse()
	require.NoError(t, err)
	require.Equal(t, StatusClosingDataConn, returnCode, response)
}

func TestBlockModeDownloadEmptyFile(t *testing.T) {
	driver := &TestServerDriver{Debug: false}
	server := NewTestServerWithTestDriver(t, driver)
	require.NoError(t, os.WriteFile(path.Join(driver.serverDir, "empty.bin"), nil, 0o600))

	_, raw := openBlockRawClient(t, server)

	dcGetter, err := raw.PrepareDataConn()
	require.NoError(t, err)

	returnCode, response, err := raw.SendCommand("RETR empty.bin")
	require.NoError(t, err)
	require.Equal(t, StatusFileStatusOK, returnCode, response)

	dataConn, err := dcGetter()
	require.NoError(t, err)

	received, err := io.ReadAll(dataConn)
	require.NoError(t, err)
	require.NoError(t, dataConn.Close())
	require.Equal(t, []byte{blockDescriptorEndOfFile, 0, 0}, received)

	returnCode, response, err = raw.ReadResponse()
	require.NoError(t, err)
	require.Equal(t, StatusClosingDataConn, returnCode, response)
}

func TestBlockModeSwitchBackToStream(t *testing.T) {
	driver := &TestServerDriver{Debug: false}
	server := NewTestServerWithTestDriver(t, driver)
	_, raw := openBlockRawClient(t, server)

	returnCode, response, err := raw.SendCommand("MODE S")
	require.NoError(t, err)
	require.Equal(t, StatusOK, returnCode, response)

	contents := []byte("raw-stream")
	dcGetter, err := raw.PrepareDataConn()
	require.NoError(t, err)

	returnCode, response, err = raw.SendCommand("STOR stream.bin")
	require.NoError(t, err)
	require.Equal(t, StatusFileStatusOK, returnCode, response)

	dataConn, err := dcGetter()
	require.NoError(t, err)
	_, err = dataConn.Write(contents)
	require.NoError(t, err)
	require.NoError(t, dataConn.Close())

	returnCode, response, err = raw.ReadResponse()
	require.NoError(t, err)
	require.Equal(t, StatusClosingDataConn, returnCode, response)

	readContents, err := os.ReadFile(path.Join(driver.serverDir, "stream.bin"))
	require.NoError(t, err)
	require.Equal(t, contents, readContents)
}
