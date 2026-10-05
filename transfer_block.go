package ftpserver

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	blockHeaderSize = 3
	blockMaxDataLen = 1<<16 - 1

	blockDescriptorSuspectData    byte = 0x10
	blockDescriptorRestartMarkers byte = 0x20
	blockDescriptorEndOfRecord    byte = 0x40
	blockDescriptorEndOfFile      byte = 0x80
	blockDescriptorKnownFlags          = blockDescriptorSuspectData |
		blockDescriptorRestartMarkers |
		blockDescriptorEndOfRecord |
		blockDescriptorEndOfFile
)

var errBlockTransferIncomplete = errors.New("block transfer closed before end-of-file marker")
var errBlockTransferInvalid = errors.New("invalid block transfer descriptor")

type blockTransfer struct {
	conn          io.ReadWriter
	sendEOF       bool
	eof           bool
	done          bool
	dataRemaining int
	blockEOF      bool
}

func newBlockTransfer(conn io.ReadWriter, sendEOF bool) *blockTransfer {
	return &blockTransfer{conn: conn, sendEOF: sendEOF}
}

func blockTransferSendsEOF(command string) bool {
	return command != "STOR" && command != "APPE"
}

func (b *blockTransfer) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if b.eof {
		return 0, io.EOF
	}

	for {
		if b.dataRemaining > 0 {
			return b.readData(p)
		}

		var header [blockHeaderSize]byte
		if _, err := io.ReadFull(b.conn, header[:]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return 0, errBlockTransferIncomplete
			}

			return 0, err
		}

		descriptor := header[0]
		if !validBlockDescriptor(descriptor) {
			return 0, fmt.Errorf("%w: %#x", errBlockTransferInvalid, descriptor)
		}

		dataLen := int(binary.BigEndian.Uint16(header[1:]))
		if descriptor&blockDescriptorRestartMarkers != 0 {
			if err := b.discard(dataLen); err != nil {
				return 0, err
			}

			continue
		}

		if dataLen == 0 {
			if descriptor&blockDescriptorEndOfFile != 0 {
				b.eof = true

				return 0, io.EOF
			}

			continue
		}

		b.dataRemaining = dataLen
		b.blockEOF = descriptor&blockDescriptorEndOfFile != 0
	}
}

func validBlockDescriptor(descriptor byte) bool {
	if descriptor&^blockDescriptorKnownFlags != 0 {
		return false
	}

	return descriptor&blockDescriptorRestartMarkers == 0 || descriptor == blockDescriptorRestartMarkers
}

func (b *blockTransfer) readData(p []byte) (int, error) {
	readLen := min(len(p), b.dataRemaining)
	if _, err := io.ReadFull(b.conn, p[:readLen]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return 0, errBlockTransferIncomplete
		}

		return 0, err
	}

	b.dataRemaining -= readLen
	if b.dataRemaining == 0 && b.blockEOF {
		b.eof = true
	}
	if b.eof {
		return readLen, nil
	}

	return readLen, nil
}

func (b *blockTransfer) discard(dataLen int) error {
	if _, err := io.CopyN(io.Discard, b.conn, int64(dataLen)); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return errBlockTransferIncomplete
		}

		return err
	}

	return nil
}

func (b *blockTransfer) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		chunkLen := min(len(p), blockMaxDataLen)
		if err := b.writeBlock(0, p[:chunkLen]); err != nil {
			return written, err
		}

		p = p[chunkLen:]
		written += chunkLen
	}

	return written, nil
}

func (b *blockTransfer) FinalizeTransfer() error {
	return b.FinalizeTransferWithError(nil)
}

func (b *blockTransfer) FinalizeTransferWithError(transferErr error) error {
	if b.done || !b.sendEOF || b.eof || transferErr != nil {
		return nil
	}

	b.done = true

	return b.writeBlock(blockDescriptorEndOfFile, nil)
}

func (b *blockTransfer) writeBlock(descriptor byte, data []byte) error {
	header := [blockHeaderSize]byte{descriptor}
	binary.BigEndian.PutUint16(header[1:], uint16(len(data)))

	if _, err := b.conn.Write(header[:]); err != nil {
		return err
	}

	if len(data) > 0 {
		if _, err := b.conn.Write(data); err != nil {
			return err
		}
	}

	return nil
}
