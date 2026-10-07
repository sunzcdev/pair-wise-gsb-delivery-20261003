package lz4block

import (
	"encoding/binary"
	"math"
	"math/bits"
	"sync"

	"github.com/pierrec/lz4/v4/internal/lz4errors"
)

// Optimal selects the cost-optimal parser. It is an opt-in compression mode
// for offline use: it spends more time and memory than the HC levels, parsing
// each block with dynamic programming that accounts for the actual LZ4 token,
// literal and match length encoding cost, instead of greedily taking the
// longest match. The existing Fast and HC compression levels are unaffected.
const Optimal CompressionLevel = 2

// Defaults of OptimalParams. They are tuned for archival: slower than Level9
// on a 4MiB block, and bounded in both candidate-search work and memory.
const (
	// defaultOptimalMemoryBudget bounds the match finder and parser tables.
	defaultOptimalMemoryBudget = 128 << 20
	// defaultOptimalCandidates bounds the hash-chain candidates probed for
	// one match position.
	defaultOptimalCandidates = 64
	// defaultOptimalGap bounds how far, in input bytes, a match may be
	// postponed so that the literals before it merge into the preceding
	// sequence.
	defaultOptimalGap = 48
	// defaultOptimalLookahead bounds how many of the match starts inside
	// that gap are considered.
	defaultOptimalLookahead = 8
	// defaultOptimalShortMatch bounds the short match lengths enumerated at
	// every considered start; the full measured match length is also
	// considered.
	defaultOptimalShortMatch = 32
	// optimalHashMin is the smallest hash table the match finder degrades to
	// before falling back to the HC algorithm.
	optimalHashMin = 10
	// matchCap bounds how far a match is measured. Longer runs are recorded
	// at the cap; the parser reaches the rest with chained matches. This
	// keeps the word-wise measurement bounded on dense periodic data, where
	// every position matches almost to the end.
	matchCap = 1024
)

// OptimalParams configures the cost-optimal parser. The zero value uses the
// defaults. The limits are explicit: Candidates bounds the candidate search,
// MemoryBudget bounds the scratch tables, and Gap, Lookahead and ShortMatch
// bound the parser work. When the budget cannot fit even the smallest
// configuration for an input, compression deterministically degrades to the
// Level9 HC algorithm, whose output meets the same block format constraints.
type OptimalParams struct {
	// MemoryBudget is the maximum bytes of scratch tables used per block.
	MemoryBudget int
	// Candidates is the maximum hash-chain candidates probed per position.
	Candidates int
	// Gap is the maximum number of input bytes a match may be postponed.
	Gap int
	// Lookahead is the maximum number of match starts examined within Gap.
	Lookahead int
	// ShortMatch is the largest sub-full match length enumerated at every
	// considered start; the full measured match length is always considered.
	ShortMatch int
}

// measureCap returns the cap on match measurement: the documented matchCap
// normally, but unlimited when the parser bounds were lifted, which the
// exhaustive correctness test relies on.
func (p OptimalParams) measureCap() int {
	if p.Gap >= 1<<30 && p.Lookahead >= 1<<30 && p.ShortMatch >= 1<<30 {
		// Exhaustive mode: matches need not be measured past the input
		// length, which the callers bound to small inputs. Capping at a
		// finite value keeps the word-wise matcher bounded.
		return 1 << 20
	}
	return matchCap
}

func (p OptimalParams) withDefaults() OptimalParams {
	if p.MemoryBudget <= 0 {
		p.MemoryBudget = defaultOptimalMemoryBudget
	}
	if p.Candidates <= 0 {
		p.Candidates = defaultOptimalCandidates
	}
	if p.Gap <= 0 {
		p.Gap = defaultOptimalGap
	}
	if p.Lookahead <= 0 {
		p.Lookahead = defaultOptimalLookahead
	}
	if p.ShortMatch <= 0 {
		p.ShortMatch = defaultOptimalShortMatch
	}
	return p
}

