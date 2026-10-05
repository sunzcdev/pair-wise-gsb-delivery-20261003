package ftpserver

import (
	"errors"
	"fmt"
	"io"
)

// Block mode (MODE B) block descriptor bits and sizes as defined in RFC 959 section 3.4.2.
// Each block is preceded by a 3-byte header: 1 descriptor byte followed by a
// 2-byte big-endian byte count.
const (
	blockDescriptorEOF = 64 // the block is the last one of the file
	blockHeaderSize    = 3
	maxBlockSize       = 65535
)

// errBlockModeUnexpectedEOF is returned when the data connection is closed
// before an end-of-file marker block has been received. Closing the TCP
// connection is NOT a valid end-of-file indication in block mode.
var errBlockModeUnexpectedEOF = errors.New("block mode transfer ended without end-of-file marker")

// blockModeReadWriter implements the RFC 959 block transfer mode (MODE B) on
// top of a data connection.
type blockModeReadWriter struct {
	conn      io.ReadWriter
	header    [blockHeaderSize]byte
	remaining int  // data bytes left to read in the current block
	eof       bool // the end-of-file marker block has been received
	wroteData bool // at least one Write call happened (download direction)
}

func newBlockModeTransfer(conn io.ReadWriter) *blockModeReadWriter {
	return &blockModeReadWriter{conn: conn}
}

// Read returns the payload of the received blocks, transparently handling
// headers split across multiple network reads and zero-length blocks.
// io.EOF is only returned once an end-of-file marker block has been received;
// a connection close before that marker is reported as an error.
func (b *blockModeReadWriter) Read(p []byte) (int, error) {
	for b.remaining == 0 {
		if b.eof {
			return 0, io.EOF
		}

		if err := b.readBlockHeader(); err != nil {
			return 0, err
		}
	}

	if len(p) > b.remaining {
		p = p[:b.remaining]
	}

	n, err := io.ReadFull(b.conn, p)
	b.remaining -= n

	if err != nil {
		// A connection close in the middle of a block is not a valid end of file
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return n, errBlockModeUnexpectedEOF
		}

		return n, err
	}

	return n, nil
}

func (b *blockModeReadWriter) readBlockHeader() error {
	// io.ReadFull handles headers split across multiple TCP segments
	if _, err := io.ReadFull(b.conn, b.header[:]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return errBlockModeUnexpectedEOF
		}

		return err
	}

	descriptor := b.header[0]
	count := int(b.header[1])<<8 | int(b.header[2])

	if descriptor&blockDescriptorEOF != 0 {
		b.eof = true

		return nil
	}

	// a zero-length block simply contains no data, we move on to the next header
	b.remaining = count

	return nil
}

// Write sends the data as a sequence of blocks of at most 65535 bytes.
func (b *blockModeReadWriter) Write(p []byte) (int, error) {
	b.wroteData = true

	total := 0

	for len(p) > 0 {
		size := len(p)
		if size > maxBlockSize {
			size = maxBlockSize
		}

		if err := b.writeBlock(0, p[:size]); err != nil {
			return total, err
		}

		p = p[size:]
		total += size
	}

	return total, nil
}

func (b *blockModeReadWriter) writeBlock(descriptor byte, data []byte) error {
	header := []byte{descriptor, byte(len(data) >> 8), byte(len(data))}
	if _, err := b.conn.Write(header); err != nil {
		return fmt.Errorf("could not write block header: %w", err)
	}

	if len(data) > 0 {
		if _, err := b.conn.Write(data); err != nil {
			return fmt.Errorf("could not write block data: %w", err)
		}
	}

	return nil
}

// FinalizeTransfer writes the end-of-file marker block for downloads.
// For uploads nothing is written: the end of the file is signaled by the client.
func (b *blockModeReadWriter) FinalizeTransfer() error {
	if !b.wroteData {
		return nil
	}

	if err := b.writeBlock(blockDescriptorEOF, nil); err != nil {
		return fmt.Errorf("could not write end-of-file marker block: %w", err)
	}

	return nil
}
