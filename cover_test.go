package geohash

import (
	"reflect"
	"sort"
	"strings"
	"testing"
)

// verifyCover checks the invariants of a CoverBox result: it respects the
// cell budget and maximum precision, is sorted, contains no ancestor/descendant
// pairs, consists only of cells intersecting the query, and its union fully
// covers the query box. Coverage is established exactly from cell boundaries:
// the query is partitioned along every cell edge, and the midpoint of each
// resulting elementary rectangle must lie inside some covering cell.
func verifyCover(t *testing.T, query Box, maxChars uint, maxCells int, cover []string) {
	t.Helper()

	if len(cover) == 0 {
		t.Fatal("empty cover")
	}
	if len(cover) > maxCells {
		t.Fatalf("cover has %d cells, exceeds budget %d", len(cover), maxCells)
	}
	if !sort.StringsAreSorted(cover) {
		t.Fatalf("cover not in sorted order: %v", cover)
	}
	for i, a := range cover {
		if uint(len(a)) > maxChars {
			t.Fatalf("cell %q exceeds maximum precision %d", a, maxChars)
		}
		for _, b := range cover[i+1:] {
			if strings.HasPrefix(b, a) {
				t.Fatalf("cover contains both %q and its descendant %q", a, b)
			}
		}
	}

	regions := splitBox(query)
	cells := make([]Box, len(cover))
	for i, h := range cover {
		cells[i] = BoundingBox(h)
		intersects := false
		for _, r := range regions {
			if boxesOverlap(cells[i], r) {
				intersects = true
				break
			}
		}
		if !intersects {
			t.Fatalf("cell %q does not intersect the query box", h)
		}
	}

	for _, r := range regions {
		verifyRegionCovered(t, r, cells)
	}
}

func boxesOverlap(a, b Box) bool {
	return a.MinLat <= b.MaxLat && b.MinLat <= a.MaxLat &&
		a.MinLng <= b.MaxLng && b.MinLng <= a.MaxLng
}

// verifyRegionCovered confirms that the union of cells covers the box r, by
// testing the midpoint of every elementary rectangle formed by the cell
// boundaries clipped to r.
func verifyRegionCovered(t *testing.T, r Box, cells []Box) {
	t.Helper()

	lats := boundaryCoords(r.MinLat, r.MaxLat, cells,
		func(b Box) (float64, float64) { return b.MinLat, b.MaxLat })
	lngs := boundaryCoords(r.MinLng, r.MaxLng, cells,
		func(b Box) (float64, float64) { return b.MinLng, b.MaxLng })

	for i := 0; i+1 < len(lats); i++ {
		for j := 0; j+1 < len(lngs); j++ {
			if lats[i] == lats[i+1] || lngs[j] == lngs[j+1] {
				continue
			}
			lat := (lats[i] + lats[i+1]) / 2
			lng := (lngs[j] + lngs[j+1]) / 2
			covered := false
			for _, c := range cells {
				if c.Contains(lat, lng) {
					covered = true
					break
				}
			}
			if !covered {
				t.Fatalf("query point (lat=%v, lng=%v) is not covered", lat, lng)
			}
		}
	}
}

// boundaryCoords returns the sorted distinct coordinates of all cell edges
// clipped to the interval [lo, hi], including the interval endpoints.
func boundaryCoords(lo, hi float64, cells []Box, bounds func(Box) (float64, float64)) []float64 {
	set := map[float64]bool{lo: true, hi: true}
	for _, c := range cells {
		a, b := bounds(c)
		set[clamp(a, lo, hi)] = true
		set[clamp(b, lo, hi)] = true
	}
	coords := make([]float64, 0, len(set))
	for x := range set {
		coords = append(coords, x)
	}
	sort.Float64s(coords)
	return coords
}

func clamp(x, lo, hi float64) float64 {
	if x < lo {
		return lo
	}
	if x > hi {
		return hi
	}
	return x
}

