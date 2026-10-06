package decimal

import (
	"math/big"
	"strings"
	"testing"
)

// Exact audit primitive: re-derive the rounding decision independently of the
// Sqrt implementation, using only integer arithmetic. It returns:
//   - r:    coefficient of floor(sqrt(d)) at scale 10^-precision
//   - exact: whether sqrt(d) == r * 10^-precision
//   - cmp:  sign of ((2r+1)*10^g)^2 - 4*N*10^(2g-ish) as computed below
//
// Concretely it compares the exact root with the midpoint r + 1/2:
//
//	cmp > 0  root is strictly below the midpoint (round-down side)
//	cmp == 0 root sits exactly on the midpoint (a genuine tie)
//	cmp < 0  root is strictly above the midpoint (round-up side)
func auditSqrt(t *testing.T, d Decimal, precision int64) (r *big.Int, exact bool, cmp int) {
	t.Helper()
	v0 := new(big.Int).Set(d.getValue())
	e := int64(d.exp) + 2*precision
	k := e
	if k < 0 {
		k = -k
	}
	g := int64(0)
	if e < 0 {
		g = -e
	}
	n := new(big.Int).Mul(v0, new(big.Int).Exp(tenInt, big.NewInt(k), nil))
	q := auditSqrtFloor(n)
	r = new(big.Int).Quo(q, new(big.Int).Exp(tenInt, big.NewInt(g), nil))
	scale := new(big.Int).Exp(tenInt, big.NewInt(2*g), nil)

	exact = new(big.Int).Mul(new(big.Int).Mul(new(big.Int).Set(r), r), scale).Cmp(n) == 0

	mid := new(big.Int).Add(new(big.Int).Lsh(new(big.Int).Set(r), 1), oneInt)
	mid.Mul(mid, mid)
	mid.Mul(mid, scale)
	fourN := new(big.Int).Lsh(n, 2)
	cmp = mid.Cmp(fourN)
	return r, exact, cmp
}

func auditSqrtFloor(n *big.Int) *big.Int {
	if n.Sign() <= 0 {
		return new(big.Int)
	}
	x := new(big.Int).Lsh(oneInt, uint((n.BitLen()+1)/2))
	for {
		y := new(big.Int).Quo(new(big.Int).Add(x, new(big.Int).Quo(new(big.Int).Set(n), x)), twoInt)
		if y.Cmp(x) >= 0 {
			return x
		}
		x = y
	}
}

