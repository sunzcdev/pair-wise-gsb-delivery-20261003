// Copyright The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package prober

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/prometheus/blackbox_exporter/config"
)

// defaultMaxFrameSize caps the memory a single frame may claim when the
// module does not configure max_frame_size.
const defaultMaxFrameSize = 1 << 20 // 1 MiB

func frameByteOrder(order string) binary.ByteOrder {
	if order == "little" {
		return binary.LittleEndian
	}
	return binary.BigEndian
}

func frameHeaderSize(f *config.Frame) int {
	if f.HeaderSize > 0 {
		return f.HeaderSize
	}
	return f.LengthField.Offset + f.LengthField.Size
}

func frameMaxFrameSize(f *config.Frame) int {
	if f.MaxFrameSize > 0 {
		return f.MaxFrameSize
	}
	return defaultMaxFrameSize
}

func readFrameUint(b []byte, order binary.ByteOrder) uint64 {
	switch len(b) {
	case 1:
		return uint64(b[0])
	case 2:
		return uint64(order.Uint16(b))
	case 4:
		return uint64(order.Uint32(b))
	default:
		return order.Uint64(b)
	}
}

// readFrame reads exactly one length-prefixed frame from r. It never relies
// on a single Read returning a whole frame: the header and the body are
// filled with io.ReadFull, so a frame split across packets (or several
// frames coalesced into one packet) are both handled. The read is bounded
// by the connection deadline and by max_frame_size.
func readFrame(r *bufio.Reader, f *config.Frame) ([]byte, error) {
	headerSize := frameHeaderSize(f)
	header := make([]byte, headerSize)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, fmt.Errorf("error reading frame header: %w", err)
	}

	lengthField := f.LengthField
	length := readFrameUint(header[lengthField.Offset:lengthField.Offset+lengthField.Size], frameByteOrder(lengthField.ByteOrder))

	total := length
	if !lengthField.IncludesHeader {
		total += uint64(headerSize)
	}
	if total < uint64(headerSize) {
		return nil, fmt.Errorf("declared frame length %d is smaller than the header size %d", length, headerSize)
	}
	if maxFrameSize := uint64(frameMaxFrameSize(f)); total > maxFrameSize {
		return nil, fmt.Errorf("declared frame size %d exceeds max_frame_size %d", total, maxFrameSize)
	}

	frame := make([]byte, total)
	copy(frame, header)
	if _, err := io.ReadFull(r, frame[headerSize:]); err != nil {
		return nil, fmt.Errorf("error reading frame body: %w", err)
	}
	return frame, nil
}

// checkFrameFields verifies the configured field assertions against a
// complete frame.
func checkFrameFields(frame []byte, fields []config.FrameFieldExpect) error {
	for i, field := range fields {
		if field.Value == nil {
			want := []byte(field.Bytes)
			if field.Offset+len(want) > len(frame) {
				return fmt.Errorf("field %d: %d bytes at offset %d exceed frame size %d", i, len(want), field.Offset, len(frame))
			}
			if got := frame[field.Offset : field.Offset+len(want)]; !bytes.Equal(got, want) {
				return fmt.Errorf("field %d: bytes at offset %d are %x, expected %x", i, field.Offset, got, want)
			}
			continue
		}
		if field.Offset+field.Size > len(frame) {
			return fmt.Errorf("field %d: %d bytes at offset %d exceed frame size %d", i, field.Size, field.Offset, len(frame))
		}
		got := readFrameUint(frame[field.Offset:field.Offset+field.Size], frameByteOrder(field.ByteOrder))
		if got != *field.Value {
			return fmt.Errorf("field %d: value at offset %d is %d, expected %d", i, field.Offset, got, *field.Value)
		}
	}
	return nil
}
