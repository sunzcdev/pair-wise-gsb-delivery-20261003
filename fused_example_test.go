package decimal_test

import (
	"errors"
	"fmt"

	"github.com/shopspring/decimal"
)

// ExampleFusedMultiplyAdd shows the residual-preservation problem solved by
// fused multiply-add: two huge numbers cancel and leave a tiny balance. Rounding
// the product and the addend separately erases the balance; the fused operation
// keeps the intermediates exact and rounds only the final result.
func ExampleFusedMultiplyAdd() {
	x := decimal.NewFromInt(2)
	y := decimal.RequireFromString("500000000000000000.002")
	add := decimal.RequireFromString("-999999999999999999.998")

	// Naive ordering: round the multiplication, then round the addition.
	naive := x.Mul(y).Round(2).Add(add.Round(2)).Round(2)

	fused, err := decimal.FusedMultiplyAdd(x, y, add, decimal.FusedOptions{
		Precision: 2,
		Rounding:  decimal.RoundHalfUp,
	})
	if err != nil {
		panic(err)
	}

	fmt.Println("naive:", naive)
	fmt.Println("fused:", fused)
	// Output:
	// naive: 0
	// fused: 0.01
}

// ExampleDotProduct shows the same effect for a finite dot product whose large
// terms cancel and whose small terms decide the cent that survives. Rounding
// every product up front loses that cent; the fused dot product keeps it.
func ExampleDotProduct() {
	a := []decimal.Decimal{
		decimal.RequireFromString("1000000.004"),
		decimal.RequireFromString("999999.996"),
	}
	b := []decimal.Decimal{
		decimal.NewFromInt(1),
		decimal.NewFromInt(-1),
	}

	// Naive ordering: round each product, then sum and round.
	naive := decimal.Zero
	for i := range a {
		naive = naive.Add(a[i].Mul(b[i]).Round(2))
	}
	naive = naive.Round(2)

	fused, err := decimal.DotProduct(a, b, decimal.FusedOptions{
		Precision: 2,
		Rounding:  decimal.RoundHalfUp,
	})
	if err != nil {
		panic(err)
	}

	fmt.Println("naive:", naive)
	fmt.Println("fused:", fused)
	// Output:
	// naive: 0
	// fused: 0.01
}

// ExampleDotProduct_empty documents the empty / all-zero convention.
func ExampleDotProduct_empty() {
	empty, err := decimal.DotProduct(nil, nil, decimal.FusedOptions{
		Precision: 2,
		Rounding:  decimal.RoundHalfUp,
	})
	if err != nil {
		panic(err)
	}

	zeros, err := decimal.DotProduct(
		[]decimal.Decimal{decimal.NewFromInt(0), decimal.RequireFromString("1000000000000000000000000")},
		[]decimal.Decimal{decimal.RequireFromString("0.000000001"), decimal.NewFromInt(0)},
		decimal.FusedOptions{Precision: 2, Rounding: decimal.RoundHalfUp},
	)
	if err != nil {
		panic(err)
	}

	fmt.Println("empty:", empty)
	fmt.Println("zeros:", zeros)
	// Output:
	// empty: 0
	// zeros: 0
}

// ExampleFusedMultiplyAdd_budget documents the configurable computation budget.
// Aligning exponents here would require more decimal digits than allowed, so
// the operation returns an error instead of falling back to float64.
func ExampleFusedMultiplyAdd_budget() {
	x := decimal.New(1, 100000) // 1 * 10^100000
	y := decimal.NewFromInt(1)
	add := decimal.New(1, -100000) // 1 * 10^-100000

	_, err := decimal.FusedMultiplyAdd(x, y, add, decimal.FusedOptions{
		Precision: 2,
		Rounding:  decimal.RoundHalfUp,
		MaxDigits: 1000,
	})
	fmt.Println(errors.Is(err, decimal.ErrFusedBudgetExceeded))
	// Output: true
}
