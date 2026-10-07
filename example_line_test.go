package geohash_test

import (
	"fmt"

	"github.com/mmcloughlin/geohash"
)

func ExampleLine() {
	// A 20-degree segment at latitude 45 crossing the antimeridian.
	polyline := []geohash.Point{
		{Lat: 45, Lng: 170},
		{Lat: 45, Lng: -170},
	}

	// Enumerate every 2-character (10-bit) cell the line passes through, in
	// first-visit order. A limit of 0 means unlimited.
	cells, complete, err := geohash.Line(polyline, 2, 0)
	if err != nil {
		panic(err)
	}

	fmt.Println(complete)
	fmt.Println(cells)

	// A finite limit reports incompleteness instead of silently dropping
	// coverage.
	_, complete, _ = geohash.Line(polyline, 2, 1)
	fmt.Println(complete)

	// Output:
	// true
	// [xz zb 8p b0]
	// false
}
