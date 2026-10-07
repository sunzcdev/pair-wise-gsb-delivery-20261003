package lz4block

import (
	"bytes"
	"math/rand"
	"testing"
)

// splitScatter partitions n logical bytes into separately allocated
// fragments according to sizes. A size of 0 produces an empty fragment.
// Every fragment is filled with fill so a test can detect exactly which
// offsets the decoder touched.
func splitScatter(t testing.TB, sizes []int, fill byte) [][]byte {
	t.Helper()
	parts := make([][]byte, len(sizes))
	for i, n := range sizes {
		parts[i] = make([]byte, n)
		for j := range parts[i] {
			parts[i][j] = fill
		}
	}
	return parts
}

func gatherScatter(parts [][]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// sizesFixed partitions n bytes into chunks of at most chunk bytes.
func sizesFixed(n, chunk int) []int {
	var sizes []int
	for remaining := n; remaining > 0; {
		s := chunk
		if s > remaining {
			s = remaining
		}
		sizes = append(sizes, s)
		remaining -= s
	}
	return sizes
}

// sizesOneByte returns one single-byte fragment per output byte, the worst
// case for a decoder that might treat fragments independently.
func sizesOneByte(n int) []int {
	sizes := make([]int, n)
	for i := range sizes {
		sizes[i] = 1
	}
	return sizes
}

// sizesRandom returns reproducible pseudo-random fragment sizes in
// [0, maxSize] (empty fragments included).
func sizesRandom(n, maxSize int, seed int64) []int {
	rng := rand.New(rand.NewSource(seed))
	var sizes []int
	for remaining := n; remaining > 0; {
		s := rng.Intn(maxSize + 1)
		if s > remaining {
			s = remaining
		}
		sizes = append(sizes, s)
		remaining -= s
	}
	return sizes
}

// sizesWithEmpties places an empty fragment before and after every byte.
func sizesWithEmpties(n int) []int {
	sizes := make([]int, 0, 2*n+2)
	sizes = append(sizes, 0)
	for i := 0; i < n; i++ {
		sizes = append(sizes, 1, 0)
	}
	return sizes
}

func mustCompressBlock(t testing.TB, src []byte) []byte {
	t.Helper()
	dst := make([]byte, CompressBlockBound(len(src)))
	n, err := CompressBlock(src, dst)
	if err != nil {
		t.Fatalf("CompressBlock: %v", err)
	}
	return dst[:n]
}

func rleBinary(n int, seed int64) []byte {
	rng := rand.New(rand.NewSource(seed))
	out := make([]byte, n)
	for i := 0; i < n; {
		run := 1 + rng.Intn(40)
		if i+run > n {
			run = n - i
		}
		v := byte(rng.Intn(4))
		for j := 0; j < run; j++ {
			out[i+j] = v
		}
		i += run
	}
	return out
}

func pseudoRandom(n int, seed int64) []byte {
	rng := rand.New(rand.NewSource(seed))
	out := make([]byte, n)
	rng.Read(out)
	return out
}

// assembleMatchBlock builds an LZ4 block of one match sequence followed by a
// one-byte last literal. The first sequence emits lit as literals and a match
// of mLen bytes at back-distance offset; expected match data must reference
// earlier literals (offset <= len(lit)), so the output is well defined.
func assembleMatchBlock(lit []byte, offset, mLen int) []byte {
	var buf bytes.Buffer
	writeToken(&buf, len(lit), mLen-minMatch)
	buf.Write(lit)
	off := []byte{byte(offset), byte(offset >> 8)}
	buf.Write(off)
	if raw := mLen - minMatch; raw >= 15 {
		writeExtended(&buf, raw-15)
	}
	// Last literal sequence: one byte.
	writeToken(&buf, 1, 0)
	buf.WriteByte('!')
	return buf.Bytes()
}

// matchFromLit returns the mLen bytes produced by a match of the given offset
// that starts right after lit. An overlapping match (offset < mLen) reads
// bytes produced earlier in the same match, so it is built against the
// running output exactly as the decoder does.
func matchFromLit(lit []byte, offset, mLen int) []byte {
	out := append([]byte(nil), lit...)
	for i := 0; i < mLen; i++ {
		out = append(out, out[len(out)-offset])
	}
	return out[len(lit):]
}

// TestScatterDifferential decodes the same compressed block into one
// contiguous buffer and into many different fragmentations, and requires the
// assembled scatter output to match byte for byte.
func TestScatterDifferential(t *testing.T) {
	dataSets := []struct {
		name string
		data []byte
	}{
		{"empty", nil},
		{"one", []byte("a")},
		{"literal", []byte("hello lz4 scatter world")},
		{"text", bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog. "), 60)},
		{"rle", bytes.Repeat([]byte{'Z'}, 2000)},
		{"runs", rleBinary(4000, 1)},
		{"runs2", rleBinary(4000, 2)},
		{"random", pseudoRandom(4000, 7)},
	}

	splits := []struct {
		name  string
		sizes func(n int) []int
	}{
		{"single", func(n int) []int {
			if n == 0 {
				return []int{0}
			}
			return []int{n}
		}},
		{"one_byte", sizesOneByte},
		{"two_bytes", func(n int) []int { return sizesFixed(n, 2) }},
		{"three_bytes", func(n int) []int { return sizesFixed(n, 3) }},
		{"sevens", func(n int) []int { return sizesFixed(n, 7) }},
		{"empties", sizesWithEmpties},
		{"random_a", func(n int) []int { return sizesRandom(n, 5, 11) }},
		{"random_b", func(n int) []int { return sizesRandom(n, 30, 23) }},
		{"random_empty", func(n int) []int { return sizesRandom(n, 12, 47) }},
	}

	for _, ds := range dataSets {
		compressed := mustCompressBlock(t, ds.data)
		ref := make([]byte, len(ds.data))
		if n := decodeBlock(ref, compressed, nil); n != len(ds.data) {
			t.Fatalf("%s: reference decode returned %d, want %d", ds.name, n, len(ds.data))
		}
		if !bytes.Equal(ref, ds.data) {
			t.Fatalf("%s: reference mismatch", ds.name)
		}

		for _, split := range splits {
			t.Run(ds.name+"/"+split.name, func(t *testing.T) {
				sizes := split.sizes(len(ds.data))
				parts := splitScatter(t, sizes, 0xCD)
				n, err := UncompressBlockScatter(compressed, parts, nil)
				if err != nil {
					t.Fatalf("decode: %v", err)
				}
				if n != len(ds.data) {
					t.Fatalf("n = %d, want %d", n, len(ds.data))
				}
				if got := gatherScatter(parts); !bytes.Equal(got, ds.data) {
					for i := range ds.data {
						if got[i] != ds.data[i] {
							t.Fatalf("first mismatch at %d: got %d want %d (fragments=%d)",
								i, got[i], ds.data[i], len(sizes))
						}
					}
				}
			})
		}
	}
}

// TestScatterMatchAcrossBoundaries assembles blocks by hand so that a match
// copy straddles at least three fragment boundaries, including empty
// fragments. Repeated copies (offset < mLen) have a read cursor that follows
// the write cursor across the same boundaries.
func TestScatterMatchAcrossBoundaries(t *testing.T) {
	cases := []struct {
		name         string
		lit          []byte
		offset, mlen int
	}{
		{"rle_offset1", []byte("A"), 1, 20},
		{"tile_offset3", []byte("abc"), 3, 21},
		{"copy_prefix", []byte("ABCDEFGHIJ"), 10, 12},
		{"rle_offset4", []byte("wxyz"), 4, 25},
	}

	splitBuilders := []struct {
		name  string
		sizes func(total int) []int
	}{
		{"one_byte", sizesOneByte},
		{"three", func(n int) []int { return sizesFixed(n, 3) }},
		{"empty_interleaved", func(n int) []int {
			sizes := []int{0}
			for i := 0; i < n; i++ {
				sizes = append(sizes, 1, 0)
				if i%4 == 3 {
					sizes = append(sizes, 0)
				}
			}
			return append(sizes, 0)
		}},
	}

	for _, c := range cases {
		compressed := assembleMatchBlock(c.lit, c.offset, c.mlen)
		want := append(append([]byte(nil), c.lit...), matchFromLit(c.lit, c.offset, c.mlen)...)
		want = append(want, '!')

		// Sanity: contiguous decoder agrees.
		ref := make([]byte, len(want))
		if n := decodeBlock(ref, compressed, nil); n != len(want) || !bytes.Equal(ref, want) {
			t.Fatalf("%s: reference decode mismatch (n=%d want=%d)", c.name, n, len(want))
		}

		for _, sb := range splitBuilders {
			t.Run(c.name+"/"+sb.name, func(t *testing.T) {
				parts := splitScatter(t, sb.sizes(len(want)), 0xEE)
				n, err := UncompressBlockScatter(compressed, parts, nil)
				if err != nil {
					t.Fatalf("decode: %v", err)
				}
				if n != len(want) {
					t.Fatalf("n = %d want %d", n, len(want))
				}
				if got := gatherScatter(parts); !bytes.Equal(got, want) {
					t.Fatalf("got %v want %v", got, want)
				}
			})
		}
	}
}

// TestScatterDict runs the hand-assembled dictionary cases from
// TestDecodeWithDict through fragmentations, including matches that start in
// the dictionary and cross the dict/output and fragment boundaries.
func TestScatterDict(t *testing.T) {
	cases := []struct {
		src, dict, want string
	}{
		// Entire match in dictionary.
		{"\x11b\x0a\x00\x401234", "barbazquux", "barbaz1234"},
		// First part in dictionary, rest in dst.
		{"\x35foo\x09\x00\x401234", "0barbaz", "foobarbazfoo1234"},
		// Copy end of dictionary three times, then a literal.
		{"\x08\x04\x00\x50abcde", "---1234", "123412341234abcde"},
		// First part in dictionary, rest in dst, copied multiple times.
		{"\x1a1\x05\x00\x50abcde", "---2345", "123451234512345abcde"},
	}

	splits := []struct {
		name  string
		sizes func(n int) []int
	}{
		{"one_byte", sizesOneByte},
		{"three", func(n int) []int { return sizesFixed(n, 3) }},
		{"empties", sizesWithEmpties},
		{"random", func(n int) []int { return sizesRandom(n, 7, 99) }},
	}

	for _, c := range cases {
		for _, split := range splits {
			t.Run(split.name, func(t *testing.T) {
				sizes := split.sizes(len(c.want))
				parts := splitScatter(t, sizes, 0)
				n, err := UncompressBlockScatter([]byte(c.src), parts, []byte(c.dict))
				if err != nil {
					t.Fatalf("decode: %v", err)
				}
				if n != len(c.want) {
					t.Fatalf("n = %d want %d", n, len(c.want))
				}
				if got := gatherScatter(parts); string(got) != c.want {
					t.Fatalf("got %q want %q", got, c.want)
				}
			})
		}
	}
}

// TestScatterDictInvalidOffset checks a match whose offset reaches before the
// dictionary: the decode fails and the reported prefix is intact.
func TestScatterDictInvalidOffset(t *testing.T) {
	// Same shape as "entire match in dictionary" but with offset 0xffff.
	src := []byte("\x35foo\xff\xff\x401234")
	dict := []byte("XYZ")
	parts := splitScatter(t, sizesOneByte(16), 0)
	n, err := UncompressBlockScatter(src, parts, dict)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	// The three literal bytes "foo" are written before the bad match.
	if n != 3 {
		t.Fatalf("n = %d, want 3", n)
	}
	if got := gatherScatter(parts[:3]); string(got) != "foo" {
		t.Fatalf("prefix = %q want %q", got, "foo")
	}
}

// TestScatterErrorContract verifies that on failure the returned count is the
// number of bytes produced, the logical prefix [0, count) holds those bytes,
// and bytes at offsets >= count are untouched.
func TestScatterErrorContract(t *testing.T) {
	const fill = 0x7A

	// Valid block for reference, truncated literal in the second sequence.
	data := bytes.Repeat([]byte("ABCDefgh"), 20)
	compressed := mustCompressBlock(t, data)

	t.Run("truncated_destination", func(t *testing.T) {
		for _, room := range []int{0, 1, len(data) - 1} {
			parts := splitScatter(t, sizesOneByte(room), fill)
			n, err := UncompressBlockScatter(compressed, parts, nil)
			if err == nil {
				t.Fatalf("room=%d: expected error", room)
			}
			if n != room {
				t.Fatalf("room=%d: n = %d, want %d", room, n, room)
			}
			got := gatherScatter(parts)
			if !bytes.Equal(got, data[:room]) {
				t.Fatalf("room=%d: prefix mismatch", room)
			}
		}
	})

	t.Run("partial_then_bad_match", func(t *testing.T) {
		// Literals "hello", then a match with offset 0 (invalid).
		src := []byte("\x50hello\x00\x00")
		parts := splitScatter(t, sizesOneByte(16), fill)
		n, err := UncompressBlockScatter(src, parts, nil)
		if err == nil {
			t.Fatal("expected error")
		}
		if n != 5 {
			t.Fatalf("n = %d want 5", n)
		}
		got := gatherScatter(parts)
		if string(got[:5]) != "hello" {
			t.Fatalf("prefix = %q", got[:5])
		}
		for i := 5; i < len(got); i++ {
			if got[i] != fill {
				t.Fatalf("byte %d modified after error: %#x", i, got[i])
			}
		}
	})

	t.Run("capacity_spill", func(t *testing.T) {
		// Each fragment has extra capacity past len; the decoder must not
		// write into it.
		parts := [][]byte{make([]byte, 4, 8), make([]byte, 4, 16)}
		for i := range parts {
			full := parts[i][:cap(parts[i])]
			for j := len(parts[i]); j < cap(parts[i]); j++ {
				full[j] = fill
			}
		}
		n, err := UncompressBlockScatter(compressed, parts, nil)
		if err == nil {
			t.Fatal("expected error for 8-byte destination")
		}
		if n != 8 {
			t.Fatalf("n = %d want 8", n)
		}
		for i, p := range parts {
			full := p[:cap(p)]
			for j := len(p); j < cap(p); j++ {
				if full[j] != fill {
					t.Fatalf("fragment %d capacity byte %d modified", i, j)
				}
			}
		}
	})
}

// TestScatterEmpty covers empty input and empty fragment sets.
func TestScatterEmpty(t *testing.T) {
	if n, err := UncompressBlockScatter([]byte{0x00}, nil, nil); err != nil || n != 0 {
		t.Fatalf("empty literal-only block: n=%d err=%v", n, err)
	}
	if n, err := UncompressBlockScatter(nil, [][]byte{{}}, nil); err != nil || n != 0 {
		t.Fatalf("nil src: n=%d err=%v", n, err)
	}
	// A valid block decoding to one byte must fail on zero total capacity.
	if _, err := UncompressBlockScatter([]byte("\x10x"), [][]byte{{}, {}, nil}, nil); err == nil {
		t.Fatal("expected error into empty destination")
	}
}

// TestScatterNoStagingAllocation proves the decoder does not allocate a
// contiguous output-sized staging buffer: a multi-KiB block decoded into a
// few fragments performs a small, constant number of allocations
// (independent of output size), namely the writer and the fragment table.
func TestScatterNoStagingAllocation(t *testing.T) {
	data := append(bytes.Repeat([]byte("ScAtTeR-"), 2000), pseudoRandom(1000, 3)...)
	compressed := mustCompressBlock(t, data)
	parts := func() [][]byte {
		return [][]byte{
			make([]byte, 4000),
			{},
			make([]byte, len(data)-4000),
		}
	}

	allocs := testing.AllocsPerRun(100, func() {
		p := parts()
		n, err := UncompressBlockScatter(compressed, p, nil)
		if err != nil || n != len(data) {
			t.Fatalf("n=%d err=%v", n, err)
		}
		if !bytes.Equal(p[0], data[:4000]) || !bytes.Equal(p[2], data[4000:]) {
			t.Fatal("output mismatch")
		}
	})
	if allocs > 3 {
		t.Fatalf("allocs per decode = %.1f, want <= 3 (no output-sized staging buffer)", allocs)
	}
}

// FuzzScatterRoundTrip compresses arbitrary data and decodes it into
// fragments of a fuzz-derived size pattern (including empty fragments), then
// checks the assembled output against the input.
func FuzzScatterRoundTrip(f *testing.F) {
	f.Add([]byte("hello"), byte(1))
	f.Add(bytes.Repeat([]byte("a"), 300), byte(1))
	f.Add(bytes.Repeat([]byte("ab"), 300), byte(3))
	f.Add(pseudoRandom(2000, 5), byte(7))
	f.Fuzz(func(t *testing.T, data []byte, pattern byte) {
		if len(data) > 1<<16 {
			data = data[:1<<16]
		}
		buf := make([]byte, CompressBlockBound(len(data)))
		n, err := CompressBlock(data, buf)
		if err != nil || n == 0 {
			return
		}
		compressed := buf[:n]

		// Deterministic fragment sizes in [0, maxFrag] derived from pattern.
		maxFrag := int(pattern%17) + 1
		sizes := sizesRandom(len(data), maxFrag, int64(pattern))
		parts := splitScatter(t, sizes, 0)
		got, err := UncompressBlockScatter(compressed, parts, nil)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got != len(data) {
			t.Fatalf("got %d want %d", got, len(data))
		}
		if assembled := gatherScatter(parts); !bytes.Equal(assembled, data) {
			t.Fatalf("mismatch (fragments=%d, maxFrag=%d)", len(sizes), maxFrag)
		}
	})
}

// FuzzScatterInvalid feeds arbitrary bytes as compressed blocks into
// fragmentations of various total capacities, checking that errors are
// reported (never surfaced as panics) and that bytes past the reported count
// are untouched.
func FuzzScatterInvalid(f *testing.F) {
	f.Add([]byte{0}, 1, byte(0))
	f.Add([]byte("\x1b0\x01\x00000000000000"), 16, byte(1))
	f.Add(make([]byte, 40), 10, byte(2))
	f.Fuzz(func(t *testing.T, src []byte, cap0 int, pattern byte) {
		if cap0 < 0 || cap0 > 4096 {
			return
		}
		parts := splitScatter(t, sizesRandom(cap0, int(pattern%8)+1, int64(pattern)+1), 0x55)
		n, _ := UncompressBlockScatter(src, parts, []byte("dict"))
		if n > cap0 {
			t.Fatalf("reported %d bytes > capacity %d", n, cap0)
		}
		assembled := gatherScatter(parts)
		for i := n; i < len(assembled); i++ {
			if assembled[i] != 0x55 {
				t.Fatalf("byte %d modified past reported count %d", i, n)
			}
		}
	})
}