// CompressorOptimal compresses data into the LZ4 block format with a
// cost-optimal parser. At every position it chooses between emitting literals
// and taking each searched match candidate based on the exact number of bytes
// the resulting sequence costs in the LZ4 block format: the token nibble, the
// literal run and its extension bytes, the offset and the match length
// extension bytes. It is neither a deeper greedy search nor a longest-match
// rule.
//
// It is slower than CompressorHC and uses up to Params.MemoryBudget bytes of
// scratch space, but can improve the compression ratio on repetitive data,
// data with long repeated prefixes and data with far-apart matches.
//
// A CompressorOptimal is not safe for concurrent use by multiple goroutines.
type CompressorOptimal struct {
	hc CompressorHC
	// Reused tables from the previous block, kept when large enough.
	hash        []int32
	chain       []int32
	bestOff     []int32
	bestNearOff []int32
	bestNearLen []int32
	bestLen     []int32
	nextMatch   []int32
	cost        []int32
	choiceSrc   []int32
	choiceMLen  []int32
	choiceOff   []int32
}

var compressorOptimalPool = sync.Pool{New: func() interface{} { return new(CompressorOptimal) }}

// CompressBlockOptimal compresses src with the cost-optimal parser using a
// pooled CompressorOptimal.
func CompressBlockOptimal(src, dst []byte, params OptimalParams) (int, error) {
	c := compressorOptimalPool.Get().(*CompressorOptimal)
	n, err := c.CompressBlock(src, dst, params)
	compressorOptimalPool.Put(c)
	return n, err
}

func grow[T any](s []T, n int) []T {
	if cap(s) >= n {
		return s[:n]
	}
	return make([]T, n)
}

// CompressBlock compresses src into dst with the cost-optimal parser.
func (c *CompressorOptimal) CompressBlock(src, dst []byte, params OptimalParams) (int, error) {
	params = params.withDefaults()
	n := len(src)
	if n == 0 {
		// An empty block is a single zero token, the same output as HC.
		if len(dst) == 0 {
			return 0, lz4errors.ErrInvalidSourceShortBuffer
		}
		dst[0] = 0
		return 1, nil
	}
	if int64(n) > math.MaxInt32 {
		// Same boundary handling as the HC compressor.
		return c.hc.CompressBlock(src, dst, 0)
	}

	// Pick the largest hash table that fits the memory budget. The parser
	// tables are fixed by the input size; if even the smallest hash table
	// does not fit, deterministically degrade to Level9 HC.
	//
	// Layout: hash table (4*2^hlog), chain table (4*winSize) and nine int32
	// tables of n+1 entries (bestOff, bestNearOff, bestNearLen, bestLen,
	// nextMatch, cost and the three choice tables).
	m := int64(n + 1)
	budget := int64(params.MemoryBudget)
	// The parser tables that actually scale with the input are the six
	// per-position tables (bestOff, bestNearOff, bestNearLen, bestLen,
	// nextMatch, cost and the three choice tables); the winSize chain table
	// and hash table are bounded. Allocate the hash table once at its full
	// size when the budget allows, avoiding shrinking it for small blocks.
	fixed := int64(winSize)*4 + 9*m*4
	hlog := winSizeLog
	for hlog >= optimalHashMin && fixed+(int64(4)<<hlog) > budget {
		hlog--
	}
	if hlog < optimalHashMin {
		return c.hc.CompressBlock(src, dst, 0)
	}

	// Acquire tables, reusing previous allocations when large enough.
	mm := n + 1
	c.hash = grow(c.hash, 1<<hlog)
	for i := range c.hash {
		c.hash[i] = 0 // 0 means empty; stored positions are biased by one.
	}
	c.chain = grow(c.chain, winSize)
	for i := range c.chain {
		c.chain[i] = 0
	}
	c.bestOff = grow(c.bestOff, mm)
	c.bestNearOff = grow(c.bestNearOff, mm)
	c.bestNearLen = grow(c.bestNearLen, mm)
	c.bestLen = grow(c.bestLen, mm)
	c.nextMatch = grow(c.nextMatch, mm)
	c.cost = grow(c.cost, mm)
	c.choiceSrc = grow(c.choiceSrc, mm)
	c.choiceMLen = grow(c.choiceMLen, mm)
	c.choiceOff = grow(c.choiceOff, mm)

	return compressBlockOptimal(src, dst, params, hlog, c.hash[:1<<hlog], c.chain, c.bestOff[:mm], c.bestNearOff[:mm], c.bestNearLen[:mm], c.bestLen[:mm], c.nextMatch[:mm], c.cost[:mm], c.choiceSrc[:mm], c.choiceMLen[:mm], c.choiceOff[:mm])
}

