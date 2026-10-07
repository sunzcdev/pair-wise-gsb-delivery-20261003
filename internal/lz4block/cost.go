package lz4block

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/bits"
	"sort"
	"sync"

	"github.com/pierrec/lz4/v4/internal/lz4errors"
)

const (
	// CostOptimized selects a cost-based LZ4 parser. It is deliberately not a
	// normal HC depth: existing compression levels keep their original output.
	CostOptimized CompressionLevel = 2

	defaultCostCandidates  = 64
	defaultCostMemoryBytes = 96 << 20
	costExhaustiveLimit    = 128
	maxCostBlockSize       = 4 << 20
	costMatchLengthBudget  = 64
)

var costFixedMatchLengths = [16]int{4, 5, 6, 7, 8, 10, 12, 16, 24, 32, 48, 64, 96, 128, 192, 256}

// CostOptions configures the cost-optimized parser.
type CostOptions struct {
	// MaxCandidates bounds the number of hash-chain references examined at a
	// position. Zero selects the default budget of 64 candidates.
	MaxCandidates int
	// MaxMatchBytes bounds bytes compared while evaluating candidates at one
	// position. Zero selects 16384. On overflow the best valid partial candidate
	// is retained, which still encodes an LZ4 match.
	MaxMatchBytes int
	// MemoryBudget bounds the parser's transient working set in bytes. Zero
	// selects 96 MiB. Blocks that do not fit deterministically use HC instead.
	MemoryBudget int
}

func (o CostOptions) normalized() CostOptions {
	if o.MaxCandidates <= 0 {
		o.MaxCandidates = defaultCostCandidates
	}
	if o.MaxMatchBytes <= 0 {
		o.MaxMatchBytes = 16 << 10
	}
	if o.MemoryBudget <= 0 {
		o.MemoryBudget = defaultCostMemoryBytes
	}
	return o
}

// CompressorCost compresses blocks by evaluating the actual LZ4 token,
// literal, offset and match-extension cost of alternative parse paths.
// It is not safe for concurrent use.
type CompressorCost struct {
	Options CostOptions

	hash  []int32
	chain []int32
	work  []uint32
}

var compressorCostPool = sync.Pool{New: func() interface{} { return new(CompressorCost) }}

func CompressBlockCost(src, dst []byte, opts CostOptions) (int, error) {
	c := compressorCostPool.Get().(*CompressorCost)
	c.Options = opts
	n, err := c.CompressBlock(src, dst)
	compressorCostPool.Put(c)
	return n, err
}

func (c *CompressorCost) CompressBlock(src, dst []byte) (int, error) {
	if len(src) == 0 {
		if len(dst) == 0 {
			return 0, lz4errors.ErrInvalidSourceShortBuffer
		}
		dst[0] = 0
		return 1, nil
	}
	opts := c.Options.normalized()
	n := len(src)
	exhaustive := n <= costExhaustiveLimit
	if !c.prepare(n, opts, exhaustive) {
		return CompressBlockHC(src, dst, winSize)
	}
	return c.compress(src, dst, opts, exhaustive)
}

type literalQueue struct {
	age     int
	penalty [255]int
	best    [255]int
	anchor  [255]int
}

func newLiteralQueue() literalQueue {
	var q literalQueue
	for i := range q.best {
		q.best[i] = math.MaxInt
	}
	return q
}

func (q *literalQueue) advanceTo(pos int) {
	for q.age < pos {
		q.age++
		q.penalty[(q.age+240)%255]++
	}
}

func (q *literalQueue) add(anchor, cost int) {
	r := anchor % 255
	base := cost - q.penalty[r]
	if base < q.best[r] {
		q.best[r] = base
		q.anchor[r] = anchor
	}
}

func (q *literalQueue) min() (cost, anchor int) {
	cost = math.MaxInt
	anchor = -1
	for r := 0; r < 255; r++ {
		if q.best[r] == math.MaxInt {
			continue
		}
		candidate := q.best[r] + q.penalty[r] + q.age - q.anchor[r]
		if candidate < cost || (candidate == cost && q.anchor[r] < anchor) {
			cost = candidate
			anchor = q.anchor[r]
		}
	}
	return cost, anchor
}

func literalExtra(length int) int {
	if length < 0xF {
		return 0
	}
	return 1 + (length-0xF)/0xFF
}

func matchExtra(length int) int {
	length -= minMatch
	if length < 0xF {
		return 0
	}
	return 1 + (length-0xF)/0xFF
}

func literalCost(length int) int {
	return length + literalExtra(length)
}

func costMatchLength(src []byte, ref, pos, max int) int {
	n := 0
	safeMax := len(src) - 8
	for n+8 <= max && pos+n <= safeMax && ref+n <= safeMax {
		x := binary.LittleEndian.Uint64(src[pos+n:]) ^ binary.LittleEndian.Uint64(src[ref+n:])
		if x != 0 {
			matchLen := n + trailingZeroBytes(x)
			if matchLen > max {
				matchLen = max
			}
			return matchLen
		}
		n += 8
	}
	for n < max && src[pos+n] == src[ref+n] {
		n++
	}
	return n
}

