package participle

import (
	"fmt"
	"sort"
)

// diffProduction compares two versions of a single production. It returns
// nil when the productions are structurally identical.
func diffProduction(id string, old, neo *productionModel) *ProductionChange {
	if old.kind != neo.kind {
		change := &ProductionChange{ID: id}
		change.Kinds = []ChangeKind{SequenceChange}
		change.Details = []string{fmt.Sprintf(
			"production kind changed from %s to %s", old.kind, neo.kind)}
		return change
	}
	d := &exprDiffer{}
	d.diff(old.expr, neo.expr, shortName(id))
	if len(d.changes) == 0 {
		return nil
	}
	change := &ProductionChange{ID: id, Kinds: d.kinds()}
	for _, c := range d.changes {
		change.Details = append(change.Details, c.detail)
	}
	sort.Strings(change.Details)
	change.Details = dedupeSorted(change.Details)
	return change
}

type changeDetail struct {
	kind   ChangeKind
	detail string
}

type exprDiffer struct {
	changes []changeDetail
}

func (d *exprDiffer) add(kind ChangeKind, detail string) {
	d.changes = append(d.changes, changeDetail{kind: kind, detail: detail})
}

func (d *exprDiffer) kinds() []ChangeKind {
	set := map[ChangeKind]bool{}
	for _, c := range d.changes {
		set[c.kind] = true
	}
	out := make([]ChangeKind, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// diff compares two expression trees at a fixed depth. References are leaf
// comparisons by identity, so the recursion never expands beyond the single
// production being compared.
func (d *exprDiffer) diff(old, neo *gExpr, path string) {
	if old.kind != neo.kind {
		d.add(SequenceChange, fmt.Sprintf("%s: changed from %s to %s",
			path, describeKind(old), describeKind(neo)))
		return
	}
	switch old.kind {
	case exprLiteral:
		if *old.literal != *neo.literal {
			d.add(TokenChange, fmt.Sprintf("%s: literal changed from %s to %s",
				path, describeLiteral(old.literal), describeLiteral(neo.literal)))
		}
	case exprToken:
		if old.token.name != neo.token.name {
			d.add(TokenChange, fmt.Sprintf("%s: token changed from %q to %q",
				path, old.token.name, neo.token.name))
		}
	case exprRef:
		if *old.ref != *neo.ref {
			d.add(SequenceChange, fmt.Sprintf("%s: reference changed from %s to %s",
				path, describeRef(old.ref), describeRef(neo.ref)))
		}
	case exprQuant:
		if old.quant.mode != neo.quant.mode {
			d.add(QuantifierChange, fmt.Sprintf("%s: quantifier changed from %s to %s",
				path, old.quant.mode, neo.quant.mode))
		}
		d.diff(old.quant.expr, neo.quant.expr, path+" "+old.quant.mode.String())
	case exprNeg:
		d.diff(old.neg.expr, neo.neg.expr, path+" ~")
	case exprLook:
		if old.look.negative != neo.look.negative {
			d.add(SequenceChange, fmt.Sprintf("%s: lookahead polarity changed", path))
		}
		d.diff(old.look.expr, neo.look.expr, path+lookSuffix(old.look.negative))
	case exprAlt:
		d.diffAlt(old.children, neo.children, path)
	case exprSeq:
		d.diffSeq(old.children, neo.children, path)
	}
}

func (d *exprDiffer) diffAlt(old, neo []*gExpr, path string) {
	if equalSequence(old, neo) {
		return
	}
	// Match branches by content, then determine whether surviving branches
	// changed order (semantic for an ordered choice) and which branches were
	// added, removed, or edited in place.
	pairs, removedOld, addedNeo := matchBranches(old, neo)
	for _, oi := range removedOld {
		d.add(terminalChangeKind(old[oi]), fmt.Sprintf("%s: branch removed %s", path, describe(old[oi])))
	}
	for _, ni := range addedNeo {
		d.add(terminalChangeKind(neo[ni]), fmt.Sprintf("%s: branch added %s", path, describe(neo[ni])))
	}
	sort.Slice(pairs, func(a, b int) bool { return pairs[a].oi < pairs[b].oi })
	// Reordering is reported when a matched branch occupies a different
	// position, but only when no branch was added or removed: with edits to
	// the set of branches, position differences are an artefact of those edits
	// rather than a deliberate swap.
	if len(removedOld) == 0 && len(addedNeo) == 0 {
		for _, p := range pairs {
			if p.oi != p.ni {
				d.add(OrderedBranchChange, fmt.Sprintf("%s: branches reordered (ordered choice)", path))
				break
			}
		}
	}
	for _, p := range pairs {
		childPath := fmt.Sprintf("%s[%d]", path, p.oi+1)
		d.diff(old[p.oi], neo[p.ni], childPath)
	}
}

func (d *exprDiffer) diffSeq(old, neo []*gExpr, path string) {
	if equalSequence(old, neo) {
		return
	}
	d.align(old, neo, path, "element")
}

type indexPair struct{ oi, ni int }

func sameTerminalKind(a, b *gExpr) bool {
	return (a.kind == exprLiteral && b.kind == exprLiteral) ||
		(a.kind == exprToken && b.kind == exprToken)
}

// terminalChangeKind classifies an added/removed standalone element: a
// terminal literal/token is a token change, anything structural is a sequence
// change.
func terminalChangeKind(e *gExpr) ChangeKind {
	switch alignmentCore(e).kind {
	case exprLiteral, exprToken:
		return TokenChange
	default:
		return SequenceChange
	}
}

// matchBranches performs a one-to-one, order-independent matching of branches
// by similarity. It is used for ordered choices so that branch reordering is
// visible independently of the surviving branches' positions.
func matchBranches(old, neo []*gExpr) (pairs []indexPair, removedOld, addedNeo []int) {
	type cand struct {
		oi, ni int
		score  float64
	}
	var cands []cand
	for i := range old {
		for j := range neo {
			oc, nc := alignmentCore(old[i]), alignmentCore(neo[j])
			// Match on the unwrapped core so ("a"|"b")? pairs with
			// ("b"|"a")? for reorder detection; the quantifier wrapper itself
			// is compared when matched pairs are diffed.
			s := similarity(oc, nc)
			// Terminals of the same kind are allowed to pair at zero content
			// overlap so an in-place literal/token edit is classified as a
			// token change rather than a remove plus an add.
			if s == 0 && sameTerminalKind(oc, nc) {
				s = 0.01
			}
			if s > 0 {
				cands = append(cands, cand{i, j, s})
			}
		}
	}
	sort.SliceStable(cands, func(a, b int) bool {
		if cands[a].score != cands[b].score {
			return cands[a].score > cands[b].score
		}
		if cands[a].oi != cands[b].oi {
			return cands[a].oi < cands[b].oi
		}
		return cands[a].ni < cands[b].ni
	})
	usedOld := map[int]bool{}
	usedNeo := map[int]bool{}
	for _, c := range cands {
		if usedOld[c.oi] || usedNeo[c.ni] {
			continue
		}
		usedOld[c.oi] = true
		usedNeo[c.ni] = true
		pairs = append(pairs, indexPair{c.oi, c.ni})
	}
	for i := range old {
		if !usedOld[i] {
			removedOld = append(removedOld, i)
		}
	}
	for j := range neo {
		if !usedNeo[j] {
			addedNeo = append(addedNeo, j)
		}
	}
	sort.Ints(removedOld)
	sort.Ints(addedNeo)
	return pairs, removedOld, addedNeo
}

// align performs a global (Needleman-Wunsch style) alignment that maximises
// cumulative similarity. Correspondences that changed in place are diffed, so
// a changed terminal is reported as a token/quantifier change rather than a
// remove plus an add; genuine gaps are reported as added/removed items.
// Alignment preserves position, so swapping two items surfaces through the
// resulting gap and pair trace.
func (d *exprDiffer) align(old, neo []*gExpr, path, noun string) {
	n, m := len(old), len(neo)
	scores := make([][]float64, n)
	for i := 0; i < n; i++ {
		scores[i] = make([]float64, m)
		for j := 0; j < m; j++ {
			scores[i][j] = similarity(alignmentCore(old[i]), alignmentCore(neo[j]))
		}
	}
	const scale = 1000
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := 1; i <= n; i++ {
		for j := 1; j <= m; j++ {
			diag := dp[i-1][j-1] + int(scores[i-1][j-1]*scale)
			dp[i][j] = max(diag, dp[i-1][j], dp[i][j-1])
		}
	}

	type pair struct{ oi, ni int }
	var (
		pairs      []pair
		removedOld []int
		addedNeo   []int
	)
	i, j := n, m
	for i > 0 || j > 0 {
		switch {
		case i > 0 && j > 0 && dp[i][j] == dp[i-1][j-1]+int(scores[i-1][j-1]*scale):
			pairs = append(pairs, pair{i - 1, j - 1})
			i--
			j--
		case i > 0 && dp[i][j] == dp[i-1][j]:
			removedOld = append(removedOld, i-1)
			i--
		default:
			addedNeo = append(addedNeo, j-1)
			j--
		}
	}

	for _, oi := range removedOld {
		d.add(terminalChangeKind(old[oi]), fmt.Sprintf("%s: %s removed %s", path, noun, describe(old[oi])))
	}
	for _, ni := range addedNeo {
		d.add(terminalChangeKind(neo[ni]), fmt.Sprintf("%s: %s added %s", path, noun, describe(neo[ni])))
	}
	// Traceback yields pairs in reverse; diff in ascending old-position order.
	sort.Slice(pairs, func(a, b int) bool { return pairs[a].oi < pairs[b].oi })
	for _, p := range pairs {
		childPath := fmt.Sprintf("%s[%d]", path, p.oi+1)
		d.diff(old[p.oi], neo[p.ni], childPath)
	}
}

// similarity returns a bounded [0,1] score of how closely two expressions
// correspond. Exact equality is 1; same-shape expressions (matching quantifier
// or terminal kind) get a partial score so an in-place terminal edit aligns
// and is classified by diff(); unrelated shapes score 0.
//
// alignmentCore strips a single unary wrapper (quantifier/negation/lookahead)
// so that, for example, ("a"|"b")? aligns with ("a"|"b")* by the underlying
// ordered choice while the quantifier change is still reported.
func alignmentCore(e *gExpr) *gExpr {
	switch e.kind {
	case exprQuant:
		return alignmentCore(e.quant.expr)
	case exprNeg:
		return alignmentCore(e.neg.expr)
	case exprLook:
		return alignmentCore(e.look.expr)
	}
	return e
}

func similarity(a, b *gExpr) float64 {
	if equalExpr(a, b) {
		return 1
	}
	if a.kind != b.kind {
		return 0
	}
	switch a.kind {
	case exprLiteral, exprToken:
		return 0.5
	case exprQuant:
		if a.quant.mode == b.quant.mode {
			return 0.6 + 0.4*similarity(a.quant.expr, b.quant.expr)
		}
		return 0.4 * similarity(a.quant.expr, b.quant.expr)
	case exprNeg:
		return 0.5 * similarity(a.neg.expr, b.neg.expr)
	case exprLook:
		s := 0.5 * similarity(a.look.expr, b.look.expr)
		if a.look.negative == b.look.negative {
			s += 0.25
		}
		return s
	case exprSeq, exprAlt:
		// Reward shared leaf content (e.g. two ordered choices that share most
		// branches but swap one) so an edited container aligns with its prior
		// version rather than being treated as remove plus add.
		s := 0.7 * leafOverlap(a, b)
		if len(a.children) == len(b.children) {
			s += 0.15
		}
		return s
	}
	return 0
}

// leafOverlap is the Jaccard similarity of the leaf (terminal/reference)
// descriptors contained within two expressions.
func leafOverlap(a, b *gExpr) float64 {
	ca := leafCounts(a)
	cb := leafCounts(b)
	if len(ca) == 0 || len(cb) == 0 {
		return 0
	}
	var inter, union int
	for k, va := range ca {
		vb := cb[k]
		if va < vb {
			inter += va
		} else {
			inter += vb
		}
	}
	for k, va := range ca {
		if vb, ok := cb[k]; ok {
			if va > vb {
				union += va
			} else {
				union += vb
			}
			continue
		}
		union += va
	}
	for k, vb := range cb {
		if _, ok := ca[k]; !ok {
			union += vb
		}
	}
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

func leafCounts(e *gExpr) map[string]int {
	out := map[string]int{}
	var walk func(*gExpr)
	walk = func(x *gExpr) {
		switch x.kind {
		case exprLiteral, exprToken, exprRef:
			out[describe(x)]++
		case exprQuant:
			walk(x.quant.expr)
		case exprNeg:
			walk(x.neg.expr)
		case exprLook:
			walk(x.look.expr)
		case exprSeq, exprAlt:
			for _, c := range x.children {
				walk(c)
			}
		}
	}
	walk(e)
	return out
}

func equalSequence(a, b []*gExpr) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !equalExpr(a[i], b[i]) {
			return false
		}
	}
	return true
}

// equalExpr is structural equality over the normalised tree. It is bounded by
// the single production depth because references are leaves.
func equalExpr(a, b *gExpr) bool {
	if a.kind != b.kind {
		return false
	}
	switch a.kind {
	case exprLiteral:
		return *a.literal == *b.literal
	case exprToken:
		return *a.token == *b.token
	case exprRef:
		return *a.ref == *b.ref
	case exprQuant:
		return a.quant.mode == b.quant.mode && equalExpr(a.quant.expr, b.quant.expr)
	case exprNeg:
		return equalExpr(a.neg.expr, b.neg.expr)
	case exprLook:
		return a.look.negative == b.look.negative && equalExpr(a.look.expr, b.look.expr)
	case exprSeq, exprAlt:
		return equalSequence(a.children, b.children)
	}
	return false
}

func lookSuffix(negative bool) string {
	if negative {
		return " (?!)"
	}
	return " (?=)"
}

func describeKind(e *gExpr) string {
	switch e.kind {
	case exprLiteral:
		return "literal"
	case exprToken:
		return "token"
	case exprRef:
		return "reference"
	case exprQuant:
		return "quantified " + e.quant.mode.String()
	case exprNeg:
		return "negation"
	case exprLook:
		return "lookahead"
	case exprSeq:
		return "sequence"
	case exprAlt:
		return "ordered choice"
	}
	return "unknown"
}

func describeLiteral(l *gLiteral) string {
	if l.tokenType == "" {
		return fmt.Sprintf("%q", l.value)
	}
	return fmt.Sprintf("%q:%s", l.value, l.tokenType)
}

func describeRef(r *gRef) string {
	return fmt.Sprintf("%s %s", r.kind, shortName(r.id))
}

// describe renders a compact, stable description of an expression for use in
// report lines. It is derived from the normalised tree, not from EBNF line
// rendering.
func describe(e *gExpr) string {
	switch e.kind {
	case exprLiteral:
		return describeLiteral(e.literal)
	case exprToken:
		return "<" + e.token.name + ">"
	case exprRef:
		return describeRef(e.ref)
	case exprQuant:
		return describe(e.quant.expr) + e.quant.mode.suffix()
	case exprNeg:
		return "~" + describe(e.neg.expr)
	case exprLook:
		return lookSuffix(e.look.negative) + describe(e.look.expr)
	case exprSeq:
		parts := make([]string, len(e.children))
		for i, c := range e.children {
			parts[i] = describe(c)
		}
		return joinGroup(parts, " ")
	case exprAlt:
		parts := make([]string, len(e.children))
		for i, c := range e.children {
			parts[i] = describe(c)
		}
		return joinGroup(parts, " | ")
	}
	return "?"
}

func joinGroup(parts []string, sep string) string {
	if len(parts) == 1 {
		return parts[0]
	}
	out := "("
	for i, p := range parts {
		if i > 0 {
			out += sep
		}
		out += p
	}
	return out + ")"
}

func (m groupMatchMode) suffix() string {
	switch m {
	case groupMatchZeroOrOne:
		return "?"
	case groupMatchZeroOrMore:
		return "*"
	case groupMatchOneOrMore:
		return "+"
	case groupMatchNonEmpty:
		return "!"
	default:
		return ""
	}
}

func dedupeSorted(in []string) []string {
	if len(in) < 2 {
		return in
	}
	out := in[:1]
	for _, s := range in[1:] {
		if s != out[len(out)-1] {
			out = append(out, s)
		}
	}
	return out
}
