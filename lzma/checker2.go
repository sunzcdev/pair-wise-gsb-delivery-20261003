// Copyright 2014-2022 Ulrich Kunitz. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package lzma

import (
	"errors"
	"fmt"
	"io"
)

// ChunkInfo describes the structure of a single chunk of an LZMA2
// stream. The information is derived from the chunk header alone; the
// payload of the chunk has been skipped and has neither been
// decompressed nor otherwise verified. So a ChunkInfo never asserts
// that the content of the chunk payload is correct.
type ChunkInfo struct {
	// Offset of the control byte of the chunk in the stream.
	Offset int64
	// Control is the raw control byte starting the chunk header.
	Control byte
	// Type is the type of the chunk.
	Type chunkType
	// Uncompressed is the size of the uncompressed data of the
	// chunk in bytes. It is zero for the end-of-stream chunk.
	Uncompressed int64
	// Compressed is the size of the compressed data of the chunk in
	// bytes. It is zero for uncompressed chunks and the
	// end-of-stream chunk.
	Compressed int64
	// Props contains the properties carried by chunks that reset
	// the properties (LRN and LRND chunks).
	Props Properties
	// DictReset indicates that the chunk resets the dictionary.
	DictReset bool
	// PropsReset indicates that the chunk resets the properties.
	PropsReset bool
	// StateReset indicates that the chunk resets the encoder state.
	StateReset bool
	// DictInit indicates that the dictionary is initialized after
	// the chunk.
	DictInit bool
	// PropsInit indicates that the properties are initialized after
	// the chunk.
	PropsInit bool
	// StateInit indicates that the encoder state is initialized
	// after the chunk.
	StateInit bool
	// StateBefore is the chunk state before the chunk.
	StateBefore chunkState
	// StateAfter is the chunk state after the chunk.
	StateAfter chunkState
}

// Payload returns the number of payload bytes following the chunk
// header.
func (info *ChunkInfo) Payload() int64 {
	if info.Type == cEOS {
		return 0
	}
	if uncompressed(info.Type) {
		return info.Uncompressed
	}
	return info.Compressed
}

// HeaderLen returns the length of the chunk header in bytes.
func (info *ChunkInfo) HeaderLen() int {
	return headerLen(info.Type)
}

// Total returns the total number of bytes covered by the chunk, the
// header and the payload.
func (info *ChunkInfo) Total() int64 {
	return int64(info.HeaderLen()) + info.Payload()
}

// String returns a string representation of the chunk information.
func (info *ChunkInfo) String() string {
	if info.Type == cEOS {
		return fmt.Sprintf("chunk@%d EOS %s->%s",
			info.Offset, info.StateBefore, info.StateAfter)
	}
	return fmt.Sprintf("chunk@%d %s control=0x%02x uncompressed=%d"+
		" compressed=%d reset(dict=%t,props=%t,state=%t)"+
		" init(dict=%t,props=%t,state=%t) %s->%s",
		info.Offset, info.Type, info.Control,
		info.Uncompressed, info.Compressed,
		info.DictReset, info.PropsReset, info.StateReset,
		info.DictInit, info.PropsInit, info.StateInit,
		info.StateBefore, info.StateAfter)
}

// errTrailingData indicates data after the end-of-stream chunk.
var errTrailingData = errors.New("lzma: data after end-of-stream chunk")

// Checker2Config stores the parameters for the LZMA2 stream checker.
type Checker2Config struct {
	// AllowTrailingData permits data after the end-of-stream chunk.
	// If it is false, data after the end-of-stream chunk is
	// reported as an error.
	AllowTrailingData bool
}

// NewChecker2 creates a new LZMA2 stream checker using the
// configuration.
func (c Checker2Config) NewChecker2(lzma2 io.Reader) *Checker2 {
	return &Checker2{r: lzma2, cfg: c, cstate: start}
}

// NewChecker2 creates an LZMA2 stream checker with the default
// parameters.
func NewChecker2(lzma2 io.Reader) *Checker2 {
	return Checker2Config{}.NewChecker2(lzma2)
}

// Checker2 checks the structure of an LZMA2 chunk sequence without
// decompressing the chunk payloads. It reads the chunk headers,
// verifies the chunk state transitions, tracks whether dictionary,
// properties and encoder state have been initialized and skips the
// payload data. Its memory usage is determined by a single chunk
// header and a small fixed state; it doesn't grow with the size of
// the stream.
//
// Note that a successful check verifies the chunk structure only. The
// content of the chunk payloads is not decompressed and not verified;
// use Reader2 for that purpose.
type Checker2 struct {
	r   io.Reader
	cfg Checker2Config

	off    int64
	cstate chunkState

	dictInit  bool
	propsInit bool
	stateInit bool

	trailing int64
	checked  bool

	err error

	buf [256]byte
}

