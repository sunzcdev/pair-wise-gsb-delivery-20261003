// Command sparse-transfer demonstrates the negotiable sparse-file transfer
// extension.
//
// It creates a large, mostly-empty file on the local filesystem, copies it
// through an in-process SFTP connection backed by a real local directory, and
// prints the logical size versus the number of payload bytes actually sent and
// received. Sparse support is advertised separately for "find data ranges"
// and "create holes"; when either side lacks a capability the copy falls back
// transparently to a dense transfer.
package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"

	"github.com/pkg/sftp"
	"github.com/pkg/sftp/internal/sparse"
)

const (
	logicalSize = int64(8 << 20) // 8 MiB logical length
	chunk       = 64 << 10      // 64 KiB of data per populated region
)

func main() {
	root, err := os.MkdirTemp("", "sftp-sparse-*")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(root)

	caps := sftp.SparseSupport()
	fmt.Printf("local platform sparse support: find-ranges=%v create-holes=%v\n",
		caps&sparse.CapExtents != 0, caps&sparse.CapPunch != 0)

	client, shutdown := startSFTPServer(root)
	defer shutdown()

	negotiated := client.SparseCapabilities()
	fmt.Printf("negotiated with server:    find-ranges=%v create-holes=%v\n\n",
		negotiated&sparse.CapExtents != 0, negotiated&sparse.CapPunch != 0)

	// Build a sparse source: three populated regions, everything else a hole.
	src := filepath.Join(root, "src.bin")
	if err := makeSparseSource(src); err != nil {
		log.Fatal(err)
	}

	allocated, err := allocatedBytes(src)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("source file: logical=%d bytes  allocated=%d bytes\n\n", logicalSize, allocated)

	// Upload to the server-side directory, preserving holes.
	up, err := client.SparseUpload(src, "uploaded.bin")
	if err != nil {
		log.Fatalf("upload: %v", err)
	}
	printStats("upload", up)

	// Download it back to a new local file.
	dst := filepath.Join(root, "got.bin")
	dn, err := client.SparseDownload("uploaded.bin", dst)
	if err != nil {
		log.Fatalf("download: %v", err)
	}
	printStats("download", dn)

	if err := compareFiles(src, dst); err != nil {
		log.Fatalf("round trip mismatch: %v", err)
	}

	dstAllocated, err := allocatedBytes(dst)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("\nround trip OK; destination logical=%d allocated=%d\n", logicalSize, dstAllocated)
}

func makeSparseSource(path string) error {
	f, sf, err := sftp.CreateSparseFile(path)
	if err != nil {
		return err
	}
	defer f.Close()

	// Reserve the logical length first; on every platform this establishes a
	// trailing hole once data is written only at the front.
	if err := f.Truncate(logicalSize); err != nil {
		return err
	}

	regions := []struct {
		off  int64
		fill byte
	}{
		{0, 'A'},
		{2 << 20, 0},            // genuinely allocated zeroes, not a hole
		{logicalSize - chunk, 'Z'},
	}
	buf := make([]byte, chunk)
	for _, r := range regions {
		if r.fill != 0 {
			for i := range buf {
				buf[i] = r.fill
			}
		}
		if _, err := f.WriteAt(buf, r.off); err != nil {
			return err
		}
	}

	// Turn every gap between allocated regions into a real on-disk hole.
	extents, err := sf.DataExtents(0, 0)
	if err != nil {
		return err
	}
	pos := int64(0)
	for _, e := range extents {
		if int64(e.Offset) > pos {
			if err := sf.PunchHole(pos, int64(e.Offset)-pos); err != nil {
				return err
			}
		}
		pos = int64(e.Offset + e.Length)
	}
	if pos < logicalSize {
		if err := sf.PunchHole(pos, logicalSize-pos); err != nil {
			return err
		}
	}
	return nil
}

func allocatedBytes(path string) (int64, error) {
	f, sf, err := sftp.NewSparseFile(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	extents, err := sf.DataExtents(0, 0)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, e := range extents {
		total += int64(e.Length)
	}
	return total, nil
}

func printStats(direction string, s sftp.SparseTransferStats) {
	mode := "dense (capability missing; fell back)"
	if s.Sparse {
		mode = "sparse"
	}
	fmt.Printf("%-9s mode=%s logical=%d bytes transferred=%d extents=%d saved=%.1f%%\n",
		direction, mode, s.LogicalSize, s.DataBytes, s.Extents,
		100*float64(s.LogicalSize-s.DataBytes)/float64(s.LogicalSize))
}

func compareFiles(a, b string) error {
	af, err := os.Open(a)
	if err != nil {
		return err
	}
	defer af.Close()

	bf, err := os.Open(b)
	if err != nil {
		return err
	}
	defer bf.Close()

	chunkSize := 128 << 10
	ab := make([]byte, chunkSize)
	bb := make([]byte, chunkSize)
	for off := int64(0); off < logicalSize; off += int64(chunkSize) {
		an, _ := io.ReadFull(af, ab)
		bn, _ := io.ReadFull(bf, bb)
		if an != bn || !bytes.Equal(ab[:an], bb[:bn]) {
			return fmt.Errorf("content differs at offset %d", off)
		}
	}
	return nil
}

// startSFTPServer runs an SFTP server over one end of a net.Pipe and returns
// a client connected to the other end. No SSH is involved.
func startSFTPServer(root string) (*sftp.Client, func()) {
	cConn, sConn := net.Pipe()

	server, err := sftp.NewServer(sConn, sftp.WithServerWorkingDirectory(root))
	if err != nil {
		log.Fatal(err)
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve() }()

	client, err := sftp.NewClientPipe(cConn, cConn)
	if err != nil {
		log.Fatal(err)
	}

	return client, func() {
		client.Close()
		server.Close()
		<-serveErr
	}
}