func trailingZeroBytes(x uint64) int {
	return bits.TrailingZeros64(x) >> 3
}

func appendCostMatchLengths(lengths, unique []int, max int, exhaustive bool) []int {
	if exhaustive {
		for n := minMatch; n <= max; n++ {
			lengths = append(lengths, n)
		}
		return lengths
	}

	for _, n := range costFixedMatchLengths {
		if n <= max {
			lengths = append(lengths, n)
		}
	}
	for n := 19; n <= max; n += 0xFF {
		if len(lengths) >= costMatchLengthBudget-1 {
			break
		}
		if n > 256 {
			lengths = append(lengths, n)
		}
	}
	if len(lengths) < costMatchLengthBudget {
		lengths = append(lengths, max)
	}

	sort.Ints(lengths)
	for _, n := range lengths {
		if len(unique) == 0 || unique[len(unique)-1] != n {
			unique = append(unique, n)
		}
	}
	return unique
}

func (c *CompressorCost) prepare(n int, opts CostOptions, exhaustive bool) bool {
	need := 24 * n
	if !exhaustive {
		need += 4 * (htSize + winSize)
	}
	if n > maxCostBlockSize || need > opts.MemoryBudget {
		return false
	}

	if !exhaustive {
		if cap(c.hash) < htSize {
			c.hash = make([]int32, htSize)
		} else {
			c.hash = c.hash[:htSize]
		}
		if cap(c.chain) < winSize {
			c.chain = make([]int32, winSize)
		} else {
			c.chain = c.chain[:winSize]
		}
		for i := range c.hash {
			c.hash[i] = -1
		}
	}

	if cap(c.work) < 6*n {
		c.work = nil
		c.work = make([]uint32, 6*n)
	} else {
		c.work = c.work[:6*n]
		for i := range c.work {
			c.work[i] = 0
		}
	}
	dp := c.work[:n]
	for i := range dp {
		dp[i] = math.MaxUint32
	}
	dp[0] = 0
	return true
}

func (c *CompressorCost) compress(src, dst []byte, opts CostOptions, exhaustive bool) (int, error) {
	n := len(src)
	dp := c.work[:n]
	packed := c.work
	queue := newLiteralQueue()
	if n < 12 {
		return c.emit(src, dst, c.work, 0)
	}

	lastMatchStart := n - 12

	for pos := 0; pos <= n; pos++ {
		queue.advanceTo(pos)
		if pos < n && (pos == 0 || dp[pos] != math.MaxUint32) {
			queue.add(pos, int(dp[pos]))
		}
		if pos > lastMatchStart {
			continue
		}

		bestLiteralCost, anchor := queue.min()
		if anchor < 0 {
			continue
		}

		lastMatchEnd := n - 5
		longest, ref := c.longestMatch(src, pos, lastMatchEnd-pos, opts.MaxCandidates, opts.MaxMatchBytes, exhaustive)
		if len(src) > 200000 && longest > 10000 {
			fmt.Println("LONGCAND", pos, longest, lastMatchEnd-pos)
		}
		if longest < minMatch {
			continue
		}

		var candidateLengths [costMatchLengthBudget]int
		var uniqueLengths [costMatchLengthBudget]int
		candidates := appendCostMatchLengths(candidateLengths[:0], uniqueLengths[:0], longest, exhaustive)
		for _, matchLen := range candidates {
			end := pos + matchLen
			cost := bestLiteralCost + 3 + matchExtra(matchLen)
			if cost >= int(dp[end]) {
				continue
			}
			dp[end] = uint32(cost)
			packed[n+end] = uint32(anchor)
			packed[2*n+end] = uint32(pos)
			packed[3*n+end] = uint32(pos - ref)
			packed[4*n+end] = uint32(matchLen)
		}
	}

	_, finalAnchor := queue.min()
	if finalAnchor < 0 {
		finalAnchor = 0
	}
	if n > 200000 {
		count := 0
		for i := 0; i < n; i++ {
			if dp[i] != math.MaxUint32 {
				if count < 10 {
					fmt.Println("DP", i, dp[i])
				}
				count++
			}
		}
		fmt.Println("FINAL", finalAnchor, count)
	}
	isNotCompressible := len(dst) < CompressBlockBound(n)
	if isNotCompressible && finalAnchor == 0 {
		return 0, nil
	}

	di, err := c.emit(src, dst, c.work, finalAnchor)
	if err != nil {
		return 0, err
	}
	return di, nil
}

