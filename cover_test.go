package geohash

import (
	"reflect"
	"sort"
	"strings"
	"testing"
)

type testInterval struct {
	min float64
	max float64
}

func coverTestIntervals(min, max float64) []testInterval {
	if min == max && min == 180 {
		return []testInterval{{min: -180, max: -180}}
	}
	if min == 180 {
		min = -180
	}
	if max == -180 {
		max = 180
	}
	if min <= max {
		return []testInterval{{min: min, max: max}}
	}
	return []testInterval{
		{min: min, max: 180},
		{min: -180, max: max},
	}
}

func coverTestIntervalOverlaps(cellMin, cellMax, queryMin, queryMax, upperBound float64) bool {
	if queryMin < queryMax {
		return cellMin < queryMax && cellMax > queryMin
	}

	if queryMin == upperBound {
		return cellMin <= queryMin && cellMax == queryMin
	}
	return cellMin <= queryMin && queryMin < cellMax
}

func coverTestIntersects(b Box, query Box) bool {
	lngIntervals := coverTestIntervals(query.MinLng, query.MaxLng)
	if !coverTestIntervalOverlaps(b.MinLat, b.MaxLat, query.MinLat, query.MaxLat, 90) {
		return false
	}
	for _, interval := range lngIntervals {
		if coverTestIntervalOverlaps(b.MinLng, b.MaxLng, interval.min, interval.max, 180) {
			return true
		}
	}
	return false
}

func coverTestFineBoxes(hash string, precision uint, query Box) []Box {
	if uint(len(hash)) == precision {
		box := BoundingBox(hash)
		if coverTestIntersects(box, query) {
			return []Box{box}
		}
		return nil
	}

	var boxes []Box
	for _, char := range base32encoding.encode {
		child := hash + string(char)
		if coverTestIntersects(BoundingBox(child), query) {
			boxes = append(boxes, coverTestFineBoxes(child, precision, query)...)
		}
	}
	return boxes
}

func assertCover(t *testing.T, query Box, precision uint, budget int, expected []string) []string {
	t.Helper()

	cover, err := query.Cover(precision, budget)
	if err != nil {
		t.Fatalf("Cover returned error: %v", err)
	}
	if expected != nil && !reflect.DeepEqual(cover, expected) {
		t.Fatalf("Cover = %q, want %q", cover, expected)
	}
	if len(cover) > budget {
		t.Fatalf("Cover has %d prefixes, budget %d", len(cover), budget)
	}
	if !sort.StringsAreSorted(cover) {
		t.Fatalf("Cover is not sorted: %q", cover)
	}

	seen := make(map[string]bool)
	for _, prefix := range cover {
		if seen[prefix] {
			t.Fatalf("Cover contains duplicate prefix %q", prefix)
		}
		seen[prefix] = true

		for _, other := range cover {
			if prefix != other && strings.HasPrefix(other, prefix) {
				t.Fatalf("Cover contains parent %q and descendant %q", prefix, other)
			}
		}

		box := BoundingBox(prefix)
		if prefix != "" && !coverTestIntersects(box, query) {
			t.Fatalf("prefix %q does not intersect the query", prefix)
		}
	}

	for _, fineBox := range coverTestFineBoxes("", precision, query) {
		fineCenterHash := func() string {
			lat := (fineBox.MinLat + fineBox.MaxLat) / 2
			lng := (fineBox.MinLng + fineBox.MaxLng) / 2
			return EncodeWithPrecision(lat, lng, precision)
		}()

		covered := false
		for _, prefix := range cover {
			if strings.HasPrefix(fineCenterHash, prefix) {
				covered = true
				break
			}
		}
		if !covered {
			t.Fatalf("fine cell %s with bounds %+v is not covered by %q", fineCenterHash, fineBox, cover)
		}
	}

	return cover
}

func TestCoverExactCellIsNotSubdivided(t *testing.T) {
	query := BoundingBox("00")
	assertCover(t, query, 4, 100, []string{"00"})
}

func TestCoverWorld(t *testing.T) {
	query := Box{MinLat: -90, MaxLat: 90, MinLng: -180, MaxLng: 180}
	assertCover(t, query, 3, 1, []string{""})
}

func TestCoverWorldLongitudeInterval(t *testing.T) {
	query := Box{MinLat: -10, MaxLat: 10, MinLng: 180, MaxLng: -180}
	assertCover(t, query, 3, 512, nil)
}