// assertSqrt runs Sqrt and independently proves the result is the correctly
// rounded grid neighbour: the returned coefficient res must equal r or r+1,
// and the choice must agree with the exact midpoint comparison.
func assertSqrt(t *testing.T, radicand string, precision int32, mode SqrtRoundingMode, want string) {
	t.Helper()
	d, err := NewFromString(radicand)
	if err != nil {
		t.Fatalf("bad radicand %q: %v", radicand, err)
	}
	got, err := d.Sqrt(precision, mode)
	if err != nil {
		t.Fatalf("Sqrt(%s, %d, %d) returned error: %v", radicand, precision, mode, err)
	}
	gotStr := got.StringFixed(precision)
	if gotStr != want {
		t.Fatalf("Sqrt(%s, %d, mode=%d) = %s, want %s", radicand, precision, mode, got.String(), want)
	}

	r, exact, cmp := auditSqrt(t, d, int64(precision))
	res := got.getValue()
	if res.Sign() < 0 {
		t.Fatalf("square root must be non-negative, got %s", got)
	}
	diff := new(big.Int).Sub(res, r)
	if diff.Sign() < 0 || diff.Cmp(oneInt) > 0 {
		t.Fatalf("result %s is not an adjacent grid neighbour of floor %s", got, r)
	}

	// Independent proof of which neighbour the mode must select.
	var expectUp bool
	switch mode {
	case SqrtRoundToZero, SqrtRoundFloor:
		expectUp = false
	case SqrtRoundAwayFromZero, SqrtRoundCeil:
		expectUp = !exact
	case SqrtRoundHalfUp:
		expectUp = !exact && cmp <= 0
	case SqrtRoundHalfEven:
		expectUp = !exact && (cmp < 0 || cmp == 0 && r.Bit(0) != 0)
	default:
		t.Fatalf("unknown mode %d", mode)
	}
	if (diff.Sign() == 1) != expectUp {
		t.Fatalf("rounding choice for %s mode=%d not justified by midpoint comparison (cmp=%d exact=%v)",
			radicand, mode, cmp, exact)
	}

	// Direct certificate on a common integer scale: move the radicand and the
	// squares of both grid neighbours onto the coefficient axis of
	// 10^(-2*precision), then require
	//
	//	res^2 <= radicand_scaled < (res+1)^2   when res is the floor, and
	//	(res-1)^2 < radicand_scaled <= res^2   when res is the ceiling.
	e := int64(d.exp) + 2*int64(precision)
	k := e
	if k < 0 {
		k = -k
	}
	g := int64(0)
	if e < 0 {
		g = -e
	}
	target := new(big.Int).Mul(new(big.Int).Set(d.getValue()),
		new(big.Int).Exp(tenInt, big.NewInt(k), nil)) // radicand * 10^(2g)
	scale2 := new(big.Int).Exp(tenInt, big.NewInt(2*g), nil)

	resSq := new(big.Int).Mul(new(big.Int).Set(res), res)
	if expectUp {
		if new(big.Int).Mul(resSq, scale2).Cmp(target) < 0 {
			t.Fatalf("answer %s squared is still below the radicand despite rounding up", got)
		}
		below := new(big.Int).Sub(res, oneInt)
		belowSq := new(big.Int).Mul(below, below)
		if new(big.Int).Mul(belowSq, scale2).Cmp(target) >= 0 {
			t.Fatalf("answer %s rounded up but floor neighbour still brackets the radicand", got)
		}
	} else {
		if new(big.Int).Mul(resSq, scale2).Cmp(target) > 0 {
			t.Fatalf("answer %s rounded down but its square exceeds the radicand", got)
		}
	}
	if !expectUp && !exact {
		above := new(big.Int).Add(res, oneInt)
		aboveSq := new(big.Int).Mul(above, above)
		if new(big.Int).Mul(aboveSq, scale2).Cmp(target) <= 0 {
			t.Fatalf("answer %s rounded down but radicand still reaches the upper neighbour", got)
		}
	}
}

func TestSqrtExactSquares(t *testing.T) {
	assertSqrt(t, "9", 0, SqrtRoundHalfEven, "3")
	assertSqrt(t, "0", 5, SqrtRoundHalfEven, "0.00000")
	assertSqrt(t, "0.25", 1, SqrtRoundHalfUp, "0.5")
	assertSqrt(t, "0.0000000001", 6, SqrtRoundHalfEven, "0.000010")
	assertSqrt(t, "144", 2, SqrtRoundToZero, "12.00")
	assertSqrt(t, "20022.25", 2, SqrtRoundHalfEven, "141.50")
}

