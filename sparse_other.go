//go:build plan9 || js || wasm
// +build plan9 js wasm

package sftp

import "os"

// These platforms do not expose sparse-file primitives; no capability is
// advertised and transfers fall back to a plain byte stream.
func localSparseCapabilities() SparseCaps { return SparseCaps{} }

type osSparseFile struct{ *os.File }

// OSSparseFile returns a wrapper that reports unsupported for sparse
// operations on this platform; CopySparseFile will fall back to a plain copy.
func OSSparseFile(f *os.File) SparseFile { return &osSparseFile{File: f} }

// sparseAdapter returns the handle unchanged on unsupported platforms.
func sparseAdapter(f any) any { return f }

func (f *osSparseFile) QueryAllocatedRanges(offset uint64, count uint32) ([]FileRange, uint64, error) {
	return nil, 0, errSparseUnsupported
}

func (f *osSparseFile) AllocateRegions([]FileRange) error { return errSparseUnsupported }
