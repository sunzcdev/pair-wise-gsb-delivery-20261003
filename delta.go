// Copyright 2014-2022 Ulrich Kunitz. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package xz

import (
	"errors"
	"fmt"
	"io"
)

// Delta filter ID as defined by the xz file format.
const deltaFilterID = 0x03

// min and max delta distance values as defined by the xz file format.
// The encoded value stores distance-1 in a single byte.
const (
	minDeltaDistance = 1
	maxDeltaDistance = 256
)

// DeltaFilter configures the Delta filter for an xz block. The Delta
// filter can only be used as a non-last filter of the filter chain;
// the LZMA2 filter has to follow. At most one Delta filter may be used
// in a chain.
//
// Delta applies a byte-wise subtraction/adding along Distance. It is
// most useful for uncompressed PCM data and similar structures.
type DeltaFilter struct {
	// Distance is the byte distance of the Delta filter in the
	// range 1 to 256. A value of zero is treated as 1.
	Distance int
}

// filter marks DeltaFilter as an implementation of the exported
// Filter interface.
func (f *DeltaFilter) filter() {}

// String returns a representation of the Delta filter.
func (f DeltaFilter) String() string {
	return fmt.Sprintf("Delta distance %d", f.Distance)
}

// id returns the ID of the Delta filter.
func (f DeltaFilter) id() uint64 { return deltaFilterID }

// verify checks the Delta filter parameters.
func (f *DeltaFilter) verify() error {
	if f.Distance == 0 {
		f.Distance = minDeltaDistance
	}
	if f.Distance < minDeltaDistance || f.Distance > maxDeltaDistance {
		return errors.New("xz: delta distance out of range")
	}
	return nil
}

// MarshalBinary encodes the Delta filter properties as stored in an xz
// block header: filter ID, properties size 1 and the distance-1.
func (f DeltaFilter) MarshalBinary() (data []byte, err error) {
	d := f.Distance
	if d == 0 {
		d = minDeltaDistance
	}
	if d < minDeltaDistance || d > maxDeltaDistance {
		return nil, errors.New("xz: delta distance out of range")
	}
	return []byte{deltaFilterID, 1, byte(d - 1)}, nil
}

// UnmarshalBinary decodes the Delta filter properties from the
// representation stored in an xz block header.
func (f *DeltaFilter) UnmarshalBinary(data []byte) error {
	if len(data) != 3 {
		return errors.New("xz: data for Delta filter has wrong length")
	}
	if data[0] != deltaFilterID {
		return errors.New("xz: wrong Delta filter id")
	}
	if data[1] != 1 {
		return errors.New("xz: wrong Delta filter size")
	}
	f.Distance = int(data[2]) + 1
	return nil
}

// reader creates a reader that applies the inverse Delta
// transformation.
func (f DeltaFilter) reader(r io.Reader, c *ReaderConfig) (fr io.Reader,
	err error) {

	if err = f.verify(); err != nil {
		return nil, err
	}
	return &deltaReader{
		r:    r,
		dist: f.Distance,
		hist: make([]byte, f.Distance),
	}, nil
}

// writeCloser creates a WriteCloser applying the Delta transformation.
func (f DeltaFilter) writeCloser(w io.WriteCloser, c *WriterConfig,
) (fw io.WriteCloser, err error) {

	if err = f.verify(); err != nil {
		return nil, err
	}
	return &deltaWriteCloser{
		WriteCloser: w,
		dist:        f.Distance,
		hist:        make([]byte, f.Distance),
	}, nil
}

// last returns false because the Delta filter cannot be the last
// filter in the chain.
func (f DeltaFilter) last() bool { return false }

// deltaReader reverses the Delta transformation on the fly. The
// transformation state is kept across Read calls so that the upstream
// filter may provide data in arbitrarily small chunks.
type deltaReader struct {
	r    io.Reader
	dist int
	hist []byte
	pos  int
}

// Read reads Delta-filtered data and returns the unfiltered bytes.
func (r *deltaReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	for i := 0; i < n; i++ {
		// The history entries before the first Distance bytes
		// remain zero, matching a missing reference byte.
		p[i] += r.hist[(r.pos+i)%r.dist]
		r.hist[(r.pos+i)%r.dist] = p[i]
	}
	r.pos += n
	return n, err
}

// deltaWriteCloser applies the Delta transformation. The transformation
// state is kept across Write calls so that the caller may provide data
// in arbitrarily small chunks.
type deltaWriteCloser struct {
	io.WriteCloser
	dist int
	hist []byte
	pos  int
}

// Write writes the Delta filtered bytes of p to the underlying writer.
func (w *deltaWriteCloser) Write(p []byte) (int, error) {
	// The filter modifies the data in place, so copy it first to
	// avoid touching the caller's slice.
	q := make([]byte, len(p))
	copy(q, p)
	for i := 0; i < len(q); i++ {
		q[i] -= w.hist[(w.pos+i)%w.dist]
		w.hist[(w.pos+i)%w.dist] = p[i]
	}
	w.pos += len(q)
	_, err := w.WriteCloser.Write(q)
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

// ensure the interface is satisfied.
var _ filter = (*DeltaFilter)(nil)