func TestSqrtAllModesVsMidpoint(t *testing.T) {
	// sqrt(2) = 1.41421356..., at 2 places: below the 1.415 midpoint (cmp>0).
	assertSqrt(t, "2", 2, SqrtRoundToZero, "1.41")
	assertSqrt(t, "2", 2, SqrtRoundFloor, "1.41")
	assertSqrt(t, "2", 2, SqrtRoundCeil, "1.42")
	assertSqrt(t, "2", 2, SqrtRoundAwayFromZero, "1.42")
	assertSqrt(t, "2", 2, SqrtRoundHalfUp, "1.41")
	assertSqrt(t, "2", 2, SqrtRoundHalfEven, "1.41")

	// Exact tie, floor digit r=141 (odd): sqrt(2.002225) = 141.5 exactly.
	assertSqrt(t, "2.002225", 2, SqrtRoundToZero, "1.41")
	assertSqrt(t, "2.002225", 2, SqrtRoundFloor, "1.41")
	assertSqrt(t, "2.002225", 2, SqrtRoundCeil, "1.42")
	assertSqrt(t, "2.002225", 2, SqrtRoundAwayFromZero, "1.42")
	assertSqrt(t, "2.002225", 2, SqrtRoundHalfUp, "1.42")
	assertSqrt(t, "2.002225", 2, SqrtRoundHalfEven, "1.42") // odd -> up

	// Exact tie, floor digit r=142 (even): sqrt(2.030625) = 142.5 exactly.
	assertSqrt(t, "2.030625", 2, SqrtRoundHalfUp, "1.43")
	assertSqrt(t, "2.030625", 2, SqrtRoundHalfEven, "1.42") // even -> stay

	// One unit below the exact tie radicand: root just under 141.5.
	assertSqrt(t, "2.002224", 2, SqrtRoundHalfUp, "1.41")
	assertSqrt(t, "2.002224", 2, SqrtRoundHalfEven, "1.41")
	assertSqrt(t, "2.002224", 2, SqrtRoundCeil, "1.42")
	assertSqrt(t, "2.002224", 2, SqrtRoundToZero, "1.41")

	// One unit above the exact tie radicand: root just over 141.5.
	assertSqrt(t, "2.002226", 2, SqrtRoundHalfUp, "1.42")
	assertSqrt(t, "2.002226", 2, SqrtRoundHalfEven, "1.42")
	assertSqrt(t, "2.002226", 2, SqrtRoundFloor, "1.41")
}

func TestSqrtErrors(t *testing.T) {
	neg := RequireFromString("-4")
	if _, err := neg.Sqrt(2, SqrtRoundHalfEven); err == nil {
		t.Fatal("expected error for negative radicand")
	}
	if _, err := RequireFromString("2").Sqrt(-1, SqrtRoundHalfEven); err == nil {
		t.Fatal("expected error for negative precision")
	}
	if _, err := RequireFromString("2").Sqrt(SqrtMaxPrecision+1, SqrtRoundHalfEven); err == nil {
		t.Fatal("expected error for precision above limit")
	}
	if _, err := New(1, SqrtMaxExponent+1).Sqrt(0, SqrtRoundHalfEven); err == nil {
		t.Fatal("expected error for exponent above limit")
	}
	if _, err := RequireFromString("2").Sqrt(2, SqrtRoundingMode(99)); err == nil {
		t.Fatal("expected error for unknown rounding mode")
	}
	big := "1" + strings.Repeat("0", SqrtMaxCoeffDigits) // SqrtMaxCoeffDigits+1 digits
	if _, err := RequireFromString(big).Sqrt(0, SqrtRoundHalfEven); err == nil {
		t.Fatal("expected error for over-long coefficient")
	}
}