const noMatch int32 = -1

// literalRunCost is the number of bytes needed to encode a run of l literals
// excluding the token byte they share with a sequence: the literal bytes plus
// the 255-byte high-nibble extension encoding.
func literalRunCost(l int) int {
	if l < 0xF {
		return l
	}
	return l + 1 + (l-0xF)/0xFF
}

// lastLiteralsCost is the encoding cost of the trailing literals, which always
// carry their own token byte with a zero match-length nibble.
func lastLiteralsCost(l int) int { return 1 + literalRunCost(l) }

// matchExtraCost is the number of bytes needed to encode the match length
// beyond the mandatory four bytes: only the low-nibble 255-byte extension,
// since the token byte is shared with the literals.
func matchExtraCost(mLen int) int {
	mLen -= minMatch
	if mLen < 0xF {
		return 0
	}
	return 1 + (mLen-0xF)/0xFF
}

// appendMatchLengthCandidates returns the distinct match lengths worth
// evaluating for one candidate: every length from four through short, and
// the full measured length. Shortening a match can pay off when the freed
// bytes begin a cheaper sequence, which is the whole point of the parser.
func appendMatchLengthCandidates(lens []int, full, maxL, short int) []int {
	lens = lens[:0]
	upto := short
	if upto > full {
		upto = full
	}
	if upto > maxL {
		upto = maxL
	}
	for l := minMatch; l <= upto; l++ {
		lens = append(lens, l)
	}
	if full >= minMatch && full <= maxL {
		if len(lens) == 0 || full > lens[len(lens)-1] {
			lens = append(lens, full)
		}
	}
	return lens
}

// matchFullLength returns the length of the match an LZ4 decoder would
// produce when copying from src[cand:] to src[si:], up to n-si. When the
// reference is entirely behind the target (cand+... never reaches si), it is
// a fixed-position comparison; otherwise the copy overlaps itself, as for a
// run or a short-period repeat, and each output byte is compared against the
// byte offset positions earlier. It reads whole words only while at least
// eight bytes remain, so it never reads past the end of src.
func matchFullLength(src []byte, cand, si int) int {
	n := len(src)
	off := si - cand
	if off <= 0 {
		return 0
	}
	ml := 0
	// Phase 1: compare against the original reference src[cand:]. It spans at
	// most off bytes; words must stay fully inside so a straddling word does
	// not mix fixed and repeated bytes.
	for ml+8 <= off && si+ml+8 <= n {
		x := binary.LittleEndian.Uint64(src[cand+ml:]) ^ binary.LittleEndian.Uint64(src[si+ml:])
		if x != 0 {
			return ml + bits.TrailingZeros64(x)>>3
		}
		ml += 8
	}
	for ml < off && si+ml < n && src[cand+ml] == src[si+ml] {
		ml++
	}
	// Phase 2: only once a full period of off original bytes matched may the
	// copy overlap itself; the decoder then repeats src[si:si+off].
	if ml == off {
		for si+ml+8 <= n {
			x := binary.LittleEndian.Uint64(src[si+ml-off:]) ^ binary.LittleEndian.Uint64(src[si+ml:])
			if x != 0 {
				return ml + bits.TrailingZeros64(x)>>3
			}
			ml += 8
		}
		for si+ml < n && src[si+ml-off] == src[si+ml] {
			ml++
		}
	}
	return ml
}