func TestCoverNearPole(t *testing.T) {
	query := Box{MinLat: 89.9, MaxLat: 90, MinLng: -180, MaxLng: 180}
	assertCover(t, query, 3, 64, nil)
}

func TestCoverCrossesAntimeridian(t *testing.T) {
	query := Box{MinLat: -10, MaxLat: 10, MinLng: 170, MaxLng: -170}
	assertCover(t, query, 3, 512, nil)
	assertCover(t, query, 1, 1, []string{""})
}

func TestCoverFullCellRemainsCoarse(t *testing.T) {
	full := BoundingBox("00")
	query := Box{
		MinLat: full.MinLat,
		MaxLat: full.MaxLat + 0.1,
		MinLng: full.MinLng,
		MaxLng: full.MaxLng,
	}
	cover := assertCover(t, query, 3, 128, nil)
	foundFull := false
	hasFinerPrefix := false
	for _, prefix := range cover {
		if strings.HasPrefix(prefix, "00") && prefix != "00" {
			t.Fatalf("fully contained cell was subdivided into %q", prefix)
		}
		if prefix == "00" {
			foundFull = true
		}
		if len(prefix) == 3 {
			hasFinerPrefix = true
		}
	}
	if !foundFull {
		t.Fatalf("expected complete cell 00 in cover %q", cover)
	}
	if !hasFinerPrefix {
		t.Fatalf("expected mixed precision cover %q", cover)
	}
}

func TestCoverDateLinePoint(t *testing.T) {
	query := Box{MinLat: 0, MaxLat: 0, MinLng: 180, MaxLng: 180}
	cover := assertCover(t, query, 4, 1, nil)
	expected := EncodeWithPrecision(0, -180, 4)
	if len(cover) != 1 || cover[0] != expected {
		t.Fatalf("Cover = %q, want [%s]", cover, expected)
	}
}

func TestCoverVeryNarrowRectangle(t *testing.T) {
	cell := BoundingBox(EncodeWithPrecision(0, 0, 8))
	query := Box{
		MinLat: cell.MaxLat - 1e-9,
		MaxLat: cell.MaxLat + 1e-9,
		MinLng: cell.MaxLng - 1e-9,
		MaxLng: cell.MaxLng + 1e-9,
	}
	assertCover(t, query, 8, 4, nil)
}

func TestCoverBudgetOneFallsBackToParent(t *testing.T) {
	query := Box{
		MinLat: -1e-9,
		MaxLat: 1e-9,
		MinLng: -1e-9,
		MaxLng: 1e-9,
	}
	assertCover(t, query, 8, 1, []string{""})
}

func TestCoverStableOrder(t *testing.T) {
	query := Box{MinLat: -10, MaxLat: 10, MinLng: 170, MaxLng: -170}
	first, err := query.Cover(3, 512)
	if err != nil {
		t.Fatalf("Cover returned error: %v", err)
	}
	second, err := query.Cover(3, 512)
	if err != nil {
		t.Fatalf("Cover returned error: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("Cover order is not stable: %q then %q", first, second)
	}
}

func TestCoverInvalidArguments(t *testing.T) {
	query := Box{MinLat: -10, MaxLat: 10, MinLng: -10, MaxLng: 10}
	cases := []struct {
		name      string
		box       Box
		precision uint
		budget    int
	}{
		{name: "zero precision", box: query, precision: 0, budget: 1},
		{name: "precision too large", box: query, precision: 13, budget: 1},
		{name: "zero budget", box: query, precision: 1, budget: 0},
		{name: "negative budget", box: query, precision: 1, budget: -1},
		{name: "latitude below range", box: Box{MinLat: -91, MaxLat: 10, MinLng: -10, MaxLng: 10}, precision: 1, budget: 1},
		{name: "latitude above range", box: Box{MinLat: -10, MaxLat: 91, MinLng: -10, MaxLng: 10}, precision: 1, budget: 1},
		{name: "longitude below range", box: Box{MinLat: -10, MaxLat: 10, MinLng: -181, MaxLng: 10}, precision: 1, budget: 1},
		{name: "longitude above range", box: Box{MinLat: -10, MaxLat: 10, MinLng: -10, MaxLng: 181}, precision: 1, budget: 1},
		{name: "inverted latitude", box: Box{MinLat: 10, MaxLat: -10, MinLng: -10, MaxLng: 10}, precision: 1, budget: 1},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := c.box.Cover(c.precision, c.budget); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}
