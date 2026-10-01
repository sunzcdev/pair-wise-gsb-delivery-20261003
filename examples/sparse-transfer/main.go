// sparse-transfer demonstrates a local sparse-aware file copy.
//
// It creates a source file whose logical size is much larger than the data it
// really stores (large leading/middle/trailing holes), copies it with
// sftp.CopySparseFile, and prints both the logical length and the number of
// bytes actually transferred. On platforms without sparse support the same
// call transparently falls back to a plain byte-stream copy.
//
// Usage:
//
//	go run ./examples/sparse-transfer [-src path] [-dst path]
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"runtime"

	"github.com/pkg/sftp"
)

func main() {
	srcPath := flag.String("src", "sparse-src.bin", "source file path")
	dstPath := flag.String("dst", "sparse-dst.bin", "destination file path")
	flag.Parse()

	const size = int64(64 << 20) // 64 MiB logical length

	if err := makeSparseSource(*srcPath, size); err != nil {
		log.Fatalf("create source: %v", err)
	}

	srcFile, err := os.OpenFile(*srcPath, os.O_RDWR, 0o644)
	if err != nil {
		log.Fatalf("open source: %v", err)
	}
	defer srcFile.Close()

	dstFile, err := os.OpenFile(*dstPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		log.Fatalf("open destination: %v", err)
	}
	defer dstFile.Close()

	caps := sftp.LocalSparseCapabilities()

	fmt.Printf("platform: %s (discover=%v allocate=%v)\n",
		runtime.GOOS, caps.Discover, caps.Allocate)

	src := sftp.OSSparseFile(srcFile)
	dst := sftp.OSSparseFile(dstFile)

	if err := sftp.MarkSparse(src); err != nil {
		log.Fatalf("mark source sparse: %v", err)
	}

	if err := sftp.MarkSparse(dst); err != nil {
		log.Fatalf("mark destination sparse: %v", err)
	}

	stats, err := sftp.CopySparseFile(
		dst,
		src,
		size,
	)
	if err != nil {
		log.Fatalf("copy: %v", err)
	}

	fmt.Printf("logical size : %d bytes\n", stats.LogicalSize)
	fmt.Printf("transferred  : %d bytes (sparse=%v)\n",
		stats.BytesTransferred, stats.Sparse)

	if stats.Sparse {
		saved := stats.LogicalSize - stats.BytesTransferred
		fmt.Printf("holes skipped: %d bytes (%.1f%%)\n",
			saved, 100*float64(saved)/float64(stats.LogicalSize))
	} else {
		fmt.Println("peer/platform lacks sparse support; plain byte stream used")
	}
}

func makeSparseSource(path string, size int64) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	payloads := []struct {
		offset int64
		data   []byte
	}{
		{offset: 4 << 10, data: make([]byte, 8<<10)}, // allocated zeros
		{offset: 16 << 20, data: []byte("real data in the middle")},
		{offset: size - 4096, data: make([]byte, 4096)},
	}

	// Mark sparse and establish the logical length *before* writing, so the
	// gaps between (and after) the writes become genuine holes.
	sf := sftp.OSSparseFile(f)
	if err := sftp.MarkSparse(sf); err != nil {
		return err
	}

	if err := f.Truncate(size); err != nil {
		return err
	}

	for _, p := range payloads {
		if _, err := f.WriteAt(p.data, p.offset); err != nil {
			return err
		}
	}

	return nil
}