func TestSqrtHundredsOfDigits(t *testing.T) {
	// radicand = ROOT^2 exactly, 420 coefficient digits; root has 180 integer
	// digits and 30 fractional digits. Every mode must reproduce the exact root.
	rootCoeff := "889722182779863926788685577268324867546217351299752891588188683711863729145216141667931695771294134965454744235251291322423968563274117941756641526111748198647655396397426432355294761838185332782983782458661765"
	radCoeff := "791605562530565593819222913744475539234292288376470292637470594225498621078938417222977834770590611856835562218451599220322947132547756224997023914435190961569838355811881554498632545756240367618020875781236295922106077163254128996982764691505773981951763719619200457356759302434465021008120554572404706159366281620564637299112123725909112801785897468289874955184404707314173321118676265548309137734931047361074672915225"
	rad := NewFromBigInt(mustBigInt(t, radCoeff), -60)
	want := NewFromBigInt(mustBigInt(t, rootCoeff), -30)
	for _, mode := range []SqrtRoundingMode{
		SqrtRoundToZero, SqrtRoundFloor, SqrtRoundCeil,
		SqrtRoundAwayFromZero, SqrtRoundHalfUp, SqrtRoundHalfEven,
	} {
		got, err := rad.Sqrt(30, mode)
		if err != nil {
			t.Fatalf("mode %d: %v", mode, err)
		}
		if got.Cmp(want) != 0 {
			t.Fatalf("mode %d: 420-digit perfect square mismatch\n got %s\nwant %s", mode, got, want)
		}
	}

	// 400-digit non-square radicand: audit each mode against the independently
	// computed floor and exact midpoint comparison.
	rad400 := NewFromBigInt(mustBigInt(t, "8897221827798639267886855772683248675462173512997528915881886837118637291452161416679316957712941349654547442352512913224239685632741179417566415261117481986476553963974264323552947618381853327829837824586617655852954459644294271546464867451953323825167867521976284755772687954669325167961938365123648633442373194265177838881693974132853517375543218431585524154931255291822554346278696422612596987689"), 0)
	r, exact, cmp := auditSqrt(t, rad400, 60)
	if exact {
		t.Fatal("400-digit pseudo-random radicand unexpectedly a perfect square")
	}
	modes := map[SqrtRoundingMode]bool{
		SqrtRoundToZero:       false,
		SqrtRoundFloor:        false,
		SqrtRoundAwayFromZero: true,
		SqrtRoundCeil:         true,
		SqrtRoundHalfUp:       cmp <= 0,
		SqrtRoundHalfEven:     cmp < 0 || cmp == 0 && r.Bit(0) != 0,
	}
	for mode, up := range modes {
		got, err := rad400.Sqrt(60, mode)
		if err != nil {
			t.Fatalf("mode %d: %v", mode, err)
		}
		expect := new(big.Int).Set(r)
		if up {
			expect.Add(expect, oneInt)
		}
		if got.getValue().Cmp(expect) != 0 {
			t.Fatalf("mode %d: got coeff %s want %s (cmp=%d)", mode, got.getValue(), expect, cmp)
		}
	}
}

func mustBigInt(t *testing.T, s string) *big.Int {
	t.Helper()
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		t.Fatalf("bad big int")
	}
	return v
}

func BenchmarkSqrtPrecision1000(b *testing.B) {
	d := RequireFromString("2")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := d.Sqrt(1000, SqrtRoundHalfEven); err != nil {
			b.Fatal(err)
		}
	}
}

// TestSqrtRandomized drives hundreds of radicands through every mode and
// proves every last digit against the independently computed floor and exact
// midpoint comparison. The LCG generator is fixed so the test is
// deterministic.
func TestSqrtRandomized(t *testing.T) {
	state := uint64(0x9e3779b97f4a7c15)
	next := func(mod uint64) uint64 {
		state = state*6364136223846793005 + 1442695040888963407
		return state % mod
	}
	modes := []SqrtRoundingMode{
		SqrtRoundToZero, SqrtRoundFloor, SqrtRoundCeil,
		SqrtRoundAwayFromZero, SqrtRoundHalfUp, SqrtRoundHalfEven,
	}
	for iter := 0; iter < 400; iter++ {
		ndig := 1 + next(120)
		var b strings.Builder
		b.WriteByte(byte('1' + next(9)))
		for i := uint64(1); i < ndig; i++ {
			b.WriteByte(byte('0' + next(10)))
		}
		coeff := b.String()
		exp := int32(next(41)) - 20
		d := NewFromBigInt(mustBigInt(t, coeff), exp)
		precision := int32(next(12))

		r, exact, cmp := auditSqrt(t, d, int64(precision))
		for _, mode := range modes {
			got, err := d.Sqrt(precision, mode)
			if err != nil {
				t.Fatalf("iter %d mode %d: %v", iter, mode, err)
			}
			up := false
			switch mode {
			case SqrtRoundCeil, SqrtRoundAwayFromZero:
				up = !exact
			case SqrtRoundHalfUp:
				up = !exact && cmp <= 0
			case SqrtRoundHalfEven:
				up = !exact && (cmp < 0 || cmp == 0 && r.Bit(0) != 0)
			}
			want := new(big.Int).Set(r)
			if up {
				want.Add(want, oneInt)
			}
			if got.getValue().Cmp(want) != 0 || got.exp != -precision {
				t.Fatalf("iter %d d=%s p=%d mode=%d: got %s, want coeff %s exp -%d (exact=%v cmp=%d)",
					iter, d, precision, mode, got, want, precision, exact, cmp)
			}
		}
	}
}