// Next advances the checker to the next chunk and returns its
// structural information. The end-of-stream chunk is reported like
// any other chunk. After it has been reported, Next checks the
// trailing data policy and returns io.EOF, if the stream is
// structurally correct.
func (c *Checker2) Next() (*ChunkInfo, error) {
	if c.err != nil {
		return nil, c.err
	}
	if c.cstate == stop {
		if err := c.checkTrailing(); err != nil {
			c.err = err
			return nil, err
		}
		return nil, io.EOF
	}
	info, err := c.readChunk()
	if err != nil {
		c.err = err
		return nil, err
	}
	return info, nil
}

// Trailing returns the number of bytes found after the end-of-stream
// chunk. The value is valid only after Next has returned io.EOF or an
// error caused by trailing data.
func (c *Checker2) Trailing() int64 {
	return c.trailing
}

// readChunk reads the header of the next chunk, verifies the chunk
// state transition and skips the chunk payload.
func (c *Checker2) readChunk() (*ChunkInfo, error) {
	start := c.off
	var p [6]byte
	if _, err := io.ReadFull(c.r, p[:1]); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, fmt.Errorf("lzma: chunk header at offset %d: %w",
			start, err)
	}
	ctype, err := headerChunkType(p[0])
	if err != nil {
		return nil, fmt.Errorf("lzma: chunk header at offset %d: %w",
			start, err)
	}
	n := headerLen(ctype)
	if _, err = io.ReadFull(c.r, p[1:n]); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, fmt.Errorf("lzma: chunk header at offset %d: %w",
			start, err)
	}
	h := new(chunkHeader)
	if err = h.UnmarshalBinary(p[:n]); err != nil {
		return nil, fmt.Errorf("lzma: chunk header at offset %d: %w",
			start, err)
	}
	if (h.ctype == cLRN || h.ctype == cLRND) &&
		h.props.LC+h.props.LP > 4 {
		return nil, fmt.Errorf(
			"lzma: chunk header at offset %d: lc+lp exceeds 4",
			start)
	}
	c.off += int64(n)

	before := c.cstate
	if err = c.cstate.next(h.ctype); err != nil {
		return nil, fmt.Errorf("lzma: %s chunk at offset %d: %w",
			h.ctype, start, err)
	}

	info := &ChunkInfo{
		Offset:      start,
		Control:     p[0],
		Type:        h.ctype,
		StateBefore: before,
		StateAfter:  c.cstate,
	}
	if h.ctype != cEOS {
		info.Uncompressed = int64(h.uncompressed) + 1
		if !uncompressed(h.ctype) {
			info.Compressed = int64(h.compressed) + 1
		}
	}
	switch h.ctype {
	case cUD:
		info.DictReset = true
		c.dictInit = true
	case cLR:
		info.StateReset = true
		c.stateInit = true
	case cLRN:
		info.PropsReset = true
		info.StateReset = true
		info.Props = h.props
		c.propsInit = true
		c.stateInit = true
	case cLRND:
		info.DictReset = true
		info.PropsReset = true
		info.StateReset = true
		info.Props = h.props
		c.dictInit = true
		c.propsInit = true
		c.stateInit = true
	}
	info.DictInit = c.dictInit
	info.PropsInit = c.propsInit
	info.StateInit = c.stateInit

	if err = c.skip(info.Payload()); err != nil {
		return nil, fmt.Errorf(
			"lzma: payload of chunk at offset %d: %w", start, err)
	}
	return info, nil
}

// skip reads and discards n bytes from the stream.
func (c *Checker2) skip(n int64) error {
	for n > 0 {
		m := int64(len(c.buf))
		if m > n {
			m = n
		}
		k, err := c.r.Read(c.buf[:m])
		c.off += int64(k)
		n -= int64(k)
		if n == 0 {
			return nil
		}
		if err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return err
		}
		if k == 0 {
			return errors.New("lzma: reader makes no progress")
		}
	}
	return nil
}

// checkTrailing drains the stream after the end-of-stream chunk and
// applies the trailing data policy.
func (c *Checker2) checkTrailing() error {
	if c.checked {
		return nil
	}
	c.checked = true
	for {
		k, err := c.r.Read(c.buf[:])
		c.trailing += int64(k)
		c.off += int64(k)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if k == 0 {
			return errors.New("lzma: reader makes no progress")
		}
	}
	if c.trailing > 0 && !c.cfg.AllowTrailingData {
		return fmt.Errorf("lzma: offset %d: %d bytes: %w",
			c.off-c.trailing, c.trailing, errTrailingData)
	}
	return nil
}