// matchCappedLength is matchFullLength stopped after at most cap bytes.
func matchCappedLength(src []byte, cand, si, cap int) int {
	n := len(src)
	off := si - cand
	if off <= 0 || cap <= 0 {
		return 0
	}
	if cap > n-si {
		cap = n - si
	}
	ml := 0
	for ml+8 <= off && ml+8 <= cap {
		x := binary.LittleEndian.Uint64(src[cand+ml:]) ^ binary.LittleEndian.Uint64(src[si+ml:])
		if x != 0 {
			return ml + bits.TrailingZeros64(x)>>3
		}
		ml += 8
	}
	for ml < off && ml < cap && src[cand+ml] == src[si+ml] {
		ml++
	}
	if ml == off {
		for ml+8 <= cap {
			x := binary.LittleEndian.Uint64(src[si+ml-off:]) ^ binary.LittleEndian.Uint64(src[si+ml:])
			if x != 0 {
				return ml + bits.TrailingZeros64(x)>>3
			}
			ml += 8
		}
		for ml < cap && src[si+ml-off] == src[si+ml] {
			ml++
		}
	}
	return ml
}

// compressBlockOptimal runs the two passes of the cost-optimal parser and
// emits the chosen parse.
//
// Pass one is a bounded hash-chain match finder. Every position is inserted.
// For each position, up to Candidates earlier positions with the same hash
// are probed within the 64kiB window; the best one (longest, ties keep the
// nearest) is recorded together with its offset.
//
// Pass two is backward dynamic programming over the exact LZ4 block encoding
// cost. A match may be postponed by at most Gap bytes: the first Lookahead
// match starts inside that gap and one match start beyond it are considered,
// which bounds the work while comparing the literal-vs-match trade-offs. At
// each considered start the match lengths from four through ShortMatch are
// enumerated, together with the full measured length, since shortening a
// match can pay off when the freed bytes begin a better sequence.
func compressBlockOptimal(src, dst []byte, params OptimalParams, hashLog int, hashHead, chain, bestOff, bestNearOff, bestNearLen, bestLen, nextMatch, cost, choiceSrc, choiceMLen, choiceOff []int32) (_ int, err error) {
	defer recoverBlock(&err)
	// Cap dst so a bounds mistake cannot reslice into the caller's memory.
	dst = dst[:len(dst):len(dst)]

	n := len(src)
	mfStart := n - mfLimit // exclusive end of positions that may start a match
	if mfStart < 0 {
		mfStart = 0
	}

	// Clear the per-block candidate tables; they are reused and must not
	// retain a previous block. The slices may be larger than this block
	// after a previous big block, so reset every entry.
	for i := range bestLen {
		bestLen[i] = 0
	}
	for i := range bestNearOff {
		bestNearOff[i] = 0
	}
	for i := range bestNearLen {
		bestNearLen[i] = 0
	}

	// Pass one: bounded hash-chain candidate search. Hash heads store
	// positions biased by one, so zero means empty and position zero is
	// usable. A chain slot is written by the position occupying it before
	// any later walk reads it, exactly as in the HC compressor.
	//
	// The walk is bounded in two ways so dense periodic data cannot make it
	// quadratic: at most Candidates chain links are followed, and per
	// position only the nearest candidate and the longest candidate are
	// retained. A far match never costs more than a near one (the offset is
	// always two bytes), and the nearest match covers the smallest offset, so
	// these two span every trade-off the parser can exploit. Measuring stops
	// once a match reaches the maximum useful length: all trailing bytes but
	// mfLimit-minMatch may be consumed by one match.
	maxDepth := params.Candidates
	measureCap := params.measureCap()
	if measureCap > n {
		measureCap = n
	}
	for si := 0; si < mfStart; si++ {
		h := int(blockHashHC(binary.LittleEndian.Uint32(src[si:]))) & (1<<hashLog - 1)
		head := hashHead[h]
		// Unconditional, as in the HC compressor: an empty head stores -1,
		// terminating the chain, and the slot is written before any later
		// walk reads it.
		chain[si&winMask] = head - 1
		hashHead[h] = int32(si + 1)
		if head == 0 {
			continue
		}

		maxUseful := n - si - (mfLimit - minMatch)
		if maxUseful > measureCap {
			maxUseful = measureCap
		}
		cand := int(head) - 1
		var nearestOff, nearestL, longestOff int
		var longestL int
		try := 0
		for ; try < maxDepth && cand >= 0 && si-cand < winSize; try++ {
			if binary.LittleEndian.Uint32(src[cand:]) == binary.LittleEndian.Uint32(src[si:]) {
				if nearestOff == 0 {
					nearestOff = si - cand
					// Measure the nearest candidate only to the parser's
					// short-match horizon: its role is local.
					nearCap := params.ShortMatch
					if nearCap > maxUseful {
						nearCap = maxUseful
					}
					nearestL = matchCappedLength(src, cand, si, nearCap)
					if nearestL > longestL {
						longestL, longestOff = nearestL, nearestOff
					}
				}
				// One-byte filter for replacing the longest: a miss at the
				// current best cannot be longer (HC does the same).
				if cand+longestL < n && src[cand+longestL] == src[si+longestL] {
					if ml := matchCappedLength(src, cand, si, maxUseful); ml > longestL {
						longestL = ml
						longestOff = si - cand
						if ml >= maxUseful {
							break
						}
					}
				}
			}
			next := int(chain[cand&winMask])
			if next < 0 || next >= cand {
				break
			}
			cand = next
		}
		if longestL < minMatch {
			continue
		}
		// Record the longest candidate as the primary one. The nearest
		// candidate, when distinct, is recorded at the free second slot
		// bestNear for the parser to also consider.
		bestLen[si] = int32(longestL)
		bestOff[si] = int32(longestOff)
		if nearestOff != 0 && nearestOff != longestOff {
			bestNearOff[si] = int32(nearestOff)
			bestNearLen[si] = int32(nearestL)
		}
	}

	// Backward extension: a candidate can start up to mfLimit-minMatch
	// bytes before its recorded position when the preceding bytes also match
	// the reference (the greedy encoders do this to shrink the literal run).
	// Every extension is measured with the decoder-aware matcher and only
	// recorded when genuine, so the parser can only choose valid references.
	for si := mfStart - 1; si > 0; si-- {
		l := int(bestLen[si])
		if l < minMatch {
			continue
		}
		off := int(bestOff[si])
		for back := 1; back < mfLimit-minMatch && si-back >= 0; back++ {
			j := si - back
			if j-off < 0 || src[j] != src[j-off] {
				break
			}
			ml := matchCappedLength(src, j-off, j, measureCap)
			if ml < minMatch {
				break
			}
			maxL := n - j - (mfLimit - minMatch)
			if ml > maxL {
				ml = maxL
			}
			// Fill an empty earlier position; do not overwrite a candidate
			// the direct search already proved.
			if bestLen[j] < minMatch && ml >= minMatch {
				bestLen[j] = int32(ml)
				bestOff[j] = int32(off)
			}
		}
	}

	// Fill nextMatch: nextMatch[i] is the smallest j >= i with a candidate.
	var nextWithMatch int32 = noMatch
	for i := n - 1; i >= 0; i-- {
		if i < mfStart && bestLen[i] >= minMatch {
			nextWithMatch = int32(i)
		}
		nextMatch[i] = nextWithMatch
	}
	// Pass two: backward dynamic programming over exact encoding cost.
	for i := 0; i <= n; i++ {
		cost[i] = int32(lastLiteralsCost(n - i))
		choiceSrc[i] = noMatch
		choiceOff[i] = 0
	}
	gap := params.Gap
	lookahead := params.Lookahead
	short := params.ShortMatch
	if short > n {
		short = n
	}
	lensBuf := make([]int, 0, short+2)
	for i := mfStart - 1; i >= 0; i-- {
		// Direct match at i: the preceding sequence (if any) owns the
		// literals before i, so this sequence has zero leading literals and
		// its token carries the match length. cost[i+l] accounts for the
		// following sequence's token.
		if bestLen[i] >= minMatch {
			full := int(bestLen[i])
			maxL := full
			if maxL > n-i-(mfLimit-minMatch) {
				// Leave mfLimit-minMatch trailing literals, as the other
				// compressors do, so the block ends with a literal run.
				maxL = n - i - (mfLimit - minMatch)
			}
			// Evaluate the longest candidate, and when distinct the nearest
			// one (whose full length may differ).
			type cand struct{ off, full int }
			cands := []cand{{int(bestOff[i]), full}}
			if no := int(bestNearOff[i]); no != 0 && no != int(bestOff[i]) {
				cands = append(cands, cand{no, int(bestNearLen[i])})
			}
			for _, cd := range cands {
				lens := appendMatchLengthCandidates(lensBuf[:0], cd.full, maxL, short)
				for _, l := range lens {
					e := i + l
					c := 1 + 2 + matchExtraCost(l) + int(cost[e])
					if c < int(cost[i]) {
						cost[i] = int32(c)
						choiceSrc[i] = int32(i)
						choiceMLen[i] = int32(l)
						choiceOff[i] = int32(cd.off)
					}
					// Far anchor: the match starts the block's first
					// sequence, with every byte from the block start as a
					// literal. This reaches a match across an incompressible
					// gap longer than Gap, independently of accidental
					// matches in between.
					cf := 1 + literalRunCost(i) + 2 + matchExtraCost(l) + int(cost[e])
					if cf < int(cost[i]) {
						cost[i] = int32(cf)
						choiceSrc[i] = int32(i)
						choiceMLen[i] = int32(l)
						choiceOff[i] = int32(cd.off)
					}
				}
			}
		}

		// Postponed matches: i is the first of d leading literals and the
		// sequence starts at j > i. The token is this sequence's token; the
		// following token is inside cost[j+l].
		j := int(nextMatch[i+1])
		for seen := 0; seen < lookahead && j != int(noMatch); seen++ {
			if j-i > gap {
				break
			}
			full := int(bestLen[j])
			maxL := full
			if maxL > n-j-(mfLimit-minMatch) {
				maxL = n - j - (mfLimit - minMatch)
			}
			type cand struct{ off, full int }
			cands := []cand{{int(bestOff[j]), full}}
			if no := int(bestNearOff[j]); no != 0 && no != int(bestOff[j]) {
				cands = append(cands, cand{no, int(bestNearLen[j])})
			}
			for _, cd := range cands {
				lens := appendMatchLengthCandidates(lensBuf[:0], cd.full, maxL, short)
				for _, l := range lens {
					e := j + l
					// Token + d literals + offset + match extension + suffix.
					c := 1 + literalRunCost(j-i) + 2 + matchExtraCost(l) + int(cost[e])
					if c < int(cost[i]) {
						cost[i] = int32(c)
						choiceSrc[i] = int32(j)
						choiceMLen[i] = int32(l)
						choiceOff[i] = int32(cd.off)
					}
				}
			}
			nj := int(nextMatch[j+1])
			if nj == int(noMatch) {
				break
			}
			j = nj
			if j-i > gap {
				// Consider one match start beyond the gap so postponing
				// past all in-gap starts remains available, then stop.
				maxL := n - j - (mfLimit - minMatch)
				type cand struct{ off, full int }
				cands := []cand{{int(bestOff[j]), int(bestLen[j])}}
				if no := int(bestNearOff[j]); no != 0 && no != int(bestOff[j]) {
					cands = append(cands, cand{no, int(bestNearLen[j])})
				}
				for _, cd := range cands {
					lens := appendMatchLengthCandidates(lensBuf[:0], cd.full, maxL, short)
					for _, l := range lens {
						e := j + l
						c := 1 + literalRunCost(j-i) + 2 + matchExtraCost(l) + int(cost[e])
						if c < int(cost[i]) {
							cost[i] = int32(c)
							choiceSrc[i] = int32(j)
							choiceMLen[i] = int32(l)
							choiceOff[i] = int32(cd.off)
						}
					}
				}
				break
			}
		}

		// Long-distance first match: the match is preceded by a literal run
		// longer than Gap, so it cannot be reached by the postponement loop.
		// The earliest match beyond the gap is the only one needed: between i
		// and it there is no other match, hence no cheaper intervening parse.
		// This is what captures far-apart matches across incompressible gaps.
		// Only i with no match inside the gap needs this consideration; for
		// other i the postponement loop above already covers the reachable
		// starts, which keeps the work linear away from actual far matches.
		{
			fj := int(nextMatch[i+1])
			if fj != int(noMatch) && fj-i > gap {
				// fj is already the first match beyond the gap.
			} else {
				fj = int(noMatch)
			}
			if fj != int(noMatch) {
				full := int(bestLen[fj])
				type cand struct{ off, full int }
				cands := []cand{{int(bestOff[fj]), full}}
				if no := int(bestNearOff[fj]); no != 0 && no != int(bestOff[fj]) {
					cands = append(cands, cand{no, int(bestNearLen[fj])})
				}
				for _, cd := range cands {
					maxL := n - fj - (mfLimit - minMatch)
					// Full-length match from the recorded start.
					lens := appendMatchLengthCandidates(lensBuf[:0], cd.full, maxL, short)
					for _, ll := range lens {
						e := fj + ll
						c := 1 + literalRunCost(fj-i) + 2 + matchExtraCost(ll) + int(cost[e])
						if c < int(cost[i]) {
							cost[i] = int32(c)
							choiceSrc[i] = int32(fj)
							choiceMLen[i] = int32(ll)
							choiceOff[i] = int32(cd.off)
						}
					}
				}
			}
		}
	}

	// cost[0] is the exact size of the selected encoding. The all-literals
	// encoding costs lastLiteralsCost(n); if no sequence improved on it, the
	// block is incompressible.
	if choiceSrc[0] == noMatch || int(cost[0]) >= n {
		// No parse beats storing the raw block. Frame writers pass a
		// destination of len(src), in which the literals-only encoding does
		// not fit either.
		return 0, nil
	}
	if int(cost[0]) > len(dst) {
		return 0, lz4errors.ErrInvalidSourceShortBuffer
	}
	nout := emitOptimal(src, dst, choiceOff, choiceSrc, choiceMLen)
	if nout != int(cost[0]) {
		// The cost model and the encoder disagree, an internal error guarded
		// as a short destination to keep the block contract.
		return 0, lz4errors.ErrInvalidSourceShortBuffer
	}
	return nout, nil
}