func TestCoverBoxWholeWorld(t *testing.T) {
	world := Box{MinLat: -90, MaxLat: 90, MinLng: -180, MaxLng: 180}
	cover := CoverBox(world, 12, 100)
	expected := []string{""}
	if !reflect.DeepEqual(cover, expected) {
		t.Fatalf("expected %v, got %v", expected, cover)
	}
}

func TestCoverBoxBudgetOne(t *testing.T) {
	query := Box{MinLat: 37.7, MaxLat: 37.8, MinLng: -122.5, MaxLng: -122.4}
	cover := CoverBox(query, 12, 1)
	if len(cover) != 1 {
		t.Fatalf("expected a single cell, got %v", cover)
	}
	// The single cell must cover the whole query box on its own.
	b := BoundingBox(cover[0])
	if !(b.MinLat <= query.MinLat && query.MaxLat <= b.MaxLat &&
		b.MinLng <= query.MinLng && query.MaxLng <= b.MaxLng) {
		t.Fatalf("single cell %q (%v) does not cover query %v", cover[0], b, query)
	}
	verifyCover(t, query, 12, 1, cover)
}

func TestCoverBoxContainedCellNotSubdivided(t *testing.T) {
	cell := BoundingBox("u4pr")
	dlat := 0.1 * (cell.MaxLat - cell.MinLat)
	dlng := 0.1 * (cell.MaxLng - cell.MinLng)
	query := Box{
		MinLat: cell.MinLat - dlat,
		MaxLat: cell.MaxLat + dlat,
		MinLng: cell.MinLng - dlng,
		MaxLng: cell.MaxLng + dlng,
	}
	cover := CoverBox(query, 7, 200)

	found := false
	lengths := map[int]bool{}
	for _, h := range cover {
		lengths[len(h)] = true
		if h == "u4pr" {
			found = true
		} else if strings.HasPrefix(h, "u4pr") {
			t.Fatalf("contained cell u4pr was subdivided into %q", h)
		}
	}
	if !found {
		t.Fatalf("cover %v does not contain u4pr", cover)
	}
	if len(lengths) < 2 {
		t.Fatalf("expected mixed precisions, got lengths %v", lengths)
	}
	verifyCover(t, query, 7, 200, cover)
}

func TestCoverBoxNearPole(t *testing.T) {
	query := Box{MinLat: 89.9, MaxLat: 90, MinLng: -10, MaxLng: 10}
	cover := CoverBox(query, 8, 64)
	verifyCover(t, query, 8, 64, cover)
}

func TestCoverBoxNarrow(t *testing.T) {
	query := Box{
		MinLat: 37.7749,
		MaxLat: 37.7749001,
		MinLng: -122.4194,
		MaxLng: -122.4193999,
	}
	cover := CoverBox(query, 12, 16)
	verifyCover(t, query, 12, 16, cover)
}

func TestCoverBoxAntimeridian(t *testing.T) {
	query := Box{MinLat: 10, MaxLat: 20, MinLng: 170, MaxLng: -170}
	cover := CoverBox(query, 6, 100)
	verifyCover(t, query, 6, 100, cover)

	east, west := false, false
	for _, h := range cover {
		_, lng := BoundingBox(h).Center()
		if lng > 0 {
			east = true
		} else {
			west = true
		}
	}
	if !east || !west {
		t.Fatalf("cover does not span both sides of the antimeridian: %v", cover)
	}
}

func TestCoverBoxBudgetFallback(t *testing.T) {
	query := Box{MinLat: 30, MaxLat: 50, MinLng: -120, MaxLng: -70}
	cover := CoverBox(query, 12, 10)
	verifyCover(t, query, 12, 10, cover)
}

func TestCoverBoxStableOrder(t *testing.T) {
	query := Box{MinLat: 40, MaxLat: 41, MinLng: 179, MaxLng: -179}
	first := CoverBox(query, 7, 50)
	second := CoverBox(query, 7, 50)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("non-deterministic result: %v vs %v", first, second)
	}
	verifyCover(t, query, 7, 50, first)
}
