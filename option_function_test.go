package gojq_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"strconv"

	"github.com/itchyny/gojq"
)

func toFloat(x any) (float64, bool) {
	switch x := x.(type) {
	case int:
		return float64(x), true
	case float64:
		return x, true
	case *big.Int:
		f, err := strconv.ParseFloat(x.String(), 64)
		return f, err == nil
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	default:
		return 0.0, false
	}
}

func ExampleWithFunction() {
	query, err := gojq.Parse(".[] | f | f(3)")
	if err != nil {
		log.Fatalln(err)
	}
	code, err := gojq.Compile(
		query,
		gojq.WithFunction("f", 0, 1, func(x any, xs []any) any {
			if x, ok := toFloat(x); ok {
				if len(xs) == 1 {
					if y, ok := toFloat(xs[0]); ok {
						x *= y
					} else {
						return fmt.Errorf("f cannot be applied to: %v, %v", x, xs)
					}
				} else {
					x += 2
				}
				return x
			}
			return fmt.Errorf("f cannot be applied to: %v, %v", x, xs)
		}),
	)
	if err != nil {
		log.Fatalln(err)
	}
	input := []any{0, 1, 2.5, json.Number("10000000000000000000000000000000000000000")}
	iter := code.Run(input)
	for {
		v, ok := iter.Next()
		if !ok {
			break
		}
		if err, ok := v.(error); ok {
			log.Fatalln(err)
		}
		fmt.Printf("%#v\n", v)
	}

	// Output:
	// 6
	// 9
	// 13.5
	// 3e+40
}

func ExampleWithFunctionWithContext() {
	query, err := gojq.Parse("map(f)")
	if err != nil {
		log.Fatalln(err)
	}
	code, err := gojq.Compile(
		query,
		gojq.WithFunctionWithContext("f", 0, 0, func(ctx context.Context, v any, _ []any) any {
			if x, ok := v.(int); ok {
				// The function receives the context of the current run, so
				// long-running functions can cooperatively respond to cancellation.
				select {
				case <-ctx.Done():
					return ctx.Err()
				default:
				}
				return x * 2
			}
			return fmt.Errorf("f cannot be applied to: %v", v)
		}),
	)
	if err != nil {
		log.Fatalln(err)
	}
	iter := code.RunWithContext(context.Background(), []any{1, 2, 3})
	for {
		v, ok := iter.Next()
		if !ok {
			break
		}
		if err, ok := v.(error); ok {
			log.Fatalln(err)
		}
		fmt.Println(v)
	}

	// Output:
	// [2 4 6]
}