// emitOptimal emits the parse selected by the dynamic programming pass and
// returns the number of bytes written.
func emitOptimal(src, dst []byte, choiceOff, choiceSrc, choiceMLen []int32) int {
	n := len(src)
	anchor := 0
	di := 0
	for anchor < n {
		j := int(choiceSrc[anchor])
		if j == int(noMatch) {
			break // only trailing literals remain
		}
		lLen := j - anchor
		l := int(choiceMLen[anchor])
		e := j + l
		offset := int(choiceOff[anchor])

		// Token: literal length in the high nibble, match length in the low.
		mLen := l - minMatch
		tok := byte(0xF)
		if mLen < 0xF {
			tok = byte(mLen)
		}
		if lLen < 0xF {
			dst[di] = tok | byte(lLen<<4)
		} else {
			dst[di] = tok | 0xF0
			di++
			rem := lLen - 0xF
			for ; rem >= 0xFF; rem -= 0xFF {
				dst[di] = 0xFF
				di++
			}
			dst[di] = byte(rem)
		}
		di++

		copy(dst[di:di+lLen], src[anchor:j])
		di += lLen

		dst[di] = byte(offset)
		dst[di+1] = byte(offset >> 8)
		di += 2

		if mLen >= 0xF {
			rem := mLen - 0xF
			for ; rem >= 0xFF; rem -= 0xFF {
				dst[di] = 0xFF
				di++
			}
			dst[di] = byte(rem)
			di++
		}

		anchor = e
	}

	// Trailing literals.
	lLen := n - anchor
	if lLen < 0xF {
		dst[di] = byte(lLen << 4)
	} else {
		dst[di] = 0xF0
		di++
		rem := lLen - 0xF
		for ; rem >= 0xFF; rem -= 0xFF {
			dst[di] = 0xFF
			di++
		}
		dst[di] = byte(rem)
	}
	di++
	di += copy(dst[di:], src[anchor:])
	return di
}