func (c *CompressorCost) longestMatch(src []byte, pos, max, maxCandidates, maxMatchBytes int, exhaustive bool) (length, ref int) {
	if pos <= 0 || max < minMatch || pos+4 > len(src) {
		return 0, -1
	}
	if exhaustive {
		comparisonBudget := math.MaxInt
		first := pos - winSize + 1
		if first < 0 {
			first = 0
		}
		for candidate := pos - 1; candidate >= first; candidate-- {
			if binary.LittleEndian.Uint32(src[candidate:]) != binary.LittleEndian.Uint32(src[pos:]) {
				continue
			}
			limit := max
			if comparisonBudget < limit {
				limit = comparisonBudget
			}
			n := costMatchLength(src, candidate, pos, limit)
			comparisonBudget -= n
			if n == limit && n < max && n > 0 {
				break
			}
			if n > length {
				length = n
				ref = candidate
			}
		}
		return length, ref
	}

	h := blockHashHC(binary.LittleEndian.Uint32(src[pos:]))
	next := int(c.hash[h])
	comparisonBudget := maxMatchBytes
	for tries := 0; tries < maxCandidates && next >= 0 && pos-next < winSize; tries++ {
		candidate := next
		next = int(c.chain[next&winMask])
		if candidate < 0 || candidate >= pos || pos-candidate >= winSize || candidate+4 > len(src) ||
			binary.LittleEndian.Uint32(src[candidate:]) != binary.LittleEndian.Uint32(src[pos:]) {
			continue
		}
		if length >= 0xF {
			probe := length + 64
			if probe < max && src[candidate+probe] != src[pos+probe] {
				continue
			}
		}
		limit := max
		if comparisonBudget < limit {
			limit = comparisonBudget
		}
		n := costMatchLength(src, candidate, pos, limit)
		if n <= comparisonBudget {
			comparisonBudget -= n
		} else {
			comparisonBudget = 0
		}
		if n > length {
			length = n
			ref = candidate
		}
		if length == max {
			break
		}
		if comparisonBudget <= 0 {
			break
		}
	}

	if c.hash[h] >= 0 {
		c.chain[pos&winMask] = c.hash[h]
	} else {
		c.chain[pos&winMask] = -1
	}
	c.hash[h] = int32(pos)
	return length, ref
}

func (c *CompressorCost) emit(src, dst []byte, work []uint32, firstEnd int) (int, error) {
	n := len(src)
	oldest := -1
	cursor := firstEnd
	for cursor != 0 {
		previous := int(work[n+cursor])
		next := n
		if oldest >= 0 {
			next = oldest
		}
		work[5*n+cursor] = uint32(next)
		oldest = cursor
		cursor = previous
	}

	anchor := 0
	di := 0
	end := oldest
	if end < 0 {
		end = n
	}

	for end != n {
		length := int(work[4*n+end])
		offset := int(work[3*n+end])
		start := int(work[2*n+end])
		literalLen := start - anchor
		matchLen := length - minMatch

		token := byte(0)
		if literalLen >= 0xF {
			token = 0xF0
		} else {
			token = byte(literalLen << 4)
		}
		if matchLen >= 0xF {
			token |= 0xF
		} else {
			token |= byte(matchLen)
		}
		if di >= len(dst) {
			return 0, lz4errors.ErrInvalidSourceShortBuffer
		}
		dst[di] = token
		di++

		if literalLen >= 0xF {
			remaining := literalLen - 0xF
			for remaining > 0xFF {
				if di >= len(dst) {
					return 0, lz4errors.ErrInvalidSourceShortBuffer
				}
				dst[di] = 0xFF
				di++
				remaining -= 0xFF
			}
			if di >= len(dst) {
				return 0, lz4errors.ErrInvalidSourceShortBuffer
			}
			dst[di] = byte(remaining)
			di++
		}

		if di+literalLen > len(dst) {
			return 0, lz4errors.ErrInvalidSourceShortBuffer
		}
		copy(dst[di:di+literalLen], src[anchor:start])
		di += literalLen

		if di+2 > len(dst) || offset <= 0 || offset > winSize || offset > start {
			return 0, lz4errors.ErrInvalidSourceShortBuffer
		}
		dst[di] = byte(offset)
		dst[di+1] = byte(offset >> 8)
		di += 2

		if matchLen >= 0xF {
			remaining := matchLen - 0xF
			for remaining > 0xFF {
				if di >= len(dst) {
					return 0, lz4errors.ErrInvalidSourceShortBuffer
				}
				dst[di] = 0xFF
				di++
				remaining -= 0xFF
			}
			if di >= len(dst) {
				return 0, lz4errors.ErrInvalidSourceShortBuffer
			}
			dst[di] = byte(remaining)
			di++
		}

		nextEnd := int(work[5*n+end])
		anchor = end
		if nextEnd < 0 {
			end = n
		} else {
			end = nextEnd
		}
	}

	literalLen := n - anchor
	if di >= len(dst) {
		return 0, lz4errors.ErrInvalidSourceShortBuffer
	}
	if literalLen >= 0xF {
		dst[di] = 0xF0
	} else {
		dst[di] = byte(literalLen << 4)
	}
	di++

	if literalLen >= 0xF {
		remaining := literalLen - 0xF
		for remaining > 0xFF {
			if di >= len(dst) {
				return 0, lz4errors.ErrInvalidSourceShortBuffer
			}
			dst[di] = 0xFF
			di++
			remaining -= 0xFF
		}
		if di >= len(dst) {
			return 0, lz4errors.ErrInvalidSourceShortBuffer
		}
		dst[di] = byte(remaining)
		di++
	}

	if di+literalLen > len(dst) {
		return 0, lz4errors.ErrInvalidSourceShortBuffer
	}
	di += copy(dst[di:di+literalLen], src[anchor:])
	return di, nil
}
