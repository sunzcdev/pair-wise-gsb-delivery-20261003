package sftp

import (
	"context"
	"fmt"
	"testing"
)

// BenchmarkGlobStreamWide measures the lazy walk over one very wide
// directory. Only one READDIR batch is retained at a time, so allocations do
// not grow with the directory width; use -benchmem to observe it.
func BenchmarkGlobStreamWide(b *testing.B) {
	const width = 5000
	entries := make([]*fakeEntry, 0, width)
	for i := 0; i < width; i++ {
		entries = append(entries, fakeFile(fmt.Sprintf("error-%05d.gz", i), 1))
	}
	root := fakeDir("root", fakeDir("logs", entries...))

	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		fs := newFakeGlobFS(root)
		fs.batchSize = 128
		seq, err := globSequence(fs, ctx, "logs/error-*.gz")
		if err != nil {
			b.Fatal(err)
		}
		count := 0
		for _, merr := range seq {
			if merr != nil {
				b.Fatal(merr)
			}
			count++
		}
		if count != width {
			b.Fatalf("got %d matches, want %d", count, width)
		}
	}
}

// BenchmarkGlobStreamDeep measures a narrow, very deep chain: memory stays
// proportional to depth (ancestor state), never to the whole tree.
func BenchmarkGlobStreamDeep(b *testing.B) {
	const depth = 2000
	logs := fakeDir("logs")
	level := logs
	for i := 0; i < depth; i++ {
		d := fakeDir(fmt.Sprintf("d%d", i), fakeFile("error.gz", 1))
		level.entries[d.name] = d
		level = d
	}
	root := fakeDir("root", logs)

	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		fs := newFakeGlobFS(root)
		fs.batchSize = 128
		seq, err := globSequence(fs, ctx, "logs/**/error.gz")
		if err != nil {
			b.Fatal(err)
		}
		count := 0
		for _, merr := range seq {
			if merr != nil {
				b.Fatal(merr)
			}
			count++
		}
		if count != depth {
			b.Fatalf("got %d matches, want %d", count, depth)
		}
	}
}
