package realroots

import (
	"math/big"
	"testing"
)

func rat(s string) *big.Rat {
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		panic("bad rat " + s)
	}
	return r
}

func rp(coeffs ...string) ratPoly {
	p := make(ratPoly, len(coeffs))
	for i, c := range coeffs {
		p[i] = rat(c)
	}
	return p
}

func TestEvalAndArithmetic(t *testing.T) {
	// (x-1)(x+2) = x^2 + x - 2
	p := rp("-2", "1", "1")
	if p.eval(rat("1")).Sign() != 0 {
		t.Fatal("p(1) should be 0")
	}
	if p.eval(rat("-2")).Sign() != 0 {
		t.Fatal("p(-2) should be 0")
	}
	if p.eval(rat("2")).Cmp(rat("4")) != 0 {
		t.Fatal("p(2) should be 4")
	}

	a := rp("1", "1")  // x+1
	b := rp("-1", "1") // x-1
	prod := a.mul(b)
	if prod.degree() != 2 || prod[0].Cmp(rat("-1")) != 0 || prod[2].Cmp(rat("1")) != 0 {
		t.Fatalf("bad product %v", prod)
	}
	q, rem := prod.divmod(a)
	if q.degree() != 1 || rem.degree() >= 0 {
		t.Fatalf("divmod q=%v rem=%v", q, rem)
	}
}

func TestGcd(t *testing.T) {
	// gcd((x-1)^2 (x+2), (x-1)(x+3)) = x-1
	xm1 := rp("-1", "1")
	xp2 := rp("2", "1")
	xp3 := rp("3", "1")
	f := xm1.mul(xm1).mul(xp2)
	g := xm1.mul(xp3)
	d := ratPolyGcd(f, g)
	if d.degree() != 1 || d[0].Cmp(rat("-1")) != 0 || d[1].Cmp(rat("1")) != 0 {
		t.Fatalf("gcd = %v", d)
	}
}

func TestDivideByLinear(t *testing.T) {
	// (x-1)(x+2) = x^2 + x - 2 ; divided by (x-1) -> x+2.
	p := rp("-2", "1", "1")
	q := p.divideByLinear(rat("1"))
	want := rp("2", "1")
	if q.degree() != want.degree() {
		t.Fatalf("degree %d", q.degree())
	}
	for i := range want {
		if q[i].Cmp(want[i]) != 0 {
			t.Fatalf("got %v want %v", q, want)
		}
	}

	// (x-3)(x-5)(x-7) = x^3 - 15x^2 + 71x - 105; divided by (x-5) ->
	// x^2 - 10x + 21.
	cubic := rp("-105", "71", "-15", "1")
	q2 := cubic.divideByLinear(rat("5"))
	want2 := rp("21", "-10", "1")
	for i := range want2 {
		if q2[i].Cmp(want2[i]) != 0 {
			t.Fatalf("got %v want %v", q2, want2)
		}
	}
}

func TestSquareFreeYun(t *testing.T) {
	type want struct {
		roots []string
		mult  int
	}
	cases := []struct {
		name    string
		p       ratPoly
		factors []want
	}{
		{
			name:    "linear square free",
			p:       rp("-3", "1"), // x-3
			factors: []want{{[]string{"3"}, 1}},
		},
		{
			name:    "square free quadratic",
			p:       rp("2", "-3", "1"), // (x-1)(x-2)
			factors: []want{{[]string{"1", "2"}, 1}},
		},
		{
			name:    "double then single",
			p:       rp("2", "-3", "0", "1"), // (x-1)^2 (x+2)
			factors: []want{{[]string{"-2"}, 1}, {[]string{"1"}, 2}},
		},
		{
			name:    "triple then double",
			p:       rp("-4", "8", "-1", "-5", "1", "1"),
			factors: []want{{[]string{"-2"}, 2}, {[]string{"1"}, 3}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := squareFreeYun(c.p)
			if len(got) != len(c.factors) {
				t.Fatalf("factor count: got %+v want %+v", got, c.factors)
			}
			for i, sf := range got {
				if sf.multiplicity != c.factors[i].mult {
					t.Fatalf("factor %d mult = %d want %d", i, sf.multiplicity, c.factors[i].mult)
				}
				if sf.factor.degree() != len(c.factors[i].roots) {
					t.Fatalf("factor %d degree = %d", i, sf.factor.degree())
				}
				for _, r := range c.factors[i].roots {
					if sf.factor.eval(rat(r)).Sign() != 0 {
						t.Fatalf("factor %+v does not vanish at %s", sf.factor, r)
					}
				}
			}
		})
	}
}
