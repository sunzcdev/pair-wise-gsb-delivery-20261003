package geohash_test

import (
	"fmt"

	"github.com/mmcloughlin/geohash"
)

func ExampleTraversePolyline() {
	// A short segment crossing the 180th meridian at 1-character precision.
	// Encoding only the two endpoints would lose cells crossed in between;
	// the traversal reports every intersected cell in first-contact order.
	polyline := []geohash.TraversePoint{
		{Lat: 10, Lng: 170},
		{Lat: 10, Lng: -170},
	}

	cells, truncated, err := geohash.TraversePolylineString(polyline, 1, 0)
	if err != nil {
		panic(err)
	}
	fmt.Println(cells)
	fmt.Println(truncated)

	// A finite cap returns an explicit incomplete status instead of silently
	// dropping cells.
	limited, truncated, err := geohash.TraversePolylineString(polyline, 1, 1)
	if err != nil {
		panic(err)
	}
	fmt.Println(limited)
	fmt.Println(truncated)
	// Output:
	// [x 8]
	// false
	// [x]
	// true
}
